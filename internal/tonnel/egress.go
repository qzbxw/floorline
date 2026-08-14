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

// egressCooldown is how long a route rests once it has been rested, escalating
// with repeat offences and reset by a single success.
var egressCooldown = []time.Duration{45 * time.Second, 3 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// restAfter is how many refusals in a row it takes to rest a route.
//
// Not one, because a residential gateway hands out a different exit address on
// every connection: a 403 there names an address we were never going to use
// again, and resting the gateway over it would take away the only route that
// works. Correlated failure is what identifies a genuinely burnt route, and a
// fixed address that is burnt fails every single time — so it still rests
// almost immediately, at the cost of two extra requests.
const restAfter = 3

// DirectEgress is the name of the route that uses the machine's own address.
const DirectEgress = "прямой"

// Egress is one route to Tonnel.
type Egress struct {
	name string
	http tls_client.HttpClient
	// metered marks a route that costs money by the byte. Residential proxies
	// are sold by traffic — the plan behind this desk is five gigabytes — and
	// filterStats alone would eat it in a day. So metered routes are the
	// fallback, never the default: reads use them only while the free route is
	// being refused, which is exactly when they are worth paying for.
	metered bool

	mu          sync.Mutex
	coolUntil   time.Time
	consecutive int // refusals since the last success, drives resting
	rests       int // times rested, drives the cooldown ladder
	lastOK      time.Time
	lastErr     string
	calls       int64
	blocks      int64
	bytes       int64
	wire        int64 // last reading of the transport's own byte counter
}

// EgressStatus is a route's health, for /status and the smoke command.
type EgressStatus struct {
	Name    string
	Metered bool
	Cooling time.Duration // remaining rest, zero when available
	Strikes int
	LastOK  time.Time
	LastErr string
	Calls   int64
	Blocks  int64
	Bytes   int64
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

// refuse records a refusal and rests the route once they stop looking like bad
// luck. It reports the rest imposed, or zero if the route is still in play.
func (e *Egress) refuse(now time.Time, err error) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.blocks++
	e.consecutive++
	if err != nil {
		e.lastErr = err.Error()
	}
	if e.consecutive < restAfter {
		return 0
	}
	e.consecutive = 0

	idx := e.rests
	if idx >= len(egressCooldown) {
		idx = len(egressCooldown) - 1
	}
	e.rests++
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
	e.consecutive = 0
	e.rests = 0
	e.coolUntil = time.Time{}
	e.lastOK = now
	e.lastErr = ""
	e.calls++
}

// meter reads the socket counter and folds whatever this route has moved since
// last time into its own total. Called after every request, whatever it came
// back with: a refusal costs bytes too.
func (e *Egress) meter() {
	tracker := e.http.GetBandwidthTracker()
	if tracker == nil {
		return // a transport built without tracking; nothing to count
	}
	moved := tracker.GetTotalBandwidth()
	e.mu.Lock()
	if moved > e.wire {
		e.bytes += moved - e.wire
	}
	e.wire = moved
	e.mu.Unlock()
}

func (e *Egress) status(now time.Time) EgressStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := EgressStatus{
		Name: e.name, Metered: e.metered, Strikes: e.consecutive, LastOK: e.lastOK,
		LastErr: e.lastErr, Calls: e.calls, Blocks: e.blocks, Bytes: e.bytes,
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
		if err := validProxyURL(raw); err != nil {
			return nil, fmt.Errorf("proxy %s: %w", proxyLabel(raw), err)
		}
		e, err := newEgress(proxyLabel(raw), raw, timeout)
		if err != nil {
			return nil, fmt.Errorf("proxy %s: %w", proxyLabel(raw), err)
		}
		e.metered = true // every proxy is assumed to be sold by the byte
		p.all = append(p.all, e)
	}
	direct, err := newEgress(DirectEgress, "", timeout)
	if err != nil {
		return nil, err
	}
	p.all = append(p.all, direct)
	return p, nil
}

// validProxyURL rejects a malformed entry loudly at startup rather than letting
// it fail as a network error on every request for the rest of the process.
//
// The list separator matters here and is the likely mistake: a residential
// login carries its country filter inline — 2a0a…__cr.de,nl,pl,fr — so commas
// are data, and a comma-joined list arrives as one entry with a host full of
// scheme separators. Saying so beats a day of "proxy unreachable".
func validProxyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("scheme %q is not one of http, https, socks5", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("no host")
	}
	// A proxy URL is scheme, credentials and a host, and nothing else. A comma
	// in the *host* or anything at all in the path means two entries ran into
	// one — the credentials may legitimately contain commas, and do, but the
	// host may not.
	if strings.Contains(u.Host, ",") || u.Path != "" || u.Opaque != "" {
		return fmt.Errorf("looks like several proxies in one entry — separate them with ';', not ','")
	}
	return nil
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
		// Counts bytes at the socket, which is the only place they can be
		// counted honestly. Tonnel serves brotli and the transport unpacks it
		// transparently, so the decoded body overstates the wire by roughly
		// four times — filterStats decodes to 359 KB and costs 80 KB. Metering
		// the wrong one would have the desk think it had burnt its plan long
		// before it had.
		tls_client.WithBandwidthTracker(),
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

// pick returns the route for the next request: the free ones first, round-robin
// among whichever of them are available, and a metered one only once no free
// route will have us.
//
// The preference is strict rather than a blend. Spreading requests evenly would
// be the better anti-bot shape, but a residential plan is sold by the byte and
// filterStats alone would drink five gigabytes in a day. Paying for traffic to
// avoid a challenge that is not currently happening is the wrong trade; paying
// for it to keep trading through one that is, is the right one.
//
// The second result is false when every route is resting; the first is then the
// one whose rest ends soonest, so a caller that must try something has the best
// available option rather than nothing.
// The `tried` argument is the routes this call has already been refused on.
// Without it a retry goes straight back to the route that just said no — and
// because a route only rests after several refusals, a single call would spend
// every one of its attempts on the same refusing address and fail, while a
// perfectly good paid route sat unused behind it. That is exactly what the
// first poll after the residential proxy was added did.
func (p *egressPool) pick(now time.Time, tried []*Egress) (*Egress, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.all) == 0 {
		return nil, false
	}
	for _, skip := range [][]*Egress{tried, nil} {
		if e, ok := p.pickTier(now, false, skip); ok {
			return e, true
		}
		if e, ok := p.pickTier(now, true, skip); ok {
			return e, true
		}
		// Nothing left untried. Fall through and allow a repeat: with one route
		// configured, a retry on the same one is the only retry there is.
	}
	return p.soonest(), false
}

// pickTier round-robins within one tier, skipping anything in `skip`. Callers
// hold p.mu.
func (p *egressPool) pickTier(now time.Time, metered bool, skip []*Egress) (*Egress, bool) {
	for range p.all {
		e := p.all[p.idx%len(p.all)]
		p.idx++
		if e.metered == metered && e.available(now) && !contains(skip, e) {
			return e, true
		}
	}
	return nil, false
}

func contains(list []*Egress, e *Egress) bool {
	for _, v := range list {
		if v == e {
			return true
		}
	}
	return false
}

// sticky returns the route a write should go out on: the free tier if anything
// there is available, and within a tier the one that most recently answered.
func (p *egressPool) sticky(now time.Time, tried []*Egress) (*Egress, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, skip := range [][]*Egress{tried, nil} {
		if e, ok := p.stickyTier(now, false, skip); ok {
			return e, true
		}
		if e, ok := p.stickyTier(now, true, skip); ok {
			return e, true
		}
	}
	return p.soonest(), false
}

// stickyTier is the most recently successful available route of one tier.
// Callers hold p.mu.
func (p *egressPool) stickyTier(now time.Time, metered bool, skip []*Egress) (*Egress, bool) {
	var best *Egress
	var bestOK time.Time
	for _, e := range p.all {
		if e.metered != metered || !e.available(now) || contains(skip, e) {
			continue
		}
		e.mu.Lock()
		ok := e.lastOK
		e.mu.Unlock()
		if best == nil || ok.After(bestOK) {
			best, bestOK = e, ok
		}
	}
	return best, best != nil
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

// meteredOnly reports whether every route that could carry a read right now
// costs money. That is the signal the desk uses to go frugal: it is not "a
// proxy is configured", it is "the free address is unavailable and every
// request from here on is being paid for".
func (p *egressPool) meteredOnly(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	free, metered := 0, 0
	for _, e := range p.all {
		if !e.available(now) {
			continue
		}
		if e.metered {
			metered++
		} else {
			free++
		}
	}
	return free == 0 && metered > 0
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
