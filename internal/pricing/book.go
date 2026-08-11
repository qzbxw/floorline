package pricing

import (
	"context"
	"sort"
	"sync"
	"time"

	"floorline/internal/tonnel"
)

// Ask is one competing offer in a model's order book.
type Ask struct {
	GiftID   int64
	GiftNum  int64
	Price    float64
	Backdrop string
	Symbol   string
	Seller   int64
}

// Book is the cheap end of one model's order book at a point in time.
type Book struct {
	Key       tonnel.ModelKey
	Asks      []Ask // ascending by price
	FetchedAt time.Time
}

// ExternalAsks returns the live depth that belongs to the market, not to us.
// The slice is a copy so callers can safely keep it while the cache refreshes.
func (b *Book) ExternalAsks(giftID, ownerID int64) []Ask {
	out := make([]Ask, 0, len(b.Asks))
	for _, a := range b.Asks {
		if a.GiftID == giftID || (ownerID != 0 && a.Seller == ownerID) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// NewBook builds a Book from raw API rows, dropping everything that would
// corrupt the price picture: bundles price a whole pack, premarket rows are not
// deliverable gifts, and Telegram-marketplace rows settle differently.
func NewBook(key tonnel.ModelKey, gifts []tonnel.Gift, at time.Time) *Book {
	b := &Book{Key: key, FetchedAt: at}
	for i := range gifts {
		g := &gifts[i]
		if g.IsBundle() || g.Premarket.Bool() || g.TelegramMarketplace.Bool() {
			continue
		}
		if g.Price.Float() <= 0 || g.Buyer != nil {
			continue
		}
		b.Asks = append(b.Asks, Ask{
			GiftID:   g.GiftID.Int(),
			GiftNum:  g.GiftNum.Int(),
			Price:    g.Price.Float(),
			Backdrop: tonnel.BaseAttr(g.Backdrop),
			Symbol:   tonnel.BaseAttr(g.Symbol),
			Seller:   g.Seller.Int(),
		})
	}
	sort.Slice(b.Asks, func(i, j int) bool { return b.Asks[i].Price < b.Asks[j].Price })
	return b
}

// BestExcluding returns the cheapest genuinely competing ask. The listing
// being evaluated and every ask owned by ownerID are excluded: undercutting
// our own inventory is not market depth and must never justify a purchase.
//
// This is the number that actually matters when buying the floor: once you own
// the cheapest lot, the price you must beat to sell is the *next* one, not the
// floor you just bought.
func (b *Book) BestExcluding(giftID, ownerID int64) (float64, bool) {
	for _, a := range b.Asks {
		if a.GiftID == giftID || (ownerID != 0 && a.Seller == ownerID) {
			continue
		}
		return a.Price, true
	}
	return 0, false
}

// BestAttributesExcluding returns the cheapest ask matching every non-empty
// attribute. It powers the patient, collector-facing exit without confusing a
// generic model floor with a genuinely comparable gift.
func (b *Book) BestAttributesExcluding(giftID, ownerID int64, backdrop, symbol string) (float64, bool) {
	for _, a := range b.Asks {
		if a.GiftID == giftID || (ownerID != 0 && a.Seller == ownerID) {
			continue
		}
		if backdrop != "" && a.Backdrop != backdrop {
			continue
		}
		if symbol != "" && a.Symbol != symbol {
			continue
		}
		return a.Price, true
	}
	return 0, false
}

func (b *Book) CountAttributesBetween(lo, hi float64, excludeID, ownerID int64, backdrop, symbol string) int {
	n := 0
	for _, a := range b.Asks {
		if a.GiftID == excludeID || (ownerID != 0 && a.Seller == ownerID) || a.Price < lo || a.Price > hi {
			continue
		}
		if backdrop != "" && a.Backdrop != backdrop {
			continue
		}
		if symbol != "" && a.Symbol != symbol {
			continue
		}
		n++
	}
	return n
}

// CountBetween counts asks in [lo, hi], excluding one gift.
func (b *Book) CountBetween(lo, hi float64, excludeID, ownerID int64) int {
	n := 0
	for _, a := range b.Asks {
		if a.GiftID == excludeID || (ownerID != 0 && a.Seller == ownerID) {
			continue
		}
		if a.Price >= lo && a.Price <= hi {
			n++
		}
	}
	return n
}

// Len returns the number of asks held.
func (b *Book) Len() int { return len(b.Asks) }

// IDs returns every gift id in the book, for lifetime bookkeeping.
func (b *Book) IDs() []int64 {
	out := make([]int64, 0, len(b.Asks))
	for _, a := range b.Asks {
		out = append(out, a.GiftID)
	}
	return out
}

// BookFetcher loads a model's order book from the marketplace.
type BookFetcher interface {
	ModelBook(ctx context.Context, key tonnel.ModelKey, limit int) ([]tonnel.Gift, error)
}

// BookCache fetches model books on demand and caches them briefly.
//
// The feed produces candidates far faster than the rate limiter allows book
// lookups, and a burst of listings in one hot model would otherwise fire the
// same query repeatedly. Single-flight plus a short TTL keeps that to one call.
type BookCache struct {
	fetcher BookFetcher
	ttl     time.Duration
	limit   int

	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	mu   sync.Mutex
	book *Book
	err  error
	at   time.Time
}

// NewBookCache builds a cache with the given freshness window.
func NewBookCache(f BookFetcher, ttl time.Duration, limit int) *BookCache {
	if limit <= 0 {
		limit = 10
	}
	return &BookCache{
		fetcher: f,
		ttl:     ttl,
		limit:   limit,
		entries: make(map[string]*cacheEntry),
	}
}

// Get returns a book no older than the cache TTL, fetching it if needed.
func (c *BookCache) Get(ctx context.Context, key tonnel.ModelKey) (*Book, error) {
	c.mu.Lock()
	e, ok := c.entries[key.ID()]
	if !ok {
		e = &cacheEntry{}
		c.entries[key.ID()] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.book != nil && time.Since(e.at) < c.ttl {
		return e.book, nil
	}
	if e.err != nil && time.Since(e.at) < c.ttl {
		return nil, e.err // do not retry a failing model faster than the TTL
	}

	gifts, err := c.fetcher.ModelBook(ctx, key, c.limit)
	e.at = time.Now()
	if err != nil {
		e.err, e.book = err, nil
		return nil, err
	}
	e.err = nil
	e.book = NewBook(key, gifts, e.at)
	return e.book, nil
}

// Invalidate drops a cached book, e.g. right after we bought out of it.
func (c *BookCache) Invalidate(key tonnel.ModelKey) {
	c.mu.Lock()
	delete(c.entries, key.ID())
	c.mu.Unlock()
}
