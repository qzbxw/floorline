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
		if _, err := c.get("k", load); err == nil {
			t.Fatal("expected the cached error")
		}
	}
	if calls != 1 {
		t.Errorf("loader ran %d times, want 1: failures must be cached too", calls)
	}
}

func TestCacheExpires(t *testing.T) {
	c := newCache[float64](20 * time.Millisecond)
	var calls int
	load := func() (float64, error) {
		calls++
		return float64(calls), nil
	}

	if v, _ := c.get("k", load); v != 1 {
		t.Fatalf("first value = %v, want 1", v)
	}
	if v, _ := c.get("k", load); v != 1 {
		t.Errorf("second value = %v, want the cached 1", v)
	}
	time.Sleep(30 * time.Millisecond)
	if v, _ := c.get("k", load); v != 2 {
		t.Errorf("value after expiry = %v, want a fresh 2", v)
	}
}

func TestCacheKeysAreIndependent(t *testing.T) {
	c := newCache[float64](time.Minute)
	a, _ := c.get("a", func() (float64, error) { return 1, nil })
	b, _ := c.get("b", func() (float64, error) { return 2, nil })
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

// MRKT quotes integer nanoTON; showing those raw would be off by a billion.
func TestNanoToTON(t *testing.T) {
	if got := nanoToTON(1_240_000_000); got != 1.24 {
		t.Errorf("nanoToTON = %v, want 1.24", got)
	}
	if got := nanoToTON(0); got != 0 {
		t.Errorf("nanoToTON(0) = %v, want 0", got)
	}
}

func TestSourcesImplementTheInterface(t *testing.T) {
	p, err := NewPortals("token", 0.02, time.Minute)
	if err != nil {
		t.Fatalf("NewPortals: %v", err)
	}
	m, err := NewMRKT("initdata", "", 0.02, time.Minute)
	if err != nil {
		t.Fatalf("NewMRKT: %v", err)
	}

	var _ Source = p
	var _ Source = m

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
	p, _ := NewPortals("", 0.02, time.Minute)
	if !p.Enabled() {
		t.Error("Portals needs no credentials and must be enabled by default")
	}

	m, _ := NewMRKT("", "", 0.02, time.Minute)
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
	static, _ := NewMRKT("", "pasted-token", 0.02, time.Minute)
	if !static.static {
		t.Error("a token supplied without initData should be marked static")
	}
	renewable, _ := NewMRKT("initdata", "", 0.02, time.Minute)
	if renewable.static {
		t.Error("a source with initData can renew and must not be marked static")
	}
}
