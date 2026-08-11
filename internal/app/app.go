// Package app wires the pollers, the detector, the executor and the Telegram
// bot into one process.
package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"floorline/internal/bot"
	"floorline/internal/config"
	"floorline/internal/exec"
	"floorline/internal/fx"
	"floorline/internal/market"
	"floorline/internal/pricing"
	"floorline/internal/risk"
	"floorline/internal/signal"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

// App owns every long-lived component.
type App struct {
	cfg *config.Config
	st  *store.Store
	api *tonnel.Client

	books *pricing.BookCache
	det   *signal.Detector
	rm    *risk.Manager
	ex    *exec.Executor
	cross *market.Comparison
	fx    *fx.Client
	tg    *bot.Bot

	// nav holds short handles for keyboard buttons, because collection and
	// model names do not fit in Telegram's 64-byte callback payload.
	nav *navRefs

	startedAt time.Time

	mu          sync.RWMutex
	pollers     map[string]*pollerState
	coverage    time.Duration
	backfillDon bool
	collections map[string]struct{}
	rotation    collectionRotation
	// alertCooldown throttles the non-trade alerts (undercut, stale, sweep)
	// which have no natural dedupe key in the database.
	alertCooldown map[string]time.Time
}

// collectionRotation walks the collection list a few at a time, because trade
// history can only be read per collection.
type collectionRotation struct {
	names  []string
	idx    int
	cycles int
}

type pollerState struct {
	LastRun time.Time
	LastOK  time.Time
	LastErr string
	Runs    int
}

// New builds the application.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	a := &App{
		cfg:           cfg,
		st:            st,
		startedAt:     time.Now(),
		nav:           newNavRefs(),
		pollers:       make(map[string]*pollerState),
		collections:   make(map[string]struct{}),
		alertCooldown: make(map[string]time.Time),
	}

	// Restore a hot-swapped authData over the one from the environment: the
	// value sent via /auth is newer by definition.
	auth := cfg.AuthData
	if stored, err := st.GetKV(ctx, "tonnel.auth"); err == nil && stored != "" {
		auth = stored
	}

	a.api, err = tonnel.New(tonnel.Options{
		AuthData:      auth,
		Origin:        cfg.TonnelOrigin,
		Timeout:       cfg.HTTPTimeout,
		ReadRPS:       cfg.ReadRPS,
		ReadBurst:     cfg.ReadBurst,
		OnBlocked:     a.onBlocked,
		OnAuthExpired: a.onAuthExpired,
	})
	if err != nil {
		st.Close()
		return nil, err
	}

	a.books = pricing.NewBookCache(a.api, cfg.BookCacheTTL, 30)

	a.rm, err = risk.New(ctx, st)
	if err != nil {
		st.Close()
		return nil, err
	}
	a.rm.OnDisarm = func(reason string) {
		a.notify("🛑 <b>Auto-buy disarmed</b>\n" + bot.Esc(reason))
	}

	a.det = signal.New(st, a.books, cfg)
	a.det.Coverage = a.Coverage
	a.det.Warm = a.Warm
	a.det.CalibrationReady = func() bool {
		n, first, err := a.st.CalibrationStats(context.Background())
		return err == nil && n >= cfg.CalibrationMinSignals && !first.IsZero() && time.Since(first) >= time.Duration(cfg.CalibrationMinDays)*24*time.Hour
	}

	a.ex = exec.New(a.api, st, a.books, a.rm, cfg)
	a.fx = fx.New(cfg.GramQuoteURL, cfg.HTTPTimeout)

	// Cross-market venues are read-only price references. A venue that fails to
	// build is dropped with a warning rather than taking the process down —
	// none of them is required to trade.
	var sources []market.Source
	if p, err := market.NewPortals(cfg.PortalsAuth, cfg.PortalsFee, cfg.CrossMarkTTL); err != nil {
		log.Warn().Err(err).Msg("portals comparison unavailable")
	} else {
		sources = append(sources, p)
	}
	if mk, err := market.NewMRKT(cfg.MrktInit, cfg.MrktToken, cfg.MrktFee, cfg.CrossMarkTTL); err != nil {
		log.Warn().Err(err).Msg("mrkt comparison unavailable")
	} else {
		sources = append(sources, mk)
	}
	a.cross = market.NewComparison(sources...)

	// The Telegram client is created in Run, not here: `smoke` and `backfill`
	// must work with nothing but a Tonnel session, and connecting to Telegram
	// would make an invalid bot token break an unrelated diagnostic.

	if err := a.refreshCoverage(ctx); err != nil {
		log.Warn().Err(err).Msg("could not read history coverage")
	}
	return a, nil
}

// Close releases resources.
func (a *App) Close() error { return a.st.Close() }

// API exposes the marketplace client (used by the smoke command).
func (a *App) API() *tonnel.Client { return a.api }

// Store exposes the database (used by the backfill command).
func (a *App) Store() *store.Store { return a.st }

// SyncInventory performs one full paginated reconciliation. It is exported for
// the read-mostly CLI portfolio report as well as used by the poller.
func (a *App) SyncInventory(ctx context.Context) error { return a.pollInventory(ctx) }

// SyncGram refreshes the public GRAM/USDT reference and its hourly history.
func (a *App) SyncGram(ctx context.Context) error { return a.pollGram(ctx) }

// PortfolioReport renders the same advice shown by /portfolio.
func (a *App) PortfolioReport(ctx context.Context) string { return a.portfolioText(ctx) }

// Run starts every poller and then blocks on the Telegram update loop.
func (a *App) Run(ctx context.Context) error {
	if err := a.cfg.RequireAuth(); err != nil {
		return err
	}
	if err := a.cfg.RequireBot(); err != nil {
		return err
	}
	tg, err := bot.New(a.cfg.BotToken, a.cfg.OwnerID, a)
	if err != nil {
		return err
	}
	a.tg = tg

	var wg sync.WaitGroup
	start := func(name string, interval time.Duration, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.loop(ctx, name, interval, fn)
		}()
	}

	// The market snapshot must land before anything is evaluated, otherwise the
	// first minute of signals would price against empty floors.
	if err := a.pollGram(ctx); err != nil {
		log.Warn().Err(err).Msg("initial GRAM quote failed")
	}
	if err := a.pollStats(ctx); err != nil {
		log.Warn().Err(err).Msg("initial market snapshot failed")
	}

	start("stats", a.cfg.StatsInterval, a.pollStats)
	start("sales", a.cfg.SalesInterval, a.pollSales)
	start("feed", a.cfg.FeedInterval, a.pollFeed)
	start("inventory", a.cfg.InventoryInterval, a.pollInventory)
	start("gram", a.cfg.GramQuoteInterval, a.pollGram)
	start("maintenance", time.Hour, a.maintenance)

	wg.Add(1)
	go func() {
		defer wg.Done()
		a.backfillIfNeeded(ctx)
	}()

	a.notify(fmt.Sprintf("✅ <b>Floorline is up</b>\n%s", bot.Esc(a.statusLine())))
	log.Info().Str("bot", a.tg.Username()).Msg("floorline running")

	a.tg.Start(ctx)
	wg.Wait()
	return nil
}

// loop runs fn on a ticker, recording health for /status.
func (a *App) loop(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := fn(runCtx)
		cancel()

		a.mu.Lock()
		ps, ok := a.pollers[name]
		if !ok {
			ps = &pollerState{}
			a.pollers[name] = ps
		}
		ps.LastRun = time.Now()
		ps.Runs++
		if err != nil {
			ps.LastErr = err.Error()
		} else {
			ps.LastErr = ""
			ps.LastOK = ps.LastRun
		}
		a.mu.Unlock()

		if err != nil && ctx.Err() == nil {
			log.Warn().Err(err).Str("poller", name).Msg("poll failed")
		}
	}
}

// Coverage reports how much trade history is stored, capped at the lookback
// window. The detector divides by this instead of the nominal window so that a
// half-filled database does not make every model look illiquid.
func (a *App) Coverage() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.coverage
}

// Warm reports whether there is enough history to trust for unattended buying.
func (a *App) Warm() bool {
	window := time.Duration(a.cfg.LookbackDays) * 24 * time.Hour
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.backfillDon && a.coverage >= time.Duration(float64(window)*0.5)
}

func (a *App) refreshCoverage(ctx context.Context) error {
	oldest, err := a.st.OldestSaleTime(ctx)
	if err != nil {
		return err
	}
	window := time.Duration(a.cfg.LookbackDays) * 24 * time.Hour

	cov := time.Duration(0)
	if !oldest.IsZero() {
		cov = time.Since(oldest)
		if cov > window {
			cov = window
		}
	}
	done, _ := a.st.GetKV(ctx, "backfill.done")

	a.mu.Lock()
	a.coverage = cov
	a.backfillDon = done == "1"
	a.mu.Unlock()
	return nil
}

// notify pushes a message to Telegram, if the bot is configured.
func (a *App) notify(text string) {
	if a.tg == nil {
		log.Info().Msg(text)
		return
	}
	a.tg.Notify(text)
}

// throttle returns true when an alert of this key may be sent now, and records
// the send. Used for alert kinds with no database dedupe.
func (a *App) throttle(key string, every time.Duration) bool {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.alertCooldown[key]; ok && now.Sub(last) < every {
		return false
	}
	a.alertCooldown[key] = now
	return true
}

func (a *App) onBlocked(err error) {
	a.rm.Pause(5*time.Minute, "anti-bot block")
	if a.throttle("blocked", 15*time.Minute) {
		a.notify("⚠️ <b>Tonnel is refusing requests</b> (Cloudflare or rate limit).\n" +
			bot.Esc(err.Error()) +
			"\n\nPollers are backing off. Auto-buy is paused for 5 minutes.")
	}
}

func (a *App) onAuthExpired(err error) {
	_ = a.rm.Disarm(context.Background(), "Tonnel session rejected")
	if a.throttle("auth", 30*time.Minute) {
		a.notify("🔑 <b>Tonnel session rejected</b>\n" + bot.Esc(err.Error()) +
			"\n\nOpen the Tonnel mini app with DevTools, copy Telegram.WebApp.initData (or user_auth from a gifts2.tonnel.network request), then send:\n<code>/auth &lt;authData&gt;</code>")
	}
}

func (a *App) window() time.Duration {
	return time.Duration(a.cfg.LookbackDays) * 24 * time.Hour
}
