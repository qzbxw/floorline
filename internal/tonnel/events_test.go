package tonnel

import (
	"testing"
	"time"
)

// Captured verbatim from https://gifts.coffin.meme/api/marketplace/events.
const (
	rawListingCreated = `{"eventId":"e6c557fe-6bd5-42cd-ba3b-6bc867998f68","version":1,"type":"listing.created","occurredAt":"2026-08-13T11:38:09.888Z","data":{"gift":{"gift_id":10377127,"gift_num":12627,"gift_name":"Loot Bag","model":"Crypto Byte (0.5%)","backdrop":"Coral Red (1.5%)","symbol":"Clubs (0.2%)"},"price":450,"asset":"TON","sale_type":"FIXED"}}`
	rawSaleCompleted  = `{"eventId":"bb5b4bf9-6095-4338-a169-8815b1321b09","version":1,"type":"sale.completed","occurredAt":"2026-08-13T11:38:06.927Z","data":{"gift":{"gift_id":10363976,"gift_num":129738,"gift_name":"Ice Cream","model":"Cyclone","backdrop":"Pacific Green (1.5%)","symbol":"Moon Eagle (0.4%)"},"price":3.49,"asset":"TON","source":"LISTING"}}`
	rawAuctionCancel  = `{"eventId":"73b0e070-d6f6-4c80-839a-769c038ee121","version":1,"type":"auction.cancelled","occurredAt":"2026-08-13T11:38:13.466Z","data":{"auction_id":"NZI88LY1","gift_id":10384940}}`
	rawGreeting       = `{"type":"marketplace.connected","version":1,"serverTime":"2026-08-13T10:00:00.000Z","replayEndpoint":"/api/marketplace/events"}`
)

func TestDecodeEventRejectsTheGreeting(t *testing.T) {
	// It has no eventId. Storing it would advance the cursor to a value the
	// replay endpoint has never heard of.
	if _, ok := DecodeEvent([]byte(rawGreeting)); ok {
		t.Fatal("the connected greeting decoded as a marketplace event")
	}
	if _, ok := DecodeEvent([]byte("not json")); ok {
		t.Fatal("garbage decoded as an event")
	}
}

func TestListingConversion(t *testing.T) {
	ev, ok := DecodeEvent([]byte(rawListingCreated))
	if !ok {
		t.Fatal("listing.created did not decode")
	}
	if ev.GiftID() != 10377127 {
		t.Fatalf("gift id = %d", ev.GiftID())
	}

	g, ok := ev.Listing()
	if !ok {
		t.Fatal("listing.created did not convert to a gift")
	}
	if g.Name != "Loot Bag" || g.Key().Model != "Crypto Byte" {
		t.Fatalf("key = %s", g.Key())
	}
	if g.Price.Float() != 450 || g.Asset != AssetGRAM {
		t.Fatalf("price = %v %s", g.Price.Float(), g.Asset)
	}
	// The event carries no numeric rarity fields, only the suffix on the value.
	// Recovering them here is what keeps a streamed row indistinguishable from
	// a polled one everywhere downstream.
	if g.ModelRarity.Float() != 0.5 || g.BackdropRarity.Float() != 1.5 || g.SymbolRarity.Float() != 0.2 {
		t.Fatalf("rarities = %v/%v/%v", g.ModelRarity.Float(), g.BackdropRarity.Float(), g.SymbolRarity.Float())
	}
	if g.Status != "forsale" {
		t.Fatalf("status = %q", g.Status)
	}
	if !g.MessagePostTime.Equal(time.Date(2026, 8, 13, 11, 38, 9, 888000000, time.UTC)) {
		t.Fatalf("posted at = %v", g.MessagePostTime)
	}
	// The privacy contract: no identity is available, and pretending otherwise
	// would make the own-listing guard silently wrong.
	if g.Seller.Int() != 0 {
		t.Fatalf("seller = %d, the feed does not carry one", g.Seller.Int())
	}
}

func TestSaleConversion(t *testing.T) {
	ev, _ := DecodeEvent([]byte(rawSaleCompleted))
	s, ok := ev.Sale()
	if !ok {
		t.Fatal("sale.completed did not convert to a trade")
	}
	if s.Name() != "Ice Cream" || s.Key().Model != "Cyclone" {
		t.Fatalf("key = %s", s.Key())
	}
	if s.Price.Float() != 3.49 {
		t.Fatalf("price = %v", s.Price.Float())
	}
	if s.When().IsZero() {
		t.Fatal("trade has no timestamp; InsertSales would drop it")
	}
	if s.Type != "SALE" {
		t.Fatalf("type = %q", s.Type)
	}

	// A listing event is not a trade, however similar the payload looks.
	other, _ := DecodeEvent([]byte(rawListingCreated))
	if _, ok := other.Sale(); ok {
		t.Fatal("listing.created converted to a trade")
	}
}

func TestBuyOfferSaleIsRecordedAsInternal(t *testing.T) {
	ev, _ := DecodeEvent([]byte(`{"eventId":"x","version":1,"type":"sale.completed","occurredAt":"2026-08-13T11:38:06.927Z","data":{"gift":{"gift_id":1,"gift_num":2,"gift_name":"Lol Pop","model":"Blood Sucker (1%)","backdrop":"Black (1.5%)","symbol":"Bat (0.5%)"},"price":2.5,"asset":"TON","source":"BUY_OFFER"}}`))
	s, ok := ev.Sale()
	if !ok {
		t.Fatal("buy-offer sale did not convert")
	}
	if s.Type != "INTERNAL_SALE" {
		t.Fatalf("type = %q, want the spelling saleHistory uses for the same trade", s.Type)
	}
}

func TestEventsWithoutAGiftAreIgnorable(t *testing.T) {
	ev, ok := DecodeEvent([]byte(rawAuctionCancel))
	if !ok {
		t.Fatal("auction.cancelled did not decode")
	}
	if ev.GiftID() != 10384940 {
		t.Fatalf("gift id = %d", ev.GiftID())
	}
	if _, ok := ev.Listing(); ok {
		t.Fatal("an event with no gift produced a listing")
	}
}

func TestGramPriced(t *testing.T) {
	usdt, _ := DecodeEvent([]byte(`{"eventId":"u","type":"listing.created","occurredAt":"2026-08-13T11:38:09.888Z","data":{"gift":{"gift_id":1},"price":5,"asset":"USDT"}}`))
	if usdt.GramPriced() {
		t.Fatal("a USDT listing counted as GRAM; it has its own book and its own floor")
	}
	gram, _ := DecodeEvent([]byte(rawListingCreated))
	if !gram.GramPriced() {
		t.Fatal("a TON listing did not count as GRAM")
	}
}
