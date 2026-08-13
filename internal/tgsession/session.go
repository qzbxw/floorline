// Package tgsession opens Telegram mini apps as a real user account and hands
// out the initData the marketplaces authenticate with.
//
// MRKT and Portals do not trade over MTProto — their orders go over ordinary
// HTTPS. What MTProto gives us is the thing that makes those HTTPS calls
// legitimate: a mini app opened by an actual account, producing a signed
// `tgWebAppData` payload that is fresh and short-lived. Pasting a hand-scraped
// initData works until it expires, which is the state that got this account
// banned for two weeks — the requests looked like an API being driven directly,
// because that is what they were.
//
// So this package logs in once, keeps the session on disk, and refreshes the
// payload on demand. Everything about it is deliberately slow: it is imitating
// a person holding a phone, not a service.
package tgsession

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"golang.org/x/net/proxy"
)

// initDataTTL is how long a payload is reused before it is fetched again.
// Telegram's own clients re-open a mini app far more often than this; the point
// is to stay well inside the window where the marketplace still accepts it.
const initDataTTL = 30 * time.Minute

// MiniApp identifies one marketplace's mini app: the bot that hosts it and the
// short name of the app itself, as they appear in a t.me/<bot>/<app> link.
type MiniApp struct {
	Venue string
	Bot   string
	App   string
}

// Known mini apps. These are the same links the operator opens by hand.
var (
	MRKT    = MiniApp{Venue: "MRKT", Bot: "mrkt", App: "app"}
	Portals = MiniApp{Venue: "Portals", Bot: "portals", App: "market"}
)

// Config is what the client needs to exist. AppID and AppHash come from
// my.telegram.org; without them there is no MTProto connection at all.
type Config struct {
	// Proxy routes MTProto through a SOCKS5 endpoint. Empty means direct.
	Proxy string

	AppID       int
	AppHash     string
	Phone       string
	SessionFile string
}

// Valid reports whether enough is configured to attempt a connection.
func (c Config) Valid() bool {
	return c.AppID != 0 && c.AppHash != "" && c.SessionFile != ""
}

// Client owns the MTProto connection and the cached mini-app payloads.
//
// It is safe for concurrent use: one connection is shared, and payload
// refreshes for the same venue are collapsed rather than racing.
type Client struct {
	cfg Config

	mu       sync.Mutex
	cached   map[string]cachedData
	running  bool
	runErr   error
	stop     context.CancelFunc
	api      *tg.Client
	ready    chan struct{}
	lastSeen time.Time
}

type cachedData struct {
	value string
	at    time.Time
}

// New builds a client. It does not connect; Start does.
func New(cfg Config) (*Client, error) {
	if !cfg.Valid() {
		return nil, errors.New("tgsession: TELEGRAM_APP_ID, TELEGRAM_APP_HASH and a session path are required")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.SessionFile), 0o700); err != nil {
		return nil, fmt.Errorf("tgsession: session directory: %w", err)
	}
	return &Client{cfg: cfg, cached: make(map[string]cachedData), ready: make(chan struct{})}, nil
}

// LoggedIn reports whether `floorline login` has completed for this session.
//
// The session file itself cannot answer this. Telegram's client writes it as
// soon as it has exchanged an auth key with the data centre, which happens
// before the user is authorised — so an abandoned login leaves a perfectly
// valid-looking file behind. Acting on that is how MRKT went from "not
// configured" to "configured and permanently failing", and a venue that fails
// slowly starves the ones that would have answered.
//
// So authorisation is recorded separately, by the code that actually observed
// it. It still says nothing about whether the session is *still* accepted —
// only Start can establish that.
func (c *Client) LoggedIn() bool {
	st, err := os.Stat(c.authorizedMarker())
	return err == nil && st.Size() > 0
}

// authorizedMarker sits next to the session file, so removing one obviously
// invalidates the other.
func (c *Client) authorizedMarker() string { return c.cfg.SessionFile + ".authorized" }

// markAuthorized records that a login completed.
func (c *Client) markAuthorized() error {
	return os.WriteFile(c.authorizedMarker(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// Status is what /status prints about the account.
type Status struct {
	Configured bool
	LoggedIn   bool
	Connected  bool
	Err        string
	// Ages is how long ago each venue's payload was refreshed.
	Ages map[string]time.Duration
}

func (c *Client) Status() Status {
	s := Status{Configured: c != nil && c.cfg.Valid(), Ages: map[string]time.Duration{}}
	if !s.Configured {
		return s
	}
	s.LoggedIn = c.LoggedIn()
	c.mu.Lock()
	defer c.mu.Unlock()
	s.Connected = c.running && c.runErr == nil
	if c.runErr != nil {
		s.Err = c.runErr.Error()
	}
	for venue, d := range c.cached {
		s.Ages[venue] = time.Since(d.at)
	}
	return s
}

// client builds the underlying telegram client with a file-backed session.
//
// MTProto is its own protocol on port 443, not HTTPS, and networks that leave
// the port open can still refuse it: this server holds a TCP connection to a
// data centre and then times out mid-handshake, which is what deep packet
// inspection looks like from the inside. A SOCKS5 dialer moves the whole
// conversation somewhere that is not being inspected.
func (c *Client) client() *telegram.Client {
	opts := telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{Path: c.cfg.SessionFile},
	}
	if d := c.dialer(); d != nil {
		opts.Resolver = dcs.Plain(dcs.PlainOptions{Dial: d})
	}
	return telegram.NewClient(c.cfg.AppID, c.cfg.AppHash, opts)
}

// dialer builds the SOCKS5 dial function, or nil to go direct.
func (c *Client) dialer() dcs.DialFunc {
	raw := strings.TrimSpace(c.cfg.Proxy)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pass}
	}
	d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return nil
	}
	ctxDialer, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil
	}
	return ctxDialer.DialContext
}

// Login performs the interactive phone-and-code flow and stores the session.
//
// It cannot be done unattended — Telegram sends a code to the device — so it is
// a separate command the operator runs once, rather than something the desk
// attempts on its own at three in the morning.
func (c *Client) Login(ctx context.Context, ask auth.UserAuthenticator) error {
	// One client, used for both the connection and the authorisation.
	//
	// This read `c.client().Run(ctx, func(...) { c.client().Auth()... })`, and
	// client() builds a *new* client every call — so the flow authenticated a
	// second, unconnected client while the connected one sat idle. Nothing ever
	// asked Telegram for a code, and the command then printed "Готово. Сессия
	// лежит в …" over a session that had never been authorised. Every attempt
	// to log in had been failing that way, silently, and reporting success.
	tg := c.client()
	return tg.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(ask, auth.SendCodeOptions{})
		if err := tg.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("tgsession: login: %w", err)
		}
		// Recorded only here, where authorisation was actually observed.
		return c.markAuthorized()
	})
}

// Start brings the connection up and keeps it up until ctx is cancelled.
//
// It returns once the client is usable, so callers can fail fast on a rejected
// session instead of discovering it on the first purchase.
func (c *Client) Start(ctx context.Context) error {
	if !c.LoggedIn() {
		return errors.New("tgsession: no session yet — run `floorline login` once")
	}
	runCtx, cancel := context.WithCancel(ctx)

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		cancel()
		return nil
	}
	c.running, c.stop = true, cancel
	c.mu.Unlock()

	started := make(chan error, 1)
	go func() {
		// One client for the whole session, as in Login and for the same
		// reason: client() builds a new one each call, so asking a second
		// instance for its auth status — and handing a third one's API to every
		// caller — meant nothing here was ever attached to the connection that
		// Run had actually opened. That is what "connection is not up" was, on
		// every mini-app payload the marketplaces asked for.
		tg := c.client()
		err := tg.Run(runCtx, func(ctx context.Context) error {
			status, err := tg.Auth().Status(ctx)
			if err != nil {
				started <- err
				return err
			}
			if !status.Authorized {
				err := errors.New("tgsession: session rejected — run `floorline login` again")
				started <- err
				return err
			}
			c.mu.Lock()
			c.api = tg.API()
			c.lastSeen = time.Now()
			c.mu.Unlock()
			close(c.ready)
			started <- nil

			<-ctx.Done() // hold the connection open
			return ctx.Err()
		})
		c.mu.Lock()
		c.running, c.runErr = false, err
		c.mu.Unlock()
	}()

	select {
	case err := <-started:
		return err
	case <-time.After(45 * time.Second):
		cancel()
		return errors.New("tgsession: timed out connecting to Telegram")
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}

// Stop closes the connection.
func (c *Client) Stop() {
	c.mu.Lock()
	stop := c.stop
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// InitData returns a currently-valid tgWebAppData payload for one mini app,
// refreshing it when the cached copy has aged out.
func (c *Client) InitData(ctx context.Context, app MiniApp) (string, error) {
	c.mu.Lock()
	if d, ok := c.cached[app.Venue]; ok && time.Since(d.at) < initDataTTL {
		c.mu.Unlock()
		return d.value, nil
	}
	c.mu.Unlock()

	raw, err := c.openMiniApp(ctx, app)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.cached[app.Venue] = cachedData{value: raw, at: time.Now()}
	c.mu.Unlock()
	return raw, nil
}

// Invalidate drops a cached payload so the next call fetches a fresh one. The
// marketplace clients call this when a token is rejected.
func (c *Client) Invalidate(venue string) {
	c.mu.Lock()
	delete(c.cached, venue)
	c.mu.Unlock()
}

// openMiniApp performs the actual requestAppWebView call and extracts the
// signed payload from the URL fragment Telegram hands back.
func (c *Client) openMiniApp(ctx context.Context, app MiniApp) (string, error) {
	c.mu.Lock()
	api := c.api
	c.mu.Unlock()
	if api == nil {
		select {
		case <-c.ready:
			c.mu.Lock()
			api = c.api
			c.mu.Unlock()
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(30 * time.Second):
			return "", errors.New("tgsession: connection is not up")
		}
	}
	if api == nil {
		return "", errors.New("tgsession: connection is not up")
	}

	peer, err := resolveBot(ctx, api, app.Bot)
	if err != nil {
		return "", fmt.Errorf("tgsession: resolve @%s: %w", app.Bot, err)
	}

	res, err := api.MessagesRequestAppWebView(ctx, &tg.MessagesRequestAppWebViewRequest{
		Peer: &tg.InputPeerUser{UserID: peer.UserID, AccessHash: peer.AccessHash},
		App: &tg.InputBotAppShortName{
			BotID:     &tg.InputUser{UserID: peer.UserID, AccessHash: peer.AccessHash},
			ShortName: app.App,
		},
		Platform: "android",
	})
	if err != nil {
		return "", fmt.Errorf("tgsession: open %s mini app: %w", app.Venue, err)
	}

	data, err := extractInitData(res.GetURL())
	if err != nil {
		return "", fmt.Errorf("tgsession: %s: %w", app.Venue, err)
	}
	return data, nil
}

// resolveBot turns a username into the peer reference the API needs.
func resolveBot(ctx context.Context, api *tg.Client, username string) (*tg.InputUser, error) {
	res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, err
	}
	for _, u := range res.GetUsers() {
		user, ok := u.(*tg.User)
		if !ok {
			continue
		}
		return &tg.InputUser{UserID: user.ID, AccessHash: user.AccessHash}, nil
	}
	return nil, fmt.Errorf("no user in the response for @%s", username)
}

// extractInitData pulls tgWebAppData out of the URL fragment.
//
// Telegram returns the payload in the fragment rather than the query, and it is
// percent-encoded once inside it. The marketplace expects exactly the decoded
// form, hash included — re-encoding or reordering it invalidates the signature.
func extractInitData(raw string) (string, error) {
	frag := raw
	if i := strings.Index(raw, "#"); i >= 0 {
		frag = raw[i+1:]
	}
	values, err := url.ParseQuery(frag)
	if err != nil {
		return "", fmt.Errorf("cannot parse the mini app URL fragment: %w", err)
	}
	data := values.Get("tgWebAppData")
	if data == "" {
		return "", errors.New("the mini app URL carried no tgWebAppData")
	}
	return data, nil
}
