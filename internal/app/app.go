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
	"floorline/internal/tgsession"
	"floorline/internal/tonnel"
	"floorline/internal/venue"
)

// App owns every long-lived component.
type App struct {
	cfg *config.Config
	st  *store.Store
	api *tonnel.Client
	// stream is the marketplace event feed: new asks, reprices, cancellations
	// and settled trades, pushed rather than polled. It is what the desk now
	// watches the market with; api is what it asks the questions the feed does
	// not answer — the order book, the snapshot, inventory, and the writes.
	stream *tonnel.Stream
	// evalQ carries listings from the feed to the evaluators. The socket must
	// not wait on a valuation, so the two are separated by this queue.
	evalQ chan tonnel.Gift

	books *pricing.BookCache
	det   *signal.Detector
	rm    *risk.Manager
	ex    *exec.Executor
	cross *market.Comparison
	fx    *fx.Client
	tg    *bot.Bot
	// session is the real Telegram account the marketplaces' mini apps are
	// opened through. Nil when it is not configured; everything degrades to
	// pasted credentials rather than failing.
	session *tgsession.Client
	// venues is the tradeable view of the marketplaces, as opposed to `cross`
	// which only reads them for pricing.
	venues *venue.Registry

	// nav holds short handles for keyboard buttons, because collection and
	// model names do not fit in Telegram's 64-byte callback payload.
	nav *navRefs
	// trade is the active trading session, cached because the feed asks whether
	// a listing is in scope for every row it sees.
	trade sessionState

	startedAt time.Time

	// notifier and lastAPIOK are seams. The Telegram client cannot be built
	// without a live token and the API client cannot be made to have succeeded,
	// so the block-and-recovery logic — which is about what gets said and when —
	// would otherwise be untestable. Both are nil in production.
	notifier  func(string)
	lastAPIOK func() time.Time
	// metered overrides "are reads being paid for", for the same reason: a pool
	// cannot be pushed onto its paid route from outside the client.
	metered func() bool

	mu          sync.RWMutex
	pollers     map[string]*pollerState
	coverage    time.Duration
	backfillDon bool
	collections map[string]struct{}
	// scanCursor walks the ranked model list across passes, so a sweep resumes
	// where the previous one stopped instead of re-reading the same busiest few.
	scanCursor int
	// scanRanked is that list, cached: building it costs a trade-history query
	// per model on the whole market, and a fourteen-day velocity does not move
	// between sweeps.
	scanRanked    []tonnel.ModelKey
	scanRankedAt  time.Time
	lastScan      time.Time
	lastScanFound int
	rotation      collectionRotation
	// alertCooldown throttles the non-trade alerts (undercut, stale, sweep)
	// which have no natural dedupe key in the database.
	alertCooldown map[string]time.Time
	// blocked is whether the anti-bot layer is currently refusing us, and
	// blockEpisodes counts consecutive blocks so the pause can escalate.
	blocked       bool
	blockEpisodes int
	// blockedAt is when the current episode began and lastBlockAt when it was
	// last refreshed. Recovery is measured against both: the first says which
	// successes count, the second holds the all-clear until the refusals have
	// actually stopped.
	blockedAt      time.Time
	lastBlockAt    time.Time
	blockAnnounced bool
	// coolUntil is when the private endpoints may be polled at full rate again,
	// and lastProbe when the single call that tests the water last went out.
	coolUntil time.Time
	lastProbe time.Time
	// streamDownAt is when the event feed dropped, and streamDownSaid whether
	// the operator has been told about this outage.
	streamDownAt   time.Time
	streamDownSaid bool
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
		ReadHosts:     cfg.TonnelReadHosts,
		Proxy:         cfg.TonnelProxy,
		Proxies:       cfg.TonnelProxies,
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

	if cfg.EventsEnabled {
		a.evalQ = make(chan tonnel.Gift, evalQueueSize)
		a.stream = a.newStream()
	}

	a.books = pricing.NewBookCache(a.api, cfg.BookCacheTTL, 30)
	a.books.Frugal = a.frugal

	a.rm, err = risk.New(ctx, st)
	if err != nil {
		st.Close()
		return nil, err
	}
	a.rm.OnDisarm = func(reason string) {
		a.notify("🛑 <b>Автобай выключен</b>\n" + bot.Esc(reason))
	}

	// SHADOW_MODE is a first-run default, not a standing instruction: after the
	// operator has switched it in the bot, the stored value is the answer.
	if err := a.rm.SeedShadowMode(ctx, cfg.ShadowMode); err != nil {
		st.Close()
		return nil, err
	}

	a.det = signal.New(st, a.books, cfg)
	a.det.OwnerID = a.api.UserID
	a.det.Coverage = a.Coverage
	a.det.Warm = a.Warm
	a.det.ShadowMode = a.rm.ShadowMode
	a.det.CalibrationReady = func() bool {
		if a.rm.CalibrationWaived() {
			return true
		}
		return a.calibrated(context.Background())
	}

	a.ex = exec.New(a.api, st, a.books, a.rm, cfg)
	a.fx = fx.New(cfg.GramQuoteURL, cfg.HTTPTimeout)

	// The Telegram account, when configured, is what lets the other venues be
	// read as a person using their mini app rather than as a bare API client.
	// It is optional: without it the venues fall back to pasted credentials.
	var venueSession market.InitDataSource
	if sc := sessionConfig(cfg); sc.Valid() {
		if s, err := tgsession.New(sc); err != nil {
			log.Warn().Err(err).Msg("telegram session unavailable")
		} else {
			a.session = s
			venueSession = sessionAdapter{c: s}
		}
	}

	// A venue that fails to build is dropped with a warning rather than taking
	// the process down — none of them is required to trade on Tonnel.
	var sources []market.Source
	if p, err := market.NewPortals(venueSession, cfg.PortalsAuth, cfg.PortalsFee, cfg.CrossMarkTTL); err != nil {
		log.Warn().Err(err).Msg("portals comparison unavailable")
	} else {
		sources = append(sources, p)
	}
	if mk, err := market.NewMRKT(venueSession, cfg.MrktInit, cfg.MrktToken, cfg.MrktFee, cfg.CrossMarkTTL); err != nil {
		log.Warn().Err(err).Msg("mrkt comparison unavailable")
	} else {
		sources = append(sources, mk)
	}
	a.cross = market.NewComparison(sources...)

	// The tradeable view of the same marketplaces. Reading and buying are
	// separate concerns: `cross` prices against every venue, `venues` is what
	// can actually execute. Portals and MRKT appear here as soon as their
	// purchase call is captured — everything around it is already wired.
	vhttp, err := venue.NewHTTPClient(15)
	if err != nil {
		log.Warn().Err(err).Msg("venue transport unavailable")
	} else {
		a.venues = venue.NewRegistry(
			venue.NewTonnel(a.api, cfg.TonnelFee),
			venue.NewPortals(vhttp, venueSession, cfg.PortalsFee, venue.HumanPace()),
			venue.NewMRKT(vhttp, venueSession, cfg.MrktFee, venue.HumanPace()),
		)
	}
	a.det.CrossSupport = a.crossMarketDepth
	a.det.Spendable = a.spendable

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

// StartSession brings the Telegram account online for a one-off command.
//
// `run` does this as part of starting up, but the diagnostics did not — so
// `smoke` reported "mrkt: could not open the mini app: connection is not up"
// against a session that works perfectly in production. A diagnostic that
// contradicts the running system is worse than no diagnostic.
func (a *App) StartSession(ctx context.Context) { a.startSession(ctx) }

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

	// The event feed comes up before the pollers: the fallback poll checks
	// whether it is healthy, and a stream that is still connecting must not be
	// mistaken for one that is down.
	a.startEvents(ctx)

	start("stats", a.cfg.StatsInterval, a.pollStats)
	start("sales", a.cfg.SalesInterval, a.pollSales)
	start("feed", a.cfg.FeedInterval, a.pollFeed)
	start("inventory", a.cfg.InventoryInterval, a.pollInventory)
	start("gram", a.cfg.GramQuoteInterval, a.pollGram)
	start("maintenance", time.Hour, a.maintenance)
	start("scan", a.cfg.ScanInterval, a.pollScan)

	// The Telegram account comes up alongside the pollers: the venues need it
	// before the first card is priced.
	a.startSession(ctx)

	wg.Add(1)
	go func() {
		defer wg.Done()
		a.backfillIfNeeded(ctx)
	}()

	a.notify(fmt.Sprintf("✅ <b>Floorline поднялся</b>\n%s", bot.Esc(a.statusLine())))
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

		if !a.mayPoll(name) {
			continue
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

		// A poller completing is the only evidence that the block has lifted,
		// so recovery is noticed here rather than guessed from a timer.
		if err == nil {
			a.noteRecovered()
		}
		if err != nil && ctx.Err() == nil {
			log.Warn().Err(err).Str("poller", name).Msg("poll failed")
		}
	}
}

// tonnelPollers are the loops that spend requests on the private endpoints.
// Everything else — the GRAM quote, maintenance — is unaffected by a block and
// must keep running through one.
var tonnelPollers = map[string]bool{
	"feed": true, "stats": true, "sales": true, "inventory": true, "scan": true,
}

// coolProbePoller is the single call allowed through while cooling. filterStats
// is one request, it covers every model of every collection, and it is
// therefore both the cheapest way to ask whether the block has lifted and the
// most useful answer to get.
const coolProbePoller = "stats"

// coolProbeEvery is how often that probe goes out. Slow enough to be a knock at
// the door rather than a second attempt to break it down.
const coolProbeEvery = 2 * time.Minute

// frugalEvery is the slowest each poller may run while the traffic is being
// paid for by the byte.
//
// Measured on the wire, compressed, with TLS overhead: filterStats is 80 KB a
// call, pageGifts 3.8 KB for thirty rows, saleHistory 3.3 KB, myGifts 2.2 KB.
// At the free-route rates that is ~157 MB a day, and a 5 GB residential plan
// lasts a month only if nothing else ever happens. The snapshot alone is 71% of
// it — once a minute for a number the desk is allowed to act on when it is up
// to five minutes old (AUTOBUY_MAX_DATA_AGE).
//
// At these intervals the same day costs about 40 MB, so the plan lasts four
// months rather than four weeks. Nothing here is a degradation the operator
// would notice: the market itself still arrives instantly on the event stream,
// which costs nothing and does not go through the proxy at all. This is only
// about the questions the stream cannot answer.
var frugalEvery = map[string]time.Duration{
	"stats":     4 * time.Minute,  // 80 KB a call — 112 MB/day becomes 28
	"sales":     10 * time.Minute, // the tape comes from the stream now anyway
	"inventory": 3 * time.Minute,  // our own gifts change when we change them
	"scan":      30 * time.Minute, // a sweep of the standing book is not urgent
	"feed":      2 * time.Minute,  // only runs at all when the stream is down
}

// mayPoll decides whether a poller may run this tick.
//
// This is the missing half of the block response, and its absence is what the
// 14 Aug logs are: the desk announced "поллеры сбавляют темп" and then did no
// such thing. Every loop kept its ticker — the feed asking pageGifts every two
// seconds, inventory, sales, stats — all of them refused, all of them retried
// with backoff inside the client, none of them slowed down. A Cloudflare block
// is a rate decision about our address; answering it with the same request rate
// is how a five-minute challenge becomes an hour of them.
//
// So while cooling, the endpoints that were refused are left alone entirely,
// and exactly one probe every couple of minutes tests whether we are welcome
// again. The event stream is what makes this affordable: the market is still
// visible throughout.
func (a *App) mayPoll(name string) bool {
	if !tonnelPollers[name] {
		return true
	}
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()
	if !now.After(a.coolUntil) {
		if name != coolProbePoller || now.Sub(a.lastProbe) < coolProbeEvery {
			return false
		}
		a.lastProbe = now
		return true
	}
	// Not blocked, but possibly paying. While every free address is refused,
	// each poller keeps its own floor on how often it may spend money.
	if every, ok := frugalEvery[name]; ok && a.frugal() {
		if ps := a.pollers[name]; ps != nil && !ps.LastRun.IsZero() && now.Sub(ps.LastRun) < every {
			return false
		}
	}
	return true
}

// frugal reports whether Tonnel reads are currently costing money. Callers hold
// no lock on the client; the pool answers from its own state.
func (a *App) frugal() bool {
	if a.metered != nil {
		return a.metered()
	}
	return a.api != nil && a.api.Metered()
}

// apiLastOK is when Tonnel last answered anything.
func (a *App) apiLastOK() time.Time {
	if a.lastAPIOK != nil {
		return a.lastAPIOK()
	}
	return a.api.LastSuccess()
}

// cooling reports whether the desk is currently backing off the private
// endpoints. Valuation reads the order book, so it has to respect this too.
func (a *App) cooling() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Now().Before(a.coolUntil)
}

// Coverage reports how much trade history is stored, capped at the lookback
// window. The detector divides by this instead of the nominal window so that a
// half-filled database does not make every model look illiquid.
func (a *App) Coverage() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.coverage
}

// calibrated reports whether enough signals have been recorded, over enough
// days, to judge the scoring against outcomes.
func (a *App) calibrated(ctx context.Context) bool {
	n, first, err := a.st.CalibrationStats(ctx)
	return err == nil && n >= a.cfg.CalibrationMinSignals && !first.IsZero() &&
		time.Since(first) >= time.Duration(a.cfg.CalibrationMinDays)*24*time.Hour
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
	if a.notifier != nil {
		a.notifier(text)
		return
	}
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

// blockBackoff is how long unattended buying stays paused, escalating with each
// consecutive episode.
//
// Five flat minutes was not a cooling-off period, it was a metronome: the pause
// expired long before Tonnel relented, the pollers went straight back in, and
// the desk announced the same block every fifteen minutes all night.
var blockBackoff = []time.Duration{5 * time.Minute, 15 * time.Minute, 45 * time.Minute, 2 * time.Hour}

const (
	// blockRecoveryHold is how long the refusals must have stopped before the
	// all-clear is believed. A block is a rolling condition, not an event: one
	// endpoint answers while another is still challenged, and calling that
	// recovery starts the whole announcement over on the next refusal.
	blockRecoveryHold = 3 * time.Minute
	// blockNoticeEvery caps how often a block may be announced at all. On 14 Aug
	// this pair of messages arrived every thirty seconds for hours.
	blockNoticeEvery = 20 * time.Minute
)

// onBlocked quiesces the desk when the anti-bot layer starts refusing.
//
// It reports the *transition*, not the condition, and it is deliberately hard
// to make it report anything at all. The pause and the host rotation are the
// useful half of this function; the message is only worth sending when the
// operator does not already know.
func (a *App) onBlocked(err error) {
	a.mu.Lock()
	now := time.Now()
	first := !a.blocked
	a.blocked = true
	a.lastBlockAt = now
	if first {
		a.blockedAt = now
		a.blockEpisodes++
	}
	idx := a.blockEpisodes - 1
	if idx >= len(blockBackoff) {
		idx = len(blockBackoff) - 1
	}
	if idx < 0 {
		idx = 0
	}
	pause := blockBackoff[idx]
	// The pollers cool for as long as unattended buying does. Both are the same
	// judgement — that the desk should stop asking for a while — and having only
	// the second one implemented is why the first was never true.
	if until := now.Add(pause); until.After(a.coolUntil) {
		a.coolUntil = until
	}
	a.mu.Unlock()

	a.rm.Pause(pause, "блок антибота")
	if !first || !a.throttle("block-notice", blockNoticeEvery) {
		return
	}
	a.mu.Lock()
	a.blockAnnounced = true
	a.mu.Unlock()

	// What the block actually costs depends on the feed. With the event stream
	// up, new asks and settled trades keep arriving and only the order book —
	// and therefore valuation, and therefore buying — is affected. Saying which
	// one it is saves the operator from guessing.
	feed := "Лента событий тоже молчит — рынок сейчас не виден вообще."
	if a.streamHealthy() {
		feed = "Лента событий Tonnel идёт дальше: новые лоты и сделки вижу, недоступен только стакан для оценки."
	}
	// Which addresses are refused decides what the operator can do about it, so
	// it is in the message rather than one /status away. With a single route
	// there is nothing to say — the block is simply the block.
	routes := ""
	if n := a.api.RouteCount(); n > 1 {
		routes = fmt.Sprintf("Отказали все %d маршрута(ов), включая прокси.\n", n)
	}
	a.notify(fmt.Sprintf(
		"⚠️ <b>Tonnel отказывает</b>\n%s\n\n%s%s\nПерестаю дёргать приватные эндпоинты на %s — раз в %s проверяю одним запросом, отпустило ли. Автобай на паузе.\nСкажу, когда отпустит.",
		bot.Esc(err.Error()), routes, feed, dur(pause), dur(coolProbeEvery)))
}

// noteRecovered clears the blocked state and says so once.
//
// Two conditions, and the missing first one is what made the chat unreadable:
// Tonnel itself has to have answered since the block began, and the refusals
// have to have stopped for long enough to mean it. Any poller finishing used to
// count as recovery — including the GRAM quote, which reads a public exchange
// and had never touched Tonnel in its life. It ticks every thirty seconds, so
// each block was declared over within half a minute of being announced, and the
// next refusal announced it again.
func (a *App) noteRecovered() {
	a.mu.Lock()
	if !a.blocked {
		a.mu.Unlock()
		return
	}
	if a.apiLastOK().Before(a.blockedAt) || time.Since(a.lastBlockAt) < blockRecoveryHold {
		a.mu.Unlock()
		return
	}
	announced := a.blockAnnounced
	a.blocked, a.blockAnnounced = false, false
	a.blockEpisodes = 0
	a.coolUntil = time.Time{} // the probe got through; full rate is welcome again
	a.mu.Unlock()

	if !announced {
		return // never announced, nothing to take back
	}
	a.notify(fmt.Sprintf("✅ <b>Tonnel снова отвечает</b> — читаю с %s", bot.Esc(a.api.ReadHost())))
}

func (a *App) onAuthExpired(err error) {
	_ = a.rm.Disarm(context.Background(), "сессия Tonnel отклонена")
	if a.throttle("auth", 30*time.Minute) {
		a.notify("🔑 <b>Сессия Tonnel отклонена</b>\n" + bot.Esc(err.Error()) +
			"\n\nОткрой мини-апп Tonnel с DevTools, скопируй Telegram.WebApp.initData (или user_auth из запроса к gifts2.tonnel.network) и пришли:\n<code>/auth &lt;authData&gt;</code>")
	}
}

func (a *App) window() time.Duration {
	return time.Duration(a.cfg.LookbackDays) * 24 * time.Hour
}
