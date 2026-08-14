package market

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource is a scriptable venue.
type fakeSource struct {
	name    string
	fee     float64
	floor   float64
	err     error
	enabled bool
	delay   time.Duration
	calls   atomic.Int32
}

type fakeDepthSource struct {
	fakeSource
	asks []float64
}

func (f *fakeDepthSource) ModelAsks(context.Context, string, string, string, string, int) ([]float64, error) {
	return append([]float64(nil), f.asks...), f.err
}

func (f *fakeSource) Venue() string { return f.name }
func (f *fakeSource) Enabled() bool { return f.enabled }
func (f *fakeSource) Fee() float64  { return f.fee }
func (f *fakeSource) ModelFloor(ctx context.Context, collection, model string) (float64, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return f.floor, f.err
}

func TestComparisonDropsDisabledSources(t *testing.T) {
	on := &fakeSource{name: "On", enabled: true, floor: 100}
	off := &fakeSource{name: "Off", enabled: false, floor: 200}

	c := NewComparison(on, off, nil)
	if got := c.Venues(); len(got) != 1 || got[0] != "On" {
		t.Errorf("venues = %v, want only the enabled one", got)
	}

	quotes := c.Quotes(context.Background(), "Plush Pepe", "Pink Diamond")
	if len(quotes) != 1 || quotes[0].Venue != "On" {
		t.Errorf("quotes = %+v, want one from On", quotes)
	}
	if off.calls.Load() != 0 {
		t.Error("a disabled venue must never be queried")
	}
}

func TestComparisonSkipsVenuesWithNothingToSay(t *testing.T) {
	good := &fakeSource{name: "Portals", enabled: true, floor: 1620, fee: 0.02}
	silent := &fakeSource{name: "MRKT", enabled: true, err: errors.New("session rejected")}
	zero := &fakeSource{name: "Zero", enabled: true, floor: 0}

	quotes := NewComparison(good, silent, zero).Quotes(context.Background(), "Plush Pepe", "Pink Diamond")
	if len(quotes) != 1 {
		t.Fatalf("quotes = %+v, want only the venue that answered", quotes)
	}
	if quotes[0].Floor != 1620 {
		t.Errorf("floor = %v, want 1620", quotes[0].Floor)
	}
	if got := quotes[0].Net(); got != 1620*0.98 {
		t.Errorf("net = %v, want the floor less the 2%% venue fee", got)
	}
}

func TestComparisonUsesActualAskDepthAndRobustReference(t *testing.T) {
	d := &fakeDepthSource{fakeSource: fakeSource{name: "Depth", enabled: true, floor: 1, fee: .02}, asks: []float64{10, 20, 21, 40}}
	quotes, unreachable := NewComparison(d).QuotesForGift(context.Background(), "c", "m", "b", "s")
	if len(quotes) != 1 || len(quotes[0].Asks) != 4 {
		t.Fatalf("quotes = %+v", quotes)
	}
	if unreachable != 0 {
		t.Errorf("unreachable = %d, want 0 for a venue that answered", unreachable)
	}
	if quotes[0].Floor != 10 || quotes[0].Reference() != 20 {
		t.Fatalf("floor/reference = %.2f/%.2f, want 10/20", quotes[0].Floor, quotes[0].Reference())
	}
	if quotes[0].Scope != "exact attributes" {
		t.Fatalf("scope = %q", quotes[0].Scope)
	}
	if quotes[0].NetReference() != 20*.98 {
		t.Fatalf("net reference = %v", quotes[0].NetReference())
	}
}

// A venue that could not be reached is not a venue with nothing listed.
//
// Production, 12 Aug: three gifts priced back to back starved the per-venue rate
// limiter, Portals timed out, and the card quietly printed "площадки 0%" — with
// the cross-market cap and the undercut veto silently switched off. Those are
// the guards that stop a hole in the Tonnel book from reading as an edge, so
// losing them has to be loud.
func TestUnreachableVenueIsReportedNotSwallowed(t *testing.T) {
	broken := &fakeSource{name: "Broken", enabled: true, err: errors.New("timeout")}
	empty := &fakeSource{name: "Empty", enabled: true, floor: 0}
	live := &fakeSource{name: "Live", enabled: true, floor: 5}

	quotes, unreachable := NewComparison(broken, empty, live).
		QuotesForGift(context.Background(), "c", "m", "", "")

	if unreachable != 1 {
		t.Errorf("unreachable = %d, want 1 — only the venue that errored", unreachable)
	}
	// The venue with nothing listed is not a failure, and must not be counted
	// as one: an empty book is real information about the market.
	if len(quotes) != 1 || quotes[0].Venue != "Live" {
		t.Errorf("quotes = %+v, want only the venue that answered with a price", quotes)
	}
}

// The comparison sits on the path of a time-sensitive card, so one slow venue
// must not delay the rest.
func TestComparisonQueriesVenuesInParallel(t *testing.T) {
	slow := &fakeSource{name: "Slow", enabled: true, floor: 1, delay: 150 * time.Millisecond}
	alsoSlow := &fakeSource{name: "AlsoSlow", enabled: true, floor: 2, delay: 150 * time.Millisecond}

	start := time.Now()
	quotes := NewComparison(slow, alsoSlow).Quotes(context.Background(), "c", "m")
	elapsed := time.Since(start)

	if len(quotes) != 2 {
		t.Fatalf("quotes = %+v, want both", quotes)
	}
	if elapsed > 280*time.Millisecond {
		t.Errorf("took %s; two 150ms venues run in parallel should finish well under 300ms", elapsed)
	}
}

func TestComparisonRespectsADeadline(t *testing.T) {
	slow := &fakeSource{name: "Slow", enabled: true, floor: 1, delay: time.Second}
	fast := &fakeSource{name: "Fast", enabled: true, floor: 2}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	quotes := NewComparison(slow, fast).Quotes(ctx, "c", "m")
	if len(quotes) != 1 || quotes[0].Venue != "Fast" {
		t.Errorf("quotes = %+v, want only the venue that beat the deadline", quotes)
	}
}

func TestComparisonPreservesOrder(t *testing.T) {
	a := &fakeSource{name: "A", enabled: true, floor: 1, delay: 60 * time.Millisecond}
	b := &fakeSource{name: "B", enabled: true, floor: 2}

	// B finishes first, but the output order must follow configuration so the
	// card does not reshuffle between refreshes.
	quotes := NewComparison(a, b).Quotes(context.Background(), "c", "m")
	if len(quotes) != 2 || quotes[0].Venue != "A" || quotes[1].Venue != "B" {
		t.Errorf("quotes = %+v, want configured order A, B", quotes)
	}
}

func TestNilComparisonIsSafe(t *testing.T) {
	var c *Comparison
	if c.Enabled() {
		t.Error("a nil comparison must report disabled")
	}
	if got := c.Venues(); got != nil {
		t.Errorf("venues = %v, want nil", got)
	}
	if got := c.Quotes(context.Background(), "c", "m"); got != nil {
		t.Errorf("quotes = %v, want nil", got)
	}
}

// A venue that is down must not be re-hit for every signal card.
func TestCacheRemembersFailures(t *testing.T) {
	c := newCache[float64](time.Minute)
	var calls int

	load := func() (float64, error) {
		calls++
		return 0, errors.New("venue is down")
	}
	for i := 0; i < 3; i++ {
		if _, err := c.get(context.Background(), "k", load); err == nil {
			t.Fatal("expected the cached error")
		}
	}
	if calls != 1 {
		t.Errorf("loader ran %d times, want 1: failures must be cached too", calls)
	}
}

// A venue read that lost a race with our own deadline says nothing about the
// venue. Remembering it as an answer meant one busy moment blocked unattended
// buying — and capped the score of every listing — for the whole TTL.
func TestCacheDoesNotRememberOurOwnDeadline(t *testing.T) {
	c := newCache[float64](time.Minute)
	var calls int
	fail := func() (float64, error) {
		calls++
		return 0, context.DeadlineExceeded
	}

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(dead, "k", fail); err == nil {
		t.Fatal("expected the deadline error to be returned")
	}
	if _, err := c.get(context.Background(), "k", func() (float64, error) {
		calls++
		return 7, nil
	}); err != nil {
		t.Fatalf("the next attempt must reach the venue: %v", err)
	}
	if calls != 2 {
		t.Errorf("loader ran %d times, want 2: a cancelled attempt must not be cached", calls)
	}

	// A rate limiter that gives up reports its own refusal rather than the
	// context's, and that is the common shape of this failure.
	rateGiveUp := errors.New("rate: Wait(n=1) would exceed context deadline")
	dead2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := c.get(dead2, "limited", func() (float64, error) { return 0, rateGiveUp }); err == nil {
		t.Fatal("expected the limiter error")
	}
	if v, err := c.get(context.Background(), "limited", func() (float64, error) { return 3, nil }); err != nil || v != 3 {
		t.Errorf("got %v, %v — a limiter give-up must not be cached either", v, err)
	}
}

func TestCacheExpires(t *testing.T) {
	c := newCache[float64](20 * time.Millisecond)
	var calls int
	load := func() (float64, error) {
		calls++
		return float64(calls), nil
	}

	if v, _ := c.get(context.Background(), "k", load); v != 1 {
		t.Fatalf("first value = %v, want 1", v)
	}
	if v, _ := c.get(context.Background(), "k", load); v != 1 {
		t.Errorf("second value = %v, want the cached 1", v)
	}
	time.Sleep(30 * time.Millisecond)
	if v, _ := c.get(context.Background(), "k", load); v != 2 {
		t.Errorf("value after expiry = %v, want a fresh 2", v)
	}
}

func TestCacheKeysAreIndependent(t *testing.T) {
	c := newCache[float64](time.Minute)
	a, _ := c.get(context.Background(), "a", func() (float64, error) { return 1, nil })
	b, _ := c.get(context.Background(), "b", func() (float64, error) { return 2, nil })
	if a != 1 || b != 2 {
		t.Errorf("got a=%v b=%v, want 1 and 2", a, b)
	}
}

// The venues do not agree on capitalisation or spacing for the same model.
func TestMatchKeyNormalises(t *testing.T) {
	cases := []string{"Pink Diamond", "pink diamond", "  PINK   DIAMOND  ", "Pink  Diamond"}
	want := matchKey(cases[0])
	for _, in := range cases[1:] {
		if got := matchKey(in); got != want {
			t.Errorf("matchKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPortalsShortName(t *testing.T) {
	cases := map[string]string{
		"Plush Pepe":      "plushpepe",
		"B-Day Candle":    "bdaycandle",
		"Durov's Cap":     "durovscap",
		"  Lunar Snake  ": "lunarsnake",
	}
	for in, want := range cases {
		if got := PortalsShortName(in); got != want {
			t.Errorf("PortalsShortName(%q) = %q, want %q", in, got, want)
		}
	}
}

// MRKT quotes integer nanoGRAM; showing those raw would be off by a billion.
func TestNanoToGRAM(t *testing.T) {
	if got := nanoToGRAM(1_240_000_000); got != 1.24 {
		t.Errorf("nanoToGRAM = %v, want 1.24", got)
	}
	if got := nanoToGRAM(0); got != 0 {
		t.Errorf("nanoToGRAM(0) = %v, want 0", got)
	}
}

func TestSourcesImplementTheInterface(t *testing.T) {
	p, err := NewPortals(nil, "token", 0.02, time.Minute)
	if err != nil {
		t.Fatalf("NewPortals: %v", err)
	}
	m, err := NewMRKT(nil, "initdata", "", 0.02, time.Minute)
	if err != nil {
		t.Fatalf("NewMRKT: %v", err)
	}

	var _ Source = p
	var _ Source = m
	var _ DepthSource = p
	var _ DepthSource = m

	if !p.Enabled() || !m.Enabled() {
		t.Error("both sources should be enabled when given credentials")
	}
	if p.Venue() != "Portals" || m.Venue() != "MRKT" {
		t.Errorf("venue names = %q, %q", p.Venue(), m.Venue())
	}
}

// Portals answers its read endpoints anonymously, so it works with no setup.
// MRKT does not, and must stay quiet until it has credentials.
func TestSourceCredentialRequirements(t *testing.T) {
	p, _ := NewPortals(nil, "", 0.02, time.Minute)
	if !p.Enabled() {
		t.Error("Portals needs no credentials and must be enabled by default")
	}

	m, _ := NewMRKT(nil, "", "", 0.02, time.Minute)
	if m.Enabled() {
		t.Error("MRKT must be disabled without initData or a token")
	}
	if _, err := m.ModelFloor(context.Background(), "c", "m"); err == nil {
		t.Error("a disabled MRKT must report why it cannot answer")
	}
}

// The floor prices come back as strings, and models with nothing listed omit
// the field entirely rather than sending a zero.
func TestPortalsDecodesFilters(t *testing.T) {
	const body = `{
	  "collections": {
	    "plushpepe": {
	      "models": [
	        {"name": "Fifty Shades", "rarity_per_mille": 1},
	        {"name": "Gucci Leap", "floor_price": "19490", "supply": 2},
	        {"name": "Pink Diamond", "floor_price": "1620.5", "supply": 7},
	        {"name": "Broken", "floor_price": "not a number"},
	        {"name": "Free", "floor_price": "0"}
	      ]
	    }
	  }
	}`

	var payload portalsFilters
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	col := payload.Collections["plushpepe"]
	if len(col.Models) != 5 {
		t.Fatalf("models = %d, want 5", len(col.Models))
	}

	out := make(map[string]float64)
	for _, m := range col.Models {
		if m.FloorPrice == "" {
			continue
		}
		f, err := strconv.ParseFloat(m.FloorPrice, 64)
		if err != nil || f <= 0 {
			continue
		}
		out[matchKey(m.Name)] = f
	}

	if len(out) != 2 {
		t.Errorf("usable floors = %v, want only the two real prices", out)
	}
	if out["pink diamond"] != 1620.5 {
		t.Errorf("Pink Diamond floor = %v, want 1620.5", out["pink diamond"])
	}
	if _, ok := out["fifty shades"]; ok {
		t.Error("a model with no listing must not produce a floor")
	}
}

// A pasted bearer token cannot be renewed, so the client must not try.
func TestMRKTStaticTokenIsNotRefreshable(t *testing.T) {
	static, _ := NewMRKT(nil, "", "pasted-token", 0.02, time.Minute)
	if !static.static {
		t.Error("a token supplied without initData should be marked static")
	}
	renewable, _ := NewMRKT(nil, "initdata", "", 0.02, time.Minute)
	if renewable.static {
		t.Error("a source with initData can renew and must not be marked static")
	}
}

// notReady is a session that exists but has never been signed in — exactly what
// putting TELEGRAM_APP_ID in the environment produces before `floorline login`.
type notReadySession struct{}

func (notReadySession) InitData(context.Context, string) (string, error) {
	return "", errors.New("not logged in")
}
func (notReadySession) Invalidate(string) {}
func (notReadySession) Ready() bool       { return false }

type readySession struct{ notReadySession }

func (readySession) Ready() bool { return true }

// Holding app credentials is not the same as having a session, and the
// difference decides money: an *unreachable* venue is a hard auto-buy block and
// a heavy score penalty, on the sound principle that it might have objected to
// the trade. A venue that was never set up cannot have objected.
//
// Production, 13 Aug: adding TELEGRAM_APP_ID to the environment — with no login
// yet — turned MRKT from ignored into permanently unreachable, and every card
// began reporting "2 площадки не ответили" while refusing to trade unattended.
func TestVenueWithoutALoginIsUnconfiguredNotUnreachable(t *testing.T) {
	unconfigured, err := NewMRKT(notReadySession{}, "", "", .02, time.Minute)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if unconfigured.Enabled() {
		t.Error("a session that has never logged in must not count as configured")
	}

	// Once signed in, or given a pasted credential, it is a real venue again.
	loggedIn, _ := NewMRKT(readySession{}, "", "", .02, time.Minute)
	if !loggedIn.Enabled() {
		t.Error("a signed-in session must enable the venue")
	}
	pasted, _ := NewMRKT(nil, "", "token", .02, time.Minute)
	if !pasted.Enabled() {
		t.Error("a pasted token must enable the venue without any session")
	}

	// And the comparison drops it, so it is never counted as a venue that
	// failed to answer.
	if c := NewComparison(unconfigured); c.Enabled() {
		t.Error("an unconfigured venue must not make the comparison live")
	}
}

// The worst number this bot has ever printed came from averaging two asks.
// Pool Float · Giant Panda, a 3.8 gift: MRKT showed 12.2 and 306, the reference
// came out at their midpoint of 159.1, and the card reported "+4067% to entry"
// while that number carried a fifth of the weight in price discovery.
func TestTwoAsksThatDisagreeAreNotAPrice(t *testing.T) {
	fantasy := Quote{Asks: []float64{12.2, 306}, Floor: 12.2}
	if ref := fantasy.Reference(); ref != 0 {
		t.Errorf("reference = %.2f, want 0 — one listing and one fantasy is not a market", ref)
	}
	if a := fantasy.Anchor(); a != 12.2 {
		t.Errorf("anchor = %.2f, want the cheapest real offer 12.20", a)
	}

	// Two asks that do agree are a thin market, and speak with the cheaper one.
	thin := Quote{Asks: []float64{4.20, 4.35}}
	if ref := thin.Reference(); ref != 4.20 {
		t.Errorf("reference = %.2f, want the conservative 4.20", ref)
	}
}

// A lone ask is a price somebody typed, not a valuation anchor. Portals showing
// a single "11" against a model trading at 3.1 must contribute nothing.
func TestOneAskIsNeverAValuationAnchor(t *testing.T) {
	lone := Quote{Asks: []float64{11}, Floor: 11}
	if ref := lone.Reference(); ref != 0 {
		t.Errorf("reference = %.2f, want 0", ref)
	}
	if lone.Anchor() != 11 {
		t.Error("the floor is still a fact about execution and must survive")
	}
}

// Robustness has to work in both directions at once: a bait listing far below
// the market must not drag the reference down either.
func TestReferenceIgnoresABaitFloorAndAFantasyAsk(t *testing.T) {
	q := Quote{Asks: []float64{0.9, 20, 21, 400}}
	if ref := q.Reference(); ref != 20 {
		t.Errorf("reference = %.2f, want 20 — the median of the three cheapest", ref)
	}
}

// The tight rung alone was misleading in the direction that costs money. A
// Midas Bunny on Old Gold quoted "по фону 19 / 20 / 24" reads as the venues
// supporting the entry — while the same venue was selling Midas Bunnies from
// 7.48 to anyone who did not care about the backdrop.
func TestQuoteCarriesTheModelQueueBesideTheTightRung(t *testing.T) {
	src := &depthStub{
		byBackdrop: []float64{19, 20, 24, 25, 35},
		byModel:    []float64{7.48, 7.49, 7.5, 8.1},
	}
	c := NewComparison(src)

	quotes, unreachable := c.QuotesForGift(context.Background(), "Spring Basket", "Midas Bunny", "Old Gold", "Chest")
	if unreachable != 0 || len(quotes) != 1 {
		t.Fatalf("quotes=%v unreachable=%d", quotes, unreachable)
	}
	q := quotes[0]
	if q.Scope != ScopeBackdrop {
		t.Fatalf("scope = %q, want the backdrop rung", q.Scope)
	}
	// The rung that bounds the exit is unchanged: that narrowing is deliberate.
	if len(q.Asks) != 5 || q.Asks[0] != 19 {
		t.Fatalf("comparable asks = %v", q.Asks)
	}
	// And the whole model queue now travels with it.
	if len(q.ModelAsks) != 4 || q.ModelAsks[0] != 7.48 {
		t.Fatalf("model asks = %v, want the venue's own model queue", q.ModelAsks)
	}
}

// When the comparable rung already *is* the model, there is no second question
// to ask and no second request to spend on it — which is what keeps this free
// for the sweep, whose lookups carry no attributes at all.
func TestModelRungCostsNoExtraRequest(t *testing.T) {
	src := &depthStub{byModel: []float64{7.48, 7.5}}
	c := NewComparison(src)

	quotes, _ := c.QuotesForGift(context.Background(), "Spring Basket", "Midas Bunny", "", "")
	if len(quotes) != 1 || quotes[0].Scope != ScopeModel {
		t.Fatalf("quotes = %+v", quotes)
	}
	if len(quotes[0].ModelAsks) != 2 {
		t.Fatalf("model asks = %v", quotes[0].ModelAsks)
	}
	if src.calls != 1 {
		t.Fatalf("made %d requests for a model-only lookup, want 1", src.calls)
	}
}

// depthStub answers the ladder the way a venue does: filtered queries return
// only what matches, and the model query returns the whole queue.
type depthStub struct {
	byBackdrop, byModel []float64
	calls               int
}

func (d *depthStub) Venue() string { return "stub" }
func (d *depthStub) Fee() float64  { return 0 }
func (d *depthStub) Enabled() bool { return true }
func (d *depthStub) ModelFloor(ctx context.Context, collection, model string) (float64, error) {
	return 0, nil
}

func (d *depthStub) ModelAsks(ctx context.Context, collection, model, backdrop, symbol string, limit int) ([]float64, error) {
	d.calls++
	switch {
	case symbol != "":
		return nil, nil // the exact bucket is the sparsest and usually empty
	case backdrop != "":
		return d.byBackdrop, nil
	default:
		return d.byModel, nil
	}
}
