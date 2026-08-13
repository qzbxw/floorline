// Package signal decides which listings are worth a human's attention, and
// which of those are safe enough to buy unattended.
package signal

import (
	"context"
	"fmt"
	"math"
	"time"

	"floorline/internal/config"
	"floorline/internal/pricing"
	"floorline/internal/risk"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

// KindBuy is the signal kind for an actionable purchase opportunity.
const KindBuy = "buy"

// alertCooldown stops one hot model from filling the chat. It suppresses the
// notification only — a lot that clears the auto-buy bar is still bought.
const alertCooldown = 5 * time.Minute

// collectionCooldown is the same idea one level up.
//
// On 12 Aug seven Lol Pop cards arrived between 22:21 and 22:31. Every one was
// a different model, so no two shared a cooldown key, and every one was priced
// at 3.3 — one seller emptying a collection, delivered as seven separate
// opportunities.
const collectionCooldown = 15 * time.Minute

const (
	// batchWindow and batchTolerance define "the same dump": other gifts of
	// this collection signalled at essentially this price, recently.
	batchWindow    = time.Hour
	batchTolerance = 0.01
	// maxBatchPeers is how many of them it takes before this stops being a
	// misprice. Scarcity is the premise of the whole trade: if the desk has
	// already been shown three lots at this number, then whatever we buy, our
	// buyer can have the next one at the same price.
	maxBatchPeers = 3
	// crowdWindow is how far back the adverse-selection check looks for other
	// sellers arriving. Long enough to catch a queue forming, short enough that
	// ordinary turnover in a busy model does not read as one.
	crowdWindow = 15 * time.Minute
)

// Decision is the full verdict on one listing.
type Decision struct {
	Gift tonnel.Gift
	Val  pricing.Valuation

	Signal     bool // worth telling the operator about
	Auto       bool // clears every unattended-purchase gate
	Suppressed bool // signal-worthy but muted or inside the alert cooldown
	Score      float64

	// Verdict is the headline, and it is the weakest of the three layers rather
	// than the verdict of the price gates alone. Layers carries all three so the
	// card can say which one is holding it down.
	Verdict Verdict
	Layers  []Layer

	SignalFails []string
	AutoFails   []string

	// BatchPeers is how many other gifts of the same collection were signalled
	// at essentially this price in the last hour — the measure of how far this
	// lot is from being scarce.
	BatchPeers int

	Age time.Duration // how long the listing has been visible to us
}

// Detector evaluates listings against stored market state.
type Detector struct {
	st    *store.Store
	books *pricing.BookCache
	cfg   *config.Config

	// Coverage reports how much trade history is actually stored, so velocity
	// is not understated while the database is still filling up.
	Coverage func() time.Duration
	// Warm reports whether the history is deep enough to trust for auto-buying.
	Warm func() bool
	// CalibrationReady and ShadowMode are supplied rather than read from config
	// because both are switches the operator flips at runtime. Reading them
	// from the immutable config meant the only way to change either was to edit
	// .env and restart, while the bot reported the block in small print at the
	// bottom of every card.
	CalibrationReady func() bool
	ShadowMode       func() bool
	// CrossSupport supplies robust external ask depth before any gate runs.
	// Leaving it nil simply disables that component.
	CrossSupport func(context.Context, pricing.Valuation) pricing.CrossMarket
	// OwnerID is resolved dynamically because /auth can replace the session.
	OwnerID func() int64
	// Spendable reports the largest single purchase the desk could actually
	// make right now — the ticket limit against the free balance. Signals for
	// lots far beyond it are noise: nobody can act on them. A false second
	// return disables the check.
	Spendable func() (float64, bool)
}

// New builds a Detector.
func New(st *store.Store, books *pricing.BookCache, cfg *config.Config) *Detector {
	return &Detector{
		st:               st,
		books:            books,
		cfg:              cfg,
		Coverage:         func() time.Duration { return time.Duration(cfg.LookbackDays) * 24 * time.Hour },
		Warm:             func() bool { return true },
		CalibrationReady: func() bool { return true },
		ShadowMode:       func() bool { return cfg.ShadowMode },
	}
}

func (d *Detector) window() time.Duration {
	return time.Duration(d.cfg.LookbackDays) * 24 * time.Hour
}

func (d *Detector) params() pricing.Params {
	return pricing.Params{
		Fee:      d.cfg.TonnelFee,
		Undercut: d.cfg.Undercut,
	}
}

// Evaluate prices one listing and applies every gate.
//
// It returns nil for rows that can never be signals, so the caller does not
// have to distinguish "rejected" from "not applicable". Network work — the
// order-book lookup — happens only after the listing survives the checks that
// can be answered from local data, because the feed produces candidates far
// faster than the rate limiter permits requests.
func (d *Detector) Evaluate(ctx context.Context, g tonnel.Gift, limits risk.Limits, now time.Time) (*Decision, error) {
	return d.evaluate(ctx, g, limits, now, true, nil)
}

// EvaluateFresh repeats every gate without signal deduplication. It is used in
// the money path after a direct GiftData quote and a forced book refresh.
func (d *Detector) EvaluateFresh(ctx context.Context, g tonnel.Gift, limits risk.Limits, now time.Time) (*Decision, error) {
	return d.evaluate(ctx, g, limits, now, false, nil)
}

// EvaluateWithCross prices a listing against a cross-market quote the caller
// already holds, instead of fetching one per listing.
//
// The sweep of the standing book needs this. The other venues are paced like a
// person tapping through a mini app — MRKT banned this account once for looking
// like anything else — and one pass over forty models times five asks was asking
// for several hundred rate-limited reads down the exact → backdrop → model
// ladder. None of it could finish: every candidate came back with the venues
// "unreachable", which is not a cosmetic loss but the cap that holds an
// optimistic exit down, a heavy score penalty and a hard block on unattended
// buying. One read per model, shared by that model's whole book, is what makes
// the sweep able to price against the other venues at all.
func (d *Detector) EvaluateWithCross(ctx context.Context, g tonnel.Gift, limits risk.Limits, now time.Time, cm pricing.CrossMarket) (*Decision, error) {
	return d.evaluate(ctx, g, limits, now, false, &cm)
}

func (d *Detector) evaluate(ctx context.Context, g tonnel.Gift, limits risk.Limits, now time.Time, dedupe bool, cross *pricing.CrossMarket) (*Decision, error) {
	if d.OwnerID != nil && d.OwnerID() != 0 && g.Seller.Int() == d.OwnerID() {
		return nil, nil
	}
	if !d.tradable(g) {
		return nil, nil
	}
	key := g.Key()
	if key.Name == "" || key.Model == "" {
		return nil, nil
	}
	price := g.Price.Float()

	if dedupe {
		already, err := d.st.AlreadySignalled(ctx, g.GiftID.Int(), KindBuy, price)
		if err != nil {
			return nil, fmt.Errorf("dedupe check: %w", err)
		}
		if already {
			return nil, nil
		}
	}

	attrDays := d.cfg.AttributeLookbackDays
	if attrDays < d.cfg.LookbackDays {
		attrDays = d.cfg.LookbackDays
	}
	since := now.Add(-time.Duration(attrDays) * 24 * time.Hour)
	sales, err := d.st.SalesSince(ctx, key, since)
	if err != nil {
		return nil, fmt.Errorf("load sales: %w", err)
	}
	rawLiq := pricing.ComputeLiquidity(sales, now, d.window(), d.Coverage())
	fxSales, fxCoverage := sales, 0.0
	if cur, ok, _ := d.st.LatestGramQuote(ctx); ok && cur.USD > 0 {
		if qs, e := d.st.GramQuotesSince(ctx, since.Add(-2*time.Hour)); e == nil {
			rates := make([]pricing.RatePoint, 0, len(qs))
			for _, q := range qs {
				rates = append(rates, pricing.RatePoint{TS: q.TS, USD: q.USD})
			}
			fxSales, fxCoverage = pricing.NormalizeSalesForRate(sales, rates, cur.USD)
		}
	}
	liq := pricing.ComputeLiquidity(fxSales, now, d.window(), d.Coverage())
	liq.RawMedian, liq.FXCoverage = rawLiq.Median, fxCoverage

	// Without enough history the signal gate is guaranteed to fail. A low
	// median is not a valid early reject: live and cross-market depth may prove
	// that the old prints are stale.
	if liq.Median <= 0 && liq.Sales < d.cfg.Sig.MinSales {
		return nil, nil // no trade history and no chance of clearing the gate
	}

	stat, err := d.st.ModelStat(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("load model stat: %w", err)
	}
	var floor, rarity float64
	var supply int
	var snapshotAt time.Time
	var fxContext pricing.FXContext
	if stat != nil {
		floor, supply, rarity = stat.Floor, stat.Supply, stat.Rarity
		snapshotAt = stat.TS
		if cur, ok, _ := d.st.LatestGramQuote(ctx); ok {
			q15, _, _ := d.st.GramQuoteAt(ctx, now.Add(-15*time.Minute))
			q1, _, _ := d.st.GramQuoteAt(ctx, now.Add(-time.Hour))
			floor1, _, _ := d.st.FloorAt(ctx, key, now.Add(-time.Hour))
			fxContext = pricing.ComputeFXContext(now, floor, pricing.RatePoint{TS: cur.TS, USD: cur.USD}, pricing.RatePoint{TS: q15.TS, USD: q15.USD}, pricing.RatePoint{TS: q1.TS, USD: q1.USD}, floor1)
		}
	}

	book, err := d.books.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("load book for %s: %w", key, err)
	}

	val := pricing.Evaluate(pricing.Input{
		GiftID:     g.GiftID.Int(),
		GiftNum:    g.GiftNum.Int(),
		OwnerID:    ownerID(d.OwnerID),
		Key:        key,
		Price:      price,
		Book:       book,
		Liq:        liq,
		Floor:      floor,
		Supply:     supply,
		Rarity:     rarity,
		Backdrop:   tonnel.BaseAttr(g.Backdrop),
		Symbol:     tonnel.BaseAttr(g.Symbol),
		Attribute:  pricing.ComputeAttributeValue(fxSales, tonnel.BaseAttr(g.Backdrop), tonnel.BaseAttr(g.Symbol), liq.Median),
		SnapshotAt: snapshotAt,
		Now:        now,
		FX:         fxContext,
		Params:     d.params(),
		TicketRef:  limits.MaxTicket,
	})
	// Applied unconditionally: a venue that could not be read is an input, not an
	// absence of one. Guarding this on Support > 0 meant an unreachable venue
	// never reached the valuation, so the score treated "priced blind" as "priced
	// on Tonnel alone" and the auto-buy gate that exists to catch it could not
	// fire.
	switch {
	case cross != nil:
		val = pricing.WithCrossDepth(val, *cross)
	case d.CrossSupport != nil:
		val = pricing.WithCrossDepth(val, d.CrossSupport(ctx, val))
	}

	// Is anyone else running for the same door? Cheap to ask — it is one indexed
	// count over the listings we already store — and it is the difference between
	// a discount and the first step of a slide.
	if fresh, err := d.st.FreshSupplySince(ctx, key, price, now.Add(-crowdWindow),
		g.GiftID.Int(), ownerID(d.OwnerID)); err == nil {
		val.AssessCrowd(pricing.Crowd{
			Window: crowdWindow, Arrivals: fresh.Arrivals,
			AtOrBelow: fresh.AtOrBelow, Cheapest: fresh.Cheapest,
		})
	}

	dec := &Decision{Gift: g, Val: val}
	if firstSeen, err := d.st.ListingFirstSeen(ctx, g.GiftID.Int()); err == nil && !firstSeen.IsZero() {
		dec.Age = now.Sub(firstSeen)
	}

	if !val.Valid {
		dec.SignalFails = []string{val.Reason}
		return dec, nil
	}

	// How much of this collection is already on offer at this price decides
	// whether the lot is scarce, so it is measured before the gates run.
	peers, err := d.st.PeersAtPrice(ctx, key.Name, KindBuy, price, batchTolerance, now.Add(-batchWindow), g.GiftID.Int())
	if err != nil {
		return nil, fmt.Errorf("batch check: %w", err)
	}
	dec.BatchPeers = peers

	dec.SignalFails = d.signalGates(val)
	if peers >= maxBatchPeers {
		dec.SignalFails = append(dec.SignalFails, fmt.Sprintf(
			"%s этой коллекции уже прошли по той же цене за час — это распродажа одного продавца, а не мисприс", plural(peers, "лот", "лота", "лотов")))
	}
	dec.Signal = len(dec.SignalFails) == 0
	dec.Score = Score(val)
	dec.Verdict, dec.Layers = Grade(val, dec.SignalFails)

	if dec.Signal {
		muted, err := d.st.IsMuted(ctx, key, now)
		if err != nil {
			return nil, fmt.Errorf("mute check: %w", err)
		}
		last, err := d.st.LastSignalForModel(ctx, key, KindBuy)
		if err != nil {
			return nil, fmt.Errorf("cooldown check: %w", err)
		}
		lastCollection, err := d.st.LastSignalForCollection(ctx, key.Name, KindBuy)
		if err != nil {
			return nil, fmt.Errorf("collection cooldown check: %w", err)
		}
		dec.Suppressed = muted ||
			(!last.IsZero() && now.Sub(last) < alertCooldown) ||
			(!lastCollection.IsZero() && now.Sub(lastCollection) < collectionCooldown)

		dec.AutoFails = d.autoGates(val, limits)
		dec.Auto = len(dec.AutoFails) == 0
	}

	return dec, nil
}

// plural renders a Russian count with the right noun form. The card layer has
// its own copy; a gate message that reads "1 лотов" undermines every other
// number on the card.
func plural(n int, one, few, many string) string {
	word := many
	if mod100 := n % 100; mod100 < 11 || mod100 > 14 {
		switch n % 10 {
		case 1:
			word = one
		case 2, 3, 4:
			word = few
		}
	}
	return fmt.Sprintf("%d %s", n, word)
}

func ownerID(fn func() int64) int64 {
	if fn == nil {
		return 0
	}
	return fn()
}

// GatesFor applies the gates to a valuation produced elsewhere. /val uses it to
// explain a listing the detector itself would skip, such as one already alerted.
func (d *Detector) GatesFor(v pricing.Valuation, limits risk.Limits) (signalFails, autoFails []string) {
	if !v.Valid {
		return []string{v.Reason}, nil
	}
	signalFails = d.signalGates(v)
	if len(signalFails) == 0 {
		autoFails = d.autoGates(v, limits)
	}
	return signalFails, autoFails
}

// tradable rejects rows whose price does not mean what it appears to mean.
func (d *Detector) tradable(g tonnel.Gift) bool {
	if g.IsBundle() {
		return false // price covers a whole pack
	}
	if g.Premarket.Bool() {
		return false // not a deliverable gift yet
	}
	if g.TelegramMarketplace.Bool() {
		return false // different settlement path
	}
	if g.Refunded.Bool() || g.Buyer != nil {
		return false
	}
	if g.Asset != "" && g.Asset != tonnel.AssetGRAM {
		return false // USDT listings are a separate book with a separate floor
	}
	p := g.Price.Float()
	if p <= 0 || p < d.cfg.Sig.MinPrice {
		return false
	}
	if d.cfg.Sig.MaxPrice > 0 && p > d.cfg.Sig.MaxPrice {
		return false
	}
	return true
}

// signalGates decides whether a valuation is worth pushing to Telegram.
func (d *Detector) signalGates(v pricing.Valuation) []string {
	g := d.cfg.Sig
	var fails []string

	// A lot the desk cannot pay for is not an opportunity, it is noise at 2am.
	if d.Spendable != nil {
		if room, ok := d.Spendable(); ok && v.Cost > room {
			fails = append(fails, fmt.Sprintf("лот стоит %.2f, а поднять сейчас можно максимум %.2f (тикет и свободный баланс)", v.Cost, room))
		}
	}
	// The arithmetic of the trade, and the reason behind it, are two different
	// statements and the operator needs both. Reporting only the second used to
	// hide the first: "the market is cheaper than you" does not tell anyone how
	// far under water the round trip actually is.
	if v.Edge <= 0 {
		fails = append(fails, fmt.Sprintf("реальный вход %.3f выше быстрого выхода %.3f — сделка уже %.1f%% в минусе до риск-буфера", v.Cost, v.FastExit, -v.Edge*100))
	} else if need := requiredEdge(g.MinEdge, v.Liq, v.Regime); v.ScoreBreakdown.RiskAdjustedEdge < need {
		fails = append(fails, fmt.Sprintf("эдж с поправкой на риск %.1f%% ниже %.1f%% (сырой %.1f%%, %s%s)",
			v.ScoreBreakdown.RiskAdjustedEdge*100, need*100, v.Edge*100, liquidityNote(v.Liq), regimeNote(v.Regime)))
	}
	// A percentage cannot tell a trade from a rounding error. Both bars have to
	// be cleared: 5% of a 3-GRAM lot is seven hundredths of a GRAM, and a card
	// for it costs more attention than the money is worth.
	if g.MinNet > 0 && v.Net < g.MinNet {
		fails = append(fails, fmt.Sprintf("чистыми всего %.2f GRAM, минимум %.2f — процент есть, денег нет", v.Net, g.MinNet))
	}
	// The horizon a flip is allowed to take, measured as a probability rather
	// than as a mean. "Expected 4.7 days" was a fourteen-day average trade rate
	// divided by a queue that included every listing of the model, and it was
	// wrong in both halves; what matters is whether this lot, at this price,
	// behind the offers actually cheaper than it, is likely to be gone in time.
	if limit := v.Regime.HoldLimit(g.MaxExitDays); limit > 0 {
		if median := v.Fill.Median(); median > limit {
			fails = append(fails, fmt.Sprintf(
				"шанс продать за %.0fд всего %.0f%% (впереди %s) — это склад, а не флип%s",
				limit, fillWithin(v.Fill, limit)*100,
				plural(v.Fill.QueueAhead, "оффер", "оффера", "офферов"), regimeNote(v.Regime)))
		}
	}
	// No trait premium, cheaper offers already standing in front of us: this is
	// an expensive listing, not a mispriced one.
	if v.PricedAboveMarket {
		fails = append(fails, fmt.Sprintf(
			"дешевле твоего входа %.3f уже стоит %d чужих асков на Tonnel и других площадках, а премии за трейты нет — это не мисприс",
			v.Cost, v.AsksBelowEntry))
	}
	if v.Liq.Velocity < g.MinVelocity {
		fails = append(fails, fmt.Sprintf("скорость %.2f/день ниже %.2f", v.Liq.Velocity, g.MinVelocity))
	}
	if v.Liq.Sales < g.MinSales {
		fails = append(fails, fmt.Sprintf("всего %d сделок за %dд, нужно %d", v.Liq.Sales, d.cfg.LookbackDays, g.MinSales))
	}
	if v.Liq.MADRatio > g.MaxMADRatio {
		fails = append(fails, fmt.Sprintf("разброс цен %.0f%% выше %.0f%%", v.Liq.MADRatio*100, g.MaxMADRatio*100))
	}
	if v.Liq.Trend < g.MinTrend {
		fails = append(fails, fmt.Sprintf("тренд %.2f ниже %.2f (падает)", v.Liq.Trend, g.MinTrend))
	}
	return fails
}

// Liquidity bands for the edge bar. Velocity here is deduplicated sales per
// day — one physical gift flipped repeatedly does not move it — so it is a
// direct measure of how fast a mistake can be unwound.
const (
	liquidPerDay = 2.0
	thinPerDay   = 1.0
	// liquidRelief and thinPenalty move the required edge around the configured
	// base. The point is not to trade more, it is to stop treating a model that
	// sells twice a day and one that sells twice a week as the same risk.
	liquidRelief = 0.005
	thinPenalty  = 0.015
)

// requiredEdge scales the edge bar by how easily the position could be unwound,
// and by which way the model's market is going.
//
// A flat threshold prices two very different trades identically. Three percent
// on a model doing 2.5 distinct sales a day is a position you can be out of by
// tomorrow; the same three percent on one selling twice a week is a fortnight of
// capital tied up for the price of a rounding error, and that is the trade the
// desk keeps getting stuck in. So the thin end pays for its illiquidity and the
// liquid end gets a little back.
//
// The regime is the second axis. Buying into a model whose recent prints are
// below its own window means the queue we plan to undercut will itself be
// cheaper by the time we reach it, so the margin has to cover the drift as well
// as the spread.
func requiredEdge(base float64, l pricing.Liquidity, r pricing.Regime) float64 {
	switch {
	case l.Velocity >= liquidPerDay:
		base = math.Max(base-liquidRelief, 0)
	case l.Velocity >= thinPerDay:
	default:
		base += thinPenalty
	}
	return base + r.EdgeSurcharge()
}

// regimeNote names a falling market wherever a threshold moved because of it, so
// a bar that changed under the operator does not look arbitrary.
func regimeNote(r pricing.Regime) string {
	if r != pricing.RegimeFalling {
		return ""
	}
	return ", рынок модели падает — планка поднята"
}

// fillWithin reads the survival curve at whichever quoted horizon is closest to
// a limit, so a message can quote a probability the card also shows.
func fillWithin(f pricing.FillCurve, limitDays float64) float64 {
	switch {
	case limitDays <= 1:
		return f.In24h
	case limitDays <= 3:
		return f.In72h
	default:
		return f.In7d
	}
}

// liquidityNote explains which band the model landed in, so the threshold in
// the message does not look arbitrary.
func liquidityNote(l pricing.Liquidity) string {
	switch {
	case l.Velocity >= liquidPerDay:
		return fmt.Sprintf("модель ликвидная, %.1f/день — планка снижена", l.Velocity)
	case l.Velocity >= thinPerDay:
		return fmt.Sprintf("%.1f продажи в день", l.Velocity)
	default:
		return fmt.Sprintf("всего %.1f продажи в день — планка поднята, из такой модели тяжело выйти", l.Velocity)
	}
}

// autoGates is layered strictly on top of signalGates. Everything here answers
// one question: if we buy this, can we actually get out of it quickly?
func (d *Detector) autoGates(v pricing.Valuation, limits risk.Limits) []string {
	a := d.cfg.Auto
	var fails []string

	if d.Warm != nil && !d.Warm() {
		fails = append(fails, "история сделок ещё прогревается")
	}
	if d.ShadowMode != nil && d.ShadowMode() {
		fails = append(fails, "включён shadow-режим — только записываю, что купил бы (/autobuy)")
	} else if d.CalibrationReady != nil && !d.CalibrationReady() {
		fails = append(fails, "калибровка скоринга не набрала выборку — можно снять в /autobuy")
	}
	if need := requiredEdge(a.MinEdge, v.Liq, v.Regime); v.ScoreBreakdown.RiskAdjustedEdge < need {
		fails = append(fails, fmt.Sprintf("эдж с поправкой на риск %.1f%% ниже порога автобая %.1f%% (%s%s)",
			v.ScoreBreakdown.RiskAdjustedEdge*100, need*100, liquidityNote(v.Liq), regimeNote(v.Regime)))
	}
	// Everything above is about the price. The verdict is about all three layers,
	// and the machine is only allowed to act on the strongest one — a trade the
	// desk itself would label speculative is not a trade to make while nobody is
	// watching.
	if verdict, layers := Grade(v, nil); verdict != VerdictBuy {
		for _, l := range Blocking(layers) {
			fails = append(fails, fmt.Sprintf("%s: %s", l.Name, l.Note))
		}
	}
	// A discount is only a gift when nobody else is running for the same door.
	if v.AdverseSelection {
		fails = append(fails, v.AdverseReason)
	}
	if v.Liq.Velocity < a.MinVelocity {
		fails = append(fails, fmt.Sprintf("скорость %.2f/день ниже порога автобая %.2f", v.Liq.Velocity, a.MinVelocity))
	}
	if v.Liq.Sales < a.MinSales {
		fails = append(fails, fmt.Sprintf("%d сделок за %dд, автобаю нужно %d", v.Liq.Sales, d.cfg.LookbackDays, a.MinSales))
	}
	if v.Liq.Turnover < a.MinTurnover {
		fails = append(fails, fmt.Sprintf(
			"в истории всего %d разных гифтов на %d сделок (оборот %.2f, автобаю надо %.2f) — похоже на гон одного NFT",
			v.Liq.DistinctGifts, v.Liq.Prints, v.Liq.Turnover, a.MinTurnover))
	}
	if v.MarketDisagreement {
		fails = append(fails, fmt.Sprintf("рынок спорит сам с собой: история и живой стакан разъехались на %.0f%% — только руками", v.MarketDivergence*100))
	}
	if v.CrossDivergence > pricing.CrossDivergenceLimit {
		fails = append(fails, fmt.Sprintf("Tonnel и другие площадки разъехались на %.0f%% — только руками", v.CrossDivergence*100))
	}
	// A specimen priced off the ordinary examples of its model is a price we do
	// not have. The exit is deliberately the conservative one, which makes this
	// look safe to buy — but the same blindness that hides the upside would hide
	// a mistake, and nothing here has established what the thing is worth.
	if v.AppearanceUnpriced {
		fails = append(fails, "выход посчитан по обычным экземплярам — по этим трейтам сравнить не с чем, оценивай руками")
	}
	// Cross-market depth is the cap that keeps a hole in the Tonnel book from
	// reading as room to sell into. Without it we are pricing blind, and "the
	// other venues did not answer" must never be mistaken for "they had no
	// objection".
	if v.Cross.Unreachable > 0 {
		fails = append(fails, fmt.Sprintf("%d площадк(и) не ответили — без их стакана автобай не работает", v.Cross.Unreachable))
	}
	if v.Liq.MADRatio > a.MaxMADRatio {
		fails = append(fails, fmt.Sprintf("разброс цен %.0f%% выше максимума автобая %.0f%%", v.Liq.MADRatio*100, a.MaxMADRatio*100))
	}
	if v.Liq.Trend < a.MinTrend {
		fails = append(fails, fmt.Sprintf("тренд %.2f ниже минимума автобая %.2f", v.Liq.Trend, a.MinTrend))
	}
	if limits.MaxExitDays > 0 && v.ExpectedDays > limits.MaxExitDays {
		fails = append(fails, fmt.Sprintf("ожидаемая продажа %s дольше лимита %.1fд — поднять: /limits set max_exit_days 7",
			days(v.ExpectedDays), limits.MaxExitDays))
	}
	if a.MaxDataAge > 0 && v.DataAge > a.MaxDataAge {
		fails = append(fails, fmt.Sprintf("данные рынка устарели на %s, максимум для автобая %s", v.DataAge.Round(time.Second), a.MaxDataAge))
	}
	if !v.FX.Valid {
		fails = append(fails, "курс GRAM недоступен или протух")
	} else if a.MaxGramMove15m > 0 && math.Abs(v.FX.Move15m) > a.MaxGramMove15m {
		fails = append(fails, fmt.Sprintf("GRAM сходил на %+.1f%% за 15м; лимит автобая %.1f%%", v.FX.Move15m*100, a.MaxGramMove15m*100))
	}
	if !v.HasCompetingAsk {
		fails = append(fails, "не от чего отталкиваться — конкурентов в стакане нет")
	} else if v.LiveDepthCount < 2 {
		fails = append(fails, fmt.Sprintf(
			"в стакане одна живая цена %.2f, дальше дырка (%d аска всего) — для автобая это не глубина", v.CompetingAsk, v.ExternalAsks))
	} else if v.DepthCapped {
		fails = append(fails, "стакан дырявый — глубину пришлось резать, автобай на такой не работает")
	}
	return fails
}

// Score ranks simultaneous signals by risk-adjusted daily ROI, fill probability
// and confidence, so a quick real 3% exit can beat a slow theoretical 10% one.
func Score(v pricing.Valuation) float64 {
	if !v.Valid || v.Price <= 0 {
		return 0
	}
	return pricing.BuildScore(v, 1).Total
}

func days(d float64) string {
	if math.IsInf(d, 1) {
		return "непонятно"
	}
	if d < 1 {
		return fmt.Sprintf("%.0fч", d*24)
	}
	return fmt.Sprintf("%.1fд", d)
}
