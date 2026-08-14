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

// The machine's own address is always a route. A proxy list says "here are more
// addresses", not "the real one is unusable" — and on the day every proxy is
// refused, the one that still works must not have been configured away.
func TestDirectRouteIsAlwaysInTheRotation(t *testing.T) {
	p := testPool(t, "socks5://user:pass@1.2.3.4:9000")
	if p.size() != 2 {
		t.Fatalf("pool has %d routes, want the proxy and the direct one", p.size())
	}
	last := p.all[len(p.all)-1]
	if last.Name() != DirectEgress {
		t.Fatalf("last route is %q, want the direct one to be the fallback", last.Name())
	}
}

// Credentials must never reach a label: route names end up in log lines and in
// Telegram messages.
func TestRouteNamesCarryNoCredentials(t *testing.T) {
	p := testPool(t, "socks5://xFm0Fe:B2nTA6@193.32.155.24:9074")
	name := p.all[0].Name()
	if name != "193.32.155.24:9074" {
		t.Fatalf("route named %q", name)
	}
	for _, secret := range []string{"xFm0Fe", "B2nTA6", "socks5"} {
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

// Reads spread across every healthy route. One address carrying the whole
// request rate is the shape that gets challenged in the first place.
func TestPickRotatesAcrossHealthyRoutes(t *testing.T) {
	p := testPool(t, "socks5://a:b@1.1.1.1:1", "socks5://a:b@2.2.2.2:2")
	now := time.Now()

	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		e, ok := p.pick(now)
		if !ok {
			t.Fatal("no route available in a healthy pool")
		}
		seen[e.Name()]++
	}
	if len(seen) != 3 {
		t.Fatalf("only %d of 3 routes were used: %v", len(seen), seen)
	}
	for name, n := range seen {
		if n != 3 {
			t.Fatalf("route %s carried %d of 9 requests, want an even spread: %v", name, n, seen)
		}
	}
}

// A refusal names an address. Rest that one and keep going on the others —
// this is the whole point of having more than one.
func TestARefusedRouteIsSkippedUntilItHasRested(t *testing.T) {
	p := testPool(t, "socks5://a:b@1.1.1.1:1")
	now := time.Now()

	bad := p.all[0]
	bad.penalise(now, nil)

	for i := 0; i < 4; i++ {
		e, ok := p.pick(now)
		if !ok {
			t.Fatal("pool reported no route while a healthy one existed")
		}
		if e.Name() == bad.Name() {
			t.Fatal("a resting route was handed out")
		}
	}
	// And it comes back once the rest is over.
	if _, ok := p.pick(now.Add(egressCooldown[0] + time.Second)); !ok {
		t.Fatal("pool unavailable after the cooldown expired")
	}
	if !bad.available(now.Add(egressCooldown[0] + time.Second)) {
		t.Fatal("the route never came back")
	}
}

func TestCooldownEscalatesAndSuccessClearsIt(t *testing.T) {
	p := testPool(t)
	e := p.all[0]
	now := time.Now()

	first := e.penalise(now, nil)
	second := e.penalise(now, nil)
	if second <= first {
		t.Fatalf("cooldown did not escalate: %s then %s", first, second)
	}
	// It is capped rather than unbounded.
	for i := 0; i < 20; i++ {
		e.penalise(now, nil)
	}
	if last := e.penalise(now, nil); last != egressCooldown[len(egressCooldown)-1] {
		t.Fatalf("cooldown ran past the ladder: %s", last)
	}

	e.reward(now)
	if !e.available(now) {
		t.Fatal("a route that answered is still resting")
	}
	if again := e.penalise(now, nil); again != egressCooldown[0] {
		t.Fatalf("a success did not clear the record: next rest was %s", again)
	}
}

// When everything is refused the caller still gets something to try — the route
// that will be welcome again first — along with the news that nothing is
// actually healthy. Returning nothing would leave the desk unable to discover
// that a block had lifted.
func TestAllRefusedStillOffersTheSoonestRoute(t *testing.T) {
	p := testPool(t, "socks5://a:b@1.1.1.1:1")
	now := time.Now()

	p.all[0].penalise(now, nil) // 45s
	p.all[1].penalise(now, nil) // 45s
	p.all[1].penalise(now, nil) // escalated to 3m
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

// Writes stay on whichever route last worked. Discovering that an address has
// gone bad is cheap on a page of listings and expensive on a purchase.
func TestWritesStickToTheLastRouteThatAnswered(t *testing.T) {
	p := testPool(t, "socks5://a:b@1.1.1.1:1", "socks5://a:b@2.2.2.2:2")
	now := time.Now()

	good := p.all[1]
	good.reward(now)
	p.all[0].reward(now.Add(-time.Hour))

	for i := 0; i < 5; i++ {
		e, ok := p.sticky(now)
		if !ok {
			t.Fatal("no route for a write")
		}
		if e.Name() != good.Name() {
			t.Fatalf("write went out via %s, want the route that last answered (%s)", e.Name(), good.Name())
		}
	}

	// Unless that route is the one being refused.
	good.penalise(now, nil)
	e, ok := p.sticky(now)
	if !ok || e.Name() == good.Name() {
		t.Fatalf("write stuck to a refused route: %v %v", e, ok)
	}
}

func TestStatusesPutTheProblemFirst(t *testing.T) {
	p := testPool(t, "socks5://a:b@1.1.1.1:1")
	now := time.Now()
	p.all[0].penalise(now, &APIError{Op: "/api/pageGifts", Status: 403, Message: "cloudflare challenge"})

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
	if st[1].Cooling != 0 {
		t.Fatal("the healthy route is reported as resting")
	}
}
