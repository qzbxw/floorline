package tonnel

import (
	"strings"
	"testing"
	"time"
)

func testPool(t *testing.T, proxies ...string) *egressPool {
	t.Helper()
	p, err := newEgressPool(proxies, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// rest refuses a route until it actually stands down.
func rest(e *Egress, now time.Time) {
	for i := 0; i < restAfter; i++ {
		e.refuse(now, nil)
	}
}

// The machine's own address is always a route. A proxy list says "here are more
// addresses", not "the real one is unusable" — and on the day every proxy is
// refused, the one that still works must not have been configured away.
func TestDirectRouteIsAlwaysInTheRotation(t *testing.T) {
	p := testPool(t, "socks5://user:pass@1.2.3.4:9000")
	if p.size() != 2 {
		t.Fatalf("pool has %d routes, want the proxy and the direct one", p.size())
	}
	direct := p.all[len(p.all)-1]
	if direct.Name() != DirectEgress {
		t.Fatalf("last route is %q, want the direct one", direct.Name())
	}
	if direct.metered {
		t.Fatal("the machine's own address was marked as costing money")
	}
	if !p.all[0].metered {
		t.Fatal("a proxy was not marked metered; reads would default to paid traffic")
	}
}

// Credentials must never reach a label: route names end up in log lines, in
// error messages and in Telegram.
func TestRouteNamesCarryNoCredentials(t *testing.T) {
	p := testPool(t, "socks5://2a0a124b79027e339479__cr.de,nl:5bd8f2cd07639eea@gw.dataimpulse.com:823")
	name := p.all[0].Name()
	if name != "gw.dataimpulse.com:823" {
		t.Fatalf("route named %q", name)
	}
	for _, secret := range []string{"2a0a124b79027e339479", "5bd8f2cd07639eea", "socks5"} {
		if strings.Contains(name, secret) {
			t.Fatalf("route name %q leaks %q", name, secret)
		}
	}
}

func TestDuplicateAndEmptyProxiesAreIgnored(t *testing.T) {
	p := testPool(t, "socks5://a:b@1.2.3.4:9000", "", "  ", "socks5://a:b@1.2.3.4:9000")
	if p.size() != 2 {
		t.Fatalf("pool has %d routes, want one proxy plus direct", p.size())
	}
}

// A residential login carries its country filter inline, commas and all, so a
// comma-joined list arrives as one unusable entry. Fail at startup, with the
// fix in the message.
func TestACommaJoinedListIsRejectedWithTheFix(t *testing.T) {
	_, err := newEgressPool([]string{"socks5://a:b@h1:1,socks5://c:d@h2:2"}, time.Second)
	if err == nil {
		t.Fatal("a comma-joined list was accepted")
	}
	if !strings.Contains(err.Error(), "';'") {
		t.Fatalf("error does not say how to separate them: %v", err)
	}
	if _, err := newEgressPool([]string{"gw.example.com:823"}, time.Second); err == nil {
		t.Fatal("a proxy with no scheme was accepted")
	}
}

// Free before paid, strictly. A residential plan is sold by the byte and
// filterStats alone would drink five gigabytes in a day.
func TestReadsStayOnTheFreeRouteWhileItWorks(t *testing.T) {
	p := testPool(t, "socks5://a:b@gw:823")
	now := time.Now()
	for i := 0; i < 8; i++ {
		e, ok := p.pick(now)
		if !ok {
			t.Fatal("no route available")
		}
		if e.metered {
			t.Fatal("a paid route carried a request while the free one was fine")
		}
	}
}

// And the moment the free one is refused, the paid one takes over — that is
// what it is for.
func TestPaidRouteTakesOverWhenTheFreeOneIsRefused(t *testing.T) {
	p := testPool(t, "socks5://a:b@gw:823")
	now := time.Now()
	direct := p.all[1]
	rest(direct, now)

	e, ok := p.pick(now)
	if !ok {
		t.Fatal("pool reported nothing available while the proxy was healthy")
	}
	if !e.metered {
		t.Fatalf("picked %s, want the paid route", e.Name())
	}
	// And it hands the traffic back once the free route has rested.
	later := now.Add(egressCooldown[0] + time.Second)
	e, ok = p.pick(later)
	if !ok || e.metered {
		t.Fatalf("still on the paid route after the free one recovered: %v %v", e, ok)
	}
}

// Several free routes share the load, so no single address carries the whole
// request rate — the shape that gets challenged in the first place.
func TestFreeRoutesShareTheLoad(t *testing.T) {
	p := testPool(t)
	extra, err := newEgress("second-free", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p.all = append(p.all, extra)

	now := time.Now()
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		e, _ := p.pick(now)
		seen[e.Name()]++
	}
	if len(seen) != 2 || seen[DirectEgress] != 3 {
		t.Fatalf("load was not shared: %v", seen)
	}
}

// A rotating residential gateway hands out a different exit address on every
// connection. Resting the whole gateway over one challenged exit would throw
// away the only route that works.
func TestOneRefusalDoesNotRestARoute(t *testing.T) {
	p := testPool(t, "socks5://a:b@gw:823")
	gw := p.all[0]
	now := time.Now()

	if d := gw.refuse(now, nil); d != 0 {
		t.Fatalf("route rested after a single refusal (%s)", d)
	}
	if !gw.available(now) {
		t.Fatal("route stood down after one unlucky exit address")
	}
	// A success in between clears the record, as it should: the gateway is fine.
	gw.reward(now)
	gw.refuse(now, nil)
	gw.refuse(now, nil)
	if !gw.available(now) {
		t.Fatal("refusals either side of a success were counted together")
	}
	// Correlated refusals are a different story.
	gw.refuse(now, nil)
	if gw.available(now) {
		t.Fatal("a route refusing every request in a row was kept in the rotation")
	}
}

func TestCooldownEscalatesAndSuccessClearsIt(t *testing.T) {
	p := testPool(t)
	e := p.all[0]
	now := time.Now()

	rest(e, now)
	first := e.refuseUntilRest(now)
	if first <= egressCooldown[0] {
		t.Fatalf("cooldown did not escalate: %s", first)
	}
	for i := 0; i < 20; i++ {
		e.refuseUntilRest(now)
	}
	if last := e.refuseUntilRest(now); last != egressCooldown[len(egressCooldown)-1] {
		t.Fatalf("cooldown ran past the ladder: %s", last)
	}

	e.reward(now)
	if !e.available(now) {
		t.Fatal("a route that answered is still resting")
	}
	if again := e.refuseUntilRest(now); again != egressCooldown[0] {
		t.Fatalf("a success did not clear the record: next rest was %s", again)
	}
}

// refuseUntilRest drives a route to its next stand-down and reports the rest.
func (e *Egress) refuseUntilRest(now time.Time) time.Duration {
	for {
		if d := e.refuse(now, nil); d != 0 {
			return d
		}
	}
}

// When everything is refused the caller still gets something to try — the route
// that will be welcome again first. Returning nothing would leave the desk
// unable to discover that a block had lifted.
func TestAllRefusedStillOffersTheSoonestRoute(t *testing.T) {
	p := testPool(t, "socks5://a:b@gw:823")
	now := time.Now()

	rest(p.all[0], now)           // 45s
	rest(p.all[1], now)           // 45s
	p.all[1].refuseUntilRest(now) // escalated to 3m
	if got := p.available(now); got != 0 {
		t.Fatalf("%d routes reported available with every one resting", got)
	}
	e, ok := p.pick(now)
	if ok {
		t.Fatal("pool claimed a route was available")
	}
	if e == nil || e.Name() != p.all[0].Name() {
		t.Fatalf("offered %v, want the route that recovers soonest", e)
	}
}

// Writes stay on whichever route last worked, and prefer the free one for the
// same reason reads do.
func TestWritesStickToTheFreeRouteThatAnswered(t *testing.T) {
	p := testPool(t, "socks5://a:b@gw:823")
	now := time.Now()
	gw, direct := p.all[0], p.all[1]

	gw.reward(now)                     // the proxy answered most recently…
	direct.reward(now.Add(-time.Hour)) // …but the free route also works
	e, ok := p.sticky(now)
	if !ok || e.metered {
		t.Fatalf("a write went out on the paid route while the free one was fine: %v", e)
	}

	// Unless the free route is the one being refused.
	rest(direct, now)
	e, ok = p.sticky(now)
	if !ok || !e.metered {
		t.Fatalf("write did not fall through to the paid route: %v %v", e, ok)
	}
}

func TestStatusesPutTheProblemFirstAndCountTraffic(t *testing.T) {
	p := testPool(t, "socks5://a:b@gw:823")
	now := time.Now()
	p.all[0].meter(3 << 20)
	rest(p.all[0], now)
	p.all[0].lastErr = (&APIError{Op: "/api/pageGifts", Route: "gw:823", Status: 403, Message: "cloudflare challenge"}).Error()

	st := p.statuses(now)
	if len(st) != 2 {
		t.Fatalf("statuses = %d", len(st))
	}
	if st[0].Cooling <= 0 {
		t.Fatal("the resting route is not listed first")
	}
	if !strings.Contains(st[0].LastErr, "403") {
		t.Fatalf("status does not say why it is resting: %q", st[0].LastErr)
	}
	if st[0].Bytes != 3<<20 || !st[0].Metered {
		t.Fatalf("traffic on a metered route is not accounted: %+v", st[0])
	}
	if st[1].Cooling != 0 {
		t.Fatal("the healthy route is reported as resting")
	}
}
