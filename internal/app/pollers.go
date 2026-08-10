package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rs/zerolog/log"

	"floorline/internal/bot"
	"floorline/internal/pricing"
	"floorline/internal/signal"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

// pollFeed reads the newest listings and evaluates whatever is new or cheaper.
//
// This is the hot path. Everything expensive — the order-book lookup — happens
// inside the detector and only for candidates that survive the local filters.
func (a *App) pollFeed(ctx context.Context) error {
	gifts, err := a.api.Feed(ctx, 30)
	if err != nil {
		return fmt.Errorf("read feed: %w", err)
	}
	if len(gifts) == 0 {
		return nil
	}

	now := time.Now()
	changes, err := a.st.UpsertListings(ctx, gifts, now)
	if err != nil {
		return fmt.Errorf("store listings: %w", err)
	}

	candidates := changes.Candidates()
	if len(candidates) == 0 {
		return nil
	}

	byID := make(map[int64]tonnel.Gift, len(gifts))
	for i := range gifts {
		byID[gifts[i].GiftID.Int()] = gifts[i]
	}

	limits := a.rm.Limits()
	watches, err := a.st.Watches(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("reading the watchlist failed")
	}
	var decisions []*signal.Decision

	for _, id := range candidates {
		g, ok := byID[id]
		if !ok {
			continue
		}
		a.checkWatch(g, watches)

		dec, err := a.det.Evaluate(ctx, g, limits, now)
		if err != nil {
			log.Warn().Err(err).Int64("gift", id).Msg("evaluation failed")
			continue
		}
		if dec != nil && dec.Signal {
			decisions = append(decisions, dec)
		}
	}

	// Highest-conviction first, so a burst does not bury the best opportunity.
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].Score > decisions[j].Score })
	for _, dec := range decisions {
		if err := a.handleDecision(ctx, dec, now); err != nil {
			log.Error().Err(err).Int64("gift", dec.Gift.GiftID.Int()).Msg("handling decision failed")
		}
	}
	return nil
}

// handleDecision records a signal, buys it if it clears every unattended gate,
// and otherwise sends the card.
func (a *App) handleDecision(ctx context.Context, dec *signal.Decision, now time.Time) error {
	v := dec.Val
	sigID, err := a.st.InsertSignal(ctx, store.SignalRow{
		TS:       now,
		Kind:     signal.KindBuy,
		GiftID:   dec.Gift.GiftID.Int(),
		Key:      v.Key,
		Price:    v.Price,
		Exit:     v.Exit,
		Edge:     v.Edge,
		Velocity: v.Liq.Velocity,
		Score:    dec.Score,
	})
	if err != nil {
		return fmt.Errorf("record signal: %w", err)
	}

	var note string
	if dec.Auto {
		allowed, why := a.rm.Allow(ctx, v.Key, v.Price, now)
		if allowed {
			// Buy first, explain afterwards — the listing is a race.
			out, buyErr := a.ex.Buy(ctx, v, dec.Gift, "auto", now)
			a.notify(a.renderPurchase(ctx, sigID, out, buyErr, true))
			return nil
		}
		note = "auto-buy blocked: " + why
	} else if len(dec.AutoFails) > 0 {
		note = "manual only — " + dec.AutoFails[0]
	}

	if dec.Suppressed {
		return nil
	}
	if a.tg == nil {
		log.Info().Str("model", v.Key.String()).Float64("edge", v.Edge).Msg("signal (no telegram configured)")
		return nil
	}
	a.tg.NotifySignal(a.renderCard(ctx, dec, note), sigID, dec.Gift.GiftID.Int(), v.Price)
	return a.st.MarkSignalSent(ctx, sigID, time.Now())
}

// pollStats refreshes the whole-market snapshot. One request covers every model
// of every collection, which is why the pricing engine is built around it.
func (a *App) pollStats(ctx context.Context) error {
	stats, err := a.api.FilterStats(ctx)
	if err != nil {
		return fmt.Errorf("read market stats: %w", err)
	}
	if len(stats) == 0 {
		return nil
	}

	now := time.Now()
	if err := a.st.ReplaceModelStats(ctx, stats, now); err != nil {
		return fmt.Errorf("store market stats: %w", err)
	}

	// The history table only needs a coarse trail; snapshotting every minute
	// would be thousands of rows an hour for no extra insight.
	if a.throttle("history-snapshot", 10*time.Minute) {
		if err := a.st.SnapshotModelHistory(ctx, stats, now.Truncate(10*time.Minute)); err != nil {
			log.Warn().Err(err).Msg("snapshotting market history failed")
		}
	}

	a.detectNewCollections(stats)
	a.detectFloorDrops(ctx, now)
	return nil
}

// detectNewCollections alerts on a collection name we have never seen. The
// first hours of a new drop are the most volatile window on this market.
func (a *App) detectNewCollections(stats []tonnel.ModelStat) {
	a.mu.Lock()
	first := len(a.collections) == 0
	var fresh []string
	for _, s := range stats {
		if _, ok := a.collections[s.Key.Name]; !ok {
			a.collections[s.Key.Name] = struct{}{}
			if !first {
				fresh = append(fresh, s.Key.Name)
			}
		}
	}
	a.mu.Unlock()

	if len(fresh) == 0 {
		return
	}
	sort.Strings(fresh)
	msg := "🆕 <b>New collection on Tonnel</b>\n"
	for _, n := range fresh {
		msg += "• " + bot.Esc(n) + "\n"
	}
	a.notify(msg)
}

// detectFloorDrops watches the models we care about — anything on the watchlist
// or in inventory — for a sharp fall in the floor.
func (a *App) detectFloorDrops(ctx context.Context, now time.Time) {
	keys, err := a.trackedModels(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("collecting tracked models failed")
		return
	}
	for _, key := range keys {
		cur, err := a.st.ModelStat(ctx, key)
		if err != nil || cur == nil || cur.Floor <= 0 {
			continue
		}
		before, ok, err := a.st.FloorAt(ctx, key, now.Add(-time.Hour))
		if err != nil || !ok || before <= 0 {
			continue
		}
		drop := (before - cur.Floor) / before
		if drop < 0.15 {
			continue
		}
		if !a.throttle("floordrop:"+key.ID(), 3*time.Hour) {
			continue
		}
		a.notify(fmt.Sprintf(
			"📉 <b>Floor drop</b> — %s\n%s → %s in an hour (−%.0f%%)\nSupply %d",
			bot.Esc(key.String()), num(before), num(cur.Floor), drop*100, cur.Supply))
	}
}

// pollSales pulls new trades. It keeps walking pages while the page it just
// read was entirely new, which is how it survives a burst without falling
// behind or re-reading the whole history every cycle.
func (a *App) pollSales(ctx context.Context) error {
	const pageSize = 50
	const maxPages = 8

	total := 0
	for page := 1; page <= maxPages; page++ {
		sales, err := a.api.SaleHistory(ctx, tonnel.SaleHistoryQuery{Page: page, Limit: pageSize, Type: "SALE"})
		if err != nil {
			return fmt.Errorf("read sale history page %d: %w", page, err)
		}
		if len(sales) == 0 {
			break
		}
		n, err := a.st.InsertSales(ctx, sales)
		if err != nil {
			return fmt.Errorf("store sales: %w", err)
		}
		total += n
		if n < len(sales) {
			break // we have caught up with what we already hold
		}
	}

	if total > 0 {
		if err := a.refreshCoverage(ctx); err != nil {
			log.Warn().Err(err).Msg("refreshing coverage failed")
		}
	}
	a.detectSweeps(ctx, time.Now())
	return nil
}

// detectSweeps flags a model that suddenly trades far above its usual rate with
// several distinct buyers — the shape of someone clearing the cheap end.
func (a *App) detectSweeps(ctx context.Context, now time.Time) {
	if !a.throttle("sweep-scan", 5*time.Minute) {
		return
	}
	rows, err := a.st.SweepCandidates(ctx, now.Add(-30*time.Minute), 5)
	if err != nil {
		log.Warn().Err(err).Msg("sweep scan failed")
		return
	}
	for _, r := range rows {
		if r.Buyers < 2 {
			continue // one buyer repeatedly hitting the book is not a market move
		}
		if !a.throttle("sweep:"+r.Key.ID(), 2*time.Hour) {
			continue
		}
		stat, _ := a.st.ModelStat(ctx, r.Key)
		floorTxt := "unknown"
		if stat != nil && stat.Floor > 0 {
			floorTxt = num(stat.Floor)
		}
		a.notify(fmt.Sprintf(
			"🌊 <b>Sweep</b> — %s\n%d trades in 30 min from %d buyers\nRange %s – %s · floor now %s",
			bot.Esc(r.Key.String()), r.Count, r.Buyers, num(r.MinPrice), num(r.MaxPrice), floorTxt))
	}
}

// pollInventory reconciles what we own against what we think we own, refreshes
// the balance, and raises book-keeping alerts.
func (a *App) pollInventory(ctx context.Context) error {
	if bal, err := a.api.Balance(ctx); err == nil {
		a.rm.SetBalance(bal.TON)
	} else {
		log.Warn().Err(err).Msg("reading balance failed")
	}

	now := time.Now()
	owned := make(map[int64]tonnel.Gift)

	for _, listed := range []bool{false, true} {
		gifts, err := a.api.MyGifts(ctx, listed, 1, 30)
		if err != nil {
			return fmt.Errorf("read inventory (listed=%v): %w", listed, err)
		}
		for i := range gifts {
			g := gifts[i]
			owned[g.GiftID.Int()] = g
			if err := a.reconcileOwned(ctx, g, listed, now); err != nil {
				log.Warn().Err(err).Int64("gift", g.GiftID.Int()).Msg("reconciling a gift failed")
			}
		}
	}

	positions, err := a.st.OpenPositions(ctx)
	if err != nil {
		return fmt.Errorf("load positions: %w", err)
	}
	for _, p := range positions {
		if _, still := owned[p.GiftID]; !still {
			a.closePosition(ctx, p, now)
			continue
		}
		a.checkUndercut(ctx, p, now)
		a.checkStale(ctx, p, now)
	}
	return nil
}

// reconcileOwned makes the local position table agree with the marketplace,
// importing anything bought outside the bot so /pnl stays honest.
func (a *App) reconcileOwned(ctx context.Context, g tonnel.Gift, listed bool, now time.Time) error {
	id := g.GiftID.Int()
	existing, err := a.st.GetPosition(ctx, id)
	if err != nil {
		return err
	}

	if existing == nil {
		// Bought outside Floorline: track it, but flag that the cost basis is
		// unknown rather than silently pretending it was free.
		pos := store.Position{
			GiftID:   id,
			GiftNum:  g.GiftNum.Int(),
			Key:      g.Key(),
			Backdrop: tonnel.BaseAttr(g.Backdrop),
			Symbol:   tonnel.BaseAttr(g.Symbol),
			BoughtAt: now,
			Status:   store.StatusOpen,
			Source:   "import",
			Note:     "imported from inventory; entry price unknown",
		}
		if listed {
			pos.Status = store.StatusListed
			pos.ListPrice = g.Price.Float()
			pos.ListedAt = now
		}
		return a.st.UpsertPosition(ctx, pos)
	}

	if listed && g.Price.Float() > 0 && existing.ListPrice != g.Price.Float() {
		return a.st.SetPositionListed(ctx, id, g.Price.Float(), now)
	}
	return nil
}

// closePosition records a position that has left our inventory, preferring the
// real sale price from the trade history over the price we listed at.
func (a *App) closePosition(ctx context.Context, p store.Position, now time.Time) {
	price := p.ListPrice
	note := ""

	sales, err := a.st.SalesSince(ctx, p.Key, p.BoughtAt)
	if err == nil {
		for _, s := range sales {
			if s.GiftID == p.GiftID {
				price = s.Price
				note = ""
				break
			}
		}
	}
	if price <= 0 {
		note = "sale price unknown"
	}

	if err := a.st.SetPositionSold(ctx, p.GiftID, price, now); err != nil {
		log.Warn().Err(err).Int64("gift", p.GiftID).Msg("closing position failed")
		return
	}

	msg := fmt.Sprintf("💰 <b>Sold</b> — %s\nEntry %s → exit %s",
		bot.Esc(p.Key.String()), num(p.BuyPrice), num(price))
	if p.BuyPrice > 0 && price > 0 {
		net := price*(1-a.cfg.TonnelFee) - p.BuyPrice
		msg += fmt.Sprintf("\nNet %s (%+.1f%%) after %.1f%% fee, held %s",
			num(net), net/p.BuyPrice*100, a.cfg.TonnelFee*100, dur(now.Sub(p.BoughtAt)))
	}
	if note != "" {
		msg += "\n<i>" + bot.Esc(note) + "</i>"
	}
	a.notify(msg)
}

// checkUndercut warns when our ask is no longer the cheapest.
func (a *App) checkUndercut(ctx context.Context, p store.Position, now time.Time) {
	if p.Status != store.StatusListed || p.ListPrice <= 0 {
		return
	}
	stat, err := a.st.ModelStat(ctx, p.Key)
	if err != nil || stat == nil || stat.Floor <= 0 {
		return
	}
	if stat.Floor >= p.ListPrice {
		return
	}
	if !a.throttle(fmt.Sprintf("undercut:%d:%.2f", p.GiftID, stat.Floor), 6*time.Hour) {
		return
	}
	a.notify(fmt.Sprintf(
		"🥊 <b>Undercut</b> — %s\nYour ask %s · floor now %s (−%.1f%%)\nEntry %s · <code>/relist %d</code>",
		bot.Esc(p.Key.String()), num(p.ListPrice), num(stat.Floor),
		(p.ListPrice-stat.Floor)/p.ListPrice*100, num(p.BuyPrice), p.GiftID))
}

// checkStale warns when a position has been sitting far longer than the model's
// trade rate says it should have.
func (a *App) checkStale(ctx context.Context, p store.Position, now time.Time) {
	held := now.Sub(p.BoughtAt)
	if held < 24*time.Hour {
		return
	}
	sales, err := a.st.SalesSince(ctx, p.Key, now.Add(-a.window()))
	if err != nil {
		return
	}
	liq := pricing.ComputeLiquidity(sales, now, a.window(), a.Coverage())
	if liq.Velocity <= 0 {
		return
	}
	expected := time.Duration(3 / liq.Velocity * float64(24*time.Hour))
	if held < expected {
		return
	}
	if !a.throttle(fmt.Sprintf("stale:%d", p.GiftID), 24*time.Hour) {
		return
	}
	a.notify(fmt.Sprintf(
		"🕸 <b>Stale position</b> — %s\nHeld %s; this model trades %.1f×/day, so it should have gone in ~%s.\nEntry %s · ask %s · <code>/relist %d</code>",
		bot.Esc(p.Key.String()), dur(held), liq.Velocity, dur(expected/3), num(p.BuyPrice), num(p.ListPrice), p.GiftID))
}

// checkWatch alerts on any listing of a watched model under the price the
// operator asked for, independently of the edge gates. A watch is an explicit
// "tell me regardless", so it bypasses the liquidity reasoning entirely.
func (a *App) checkWatch(g tonnel.Gift, watches []store.Watch) {
	key := g.Key()
	for _, w := range watches {
		if w.Key != key {
			continue
		}
		price := g.Price.Float()
		if w.MaxPrice > 0 && price > w.MaxPrice {
			continue
		}
		if !a.throttle(fmt.Sprintf("watch:%d", g.GiftID.Int()), 24*time.Hour) {
			continue
		}
		a.notify(fmt.Sprintf(
			"👁 <b>Watchlist</b> — %s\nListed at %s\n<code>/val %d</code>",
			bot.Esc(key.String()), num(price), g.GiftID.Int()))
		return
	}
}

// trackedModels is the watchlist plus everything currently in inventory.
func (a *App) trackedModels(ctx context.Context) ([]tonnel.ModelKey, error) {
	seen := make(map[string]struct{})
	var out []tonnel.ModelKey

	add := func(k tonnel.ModelKey) {
		if _, ok := seen[k.ID()]; ok {
			return
		}
		seen[k.ID()] = struct{}{}
		out = append(out, k)
	}

	watches, err := a.st.Watches(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range watches {
		add(w.Key)
	}
	positions, err := a.st.OpenPositions(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range positions {
		add(p.Key)
	}
	return out, nil
}

// maintenance prunes data that no longer affects a decision.
func (a *App) maintenance(ctx context.Context) error {
	return a.st.Prune(ctx, time.Now(), a.cfg.LookbackDays*3)
}
