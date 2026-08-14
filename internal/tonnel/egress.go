package tonnel

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// A Cloudflare block is a judgement about an address, not about a request.
// Backing off is the right answer when there is only one address; when there
// are several, the right answer is to use a different one and let the refused
// one rest.
//
// So a route is a first-class thing here: its own TLS client, its own cookie
// jar, its own reputation. Reads spread across every healthy route, which also
// means no single address ever carries the whole request rate — the shape that
// got this desk challenged in the first place. Writes stick to whichever route
// last worked, because a purchase is not the place to discover that an address
// has gone bad.

// egressCooldown is how long a route rests after being refused, escalating with
// consecutive refusals and reset by a single success. The first step is short
// on purpose: one 403 is often just this address's turn to be challenged, and a
// route that is actually fine should come back quickly.
var egressCooldown = []time.Duration{45 * time.Second, 3 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// DirectEgress is the name of the route that uses the machine's own address.
const DirectEgress = "прямой"

// Egress is one route to Tonnel.
type Egress struct {
	name string
	http tls_client.HttpClient

	mu        sync.Mutex
	coolUntil time.Time
	strikes   int
	lastOK    time.Time
	lastErr   string
	calls     int64
	blocks    int64
}

// EgressStatus is a route's health, for /status and the smoke command.
type EgressStatus struct {
	Name    string
	Cooling time.Duration // remaining rest, zero when available
	Strikes int
	LastOK  time.Time
	LastErr string
	Calls   int64
	Blocks  int64
}

// Name identifies the route in logs and messages. For a proxy it is host:port —
// never the credentials, which would otherwise end up in a Telegram message the
// first time a route went bad.
func (e *Egress) Name() string { return e.name }

func (e *Egress) available(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !now.Before(e.coolUntil)
}

// penalise rests a refused route and reports for how long.
func (e *Egress) penalise(now time.Time, err error) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.blocks++
	if err != nil {
		e.lastErr = err.Error()
	}
	idx := e.strikes
	if idx >= len(egressCooldown) {
		idx = len(egressCooldown) - 1
	}
	e.strikes++
	d := egressCooldown[idx]
	if until := now.Add(d); until.After(e.coolUntil) {
		e.coolUntil = until
	}
	return d
}

// reward clears a route's record after it answers.
func (e *Egress) reward(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.strikes = 0
	e.coolUntil = time.Time{}
	e.lastOK = now
	e.lastErr = ""
	e.calls++
}

func (e *Egress) status(now time.Time) EgressStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := EgressStatus{
		Name: e.name, Strikes: e.strikes, LastOK: e.lastOK,
		LastErr: e.lastErr, Calls: e.calls, Blocks: e.blocks,
	}
	if now.Before(e.coolUntil) {
		st.Cooling = e.coolUntil.Sub(now)
	}
	return st
}

// egressPool owns every route and decides which one carries the next request.
type egressPool struct {
	mu  sync.Mutex
	all []*Egress
	idx int
}

// newEgressPool builds one route per proxy, plus the direct one.
//
// The direct route is always present and always last in the rotation. A proxy
// list is a way to have more addresses than one, not a promise that the
// machine's own is unusable — and on the day every proxy is refused, the route
// that still works must not have been configured away.
func newEgressPool(proxies []string, timeout time.Duration) (*egressPool, error) {
	p := &egressPool{}
	seen := map[string]bool{}
	for _, raw := range proxies {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			continue
		}
		seen[raw] = true
		e, err := newEgress(proxyLabel(raw), raw, timeout)
		if err != nil {
			return nil, fmt.Errorf("proxy %s: %w", proxyLabel(raw), err)
		}
		p.all = append(p.all, e)
	}
	direct, err := newEgress(DirectEgress, "", timeout)
	if err != nil {
		return nil, err
	}
	p.all = append(p.all, direct)
	return p, nil
}

func newEgress(name, proxy string, timeout time.Duration) (*Egress, error) {
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeout(int(timeout.Seconds())),
		tls_client.WithClientProfile(profiles.Chrome_131),
		// One jar per route. Cloudflare's clearance cookies are issued to an
		// address, so sharing a jar across routes would present another
		// address's clearance and prove we are not who we claim to be.
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithCatchPanics(),
	}
	if proxy != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxy))
	}
	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, err
	}
	return &Egress{name: name, http: hc}, nil
}

// proxyLabel reduces a proxy URL to something safe to print. Credentials are in
// these strings, and everything that names a route ends up in a log line or a
// Telegram message eventually.
func proxyLabel(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		return raw[at+1:]
	}
	return raw
}

// pick returns the route for the next request, round-robin across the ones that
// are available. The second result is false when every route is resting; the
// first is then the one whose rest ends soonest, so a caller that must try
// something has the best available option rather than nothing.
func (p *egressPool) pick(now time.Time) (*Egress, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.all) == 0 {
		return nil, false
	}
	for range p.all {
		e := p.all[p.idx%len(p.all)]
		p.idx++
		if e.available(now) {
			return e, true
		}
	}
	return p.soonest(), false
}

// sticky returns the route that most recently answered, for the write path.
func (p *egressPool) sticky(now time.Time) (*Egress, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var best *Egress
	var bestOK time.Time
	for _, e := range p.all {
		if !e.available(now) {
			continue
		}
		e.mu.Lock()
		ok := e.lastOK
		e.mu.Unlock()
		if best == nil || ok.After(bestOK) {
			best, bestOK = e, ok
		}
	}
	if best != nil {
		return best, true
	}
	return p.soonest(), false
}

// soonest is the route that will be available first. Callers hold p.mu.
func (p *egressPool) soonest() *Egress {
	best := p.all[0]
	best.mu.Lock()
	bestUntil := best.coolUntil
	best.mu.Unlock()
	for _, e := range p.all[1:] {
		e.mu.Lock()
		until := e.coolUntil
		e.mu.Unlock()
		if until.Before(bestUntil) {
			best, bestUntil = e, until
		}
	}
	return best
}

// available counts the routes that could carry a request right now.
func (p *egressPool) available(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.all {
		if e.available(now) {
			n++
		}
	}
	return n
}

func (p *egressPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.all)
}

// statuses reports every route, worst first, so a summary that has to be cut
// short shows the problem rather than the healthy majority.
func (p *egressPool) statuses(now time.Time) []EgressStatus {
	p.mu.Lock()
	all := append([]*Egress(nil), p.all...)
	p.mu.Unlock()

	out := make([]EgressStatus, 0, len(all))
	for _, e := range all {
		out = append(out, e.status(now))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Cooling > out[j].Cooling })
	return out
}
