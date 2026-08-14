package tonnel

import (
	"encoding/json"
	"strings"
)

// The marketplace event API. This is Tonnel's *public*, documented feed —
// https://gifts.coffin.meme/MARKETPLACE_EVENTS.md — and it is the reason this
// file exists at all.
//
// Everything else in this package talks to the private endpoints behind
// Cloudflare, which refuse a poller long before they refuse a browser. Reading
// the newest listings by asking pageGifts every two seconds is what produced
// the 403 storms: thirty reads a minute of the same page, from one address, for
// data the marketplace is willing to push to us for free.
//
// So the live feed moves here. The private endpoints stay for what the event
// stream deliberately does not carry — the order book, the market snapshot, our
// own inventory, and the writes — and the read budget they need is now
// available to them.

// EventHost serves both the live socket and the replay endpoint. It is the same
// front end the writes already go to, and it is not behind the challenge that
// keeps refusing gifts2/gifts3.
const EventHost = "gifts.coffin.meme"

// Event types we act on. The catalogue is larger — auctions, offers, bundles,
// trades, premarket — and unknown types are ignored rather than rejected, which
// is what the contract asks consumers to do.
const (
	EventListingCreated      = "listing.created"
	EventListingPriceChanged = "listing.price_changed"
	EventListingCancelled    = "listing.cancelled"
	EventSaleCompleted       = "sale.completed"
	EventGiftIndexed         = "gift.indexed"
)

// EventGift is the public gift identity carried by most events. The feed
// exposes no seller, buyer or bidder — that is a deliberate privacy contract,
// not an omission to work around.
type EventGift struct {
	GiftID   FlexInt `json:"gift_id"`
	GiftNum  FlexInt `json:"gift_num"`
	GiftName string  `json:"gift_name"`
	Model    string  `json:"model"`
	Backdrop string  `json:"backdrop"`
	Symbol   string  `json:"symbol"`
}

// EventData is the union of every payload shape in the catalogue. One struct
// rather than a discriminated union: the fields that matter are shared across
// most types, and absent ones simply decode as zero.
type EventData struct {
	Gift *EventGift `json:"gift"`
	// GiftID is set by the events that identify a gift without describing it
	// (cancelled auctions, rejected offers).
	GiftID FlexInt `json:"gift_id"`

	Price         Flex64 `json:"price"`
	PreviousPrice Flex64 `json:"previous_price"`
	WinningBid    Flex64 `json:"winning_bid"`
	Asset         string `json:"asset"`

	SaleType string `json:"sale_type"` // FIXED | DUTCH
	Source   string `json:"source"`    // LISTING | DUTCH | BUY_OFFER | AUCTION
	Market   string `json:"market"`    // PREMARKET on gift.indexed
	Status   string `json:"status"`
}

// Event is the envelope every marketplace event arrives in.
type Event struct {
	EventID    string    `json:"eventId"`
	Version    int       `json:"version"`
	Type       string    `json:"type"`
	OccurredAt FlexTime  `json:"occurredAt"`
	Data       EventData `json:"data"`
}

// DecodeEvent parses one frame. The second result is false for anything that is
// not a marketplace event — notably the `marketplace.connected` greeting, which
// carries no eventId and must never be stored or acted on.
func DecodeEvent(b []byte) (Event, bool) {
	var ev Event
	if err := json.Unmarshal(b, &ev); err != nil {
		return Event{}, false
	}
	if ev.EventID == "" || ev.Type == "" {
		return Event{}, false
	}
	return ev, true
}

// GiftID returns the gift an event is about, whichever way it identifies it.
func (e Event) GiftID() int64 {
	if e.Data.Gift != nil && e.Data.Gift.GiftID.Int() != 0 {
		return e.Data.Gift.GiftID.Int()
	}
	return e.Data.GiftID.Int()
}

// Listing turns a listing event into an order-book row, the same shape
// pageGifts produces, so everything downstream is unaware of which one it came
// from.
//
// Three fields the private endpoint carries are absent here and cannot be
// invented: the seller, the numeric rarities, and export_at. The rarities are
// recovered from the "(0.4%)" suffix on the attribute values, which is where
// the rest of the code already reads them from. The seller stays zero — the
// feed hides identities by design — so the own-listing guard has to come from
// our own position book instead of from the row. See app.ownListing.
func (e Event) Listing() (Gift, bool) {
	g := e.Data.Gift
	if g == nil || g.GiftID.Int() == 0 {
		return Gift{}, false
	}
	price := e.Data.Price
	if price <= 0 {
		return Gift{}, false
	}
	asset := e.Data.Asset
	if asset == "" {
		asset = AssetGRAM
	}
	_, modelRarity := SplitAttr(g.Model)
	_, backdropRarity := SplitAttr(g.Backdrop)
	_, symbolRarity := SplitAttr(g.Symbol)

	return Gift{
		GiftID:   g.GiftID,
		GiftNum:  g.GiftNum,
		Name:     g.GiftName,
		Model:    g.Model,
		Backdrop: g.Backdrop,
		Symbol:   g.Symbol,

		Price: price,
		Asset: asset,

		ModelRarity:    Flex64(modelRarity),
		BackdropRarity: Flex64(backdropRarity),
		SymbolRarity:   Flex64(symbolRarity),

		Status: "forsale",
		// The event's own timestamp is when the ask appeared, which is exactly
		// what message_post_time means on the private endpoint.
		MessagePostTime: FlexTime{Time: e.OccurredAt.Time},
	}, true
}

// Sale turns a completed sale into a trade-tape row.
//
// The tape's `type` column is written for the record only — nothing filters on
// it — so the event's `source` is mapped onto the two spellings saleHistory
// uses for the same trades, and an unknown source is recorded as a plain sale
// rather than dropped.
func (e Event) Sale() (Sale, bool) {
	if e.Type != EventSaleCompleted {
		return Sale{}, false
	}
	g := e.Data.Gift
	if g == nil || g.GiftID.Int() == 0 {
		return Sale{}, false
	}
	price := e.Data.Price
	if price <= 0 {
		price = e.Data.WinningBid
	}
	if price <= 0 || e.OccurredAt.IsZero() {
		return Sale{}, false
	}

	kind := "SALE"
	if strings.EqualFold(e.Data.Source, "BUY_OFFER") {
		kind = "INTERNAL_SALE"
	}
	return Sale{
		GiftID:    g.GiftID,
		GiftNum:   g.GiftNum,
		GiftName:  g.GiftName,
		Model:     g.Model,
		Backdrop:  g.Backdrop,
		Symbol:    g.Symbol,
		Price:     price,
		Asset:     e.Data.Asset,
		Type:      kind,
		Timestamp: e.OccurredAt,
	}, true
}

// GramPriced reports whether an event is denominated in the currency the desk
// trades. USDT and TONNEL listings are a separate book with a separate floor,
// and mixing them into the tape corrupts every median it feeds.
func (e Event) GramPriced() bool {
	return e.Data.Asset == "" || e.Data.Asset == AssetGRAM
}
