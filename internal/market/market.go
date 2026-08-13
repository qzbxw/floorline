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
	"golang.org/x/time/rate"
)

// userAgent is fixed per process and matched to the TLS profile below, for the
// same reason as in the Tonnel client: a stable fingerprint paired with a
// churning User-Agent is something no real browser produces.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// nanoGRAM is the denomination both Portals and MRKT quote integer prices in.
const nanoGRAM = 1_000_000_000

// crossGapLimit is how far one external ask may stand above the previous one and
// still belong to the same pool of liquidity.
//
// It is looser than the local one because these venues are thinner and their
// queues are genuinely steppier. It is not loose enough to matter for the case
// it exists for: a queue reading 12.2 / 306 is one listing and one fantasy.
const crossGapLimit = 0.40

// minReferenceDepth is how many contiguous asks a venue needs before its prices
// are allowed to mean anything about what a model is worth.
//
// One ask is not a market, it is a price somebody typed. The old reference was
// the median of the first three *whatever they were*, so a queue of 12.2 and 306
// produced a "reference" of 159.1 — the average of a real listing and a
// fantasy — and that number then carried a fifth of the weight in price
// discovery and printed on the card as "+4067% to entry".
const minReferenceDepth = 2

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

// Depth is how many of the cheapest asks the reference is allowed to look at.
func (q Quote) Depth() int {
	if len(q.Asks) > 3 {
		return 3
	}
	return len(q.Asks)
}

// Reference is what this venue says the model is worth, or zero when it has not
// said anything a price can be built on.
//
// Three asks are enough to be robust in both directions at once, which is the
// whole difficulty: one abandoned listing far *above* the market must not drag
// the number up, and one bait listing far *below* it must not drag the number
// down. The median of the three cheapest does both.
//
// Two asks cannot do that, and pretending otherwise is what produced the worst
// number this bot has ever printed. Averaging them turned a queue of 12.2 and
// 306 into a "reference" of 159.1 — the midpoint of a real listing and a
// fantasy — which then carried a fifth of the weight in price discovery and
// appeared on the card as "+4067% to entry". So two asks only speak when they
// agree with each other, and then they speak with the cheaper one.
//
// One ask says nothing at all. It is not a market, it is a price somebody typed.
// The floor is still shown on the card; it is simply not evidence of a value.
func (q Quote) Reference() float64 {
	x := q.sorted()
	switch {
	case len(x) < minReferenceDepth:
		return 0
	case len(x) == 2:
		if x[0] <= 0 || x[1] > x[0]*(1+crossGapLimit) {
			return 0 // they are not describing the same market
		}
		return x[0]
	default:
		return x[1] // median of the three cheapest
	}
}

// Anchor is the cheapest thing actually on offer here. Unlike Reference it does
// not need depth, because "somebody will sell you one at this price" is a fact
// about execution even when it is a lonely one.
func (q Quote) Anchor() float64 {
	if len(q.Asks) == 0 {
		return q.Floor
	}
	return q.sorted()[0]
}

func (q Quote) sorted() []float64 {
	x := append([]float64(nil), q.Asks...)
	sort.Float64s(x)
	return x
}

func (q Quote) NetReference() float64 { return q.Reference() * (1 - q.Fee) }

// InitDataSource mints a Telegram mini-app payload on demand. It is satisfied
// by *tgsession.Client; the interface keeps this package free of the MTProto
// dependency and lets the venues be tested without one.
type InitDataSource interface {
	InitData(ctx context.Context, venue string) (string, error)
	Invalidate(venue string)
	// Ready reports whether the account has actually been signed in. Holding
	// app credentials is not the same as having a session: before `floorline
	// login` there is nothing to mint a mini-app payload with, and a venue that
	// depends on one is *not configured* rather than *unreachable*.
	//
	// The difference decides money. Unreachable is a hard auto-buy block and a
	// heavy score penalty, on the sound principle that a venue we could not read
	// might have objected. A venue we never set up cannot have objected, and
	// treating the two alike meant that merely putting TELEGRAM_APP_ID in the
	// environment silently stopped the desk from trading.
	Ready() bool
}

// humanPace is the request budget for a venue.
//
// MRKT banned this account for two weeks for reading its API without going
// through the mini app, and the request rate is half of what gave that away: a
// person tapping through listings does not sustain a request a second, and
// never does so at four in the morning with no gaps. One request every two
// seconds with a small burst is roughly a fast human, and it is still far more
// throughput than the desk needs.
func humanPace() *rate.Limiter {
	return rate.NewLimiter(rate.Every(2*time.Second), 3)
}

// publicPace is for a venue whose read endpoints are open and which has never
// objected to being read.
//
// Portals answers /nfts/search without credentials, so the caution that
// humanPace exists for — MRKT banning an account for API-shaped traffic —
// does not apply to it. Pacing them identically was costing real money: the
// desk prices six positions at once, each needing up to three queries down the
// exact → backdrop → model ladder, and at one request every two seconds that
// queue could not finish inside the cross-market deadline. Every card then read
// "площадки не ответили", which is not cosmetic — it caps the score hard and
// blocks unattended buying, because a venue we could not read is a venue whose
// objection we did not hear.
func publicPace() *rate.Limiter {
	return rate.NewLimiter(rate.Every(400*time.Millisecond), 5)
}

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
				asks, scope, err = askLadder(ctx, ds, collection, model, backdrop, symbol)
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

// Scope values, ordered from the tightest comparable to the loosest. They
// travel with the quote because the difference decides money: a queue matched
// only on the model is a different asset from the gift being priced whenever
// its backdrop is what makes it worth anything.
const (
	ScopeExact    = "exact attributes"
	ScopeBackdrop = "backdrop"
	ScopeModel    = "model"
	ScopeFloor    = "floor only"
)

// Comparable reports whether a quote is tight enough to bound the exit of a
// gift with distinctive attributes, rather than merely inform it.
func Comparable(scope string) bool { return scope == ScopeExact || scope == ScopeBackdrop }

// askLadder walks from the tightest comparable down to the loosest.
//
// The middle rung is the point. Backdrop is the dominant price driver on this
// market — an Onyx Black specimen is not the same asset as the ordinary model,
// whatever they share — and the exact backdrop+symbol pair is often the
// sparsest bucket there is, so requiring both means the common outcome is no
// match at all. Dropping straight from there to the whole model queue priced
// Onyx gifts off ordinary ones, and after the exit began keying on the cheapest
// external offer that stopped being a cosmetic problem on the card and became
// the number the trade is decided by.
func askLadder(ctx context.Context, ds DepthSource, collection, model, backdrop, symbol string) ([]float64, string, error) {
	if backdrop != "" || symbol != "" {
		asks, err := ds.ModelAsks(ctx, collection, model, backdrop, symbol, 20)
		if err != nil {
			return nil, ScopeFloor, err
		}
		if len(asks) > 0 {
			return asks, ScopeExact, nil
		}
	}
	if backdrop != "" {
		asks, err := ds.ModelAsks(ctx, collection, model, backdrop, "", 20)
		if err != nil {
			return nil, ScopeFloor, err
		}
		if len(asks) > 0 {
			return asks, ScopeBackdrop, nil
		}
	}
	asks, err := ds.ModelAsks(ctx, collection, model, "", "", 20)
	if err != nil {
		return nil, ScopeFloor, err
	}
	return asks, ScopeModel, nil
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
//
// A failure caused by the caller's own deadline is deliberately not stored. Our
// budget running out says nothing about the venue, and remembering it as an
// answer was expensive in a way that did not look like a cache problem: one card
// rendered while the desk was busy left "this venue did not answer" in place for
// the whole TTL, and that is not a cosmetic gap — it is the cap that holds an
// optimistic exit down, a heavy score penalty, and a hard block on unattended
// buying, applied to every listing priced in the next five minutes.
func (c *cache[T]) get(ctx context.Context, key string, load func() (T, error)) (T, error) {
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
	value, err := load()
	// ctx is consulted as well as the error itself because a rate limiter that
	// gives up reports its own refusal ("would exceed context deadline"), not the
	// context's — and that is the most common way a venue read ends when several
	// cards are priced at once.
	if err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return value, err
	}
	e.value, e.err = value, err
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
