package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/rs/zerolog/log"

	"floorline/internal/bot"
	"floorline/internal/market"
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
			if fit, _, err := a.rm.PortfolioFit(ctx, dec.Val.Key, dec.Val.Cost); err == nil {
				dec.Val.ScoreBreakdown = pricing.BuildScore(dec.Val, fit)
				dec.Score = dec.Val.ScoreBreakdown.Total
			}
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

// spendable is the largest single purchase the desk could make right now: the
// ticket limit, cut down by whatever the balance leaves above the reserve.
//
// Alerting on a 53 GRAM lot while the ticket cap is 5.5 and the wallet holds 25
// is not a trading signal, it is a notification nobody can act on.
func (a *App) spendable() (float64, bool) {
	l := a.rm.Limits()
	room, known := math.Inf(1), false
	if l.MaxTicket > 0 {
		room, known = l.MaxTicket, true
	}
	if bal, ok := a.rm.Balance(); ok {
		known = true
		room = math.Min(room, math.Max(bal-l.MinBalanceReserve, 0))
	}
	if !known {
		return 0, false
	}
	return room, true
}

// crossMarketDepth is robust price discovery: each venue contributes the middle
// of its first three asks, then venues are combined by their median.
//
// The individual asks travel with the reference. A single number can only ever
// nudge the blend; the queue behind it is what proves a buyer has somewhere
// cheaper to go, which is the difference between a misprice and an overprice.
//
// Two of those asks are also restated as buyer cost, fee included. This is the
// only place that knows which venue an ask came from, so it is the only place
// that can make the fee correction — and comparing gross stickers across venues
// with different buyer fees quietly mispriced the walkaway on every trade.
func (a *App) crossMarketDepth(ctx context.Context, v pricing.Valuation) pricing.CrossMarket {
	if !a.cross.Enabled() {
		return pricing.CrossMarket{} // no venue configured is a choice, not a failure
	}
	// The budget has to fit a venue's own pacing. Each one may need two calls —
	// exact attributes, then a model-wide fallback — and they are rate-limited
	// to roughly one per second, so a four-second deadline silently starved the
	// comparison whenever several cards were priced in a row. Losing it is not
	// cosmetic: it is the cap that holds an optimistic exit down.
	qctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	quotes, unreachable := a.cross.QuotesForGift(qctx, v.Key.Name, v.Key.Model, v.Backdrop, v.Symbol)

	cm := pricing.CrossMarket{Unreachable: unreachable}
	var refs, buyerCosts []float64
	// The cheapest offer is what bounds the exit, so it is that offer's match
	// quality — not the average across venues — that decides whether the bound
	// is about the same asset at all.
	cheapest, comparable := 0.0, false
	for _, q := range quotes {
		ref := q.Reference()
		if ref <= 0 {
			continue
		}
		refs = append(refs, ref)
		cm.Venues++
		asks := q.Asks
		if len(asks) == 0 {
			asks = []float64{q.Floor}
		}
		if cheapest <= 0 || asks[0] < cheapest {
			cheapest, comparable = asks[0], market.Comparable(q.Scope)
		}
		cm.Asks = append(cm.Asks, asks...)
		// The displayed ask is what the buyer pays. Quote.Fee is deliberately
		// not added here: on these venues it is the *seller's* commission —
		// Quote.Net is Floor*(1-Fee) — so charging it to the buyer would inflate
		// the walkaway price and hand back exactly the optimism being removed.
		// The fee that does fall on a buyer is Tonnel's own referral, and it is
		// applied on our side of the comparison in pricing.
		for _, p := range asks {
			if p > 0 {
				buyerCosts = append(buyerCosts, p)
			}
		}
	}
	if len(refs) == 0 {
		return pricing.CrossMarket{Unreachable: unreachable}
	}
	sort.Float64s(refs)
	sort.Float64s(cm.Asks)
	sort.Float64s(buyerCosts)
	cm.Support = refs[len(refs)/2]
	cm.Comparable = comparable
	if len(buyerCosts) > 0 {
		cm.BestBuyerCost = buyerCosts[0]
		cm.DepthBuyerCost = buyerCosts[minInt(3, len(buyerCosts))-1]
	}
	return cm
}

// handleDecision records a signal, buys it if it clears every unattended gate,
// and otherwise sends the card.
func (a *App) handleDecision(ctx context.Context, dec *signal.Decision, now time.Time) error {
	v := dec.Val
	payload, _ := json.Marshal(struct {
		Score                 pricing.ScoreBreakdown `json:"score"`
		Confidence            float64                `json:"confidence"`
		ChosenExit            string                 `json:"chosen_exit"`
		FastExit, PatientExit float64
		Attribute             pricing.AttributeValue `json:"attribute"`
		DataAgeSeconds        float64                `json:"data_age_seconds"`
	}{v.ScoreBreakdown, v.Confidence, v.ChosenExit, v.FastExit, v.PatientExit, v.Attribute, v.DataAge.Seconds()})
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
		Payload:  string(payload),
	})
	if err != nil {
		return fmt.Errorf("record signal: %w", err)
	}

	var note string
	if dec.Auto {
		// Reserve against the worst known referral treatment. The executor later
		// replaces this estimate with the exact balance debit.
		permit, why := a.rm.ReservePurchase(ctx, v.Key, v.Cost, now)
		if permit != nil {
			defer permit.Release()
			fresh, freshWhy := a.revalidateAuto(ctx, dec)
			if freshWhy == "" {
				costCeiling := fresh.Val.Cost
				if allowed, riskWhy := a.rm.Allow(ctx, fresh.Val.Key, costCeiling, time.Now()); allowed {
					out, buyErr := a.ex.Buy(ctx, fresh.Val, fresh.Gift, "auto", time.Now())
					a.notify(a.renderPurchase(ctx, sigID, out, buyErr, true))
					return nil
				} else {
					freshWhy = riskWhy
				}
			}
			note = "автобай заблокирован после перепроверки: " + freshWhy
		} else {
			note = "автобай заблокирован: " + why
		}
	} else if len(dec.AutoFails) > 0 {
		note = "только вручную — " + dec.AutoFails[0]
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

// revalidateAuto re-reads the listing and every executable market input. A
// signal is only an invitation to check; no valuation object crosses into the
// write path without passing all gates again against a forced-fresh book.
func (a *App) revalidateAuto(ctx context.Context, old *signal.Decision) (*signal.Decision, string) {
	if old == nil {
		return nil, "пустое решение"
	}
	g, err := a.api.GiftData(ctx, old.Gift.GiftID.Int())
	if err != nil {
		return nil, "не удалось перечитать лот: " + err.Error()
	}
	if g == nil {
		return nil, "перепроверка вернула пустой лот"
	}
	// /api/giftData reports no seller. Carry the one the feed gave us, or the
	// own-lot guard downstream has nothing to compare against.
	if g.Seller.Int() == 0 {
		g.Seller = old.Gift.Seller
	}
	a.books.Invalidate(old.Val.Key)
	fresh, err := a.det.EvaluateFresh(ctx, *g, a.rm.Limits(), time.Now())
	if err != nil {
		return nil, "не удалось повторить оценку: " + err.Error()
	}
	if fresh == nil || !fresh.Signal || !fresh.Auto {
		if fresh != nil && len(fresh.AutoFails) > 0 {
			return nil, fresh.AutoFails[0]
		}
		if fresh != nil && len(fresh.SignalFails) > 0 {
			return nil, fresh.SignalFails[0]
		}
		return nil, "лот больше не проходит фильтры"
	}
	return fresh, ""
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
	msg := "🆕 <b>Новая коллекция на Tonnel</b>\n"
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
			"📉 <b>Флор упал</b> — %s\n%s → %s за час (−%.0f%%)\nСаплай %d",
			bot.Esc(key.String()), num(before), num(cur.Floor), drop*100, cur.Supply))
	}
}

// pollSales pulls new trades, one slice of the market per tick.
//
// Trade history can only be read per collection and only reliably by asking for
// an explicit time window — the endpoint's ordering cannot be trusted, verified
// against the live API. So the whole market is covered by rotation, each
// collection refreshed over the full lookback window.
//
// Staleness is not a concern here: a fourteen-day velocity does not move in the
// minutes it takes one cycle to come back around.
func (a *App) pollSales(ctx context.Context) error {
	batch := a.nextCollections(a.cfg.SalesBatch)
	if len(batch) == 0 {
		return nil // the market snapshot has not landed yet
	}

	total := 0
	for _, name := range batch {
		// A short window keeps this to one page per type: the rotation comes
		// back around every few minutes, and the deep history already exists
		// from the backfill.
		sales, err := a.api.TradeHistory(ctx, name, time.Now().Add(-a.cfg.SalesWindow), 3)
		if err != nil {
			return fmt.Errorf("read trades for %s: %w", name, err)
		}
		n, err := a.st.InsertSales(ctx, sales)
		if err != nil {
			return fmt.Errorf("store trades for %s: %w", name, err)
		}
		total += n
	}

	if total > 0 {
		if err := a.refreshCoverage(ctx); err != nil {
			log.Warn().Err(err).Msg("refreshing coverage failed")
		}
	}
	a.detectSweeps(ctx, time.Now())
	return nil
}

// nextCollections returns the next slice of the rotation, refreshing the list
// from the market snapshot when it is empty or exhausted.
func (a *App) nextCollections(n int) []string {
	if n <= 0 {
		n = 4
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.rotation.idx >= len(a.rotation.names) {
		names := make([]string, 0, len(a.collections))
		for name := range a.collections {
			names = append(names, name)
		}
		sort.Strings(names)
		a.rotation.names = names
		a.rotation.idx = 0
		a.rotation.cycles++
	}
	if len(a.rotation.names) == 0 {
		return nil
	}

	end := a.rotation.idx + n
	if end > len(a.rotation.names) {
		end = len(a.rotation.names)
	}
	out := append([]string(nil), a.rotation.names[a.rotation.idx:end]...)
	a.rotation.idx = end
	return out
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
		if r.Gifts < 2 {
			continue // the same gift changing hands is not a market move
		}
		if !a.throttle("sweep:"+r.Key.ID(), 2*time.Hour) {
			continue
		}
		stat, _ := a.st.ModelStat(ctx, r.Key)
		floorTxt := "неизвестен"
		if stat != nil && stat.Floor > 0 {
			floorTxt = num(stat.Floor)
		}
		a.notify(fmt.Sprintf(
			"🌊 <b>Скупка</b> — %s\n%d сделок за 30 минут по %d разным гифтам\nДиапазон %s – %s · флор сейчас %s",
			bot.Esc(r.Key.String()), r.Count, r.Gifts, num(r.MinPrice), num(r.MaxPrice), floorTxt))
	}
}

// pollInventory reconciles what we own against what we think we own, refreshes
// the balance, and raises book-keeping alerts.
func (a *App) pollInventory(ctx context.Context) error {
	if bal, err := a.api.Balance(ctx); err == nil {
		a.rm.SetBalance(bal.GRAM)
	} else {
		log.Warn().Err(err).Msg("reading balance failed")
	}

	now := time.Now()
	owned := make(map[int64]tonnel.Gift)

	for _, listed := range []bool{false, true} {
		for page := 1; ; page++ {
			gifts, err := a.api.MyGifts(ctx, listed, page, 30)
			if err != nil {
				return fmt.Errorf("read inventory (listed=%v page=%d): %w", listed, page, err)
			}
			for i := range gifts {
				g := gifts[i]
				owned[g.GiftID.Int()] = g
				if err := a.reconcileOwned(ctx, g, listed, now); err != nil {
					log.Warn().Err(err).Int64("gift", g.GiftID.Int()).Msg("reconciling a gift failed")
				}
			}
			if len(gifts) < 30 {
				break
			}
		}
	}

	positions, err := a.st.TrackedPositions(ctx)
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
	if a.throttle("position-marks", 10*time.Minute) {
		if current, err := a.st.OpenPositions(ctx); err == nil {
			a.snapshotPositionMarks(ctx, current, now)
		}
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

	reacquired := existing != nil && (existing.Status == store.StatusSold || existing.Status == store.StatusReturned)
	if existing == nil || reacquired {
		// Bought outside Floorline: track it, but flag that the cost basis is
		// unknown rather than silently pretending it was free.
		after := time.Time{}
		if reacquired {
			after = existing.SoldAt
		}
		buyPrice, boughtAt, found, lookupErr := a.st.AcquisitionForGiftAfter(ctx, id, after)
		if lookupErr != nil {
			return lookupErr
		}
		costSource, costConfidence := "unknown", 0.0
		note := "импортирован из инвентаря; цена входа неизвестна — задай через /cost"
		if found {
			buyPrice *= 1 + math.Max(a.cfg.TonnelFee, 0)
			costSource, costConfidence, note = "sale_history", .85, "импортирован из инвентаря; цену входа восстановил по истории сделок"
		} else {
			boughtAt = now
		}
		_, mr := tonnel.SplitAttr(g.Model)
		_, br := tonnel.SplitAttr(g.Backdrop)
		_, sr := tonnel.SplitAttr(g.Symbol)
		source, eventKind := "import", "acquired"
		if reacquired {
			source, eventKind = "reacquired", "reacquired"
		}
		pos := store.Position{
			GiftID:      id,
			GiftNum:     g.GiftNum.Int(),
			Key:         g.Key(),
			Backdrop:    tonnel.BaseAttr(g.Backdrop),
			Symbol:      tonnel.BaseAttr(g.Symbol),
			ModelRarity: mr, BackdropRarity: br, SymbolRarity: sr,
			BuyPrice: buyPrice, BoughtAt: boughtAt,
			CostSource: costSource, CostConfidence: costConfidence,
			Status: store.StatusOpen,
			Source: source,
			Note:   note,
		}
		if listed {
			pos.Status = store.StatusListed
			pos.ListPrice = g.Price.Float()
			pos.ListedAt = now
		}
		if err := a.st.UpsertPosition(ctx, pos); err != nil {
			return err
		}
		if err := a.st.RecordPositionEvent(ctx, id, eventKind, 0, buyPrice, note, boughtAt); err != nil {
			return err
		}
		if listed {
			return a.st.RecordPositionEvent(ctx, id, "listed", 0, g.Price.Float(), "observed in Tonnel inventory", now)
		}
		return nil
	}

	if existing.BuyPrice <= 0 {
		after := time.Time{}
		if existing.Source == "reacquired" {
			if cycles, _ := a.st.PositionTradesForGift(ctx, id); len(cycles) > 0 {
				after = cycles[0].SoldAt
			}
		}
		if price, boughtAt, found, err := a.st.AcquisitionForGiftAfter(ctx, id, after); err != nil {
			return err
		} else if found {
			price *= 1 + math.Max(a.cfg.TonnelFee, 0)
			if err := a.st.SetRecoveredCostBasis(ctx, id, price, boughtAt, "sale_history", .85); err != nil {
				return err
			}
			_ = a.st.RecordPositionEvent(ctx, id, "cost_recovered", 0, price, "matched physical gift in Tonnel sales", now)
			existing.BuyPrice, existing.BoughtAt = price, boughtAt
		}
	}
	if existing.Status == store.StatusMissing {
		if listed {
			if err := a.st.SetPositionListed(ctx, id, g.Price.Float(), now); err != nil {
				return err
			}
			return a.st.RecordPositionEvent(ctx, id, "returned", 0, g.Price.Float(), "gift reappeared listed", now)
		}
	}
	if listed && g.Price.Float() > 0 && existing.ListPrice != g.Price.Float() {
		old := existing.ListPrice
		if err := a.st.SetPositionListed(ctx, id, g.Price.Float(), now); err != nil {
			return err
		}
		kind := "listed"
		if old > 0 {
			kind = "repriced"
			_ = a.st.RecordReprice(ctx, id, old, g.Price.Float(), "observed manual/external reprice", now)
		}
		return a.st.RecordPositionEvent(ctx, id, kind, old, g.Price.Float(), "observed in Tonnel inventory", now)
	}
	if !listed && (existing.Status == store.StatusListed || existing.Status == store.StatusMissing || existing.ListPrice > 0) {
		old := existing.ListPrice
		if err := a.st.SetPositionUnlisted(ctx, id); err != nil {
			return err
		}
		kind := "unlisted"
		detail := "removed from sale"
		if existing.Status == store.StatusMissing {
			kind = "returned"
			detail = "gift reappeared unlisted"
		}
		return a.st.RecordPositionEvent(ctx, id, kind, old, 0, detail, now)
	}
	return nil
}

// closePosition records a position that has left our inventory, preferring the
// real sale price from the trade history over the price we listed at.
func (a *App) closePosition(ctx context.Context, p store.Position, now time.Time) {
	after := p.BoughtAt
	price, soldAt, found, err := a.st.SaleForGiftAfter(ctx, p.GiftID, after)
	if err != nil {
		log.Warn().Err(err).Int64("gift", p.GiftID).Msg("confirming position sale failed")
		return
	}
	if !found {
		if p.Status != store.StatusMissing {
			if err := a.st.SetPositionMissing(ctx, p.GiftID, now); err != nil {
				return
			}
			_ = a.st.RecordPositionEvent(ctx, p.GiftID, "missing", p.ListPrice, 0, "пропал из инвентаря Tonnel, продажа не найдена", now)
			a.notify(fmt.Sprintf("🔎 <b>Изменение в инвентаре</b> — %s\nГифт пропал из инвентаря Tonnel, но продажа не подтверждена. Помечаю пропавшим, не проданным — буду дальше сверять историю.\n<code>/history %d</code>", bot.Esc(p.Key.String()), p.GiftID))
		}
		return
	}

	if err := a.st.SetPositionSold(ctx, p.GiftID, price, soldAt); err != nil {
		log.Warn().Err(err).Int64("gift", p.GiftID).Msg("closing position failed")
		return
	}
	_ = a.st.RecordPositionEvent(ctx, p.GiftID, "sold", p.ListPrice, price, "confirmed by Tonnel sale history", soldAt)

	msg := fmt.Sprintf("💰 <b>Продано</b> — %s\nВход %s → выход %s",
		bot.Esc(p.Key.String()), num(p.BuyPrice), num(price))
	if p.BuyPrice > 0 && price > 0 {
		net := price - p.BuyPrice
		msg += fmt.Sprintf("\nНет %s (%+.1f%%), держали %s",
			num(net), net/p.BuyPrice*100, dur(soldAt.Sub(p.BoughtAt)))
	}
	a.notify(msg)
}

func (a *App) snapshotPositionMarks(ctx context.Context, positions []store.Position, now time.Time) {
	q, _, _ := a.st.LatestGramQuote(ctx)
	for _, p := range positions {
		events, _ := a.st.PositionEvents(ctx, p.GiftID, 1)
		if len(events) == 0 {
			_ = a.st.RecordPositionEvent(ctx, p.GiftID, "tracking_started", p.BuyPrice, p.ListPrice, "existing position adopted by lifecycle tracker", now)
		}
		ad := a.advisePosition(ctx, p, now)
		m := store.PositionMark{TS: now, EntryPrice: p.BuyPrice, AskPrice: p.ListPrice, RecommendedExit: ad.Val.Exit, ExternalRef: ad.CrossReference, GramUSD: q.USD, Edge: ad.Val.Edge, ExpectedDays: ad.Val.ExpectedDays, Score: ad.Val.ScoreBreakdown.Total, Action: ad.Action}
		if stat, _ := a.st.ModelStat(ctx, p.Key); stat != nil {
			m.ModelFloor = stat.Floor
		}
		previous, _ := a.st.PositionMarks(ctx, p.GiftID, 1)
		if len(previous) > 0 && now.Sub(previous[0].TS) < 10*time.Minute && previous[0].Action == m.Action && previous[0].AskPrice == m.AskPrice && relativeChange(previous[0].RecommendedExit, m.RecommendedExit) < .005 && relativeChange(previous[0].ModelFloor, m.ModelFloor) < .01 && relativeChange(previous[0].GramUSD, m.GramUSD) < .01 {
			continue
		}
		if err := a.st.InsertPositionMark(ctx, p.GiftID, m); err != nil {
			log.Warn().Err(err).Int64("gift", p.GiftID).Msg("saving position mark failed")
			continue
		}
		if len(previous) > 0 && previous[0].Action != m.Action {
			_ = a.st.RecordPositionEvent(ctx, p.GiftID, "advice_changed", previous[0].RecommendedExit, m.RecommendedExit, previous[0].Action+" → "+m.Action, now)
		}
	}
}

func relativeChange(old, new float64) float64 {
	if old == 0 {
		if new == 0 {
			return 0
		}
		return 1
	}
	return math.Abs(new/old - 1)
}

// checkUndercut warns when our ask is no longer the cheapest.
//
// The comparison has to come from the live book, not the market snapshot. The
// snapshot is refreshed once a minute and its floor is a plain minimum over
// every listing — including the lot we have just bought out of the book and the
// one we have just relisted ourselves. Reading it produced the alert that fired
// seconds after a purchase, telling the operator they had been undercut at
// exactly the price they had paid.
func (a *App) checkUndercut(ctx context.Context, p store.Position, now time.Time) {
	if p.Status != store.StatusListed || p.ListPrice <= 0 {
		return
	}
	book, err := a.books.Get(ctx, p.Key)
	if err != nil || book == nil {
		return
	}
	// A book fetched before we listed cannot say anything about our listing.
	if !p.ListedAt.IsZero() && book.FetchedAt.Before(p.ListedAt) {
		return
	}
	// BestExcluding drops our own gift and everything else we are selling, so
	// what is left is a genuine competitor.
	best, ok := book.BestExcluding(p.GiftID, a.api.UserID())
	if !ok || best <= 0 || best >= p.ListPrice {
		return
	}
	if !a.throttle(fmt.Sprintf("undercut:%d:%.2f", p.GiftID, best), 6*time.Hour) {
		return
	}
	// Safe automatic repricing: never below cost+markup, never a large jump,
	// and never more than once per six hours. Loss-taking remains manual, and
	// the whole branch is off unless selling has been switched on.
	if a.rm.ResellEnabled() && !a.cfg.ShadowMode && a.rm.Armed() && p.BuyPrice > 0 {
		last, _ := a.st.LastReprice(ctx, p.GiftID)
		if last.IsZero() || now.Sub(last) >= 6*time.Hour {
			ad := a.advisePosition(ctx, p, now)
			if preview := ad.Val; preview.Valid && ad.Action == actRelist && !preview.MarketDisagreement && ad.CrossDivergence <= pricing.CrossDivergenceLimit {
				target := math.Floor(preview.Exit*100) / 100
				change := math.Abs(target/p.ListPrice - 1)
				if target >= p.BuyPrice*(1+a.rm.Limits().MinMarkup) && change >= .02 && change <= .15 {
					if actual, _, err := a.ex.ListAt(ctx, p.GiftID, p.Key, target, p.BuyPrice, now); err == nil && actual > 0 {
						_ = a.st.RecordReprice(ctx, p.GiftID, p.ListPrice, actual, "safe undercut response", now)
						_ = a.st.RecordPositionEvent(ctx, p.GiftID, "repriced", p.ListPrice, actual, "safe automatic undercut response", now)
						a.notify(fmt.Sprintf("♻️ <b>Автопереставил</b> — %s\n%s → %s", bot.Esc(p.Key.String()), num(p.ListPrice), num(actual)))
						return
					}
				}
			}
		}
	}
	a.notify(fmt.Sprintf(
		"🥊 <b>Тебя андеркатнули</b> — %s\nТвой аск %s · чужой аск %s (−%.1f%%)\nВход %s · <code>/relist %d</code>",
		bot.Esc(p.Key.String()), num(p.ListPrice), num(best),
		(p.ListPrice-best)/p.ListPrice*100, num(p.BuyPrice), p.GiftID))
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
		"🕸 <b>Позиция залежалась</b> — %s\nДержим %s, а модель торгуется %.1f×/день — должна была уйти за ~%s.\nВход %s · аск %s · <code>/relist %d</code>",
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
			"👁 <b>Вотчлист</b> — %s\nВыставили по %s\n<code>/val %d</code>",
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
	now := time.Now()
	for _, h := range []int{1, 6, 24, 72} {
		sigs, err := a.st.SignalsNeedingOutcome(ctx, h, now)
		if err != nil {
			return err
		}
		for _, s := range sigs {
			price, sold, err := a.st.SaleForGiftBetween(ctx, s.GiftID, s.TS, s.TS.Add(time.Duration(h)*time.Hour))
			if err != nil {
				continue
			}
			floor := 0.0
			if f, ok, _ := a.st.FloorAt(ctx, s.Key, s.TS.Add(time.Duration(h)*time.Hour)); ok {
				floor = f
			}
			profitable := sold && price > s.Price*(1+math.Max(a.cfg.TonnelFee, 0))
			_ = a.st.PutSignalOutcome(ctx, s.ID, h, now, sold, price, floor, profitable)
		}
	}
	keep := a.cfg.LookbackDays * 3
	if a.cfg.AttributeLookbackDays+15 > keep {
		keep = a.cfg.AttributeLookbackDays + 15
	}
	return a.st.Prune(ctx, now, keep)
}
