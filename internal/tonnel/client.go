package tonnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/time/rate"
)

// Hosts. Reads and writes live on different domains.
//
// HostRead is a *label* rather than a fixed address: callers pass it and the
// client resolves it to whichever candidate is currently answering. Tonnel has
// moved this endpoint before and did it again on 13 Aug, when every read began
// returning HTTP 200-shaped 429s carrying a redirect to rs-market — with the
// alternate host sitting in this file, unused, the whole time.
const (
	HostRead    = "gifts2.tonnel.network"
	HostReadAlt = "gifts3.tonnel.network"
	// HostReadRS is the host Tonnel's own throttle page redirects to.
	HostReadRS = "rs-market.tonnel.network"
	HostWrite  = "gifts.coffin.meme"

	// DefaultOrigin is the front end a real browser sends these requests from.
	// Tonnel moved from market.tonnel.network to marketplace.tonnel.network;
	// both still resolve, and the older one is what the public Python wrappers
	// still send. If the backend ever starts checking this strictly, the
	// alternative is one env var away.
	DefaultOrigin = "https://marketplace.tonnel.network"
	LegacyOrigin  = "https://market.tonnel.network"
)

// userAgent is deliberately fixed for the lifetime of the process and matched to
// the TLS profile below. Rotating the UA per request — which the popular Python
// wrappers do — is itself an anti-bot signal, because a single TLS fingerprint
// paired with a churning UA is something no real browser produces.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Options configures a Client.
type Options struct {
	AuthData  string
	Timeout   time.Duration
	ReadRPS   float64
	ReadBurst int
	Proxy     string
	// Origin overrides the front-end origin sent with every request.
	Origin string
	// ReadHosts overrides the read front ends HostRead rotates through. Empty
	// means DefaultReadHosts.
	ReadHosts []string

	// OnBlocked fires when the anti-bot layer starts refusing requests, so the
	// caller can quiesce pollers and disarm auto-buy.
	OnBlocked func(err error)
	// OnAuthExpired fires when the backend rejects the stored authData.
	OnAuthExpired func(err error)
}

// Client talks to the private Tonnel endpoints.
type Client struct {
	http tls_client.HttpClient

	readLim  *rate.Limiter
	writeLim *rate.Limiter

	auth   atomic.Pointer[string]
	userID atomic.Int64

	origin string

	// readHosts are the candidates HostRead resolves to, and readIdx is the one
	// currently answering. Rotation happens on a block, so an endpoint that has
	// moved costs one failed request rather than an outage.
	readHosts []string
	readIdx   atomic.Int32

	blockedStreak atomic.Int32
	lastOK        atomic.Int64 // unix nanos of the last successful call

	onBlocked     func(error)
	onAuthExpired func(error)
}

// New builds a Client with a browser-grade TLS fingerprint. Plain net/http is
// rejected by Cloudflare on these hosts, so the impersonating transport is not
// optional.
func New(o Options) (*Client, error) {
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Second
	}
	if o.ReadRPS <= 0 {
		o.ReadRPS = 2
	}
	if o.ReadBurst <= 0 {
		o.ReadBurst = 5
	}
	if o.Origin == "" {
		o.Origin = DefaultOrigin
	}

	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeout(int(o.Timeout.Seconds())),
		tls_client.WithClientProfile(profiles.Chrome_131),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithCatchPanics(),
	}
	if o.Proxy != "" {
		opts = append(opts, tls_client.WithProxyUrl(o.Proxy))
	}

	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("build tls client: %w", err)
	}

	c := &Client{
		http:      hc,
		origin:    strings.TrimSuffix(o.Origin, "/"),
		readHosts: o.ReadHosts,
		readLim:   rate.NewLimiter(rate.Limit(o.ReadRPS), o.ReadBurst),
		writeLim:  rate.NewLimiter(rate.Limit(1), 2),

		onBlocked:     o.OnBlocked,
		onAuthExpired: o.OnAuthExpired,
	}
	if len(c.readHosts) == 0 {
		c.readHosts = DefaultReadHosts()
	}
	c.SetAuth(o.AuthData)
	return c, nil
}

// DefaultReadHosts lists the read front ends in the order they are tried.
//
// HostReadRS is deliberately not among them. Tonnel's throttle page redirects
// there, which looks like a migration notice, but probing it answers 405 to
// POST /api/pageGifts: it is the marketplace web front end, not the API. Since
// 405 is a business rejection rather than a block, keeping it in the rotation
// would fail every third request for no reason. It stays exported so
// TONNEL_READ_HOSTS can reach it the day that changes.
func DefaultReadHosts() []string { return []string{HostRead, HostReadAlt} }

// ReadHost is the candidate currently in use.
func (c *Client) ReadHost() string {
	return c.readHosts[int(c.readIdx.Load())%len(c.readHosts)]
}

// rotateReadHost moves to the next candidate and reports it.
//
// Only a block rotates: a business rejection means the host answered us
// perfectly well and simply said no, and moving on that would spread a bad
// request across every front end Tonnel has.
func (c *Client) rotateReadHost() string {
	if len(c.readHosts) < 2 {
		return c.ReadHost()
	}
	return c.readHosts[int(c.readIdx.Add(1))%len(c.readHosts)]
}

// resolveHost turns the HostRead label into the address to call.
func (c *Client) resolveHost(host string) string {
	if host == HostRead {
		return c.ReadHost()
	}
	return host
}

// SetAuth swaps the authData at runtime (the /auth command) and re-derives the
// Telegram user id embedded in it.
func (c *Client) SetAuth(auth string) {
	auth = strings.TrimSpace(auth)
	c.auth.Store(&auth)
	if id, err := userIDFromAuth(auth); err == nil {
		c.userID.Store(id)
	}
}

// Origin returns the front-end origin sent with every request.
func (c *Client) Origin() string { return c.origin }

// Auth returns the current authData.
func (c *Client) Auth() string {
	if p := c.auth.Load(); p != nil {
		return *p
	}
	return ""
}

// UserID returns the Telegram id parsed out of authData, or 0.
func (c *Client) UserID() int64 { return c.userID.Load() }

// BlockedStreak returns how many anti-bot rejections happened in a row.
func (c *Client) BlockedStreak() int { return int(c.blockedStreak.Load()) }

// LastSuccess returns when the last call succeeded.
func (c *Client) LastSuccess() time.Time {
	n := c.lastOK.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// userIDFromAuth pulls the numeric Telegram id out of the WebApp initData
// query string. myGifts filters on it server-side, so we need it locally.
func userIDFromAuth(auth string) (int64, error) {
	if auth == "" {
		return 0, errors.New("empty authData")
	}
	vals, err := url.ParseQuery(auth)
	if err != nil {
		return 0, err
	}
	raw := vals.Get("user")
	if raw == "" {
		return 0, errors.New("authData has no user field")
	}
	var u struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		return 0, fmt.Errorf("parse user field: %w", err)
	}
	if u.ID == 0 {
		return 0, errors.New("authData user id is zero")
	}
	return u.ID, nil
}

type callOpts struct {
	host    string
	path    string
	body    any
	out     any
	write   bool
	retries int
}

// call performs one JSON POST with rate limiting, bounded retries and
// anti-bot bookkeeping.
func (c *Client) call(ctx context.Context, o callOpts) error {
	lim := c.readLim
	if o.write {
		lim = c.writeLim
	}

	payload, err := json.Marshal(o.body)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", o.path, err)
	}

	var lastErr error
	attempts := o.retries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter. Anti-bot rejections wait longer
			// than transient server errors.
			base := 400 * time.Millisecond
			var ae *APIError
			if errors.As(lastErr, &ae) && (ae.IsBlocked() || ae.IsRateLimited()) {
				// Both are the server asking for room. Coming back in
				// milliseconds is how a throttle becomes a ban.
				base = 3 * time.Second
			}
			d := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
			d += time.Duration(rand.Int63n(int64(d/2 + 1)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}

		if err := lim.Wait(ctx); err != nil {
			return err
		}

		lastErr = c.do(ctx, o, payload)
		if lastErr == nil {
			c.blockedStreak.Store(0)
			c.lastOK.Store(time.Now().UnixNano())
			return nil
		}

		var ae *APIError
		if errors.As(lastErr, &ae) {
			if ae.IsAuth() {
				if c.onAuthExpired != nil {
					c.onAuthExpired(lastErr)
				}
				return lastErr // retrying with dead credentials is pointless
			}
			if ae.IsBlocked() {
				n := c.blockedStreak.Add(1)
				// A refusal from one front end is not a refusal from Tonnel.
				// Move to the next candidate before backing off, so a moved
				// endpoint recovers on the retry we were making anyway.
				if !o.write {
					c.rotateReadHost()
				}
				if n >= 3 && c.onBlocked != nil {
					c.onBlocked(lastErr)
				}
				continue
			}
			// An explicit "try again in a minute" is the one refusal that asks
			// to be repeated. It arrives as HTTP 200 with a message, so without
			// this it fell through to the business-rejection branch below and
			// was never retried at all.
			if ae.IsRateLimited() {
				continue
			}
			if ae.Status >= 500 {
				continue
			}
			return lastErr // a genuine business rejection; do not hammer it
		}
		// Network/transport error: worth one more shot.
	}
	return lastErr
}

func (c *Client) do(ctx context.Context, o callOpts, payload []byte) error {
	// Resolved per attempt, not once per call: a retry after a block has to go
	// to the candidate the rotation just picked.
	endpoint := "https://" + c.resolveHost(o.host) + o.path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request %s: %w", o.path, err)
	}
	req.Header = http.Header{
		"accept":             {"*/*"},
		"accept-language":    {"en-US,en;q=0.9"},
		"content-type":       {"application/json"},
		"origin":             {c.origin},
		"referer":            {c.origin + "/"},
		"sec-ch-ua":          {`"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {`"Windows"`},
		"sec-fetch-dest":     {"empty"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-site":     {"same-site"},
		"user-agent":         {userAgent},
		http.HeaderOrderKey: {
			"content-length", "sec-ch-ua-platform", "user-agent", "sec-ch-ua",
			"content-type", "sec-ch-ua-mobile", "accept", "origin",
			"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer",
			"accept-language",
		},
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", o.path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("%s: read body: %w", o.path, err)
	}

	if resp.StatusCode != http.StatusOK {
		return &APIError{Op: o.path, Status: resp.StatusCode, Message: extractMessage(body), Body: string(body)}
	}
	return decodeInto(o.path, resp.StatusCode, body, o.out)
}

// envelope is the common wrapper shape. Some endpoints return a bare array
// instead, which decodeInto handles separately.
type envelope struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

func decodeInto(op string, status int, body []byte, out any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return &APIError{Op: op, Status: status, Message: "empty response"}
	}

	target := trimmed
	if trimmed[0] == '{' {
		var env envelope
		if err := json.Unmarshal(trimmed, &env); err == nil {
			if env.Status == "error" || env.Error != "" {
				msg := env.Message
				if msg == "" {
					msg = env.Error
				}
				return &APIError{Op: op, Status: status, Message: msg, Body: string(trimmed)}
			}
			if len(env.Data) > 0 {
				target = env.Data
			}
		}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(target, out); err != nil {
		// The envelope guess may have been wrong; fall back to the raw body.
		if !bytes.Equal(target, trimmed) {
			if err2 := json.Unmarshal(trimmed, out); err2 == nil {
				return nil
			}
		}
		return &APIError{Op: op, Status: status, Message: "decode: " + err.Error(), Body: string(trimmed)}
	}
	return nil
}

// extractMessage best-effort pulls a human message out of an error body.
// Cloudflare interstitials are HTML, so this reports the block plainly.
func extractMessage(body []byte) string {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return ""
	}
	if t[0] == '{' {
		var env envelope
		if err := json.Unmarshal(t, &env); err == nil {
			if env.Message != "" {
				return env.Message
			}
			if env.Error != "" {
				return env.Error
			}
		}
		return ""
	}
	lower := bytes.ToLower(t)
	if bytes.Contains(lower, []byte("cloudflare")) {
		return "cloudflare challenge"
	}
	// An HTML body is a front end talking to a browser, not an API answering
	// us. Two hundred characters of it ended up quoted in Telegram — inside
	// position cards, inside portfolio advice, inside every block notification —
	// where it buried the one fact that mattered. Summarise it instead, keeping
	// the redirect target, which is how a moved endpoint announces itself.
	if bytes.HasPrefix(lower, []byte("<!doctype html")) || bytes.HasPrefix(lower, []byte("<html")) {
		if to := redirectTarget(t); to != "" {
			return "страница-редирект на " + to
		}
		return "страница вместо ответа API"
	}
	return truncate(strings.TrimSpace(string(t)), 200)
}

// redirectTarget pulls the destination out of a meta-refresh page, which is how
// Tonnel's throttle page points at whichever host it wants us on.
var metaRefresh = regexp.MustCompile(`(?i)url=['"]?(https?://[^'"\s>]+)`)

func redirectTarget(body []byte) string {
	if m := metaRefresh.FindSubmatch(body); m != nil {
		return strings.TrimSuffix(string(m[1]), "/")
	}
	return ""
}
