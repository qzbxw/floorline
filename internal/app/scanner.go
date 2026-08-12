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

// Candidate is one standing listing worth a look, with the numbers that got it
// onto the list.
type Candidate struct {
	Gift  tonnel.Gift
	Val   pricing.Valuation
	Score float64
	Fails []string
}

// scanPass walks the next slice of the market and returns what it found,
// best first.
//
// Only models with enough trade history are considered: pricing a model that
// has never traded is guesswork, and the gates would reject it anyway. The
// budget filter comes first because a lot the desk cannot pay for is not a
// candidate at any score.
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
			dec, err := a.det.EvaluateFresh(ctx, g, limits, now)
			if err != nil || dec == nil || !dec.Val.Valid {
				continue
			}
			if dec.Val.Edge <= 0 {
				continue
			}
			found = append(found, Candidate{
				Gift: g, Val: dec.Val, Score: dec.Val.ScoreBreakdown.Total, Fails: dec.SignalFails,
			})
		}
	}
	return rankCandidates(found)
}

func rankCandidates(in []Candidate) []Candidate {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Score > in[j].Score })
	if len(in) > scanKeep {
		in = in[:scanKeep]
	}
	return in
}

// scanKeys picks the next slice of models to examine, newest snapshot first.
//
// Models are ordered by how much they actually trade, so a pass spends its
// request budget where a misprice is most likely to be sellable rather than
// walking the alphabet.
func (a *App) scanKeys(ctx context.Context, limit int) []tonnel.ModelKey {
	stats, err := a.st.ModelStats(ctx)
	if err != nil || len(stats) == 0 {
		return nil
	}
	type scored struct {
		key   tonnel.ModelKey
		trade float64
	}
	window := a.window()
	ranked := make([]scored, 0, len(stats))
	for _, s := range stats {
		if s.Floor <= 0 || s.Supply <= 0 {
			continue
		}
		sales, err := a.st.SalesSince(ctx, s.Key, time.Now().Add(-window))
		if err != nil || len(sales) < a.cfg.Sig.MinSales {
			continue
		}
		liq := pricing.ComputeLiquidity(sales, time.Now(), window, a.Coverage())
		if liq.Velocity < a.cfg.Sig.MinVelocity {
			continue
		}
		ranked = append(ranked, scored{key: s.Key, trade: liq.Velocity})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].trade > ranked[j].trade })

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
	for _, r := range ranked[start:end] {
		out = append(out, r.key)
	}
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
	found := a.scanPass(ctx, keys, now)

	a.mu.Lock()
	a.lastScan, a.lastScanFound = now, len(found)
	a.mu.Unlock()

	for i := range found {
		c := found[i]
		if len(c.Fails) > 0 {
			continue // not a signal; the report still shows it, the desk does not act
		}
		dec := &signal.Decision{Gift: c.Gift, Val: c.Val, Signal: true, Score: c.Score}
		dec.AutoFails = nil
		fresh, err := a.det.EvaluateFresh(ctx, c.Gift, a.rm.Limits(), now)
		if err != nil || fresh == nil {
			continue
		}
		if err := a.handleDecision(ctx, fresh, now); err != nil {
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

	found := a.scanPass(ctx, keys, time.Now())

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
		if len(c.Fails) > 0 {
			fmt.Fprintf(&b, "   <i>%s</i>\n", bot.Esc(c.Fails[0]))
		}
		fmt.Fprintf(&b, "   <code>/val %d</code>\n\n", c.Gift.GiftID.Int())
	}
	return b.String()
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
