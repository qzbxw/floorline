package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"floorline/internal/bot"
	"floorline/internal/pricing"
	"floorline/internal/signal"
	"floorline/internal/tonnel"
)

// The scanner answers a question the feed poller cannot.
//
// The feed only ever sees *new* listings. A gift that has been sitting at the
// same price for a day and became interesting because the market moved around
// it is invisible to the desk — it was priced once, when nobody wanted it, and
// never looked at again. Most of the market is in that state at any moment.
//
// So this walks the standing book instead of the arrival stream: cheapest asks
// first, model by model, ranked against what the desk could actually pay for.

const (
	// scanModelsPerPass is how many models one pass looks at. The whole-market
	// snapshot covers several thousand, so a pass takes a slice and the next
	// one continues from where it stopped.
	scanModelsPerPass = 40
	// scanBookLimit is how deep into each model's book to look. The cheap end is
	// the only part that can be a misprice.
	scanBookLimit = 5
	// scanKeep is how many candidates a report holds. More than this is a wall
	// of text nobody reads.
	scanKeep = 8
)

// scanRefine is how many of the ranked candidates are re-priced against the
// queue for their exact backdrop and symbol before anyone acts on them. The
// sweep itself prices against the model-wide external queue — one read per
// model instead of one per listing — and that is the right trade for ranking,
// but it is the wrong number to buy on: for a gift whose value is its backdrop,
// the model-wide queue is a different asset.
const scanRefine = 4

// Candidate is one standing listing worth a look, with the numbers that got it
// onto the list.
type Candidate struct {
	Gift  tonnel.Gift
	Val   pricing.Valuation
	Score float64
	Fails []string
	// Dec is the decision the numbers above came from, so the money path does not
	// have to price the same listing a third time to act on it.
	Dec *signal.Decision
	// Refined records that this candidate has been re-priced against its own
	// attributes rather than against the model-wide external queue.
	Refined bool
}

// scanPass walks the next slice of the market and returns what it found,
// best first.
//
// Only models with enough trade history are considered: pricing a model that
// has never traded is guesswork, and the gates would reject it anyway. The
// budget filter comes first because a lot the desk cannot pay for is not a
// candidate at any score.
//
// The other venues are read once per model, not once per listing. That is the
// whole difference between a sweep that prices against the rest of the market
// and one that only claims to: the venues are paced like a person tapping
// through a mini app, and a pass asking for a fresh exact-attribute quote per
// ask needed several hundred rate-limited reads it could never finish. What came
// back instead was "the venues did not answer" on every candidate — which caps
// the score, blocks unattended buying, and poisoned the shared quote cache for
// the feed as well.
func (a *App) scanPass(ctx context.Context, keys []tonnel.ModelKey, now time.Time) []Candidate {
	room, hasRoom := a.spendable()
	limits := a.rm.Limits()

	var found []Candidate
	for _, key := range keys {
		select {
		case <-ctx.Done():
			return rankCandidates(found)
		default:
		}

		book, err := a.books.Get(ctx, key)
		if err != nil || book == nil || book.Len() == 0 {
			continue
		}
		cross := a.crossDepth(ctx, key, "", "")
		for i, ask := range book.Asks {
			if i >= scanBookLimit {
				break
			}
			// Our own inventory is not an opportunity.
			if owner := a.api.UserID(); owner != 0 && ask.Seller == owner {
				continue
			}
			if hasRoom && ask.Price*(1+a.cfg.TonnelFee) > room {
				break // the book only gets more expensive from here
			}
			g := tonnel.Gift{
				GiftID: tonnel.FlexInt(ask.GiftID), GiftNum: tonnel.FlexInt(ask.GiftNum),
				Name: key.Name, Model: key.Model, Price: tonnel.Flex64(ask.Price),
				Backdrop: ask.Backdrop, Symbol: ask.Symbol,
				Seller: tonnel.FlexInt(ask.Seller), Asset: tonnel.AssetGRAM, Status: "forsale",
			}
			dec, err := a.det.EvaluateWithCross(ctx, g, limits, now, cross)
			if err != nil || dec == nil || !dec.Val.Valid {
				continue
			}
			if dec.Val.Edge <= 0 {
				continue
			}
			found = append(found, Candidate{
				Gift: g, Val: dec.Val, Score: dec.Val.ScoreBreakdown.Total, Fails: dec.SignalFails, Dec: dec,
			})
		}
	}
	return rankCandidates(found)
}

// refine re-prices the head of the shortlist against the queue for its own
// backdrop and symbol.
//
// This is where the sweep's cheap approximation is paid back. Ranking on the
// model-wide external queue is fine — it is the same approximation for every
// candidate — but acting on it is not: an Onyx Black specimen bounded by the
// queue of ordinary ones is being compared to a different asset, in the
// direction that makes a bad trade look good.
func (a *App) refine(ctx context.Context, in []Candidate, now time.Time, limit int) []Candidate {
	limits := a.rm.Limits()
	for i := range in {
		if i >= limit {
			break
		}
		if in[i].Gift.Backdrop == "" && in[i].Gift.Symbol == "" {
			in[i].Refined = true // the model-wide queue *is* this gift's queue
			continue
		}
		dec, err := a.det.EvaluateFresh(ctx, in[i].Gift, limits, now)
		if err != nil || dec == nil || !dec.Val.Valid {
			continue
		}
		// Ranked the same way the feed ranks: a fourth lot of a model the desk is
		// already heavy in is not the same opportunity as the first one, and the
		// sweep was the one path where that never counted.
		if fit, _, err := a.rm.PortfolioFit(ctx, dec.Val.Key, dec.Val.Cost); err == nil {
			dec.Val.ScoreBreakdown = pricing.BuildScore(dec.Val, fit)
			dec.Score = dec.Val.ScoreBreakdown.Total
		}
		in[i] = Candidate{
			Gift: in[i].Gift, Val: dec.Val, Score: dec.Val.ScoreBreakdown.Total,
			Fails: dec.SignalFails, Dec: dec, Refined: true,
		}
	}
	// A refined price can move a candidate a long way down the list, so the order
	// is only meaningful after they have all been re-priced.
	sort.SliceStable(in, func(i, j int) bool { return in[i].Score > in[j].Score })
	return in
}

func rankCandidates(in []Candidate) []Candidate {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Score > in[j].Score })
	if len(in) > scanKeep {
		in = in[:scanKeep]
	}
	return in
}

// scanRankTTL is how long the ranked model list is reused.
//
// Building it reads the whole lookback window of trades for every model on the
// market — thousands of queries — to sort them by a fourteen-day velocity. That
// number does not move in twelve minutes, so rebuilding it every sweep was the
// most expensive thing the scanner did and it never changed the answer.
const scanRankTTL = 30 * time.Minute

// scanKeys picks the next slice of models to examine, newest snapshot first.
//
// Models are ordered by how much they actually trade, so a pass spends its
// request budget where a misprice is most likely to be sellable rather than
// walking the alphabet.
func (a *App) scanKeys(ctx context.Context, limit int) []tonnel.ModelKey {
	ranked := a.rankedModels(ctx)
	if len(ranked) == 0 {
		return nil
	}

	a.mu.Lock()
	start := a.scanCursor
	a.mu.Unlock()
	if start >= len(ranked) {
		start = 0
	}
	end := start + limit
	if end > len(ranked) {
		end = len(ranked)
	}
	a.mu.Lock()
	a.scanCursor = end
	if a.scanCursor >= len(ranked) {
		a.scanCursor = 0
	}
	a.mu.Unlock()

	out := make([]tonnel.ModelKey, 0, end-start)
	out = append(out, ranked[start:end]...)
	return out
}

// rankedModels orders every model the desk could trade by how fast it actually
// trades, cached for scanRankTTL.
func (a *App) rankedModels(ctx context.Context) []tonnel.ModelKey {
	a.mu.RLock()
	cached, at := a.scanRanked, a.scanRankedAt
	a.mu.RUnlock()
	if len(cached) > 0 && time.Since(at) < scanRankTTL {
		return cached
	}

	stats, err := a.st.ModelStats(ctx)
	if err != nil || len(stats) == 0 {
		return cached // a failed rebuild keeps the previous order rather than stopping the sweep
	}
	type scored struct {
		key   tonnel.ModelKey
		trade float64
	}
	now := time.Now()
	window := a.window()
	coverage := a.Coverage()
	ranked := make([]scored, 0, len(stats))
	for _, s := range stats {
		if s.Floor <= 0 || s.Supply <= 0 {
			continue
		}
		sales, err := a.st.SalesSince(ctx, s.Key, now.Add(-window))
		if err != nil || len(sales) < a.cfg.Sig.MinSales {
			continue
		}
		liq := pricing.ComputeLiquidity(sales, now, window, coverage)
		if liq.Velocity < a.cfg.Sig.MinVelocity {
			continue
		}
		ranked = append(ranked, scored{key: s.Key, trade: liq.Velocity})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].trade > ranked[j].trade })

	out := make([]tonnel.ModelKey, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.key)
	}
	a.mu.Lock()
	a.scanRanked, a.scanRankedAt = out, now
	// The cursor walks the ranked list, and a rebuilt list is a different list.
	// Left alone, a shorter one leaves the cursor past the end — which silently
	// restarts the sweep at the busiest models and never reaches the rest.
	if a.scanCursor > len(out) {
		a.scanCursor = 0
	}
	a.mu.Unlock()
	return out
}

// pollScan is the periodic sweep. It feeds anything that clears every gate into
// the same decision path the feed uses, so unattended buying works on standing
// listings and not only on new arrivals.
func (a *App) pollScan(ctx context.Context) error {
	keys := a.scanKeys(ctx, scanModelsPerPass)
	if len(keys) == 0 {
		return nil
	}
	now := time.Now()
	found := a.refine(ctx, a.scanPass(ctx, keys, now), now, scanRefine)

	a.mu.Lock()
	a.lastScan, a.lastScanFound = now, len(found)
	a.mu.Unlock()

	for i := range found {
		c := found[i]
		// Not a signal: the report still shows it, the desk does not act. And an
		// unrefined candidate is a ranking, not a price — it was never compared
		// against the queue for its own attributes, so it does not get to move
		// money on the strength of a model-wide one.
		if len(c.Fails) > 0 || !c.Refined || c.Dec == nil || !c.Dec.Signal {
			a.observe(ctx, c.Dec, now)
			continue
		}
		// The sweep exists to find listings that have been standing there for a
		// day, which means it finds the same one every twelve minutes until
		// somebody takes it. The feed path is deduplicated against the signal
		// journal for exactly this reason; the sweep has to be too, or its best
		// find becomes the reason the operator mutes the bot.
		already, err := a.st.AlreadySignalled(ctx, c.Gift.GiftID.Int(), signal.KindBuy, c.Val.Price)
		if err != nil {
			log.Warn().Err(err).Msg("scan dedupe check failed")
			continue
		}
		if already {
			continue
		}
		if err := a.handleDecision(ctx, c.Dec, now); err != nil {
			log.Warn().Err(err).Msg("scan decision failed")
		}
	}
	return nil
}

// scanText renders the current shortlist for /scan.
func (a *App) scanText(ctx context.Context, collection string) string {
	room, hasRoom := a.spendable()

	var keys []tonnel.ModelKey
	if collection != "" {
		name := a.resolveCollection(ctx, collection)
		stats, err := a.st.ModelStats(ctx)
		if err != nil {
			return "Не прочитал снимок рынка: " + bot.Esc(err.Error())
		}
		for _, s := range stats {
			if s.Key.Name == name {
				keys = append(keys, s.Key)
			}
		}
		if len(keys) == 0 {
			return "Не нашёл коллекцию " + bot.Esc(collection)
		}
	} else {
		keys = a.scanKeys(ctx, scanModelsPerPass)
	}
	if len(keys) == 0 {
		return "Пока нечего сканировать — рынок ещё не прогрузился."
	}

	now := time.Now()
	found := a.refine(ctx, a.scanPass(ctx, keys, now), now, scanRefine)

	var b strings.Builder
	fmt.Fprintf(&b, "🔭 <b>Скан</b> · %s", plural(len(keys), "модель", "модели", "моделей"))
	if hasRoom {
		fmt.Fprintf(&b, " · в пределах %s", num(room))
	}
	b.WriteString("\n\n")

	if len(found) == 0 {
		b.WriteString("Пусто. Это норма — рынок чаще честный, чем нет.")
		return b.String()
	}

	for _, c := range found {
		// A candidate that fails a gate still earns its place on the list: the
		// operator can take a trade the unattended path may not. But it must not
		// look like one that passed.
		verdict := "✅"
		if len(c.Fails) > 0 {
			verdict = "⚪️"
		}
		fmt.Fprintf(&b, "%s <b>%s</b>\n", verdict, bot.Esc(c.Val.Key.String()))
		fmt.Fprintf(&b, "   %s → %s · <b>%s</b> · %s · скор %.0f\n",
			num(c.Val.Cost), num(c.Val.FastExit), pct(c.Val.Edge), days(c.Val.FastExpectedDays), c.Score)
		// Where the exit came from, in one line. The whole point of the sweep is
		// that it prices against every venue, and that is invisible unless it says
		// so — "priced off Portals at 4.20, with two of their offers already under
		// our entry" is a different trade from the same numbers off Tonnel alone.
		if note := scanMarketNote(c); note != "" {
			fmt.Fprintf(&b, "   %s\n", note)
		}
		if len(c.Fails) > 0 {
			fmt.Fprintf(&b, "   <i>%s</i>\n", bot.Esc(c.Fails[0]))
		}
		fmt.Fprintf(&b, "   <code>/val %d</code>\n\n", c.Gift.GiftID.Int())
	}
	return b.String()
}

// scanMarketNote says what the rest of the market had to do with this price.
func scanMarketNote(c Candidate) string {
	v := c.Val
	var parts []string
	switch {
	case v.Cross.Unreachable > 0:
		parts = append(parts, fmt.Sprintf("⚠️ %s не ответил%s",
			plural(v.Cross.Unreachable, "площадка", "площадки", "площадок"),
			map[bool]string{true: "а", false: "и"}[v.Cross.Unreachable == 1]))
	case v.ExitFromCross:
		parts = append(parts, "выход по чужой площадке "+num(v.Walkaway))
	case v.CrossMarketSupport > 0:
		parts = append(parts, "площадки "+num(v.CrossMarketSupport))
	}
	if v.CrossCompetitorsNear > 0 {
		parts = append(parts, fmt.Sprintf("рядом %s на площадках",
			plural(v.CrossCompetitorsNear, "оффер", "оффера", "офферов")))
	}
	if !c.Refined && (v.Backdrop != "" || v.Symbol != "") {
		// Said plainly, because it changes what the number means: the exit was
		// bounded by the queue of ordinary specimens of this model.
		parts = append(parts, "оценка по модели, не по трейтам")
	}
	if len(parts) == 0 {
		return ""
	}
	return "<i>" + bot.Esc(strings.Join(parts, " · ")) + "</i>"
}

// scanLine reports the sweep's health for /status.
func (a *App) scanLine() string {
	a.mu.RLock()
	last, n := a.lastScan, a.lastScanFound
	a.mu.RUnlock()
	if last.IsZero() {
		return "Скан рынка ещё не проходил"
	}
	return fmt.Sprintf("Скан рынка %s назад · нашёл %d", dur(time.Since(last)), n)
}
