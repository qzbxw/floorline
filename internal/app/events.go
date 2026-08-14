package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"floorline/internal/bot"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

// The marketplace event stream is the desk's primary view of the market.
//
// Until now the live feed was pageGifts polled every two seconds. That is the
// single heaviest read the desk makes, it asks for the same page over and over,
// and it is what the Cloudflare challenge in front of gifts2/gifts3 kept
// refusing — one 403 storm produced hours of ⚠️/✅ pairs in the chat while the
// desk ping-ponged between front ends and paused unattended buying every few
// minutes.
//
// Tonnel publishes the same information as a push feed with seven-day replay,
// documented at https://gifts.coffin.meme/MARKETPLACE_EVENTS.md, on a host that
// does not challenge us. So new asks, reprices, cancellations and completed
// sales now arrive the moment they happen, market-wide, for no request budget at
// all — and the private endpoints are left to what only they can answer.

const (
	// eventCursorKey stores the last fully processed eventId, so a restart
	// resumes from the gap rather than from the live edge.
	eventCursorKey = "tonnel.events.cursor"
	// evalQueueSize bounds the work the stream may hand to the evaluators. The
	// socket must never block on valuation: a slow consumer is disconnected
	// server-side with close code 1013.
	evalQueueSize = 2048
	// evalBatchWindow coalesces a burst into one pass, so a seller emptying a
	// collection is ranked as a group instead of arriving as a race.
	evalBatchWindow = 400 * time.Millisecond
	// evalBatchMax caps one pass; the rest waits for the next window.
	evalBatchMax = 24
	// streamDownGrace is how long the feed may be down before it is worth a
	// message. Reconnects are routine and self-healing; an outage is not.
	streamDownGrace = 5 * time.Minute
)

// newStream builds the consumer. It does not connect until Run starts it.
func (a *App) newStream() *tonnel.Stream {
	return tonnel.NewStream(tonnel.StreamOptions{
		Host: a.cfg.EventHost,
		LoadCursor: func(ctx context.Context) (string, error) {
			return a.st.GetKV(ctx, eventCursorKey)
		},
		SaveCursor: func(ctx context.Context, v string) error {
			return a.st.SetKV(ctx, eventCursorKey, v)
		},
		Handle: a.handleEvent,
		OnUp:   a.onStreamUp,
		OnDown: a.onStreamDown,
	})
}

// startEvents brings the feed up and starts the evaluators behind it.
func (a *App) startEvents(ctx context.Context) {
	if a.stream == nil {
		return
	}
	go a.evalWorker(ctx)
	go a.stream.Run(ctx)
}

// handleEvent applies one marketplace event. It runs on the socket's own
// goroutine, so everything here is a bounded local write; the valuation, which
// needs the network, happens on the evaluator behind the queue.
func (a *App) handleEvent(ctx context.Context, ev tonnel.Event) error {
	if !ev.GramPriced() {
		return nil // USDT and TONNEL listings are a separate book
	}
	switch ev.Type {
	case tonnel.EventListingCreated, tonnel.EventListingPriceChanged:
		return a.onListingEvent(ctx, ev)
	case tonnel.EventListingCancelled:
		return a.st.MarkListingGone(ctx, ev.GiftID(), time.Now())
	case tonnel.EventSaleCompleted:
		return a.onSaleEvent(ctx, ev)
	}
	return nil
}

// onListingEvent records a new or repriced ask and queues it for valuation.
func (a *App) onListingEvent(ctx context.Context, ev tonnel.Event) error {
	g, ok := ev.Listing()
	if !ok {
		return nil
	}
	// The feed carries no seller — identities are withheld by design — so the
	// guard that used to compare against our own user id has to come from the
	// position book instead. Without it the desk would price its own asks as
	// opportunities and, with unattended buying armed, bid for them.
	own, err := a.ownListing(ctx, g.GiftID.Int())
	if err != nil {
		return err
	}

	now := time.Now()
	changes, err := a.st.UpsertListings(ctx, []tonnel.Gift{g}, now)
	if err != nil {
		return fmt.Errorf("store listing %d: %w", g.GiftID.Int(), err)
	}
	if own || len(changes.Candidates()) == 0 {
		return nil
	}

	select {
	case a.evalQ <- g:
	default:
		// The queue is full only when the evaluators are already saturated, and
		// the sweep of the standing book covers what is skipped here. Dropping
		// beats blocking the socket into a server-side disconnect.
		log.Warn().Int64("gift", g.GiftID.Int()).Msg("evaluation queue full, listing skipped")
	}
	return nil
}

// onSaleEvent puts a completed trade on the tape.
//
// This is the larger half of what the stream buys us. Trade history is readable
// only one collection at a time, so the poller walks the market in a rotation
// that takes twenty minutes to come round; sales now land whole-market, in the
// second they settle.
func (a *App) onSaleEvent(ctx context.Context, ev tonnel.Event) error {
	sale, ok := ev.Sale()
	if !ok {
		return nil
	}
	if _, err := a.st.InsertSales(ctx, []tonnel.Sale{sale}); err != nil {
		return fmt.Errorf("store sale %d: %w", sale.GiftID.Int(), err)
	}
	// A sold gift is no longer an ask standing in front of anything.
	return a.st.MarkListingGone(ctx, sale.GiftID.Int(), time.Now())
}

// ownListing reports whether a gift is one of ours.
func (a *App) ownListing(ctx context.Context, giftID int64) (bool, error) {
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil || p == nil {
		return false, err
	}
	return p.Status == store.StatusOpen || p.Status == store.StatusListed, nil
}

// evalWorker prices what the stream queued, in coalesced batches so a burst is
// ranked by conviction rather than by arrival order — the same treatment the
// feed poller gave a page of thirty.
func (a *App) evalWorker(ctx context.Context) {
	for {
		var batch []tonnel.Gift
		select {
		case <-ctx.Done():
			return
		case g := <-a.evalQ:
			batch = append(batch, g)
		}

		timer := time.NewTimer(evalBatchWindow)
	collect:
		for len(batch) < evalBatchMax {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case g := <-a.evalQ:
				batch = append(batch, g)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()

		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		ids := make([]int64, 0, len(batch))
		for i := range batch {
			ids = append(ids, batch[i].GiftID.Int())
		}
		a.evaluateListings(runCtx, batch, ids, time.Now())
		cancel()
	}
}

// onStreamUp records that the feed is live and says so if it had been missed.
func (a *App) onStreamUp(replayed int) {
	a.mu.Lock()
	announced := a.streamDownSaid
	a.streamDownAt = time.Time{}
	a.streamDownSaid = false
	a.mu.Unlock()

	log.Info().Int("replayed", replayed).Msg("marketplace event stream live")
	if !announced {
		return
	}
	msg := "✅ <b>Лента событий Tonnel снова идёт</b>"
	if replayed > 0 {
		msg += fmt.Sprintf("\nДогнал %d пропущенных событий через реплей.", replayed)
	}
	a.notify(msg)
}

// onStreamDown reports an outage once it has lasted long enough to matter. A
// reconnect takes seconds and happens on its own; announcing every one of them
// would recreate exactly the noise this change exists to remove.
func (a *App) onStreamDown(err error) {
	log.Warn().Err(err).Msg("marketplace event stream dropped")

	a.mu.Lock()
	if a.streamDownAt.IsZero() {
		a.streamDownAt = time.Now()
	}
	down := time.Since(a.streamDownAt)
	report := !a.streamDownSaid && down >= streamDownGrace
	if report {
		a.streamDownSaid = true
	}
	a.mu.Unlock()

	if !report {
		return
	}
	a.notify(fmt.Sprintf(
		"⚠️ <b>Лента событий Tonnel молчит</b> %s\n%s\n\nПереключился на опрос стакана — это медленнее и упирается в антибот. Продолжаю переподключаться.",
		dur(down), bot.Esc(err.Error())))
}

// streamLine is the feed's health, for /status.
func (a *App) streamLine() string {
	if a.stream == nil {
		return "Лента событий: выключена (EVENTS_ENABLED=0)"
	}
	switch {
	case a.stream.Healthy():
		return fmt.Sprintf("Лента событий: живая с %s · %d событий · последнее %s",
			dur(time.Since(a.stream.ConnectedAt())), a.stream.Events(), ago(a.stream.LastEvent()))
	case a.stream.Connected():
		return fmt.Sprintf("⚠️ Лента событий: подключена, но молчит %s", ago(a.stream.LastEvent()))
	default:
		line := "⚠️ Лента событий: нет соединения"
		if e := a.stream.LastError(); e != "" {
			line += " · " + truncate(e, 90)
		}
		return line
	}
}

// routesBlock lists the egresses and their standing with the anti-bot layer.
//
// It is one line per route because the interesting question during a block is
// never "are we blocked" — the desk already said so — but "which addresses
// still work", and that is what decides whether the answer is to wait or to
// add another proxy.
func (a *App) routesBlock() string {
	routes := a.api.Routes()
	if len(routes) <= 1 {
		return "" // one route is not a rotation worth reporting on
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n<b>Маршруты</b> — %d из %d доступны\n", a.api.RoutesAvailable(), len(routes))
	for _, r := range routes {
		mark, note := "✅", "готов"
		switch {
		case r.Cooling > 0:
			mark, note = "❄️", "остывает "+dur(r.Cooling)
			if r.LastErr != "" {
				note += " · " + truncate(r.LastErr, 60)
			}
		case !r.LastOK.IsZero():
			note = "последний ответ " + ago(r.LastOK)
		}
		fmt.Fprintf(&b, "%s %s — %s\n", mark, bot.Esc(r.Name), bot.Esc(note))
	}
	return b.String()
}

// streamHealthy reports whether the push feed can be trusted right now. The
// pollers it replaced read this to decide whether to spend a request.
func (a *App) streamHealthy() bool {
	return a.stream != nil && a.stream.Healthy()
}

// probeStream connects to the event feed and waits for real traffic. It is used
// by the smoke command, so it deliberately builds its own consumer rather than
// touching the running one: nothing here writes to the database.
func (a *App) probeStream(ctx context.Context, wait time.Duration) (string, error) {
	if !a.cfg.EventsEnabled {
		return "", fmt.Errorf("disabled by EVENTS_ENABLED")
	}

	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	seen := make(chan tonnel.Event, 1)
	probe := tonnel.NewStream(tonnel.StreamOptions{
		Host: a.cfg.EventHost,
		Handle: func(_ context.Context, ev tonnel.Event) error {
			select {
			case seen <- ev:
			default:
			}
			return nil
		},
	})
	go probe.Run(ctx)

	for {
		select {
		case ev := <-seen:
			what := ev.Type
			if g := ev.Data.Gift; g != nil {
				what += fmt.Sprintf(" · %s / %s", g.GiftName, tonnel.BaseAttr(g.Model))
			}
			return fmt.Sprintf("live on %s; first event %s", a.cfg.EventHost, what), nil
		case <-ctx.Done():
			if e := probe.LastError(); e != "" {
				return "", fmt.Errorf("%s", e)
			}
			if probe.Connected() {
				return "", fmt.Errorf("connected to %s but no event arrived in %s", a.cfg.EventHost, wait)
			}
			return "", fmt.Errorf("could not connect to %s within %s", a.cfg.EventHost, wait)
		}
	}
}
