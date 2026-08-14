package tonnel

import (
	"context"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/bandwidth"
	"golang.org/x/net/proxy"
	"golang.org/x/time/rate"
)

// fakeTransport answers however the test tells it to, and records which route
// each request went out on.
type fakeTransport struct {
	route  string
	log    *routeLog
	status int
	body   string
}

type routeLog struct {
	mu   sync.Mutex
	used []string
}

func (l *routeLog) note(name string) {
	l.mu.Lock()
	l.used = append(l.used, name)
	l.mu.Unlock()
}

func (l *routeLog) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.used...)
}

func (f *fakeTransport) Do(req *http.Request) (*http.Response, error) {
	f.log.note(f.route)
	body := f.body
	if body == "" {
		body = "[]"
	}
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

// The rest of the interface is never exercised by the client.
func (f *fakeTransport) GetCookies(*url.URL) []*http.Cookie  { return nil }
func (f *fakeTransport) SetCookies(*url.URL, []*http.Cookie) {}
func (f *fakeTransport) SetCookieJar(http.CookieJar)         {}
func (f *fakeTransport) GetCookieJar() http.CookieJar        { return nil }
func (f *fakeTransport) SetProxy(string) error               { return nil }
func (f *fakeTransport) GetProxy() string                    { return "" }
func (f *fakeTransport) SetFollowRedirect(bool)              {}
func (f *fakeTransport) GetFollowRedirect() bool             { return false }
func (f *fakeTransport) CloseIdleConnections()               {}
func (f *fakeTransport) Get(string) (*http.Response, error)  { return nil, nil }
func (f *fakeTransport) Head(string) (*http.Response, error) { return nil, nil }
func (f *fakeTransport) Post(string, string, io.Reader) (*http.Response, error) {
	return nil, nil
}
func (f *fakeTransport) GetBandwidthTracker() bandwidth.BandwidthTracker     { return nil }
func (f *fakeTransport) GetDialer() proxy.ContextDialer                      { return nil }
func (f *fakeTransport) GetTLSDialer() tls_client.TLSDialerFunc              { return nil }
func (f *fakeTransport) AddPreRequestHook(tls_client.PreRequestHookFunc)     {}
func (f *fakeTransport) AddPostResponseHook(tls_client.PostResponseHookFunc) {}
func (f *fakeTransport) ResetPreHooks()                                      {}
func (f *fakeTransport) ResetPostHooks()                                     {}

// routedClient builds a Client whose routes answer with the given statuses, in
// pool order.
func routedClient(t *testing.T, blocked func(error), statuses ...int) (*Client, *routeLog) {
	t.Helper()
	log := &routeLog{}
	pool := &egressPool{}
	for i, status := range statuses {
		name := string(rune('a' + i))
		pool.all = append(pool.all, &Egress{
			name: name,
			http: &fakeTransport{route: name, log: log, status: status},
		})
	}
	c := &Client{
		pool:      pool,
		origin:    DefaultOrigin,
		readHosts: DefaultReadHosts(),
		readLim:   rate.NewLimiter(rate.Inf, 1),
		writeLim:  rate.NewLimiter(rate.Inf, 1),
		onBlocked: blocked,
		retryBase: time.Millisecond,
	}
	c.SetAuth("")
	return c, log
}

// A 403 on one address is that address being challenged. The desk should end up
// on another one and never hear about it.
func TestARefusedRouteIsReplacedWithoutAlarming(t *testing.T) {
	var alarms int
	c, log := routedClient(t, func(error) { alarms++ }, http.StatusForbidden, http.StatusOK)

	var out []Gift
	err := c.call(context.Background(), callOpts{host: HostRead, path: "/api/pageGifts", body: map[string]any{}, out: &out, retries: 2})
	if err != nil {
		t.Fatalf("call failed even though a healthy route existed: %v", err)
	}
	used := log.seen()
	if len(used) != 2 || used[0] != "a" || used[1] != "b" {
		t.Fatalf("routes used: %v, want the refused one then the healthy one", used)
	}
	if alarms != 0 {
		t.Fatalf("the desk was alarmed %d times over a block it routed around", alarms)
	}
}

// Only when every address is refused, repeatedly, is this Tonnel saying no to
// us rather than to one unlucky exit. Then the desk hears about it and the
// whole pool stands down.
func TestEveryRouteRefusedRaisesTheAlarm(t *testing.T) {
	var alarms int
	c, log := routedClient(t, func(error) { alarms++ }, http.StatusForbidden, http.StatusForbidden, http.StatusForbidden)

	var lastErr error
	for i := 0; i < restAfter; i++ {
		var out []Gift
		lastErr = c.call(context.Background(), callOpts{host: HostRead, path: "/api/pageGifts", body: map[string]any{}, out: &out, retries: 2})
		if lastErr == nil {
			t.Fatal("call succeeded with every route refused")
		}
	}
	if n := len(log.seen()); n < 3*restAfter {
		t.Fatalf("only %d attempts across %d routes: %v", n, 3, log.seen())
	}
	if alarms == 0 {
		t.Fatal("nothing was reported after every route was refused")
	}
	// The refusal has to name the address it came from. A rotation that cannot
	// be read in the log cannot be debugged at all.
	if !strings.Contains(lastErr.Error(), " via ") {
		t.Fatalf("error does not say which route was refused: %v", lastErr)
	}
	if c.pool.available(time.Now()) != 0 {
		t.Fatal("a route is still considered available")
	}
}

// A single refusal must not stand a route down: a residential gateway changes
// exit address every connection, so one 403 there is about an address we will
// never see again.
func TestOneRefusalKeepsTheRouteInPlay(t *testing.T) {
	c, _ := routedClient(t, nil, http.StatusForbidden, http.StatusOK)

	var out []Gift
	if err := c.call(context.Background(), callOpts{host: HostRead, path: "/api/pageGifts", body: map[string]any{}, out: &out, retries: 2}); err != nil {
		t.Fatal(err)
	}
	if c.pool.available(time.Now()) != 2 {
		t.Fatal("a route stood down after one refusal")
	}
}

// A business rejection is the endpoint answering us perfectly well and saying
// no. Spreading it across every address would burn the whole rotation on one
// bad request.
func TestABusinessRejectionDoesNotBurnTheRotation(t *testing.T) {
	c, log := routedClient(t, nil, http.StatusBadRequest, http.StatusOK)

	var out []Gift
	if err := c.call(context.Background(), callOpts{host: HostRead, path: "/api/pageGifts", body: map[string]any{}, out: &out, retries: 2}); err == nil {
		t.Fatal("a 400 was retried into success")
	}
	if n := len(log.seen()); n != 1 {
		t.Fatalf("a rejected request was sent %d times", n)
	}
	if c.pool.available(time.Now()) != 2 {
		t.Fatal("a route was rested over a business rejection")
	}
}
