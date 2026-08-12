// Package market reads active sell queues from other gift marketplaces.
//
// Floorline does not yet trade on these venues: moving a gift out of Tonnel's
// off-chain engine to sell it elsewhere takes minutes to hours plus gas, by
// which time any spread has gone.
//
// It does, however, price against them, and that is not a footnote. Our buyer
// can shop anywhere, so the cheapest ask across every venue — not the next ask
// on Tonnel — is what bounds our exit. A hole in the Tonnel book above a dense
// queue elsewhere is a hole, not liquidity, and reading it as room to sell into
// is what used to manufacture double-digit edges out of nothing.
//
// Because these quotes are load-bearing, a venue that cannot be reached is
// reported rather than skipped: "nobody answered" must never be mistaken for
// "nobody objected".
package market

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// userAgent is fixed per process and matched to the TLS profile below, for the
// same reason as in the Tonnel client: a stable fingerprint paired with a
// churning User-Agent is something no real browser produces.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// nanoGRAM is the denomination both Portals and MRKT quote integer prices in.
const nanoGRAM = 1_000_000_000

// Quote is one venue's live sell queue for a model.
type Quote struct {
	Venue string
	Floor float64
	// Asks is the actual cheapest sell queue, not a cached headline floor.
	Asks  []float64
	Scope string // exact attributes | model | floor only
	// Fee is that venue's sale commission, used only to annotate the quote.
	Fee float64
}

// Net is what the floor would actually pay out on that venue.
func (q Quote) Net() float64 { return q.Floor * (1 - q.Fee) }

// Reference is deliberately robust to one bait floor: with enough depth it is
// the median of the three cheapest real asks. This is a market sanity check,
// not a promise that a cross-venue transfer can execute instantly.
func (q Quote) Reference() float64 {
	if len(q.Asks) == 0 {
		return q.Floor
	}
	n := len(q.Asks)
	if n > 3 {
		n = 3
	}
	x := append([]float64(nil), q.Asks[:n]...)
	sort.Float64s(x)
	if n%2 == 1 {
		return x[n/2]
	}
	return (x[n/2-1] + x[n/2]) / 2
}

func (q Quote) NetReference() float64 { return q.Reference() * (1 - q.Fee) }

// Source is one marketplace we can read a model floor from.
type Source interface {
	Venue() string
	Enabled() bool
	Fee() float64
	// ModelFloor returns the cheapest ask for a model. A zero price with a nil
	// error means the venue simply has nothing listed; an error means it could
	// not be asked. Cards ignore the distinction, diagnostics do not.
	ModelFloor(ctx context.Context, collection, model string) (float64, error)
}

type AttributeSource interface {
	ModelFloorForAttributes(ctx context.Context, collection, model, backdrop, symbol string) (float64, error)
}

// DepthSource exposes live listings. Sources without it still contribute a
// floor, but depth-aware venues are preferred everywhere decisions are made.
type DepthSource interface {
	ModelAsks(ctx context.Context, collection, model, backdrop, symbol string, limit int) ([]float64, error)
}

// Comparison fans a lookup out across every configured venue.
type Comparison struct {
	sources []Source
}

// NewComparison keeps only the sources that have credentials.
func NewComparison(sources ...Source) *Comparison {
	c := &Comparison{}
	for _, s := range sources {
		if s != nil && s.Enabled() {
			c.sources = append(c.sources, s)
		}
	}
	return c
}

// Enabled reports whether any venue is configured.
func (c *Comparison) Enabled() bool { return c != nil && len(c.sources) > 0 }

// Venues lists the configured venue names, for /status.
func (c *Comparison) Venues() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.sources))
	for _, s := range c.sources {
		out = append(out, s.Venue())
	}
	return out
}

// Quotes gathers a floor from every venue in parallel, skipping the ones that
// have nothing to say. A slow or broken venue delays the card by at most the
// context deadline and never blocks the others.
func (c *Comparison) Quotes(ctx context.Context, collection, model string) []Quote {
	q, _ := c.QuotesForGift(ctx, collection, model, "", "")
	return q
}

// QuotesForGift asks venues with attribute-aware APIs for a tighter comparable
// and gracefully falls back to their model floor otherwise.
//
// The second return is how many venues could not be reached at all. That is not
// the same as a venue having nothing listed, and the difference decides whether
// the desk is allowed to trade unattended: cross-market depth is what caps an
// over-optimistic exit, so losing it to a timeout must not read as the market
// having no objection.
func (c *Comparison) QuotesForGift(ctx context.Context, collection, model, backdrop, symbol string) ([]Quote, int) {
	if !c.Enabled() {
		return nil, 0
	}

	results := make([]Quote, len(c.sources))
	failed := make([]bool, len(c.sources))
	var wg sync.WaitGroup
	for i, s := range c.sources {
		wg.Add(1)
		go func(i int, s Source) {
			defer wg.Done()
			var floor float64
			var asks []float64
			var err error
			scope := "floor only"
			if ds, ok := s.(DepthSource); ok {
				asks, err = ds.ModelAsks(ctx, collection, model, backdrop, symbol, 20)
				if err == nil && len(asks) == 0 && (backdrop != "" || symbol != "") {
					asks, err = ds.ModelAsks(ctx, collection, model, "", "", 20)
					scope = "model"
				} else if len(asks) > 0 && (backdrop != "" || symbol != "") {
					scope = "exact attributes"
				} else if len(asks) > 0 {
					scope = "model"
				}
				if len(asks) > 0 {
					floor = asks[0]
				}
			} else if as, ok := s.(AttributeSource); ok && (backdrop != "" || symbol != "") {
				floor, err = as.ModelFloorForAttributes(ctx, collection, model, backdrop, symbol)
			} else {
				floor, err = s.ModelFloor(ctx, collection, model)
			}
			if err != nil {
				failed[i] = true
				return
			}
			if floor > 0 {
				results[i] = Quote{Venue: s.Venue(), Floor: floor, Asks: asks, Scope: scope, Fee: s.Fee()}
			}
		}(i, s)
	}
	wg.Wait()

	// Preserve the configured order so the card does not reshuffle between refreshes.
	out := make([]Quote, 0, len(results))
	for _, q := range results {
		if q.Floor > 0 {
			out = append(out, q)
		}
	}
	n := 0
	for _, f := range failed {
		if f {
			n++
		}
	}
	return out, n
}

// Probing is the diagnostic form of Quotes: it keeps the per-venue error so
// "nothing listed here" can be told apart from "credentials rejected".
type Probing struct {
	Venue     string
	Floor     float64
	Asks      []float64
	Reference float64
	Err       error
}

// Probe queries every venue and reports exactly what each one said.
func (c *Comparison) Probe(ctx context.Context, collection, model string) []Probing {
	if !c.Enabled() {
		return nil
	}
	out := make([]Probing, len(c.sources))
	var wg sync.WaitGroup
	for i, s := range c.sources {
		wg.Add(1)
		go func(i int, s Source) {
			defer wg.Done()
			var floor float64
			var asks []float64
			var err error
			if ds, ok := s.(DepthSource); ok {
				asks, err = ds.ModelAsks(ctx, collection, model, "", "", 20)
				if len(asks) > 0 {
					floor = asks[0]
				}
			} else {
				floor, err = s.ModelFloor(ctx, collection, model)
			}
			q := Quote{Floor: floor, Asks: asks}
			out[i] = Probing{Venue: s.Venue(), Floor: floor, Asks: asks, Reference: q.Reference(), Err: err}
		}(i, s)
	}
	wg.Wait()
	return out
}

// newHTTPClient builds a browser-grade transport. Both venues sit behind the
// same kind of anti-bot layer as Tonnel.
func newHTTPClient(timeoutSeconds int) (tls_client.HttpClient, error) {
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeout(timeoutSeconds),
		tls_client.WithClientProfile(profiles.Chrome_131),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithCatchPanics(),
	)
}

// cache is a tiny TTL cache that also remembers failures, so a venue that is
// down is not retried on every single signal card.
type cache[T any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]*cacheEntry[T]
}

type cacheEntry[T any] struct {
	mu    sync.Mutex
	value T
	err   error
	at    time.Time
}

func newCache[T any](ttl time.Duration) *cache[T] {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &cache[T]{ttl: ttl, entries: make(map[string]*cacheEntry[T])}
}

// get returns a cached value or loads it, collapsing concurrent lookups of the
// same key into one request.
func (c *cache[T]) get(key string, load func() (T, error)) (T, error) {
	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok {
		e = &cacheEntry[T]{}
		c.entries[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.at.IsZero() && time.Since(e.at) < c.ttl {
		return e.value, e.err
	}
	e.value, e.err = load()
	e.at = time.Now()
	return e.value, e.err
}

// matchKey normalises a name for cross-venue matching. The marketplaces do not
// agree on capitalisation or spacing for the same model.
func matchKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// nanoToGRAM converts an integer nanoGRAM price to GRAM.
func nanoToGRAM(n int64) float64 { return float64(n) / nanoGRAM }

// errNoCredentials is returned by a source that was asked despite being disabled.
var errNoCredentials = errors.New("no credentials configured")
