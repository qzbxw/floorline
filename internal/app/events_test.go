package app

import (
	"context"
	"testing"
	"time"

	"floorline/internal/store"
	"floorline/internal/tonnel"
)

func streamApp(t *testing.T) *App {
	t.Helper()
	a := coolingApp(t)
	a.evalQ = make(chan tonnel.Gift, 8)
	return a
}

func listingEvent(t *testing.T, raw string) tonnel.Event {
	t.Helper()
	ev, ok := tonnel.DecodeEvent([]byte(raw))
	if !ok {
		t.Fatalf("could not decode %s", raw)
	}
	return ev
}

const streamedListing = `{"eventId":"a","version":1,"type":"listing.created","occurredAt":"2026-08-13T11:38:09.888Z","data":{"gift":{"gift_id":77,"gift_num":12,"gift_name":"Lol Pop","model":"Blood Sucker (1%)","backdrop":"Black (1.5%)","symbol":"Bat (0.5%)"},"price":3.2,"asset":"TON","sale_type":"FIXED"}}`

func TestStreamedListingIsStoredAndQueued(t *testing.T) {
	ctx := context.Background()
	a := streamApp(t)

	if err := a.handleEvent(ctx, listingEvent(t, streamedListing)); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-a.evalQ:
		if g.GiftID.Int() != 77 || g.Price.Float() != 3.2 {
			t.Fatalf("queued %+v", g)
		}
	default:
		t.Fatal("a new ask was not queued for valuation")
	}
	if seen, err := a.st.ListingFirstSeen(ctx, 77); err != nil || seen.IsZero() {
		t.Fatalf("listing not stored: %v %v", seen, err)
	}
}

// The event feed carries no seller — identities are withheld by design — so the
// guard that used to compare against our own user id has to come from the
// position book. Without it the desk prices its own asks as opportunities and,
// with unattended buying armed, bids for them.
func TestOwnListingIsNeverQueued(t *testing.T) {
	ctx := context.Background()
	a := streamApp(t)

	err := a.st.UpsertPosition(ctx, store.Position{
		GiftID: 77, GiftNum: 12,
		Key:      tonnel.ModelKey{Name: "Lol Pop", Model: "Blood Sucker"},
		BuyPrice: 2.9, BoughtAt: time.Now().Add(-time.Hour),
		ListPrice: 3.2, ListedAt: time.Now(),
		Status: store.StatusListed, Source: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := a.handleEvent(ctx, listingEvent(t, streamedListing)); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-a.evalQ:
		t.Fatalf("queued our own listing %d for purchase", g.GiftID.Int())
	default:
	}
	// It is still recorded: the book and the crowd counts need to know the ask
	// is standing, whoever placed it.
	if seen, err := a.st.ListingFirstSeen(ctx, 77); err != nil || seen.IsZero() {
		t.Fatalf("our own listing was not recorded: %v %v", seen, err)
	}
}

func TestStreamedSaleLandsOnTheTapeAndClosesTheAsk(t *testing.T) {
	ctx := context.Background()
	a := streamApp(t)

	if err := a.handleEvent(ctx, listingEvent(t, streamedListing)); err != nil {
		t.Fatal(err)
	}
	<-a.evalQ

	sale := `{"eventId":"b","version":1,"type":"sale.completed","occurredAt":"2026-08-13T11:39:00.000Z","data":{"gift":{"gift_id":77,"gift_num":12,"gift_name":"Lol Pop","model":"Blood Sucker (1%)","backdrop":"Black (1.5%)","symbol":"Bat (0.5%)"},"price":3.2,"asset":"TON","source":"LISTING"}}`
	if err := a.handleEvent(ctx, listingEvent(t, sale)); err != nil {
		t.Fatal(err)
	}

	key := tonnel.ModelKey{Name: "Lol Pop", Model: "Blood Sucker"}
	rows, err := a.st.SalesSince(ctx, key, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Price != 3.2 {
		t.Fatalf("tape = %+v", rows)
	}

	var gone *int64
	if err := a.st.DB().QueryRowContext(ctx, `SELECT gone_at FROM listings WHERE gift_id=77`).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	if gone == nil {
		t.Fatal("a sold gift is still standing in the book as a competing ask")
	}
}

// A cancelled ask stops being competition the moment it is withdrawn. The book
// sweep could only ever notice this by finding the gift absent from a page it
// happened to read.
func TestCancelledListingIsClosedImmediately(t *testing.T) {
	ctx := context.Background()
	a := streamApp(t)
	if err := a.handleEvent(ctx, listingEvent(t, streamedListing)); err != nil {
		t.Fatal(err)
	}
	<-a.evalQ

	cancel := `{"eventId":"c","version":1,"type":"listing.cancelled","occurredAt":"2026-08-13T11:40:00.000Z","data":{"gift":{"gift_id":77,"gift_num":12,"gift_name":"Lol Pop","model":"Blood Sucker (1%)","backdrop":"Black (1.5%)","symbol":"Bat (0.5%)"},"price":3.2,"asset":"TON","sale_type":"FIXED"}}`
	if err := a.handleEvent(ctx, listingEvent(t, cancel)); err != nil {
		t.Fatal(err)
	}
	var gone *int64
	if err := a.st.DB().QueryRowContext(ctx, `SELECT gone_at FROM listings WHERE gift_id=77`).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	if gone == nil {
		t.Fatal("a cancelled ask is still counted as competition")
	}
}

// USDT and TONNEL listings are a separate book with a separate floor. Letting
// one onto the tape corrupts every median computed from it.
func TestForeignCurrencyEventsAreIgnored(t *testing.T) {
	ctx := context.Background()
	a := streamApp(t)

	usdt := `{"eventId":"d","version":1,"type":"listing.created","occurredAt":"2026-08-13T11:38:09.888Z","data":{"gift":{"gift_id":99,"gift_num":1,"gift_name":"Lol Pop","model":"Blood Sucker (1%)"},"price":9,"asset":"USDT","sale_type":"FIXED"}}`
	if err := a.handleEvent(ctx, listingEvent(t, usdt)); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-a.evalQ:
		t.Fatalf("queued a %s listing", g.Asset)
	default:
	}
	if seen, _ := a.st.ListingFirstSeen(ctx, 99); !seen.IsZero() {
		t.Fatal("a USDT listing was written into the GRAM book")
	}
}

// A full queue must never block the socket: a slow consumer is disconnected
// server-side, which costs the whole feed rather than one listing.
func TestAFullQueueDropsRatherThanBlocks(t *testing.T) {
	ctx := context.Background()
	a := streamApp(t)
	for i := 0; i < cap(a.evalQ); i++ {
		a.evalQ <- tonnel.Gift{GiftID: tonnel.FlexInt(1000 + i)}
	}

	done := make(chan error, 1)
	go func() { done <- a.handleEvent(ctx, listingEvent(t, streamedListing)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event handler blocked on a full evaluation queue")
	}
}
