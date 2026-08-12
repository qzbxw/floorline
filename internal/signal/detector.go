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

// Decision is the full verdict on one listing.
type Decision struct {
	Gift tonnel.Gift
	Val  pricing.Valuation

	Signal     bool // worth telling the operator about
	Auto       bool // clears every unattended-purchase gate
	Suppressed bool // signal-worthy but muted or inside the alert cooldown
	Score      float64

	SignalFails []string
	AutoFails   []string

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
	Warm             func() bool
	CalibrationReady func() bool
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
	return d.evaluate(ctx, g, limits, now, true)
}

// EvaluateFresh repeats every gate without signal deduplication. It is used in
// the money path after a direct GiftData quote and a forced book refresh.
func (d *Detector) EvaluateFresh(ctx context.Context, g tonnel.Gift, limits risk.Limits, now time.Time) (*Decision, error) {
	return d.evaluate(ctx, g, limits, now, false)
}

func (d *Detector) evaluate(ctx context.Context, g tonnel.Gift, limits risk.Limits, now time.Time, dedupe bool) (*Decision, error) {
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
	})
	if d.CrossSupport != nil {
		if cm := d.CrossSupport(ctx, val); cm.Support > 0 {
			val = pricing.WithCrossDepth(val, cm)
		}
	}

	dec := &Decision{Gift: g, Val: val}
	if firstSeen, err := d.st.ListingFirstSeen(ctx, g.GiftID.Int()); err == nil && !firstSeen.IsZero() {
		dec.Age = now.Sub(firstSeen)
	}

	if !val.Valid {
		dec.SignalFails = []string{val.Reason}
		return dec, nil
	}

	dec.SignalFails = d.signalGates(val)
	dec.Signal = len(dec.SignalFails) == 0
	dec.Score = Score(val)

	if dec.Signal {
		muted, err := d.st.IsMuted(ctx, key, now)
		if err != nil {
			return nil, fmt.Errorf("mute check: %w", err)
		}
		last, err := d.st.LastSignalForModel(ctx, key, KindBuy)
		if err != nil {
			return nil, fmt.Errorf("cooldown check: %w", err)
		}
		dec.Suppressed = muted || (!last.IsZero() && now.Sub(last) < alertCooldown)

		dec.AutoFails = d.autoGates(val, limits)
		dec.Auto = len(dec.AutoFails) == 0
	}

	return dec, nil
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
	} else if v.ScoreBreakdown.RiskAdjustedEdge < g.MinEdge {
		fails = append(fails, fmt.Sprintf("эдж с поправкой на риск %.1f%% ниже %.1f%% (сырой %.1f%%)", v.ScoreBreakdown.RiskAdjustedEdge*100, g.MinEdge*100, v.Edge*100))
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

// autoGates is layered strictly on top of signalGates. Everything here answers
// one question: if we buy this, can we actually get out of it quickly?
func (d *Detector) autoGates(v pricing.Valuation, limits risk.Limits) []string {
	a := d.cfg.Auto
	var fails []string

	if d.Warm != nil && !d.Warm() {
		fails = append(fails, "история сделок ещё прогревается")
	}
	if d.cfg.ShadowMode {
		fails = append(fails, "включён shadow-режим")
	} else if d.CalibrationReady != nil && !d.CalibrationReady() {
		fails = append(fails, "калибровка скоринга не набрала минимум выборки")
	}
	if v.ScoreBreakdown.RiskAdjustedEdge < a.MinEdge {
		fails = append(fails, fmt.Sprintf("эдж с поправкой на риск %.1f%% ниже порога автобая %.1f%%", v.ScoreBreakdown.RiskAdjustedEdge*100, a.MinEdge*100))
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
		fails = append(fails, fmt.Sprintf("ожидаемая продажа %s дольше лимита %.1fд",
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
