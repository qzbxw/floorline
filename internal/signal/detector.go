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
	Warm func() bool
}

// New builds a Detector.
func New(st *store.Store, books *pricing.BookCache, cfg *config.Config) *Detector {
	return &Detector{
		st:       st,
		books:    books,
		cfg:      cfg,
		Coverage: func() time.Duration { return time.Duration(cfg.LookbackDays) * 24 * time.Hour },
		Warm:     func() bool { return true },
	}
}

func (d *Detector) window() time.Duration {
	return time.Duration(d.cfg.LookbackDays) * 24 * time.Hour
}

func (d *Detector) params() pricing.Params {
	return pricing.Params{
		Fee:      d.cfg.TonnelFee,
		Undercut: d.cfg.Undercut,
		Window:   d.window(),
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
	if !d.tradable(g) {
		return nil, nil
	}
	key := g.Key()
	if key.Name == "" || key.Model == "" {
		return nil, nil
	}
	price := g.Price.Float()

	already, err := d.st.AlreadySignalled(ctx, g.GiftID.Int(), KindBuy, price)
	if err != nil {
		return nil, fmt.Errorf("dedupe check: %w", err)
	}
	if already {
		return nil, nil
	}

	sales, err := d.st.SalesSince(ctx, key, now.Add(-d.window()))
	if err != nil {
		return nil, fmt.Errorf("load sales: %w", err)
	}
	liq := pricing.ComputeLiquidity(sales, now, d.window(), d.Coverage())

	// Cheap pre-filter. The exit price can never exceed the median of recent
	// trades, so this ceiling on the achievable edge is exact — and it rejects
	// almost the entire feed without spending a single request.
	if liq.Median > 0 {
		best := (liq.Median*(1-d.cfg.TonnelFee) - price) / price
		if best < d.cfg.Sig.MinEdge {
			return nil, nil
		}
	} else if liq.Sales < d.cfg.Sig.MinSales {
		return nil, nil // no trade history and no chance of clearing the gate
	}

	stat, err := d.st.ModelStat(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("load model stat: %w", err)
	}
	var floor, rarity float64
	var supply int
	if stat != nil {
		floor, supply, rarity = stat.Floor, stat.Supply, stat.Rarity
	}

	book, err := d.books.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("load book for %s: %w", key, err)
	}

	val := pricing.Evaluate(pricing.Input{
		GiftID: g.GiftID.Int(),
		Key:    key,
		Price:  price,
		Book:   book,
		Liq:    liq,
		Floor:  floor,
		Supply: supply,
		Rarity: rarity,
		Params: d.params(),
	})

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
	if g.Asset != "" && g.Asset != tonnel.AssetTON {
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

	if v.Edge < g.MinEdge {
		fails = append(fails, fmt.Sprintf("edge %.1f%% below %.1f%%", v.Edge*100, g.MinEdge*100))
	}
	if v.Liq.Velocity < g.MinVelocity {
		fails = append(fails, fmt.Sprintf("velocity %.2f/day below %.2f", v.Liq.Velocity, g.MinVelocity))
	}
	if v.Liq.Sales < g.MinSales {
		fails = append(fails, fmt.Sprintf("only %d trades in %dd, need %d", v.Liq.Sales, d.cfg.LookbackDays, g.MinSales))
	}
	if v.Liq.MADRatio > g.MaxMADRatio {
		fails = append(fails, fmt.Sprintf("price dispersion %.0f%% above %.0f%%", v.Liq.MADRatio*100, g.MaxMADRatio*100))
	}
	if v.Liq.Trend < g.MinTrend {
		fails = append(fails, fmt.Sprintf("trend %.2f below %.2f (falling)", v.Liq.Trend, g.MinTrend))
	}
	return fails
}

// autoGates is layered strictly on top of signalGates. Everything here answers
// one question: if we buy this, can we actually get out of it quickly?
func (d *Detector) autoGates(v pricing.Valuation, limits risk.Limits) []string {
	a := d.cfg.Auto
	var fails []string

	if d.Warm != nil && !d.Warm() {
		fails = append(fails, "trade history still warming up")
	}
	if v.Edge < a.MinEdge {
		fails = append(fails, fmt.Sprintf("edge %.1f%% below auto threshold %.1f%%", v.Edge*100, a.MinEdge*100))
	}
	if v.Liq.Velocity < a.MinVelocity {
		fails = append(fails, fmt.Sprintf("velocity %.2f/day below auto threshold %.2f", v.Liq.Velocity, a.MinVelocity))
	}
	if v.Liq.Sales < a.MinSales {
		fails = append(fails, fmt.Sprintf("%d trades in %dd, auto needs %d", v.Liq.Sales, d.cfg.LookbackDays, a.MinSales))
	}
	if v.Liq.Turnover < a.MinTurnover {
		fails = append(fails, fmt.Sprintf(
			"only %d distinct gifts across %d trades (turnover %.2f, auto needs %.2f) — looks self-dealt",
			v.Liq.DistinctGifts, v.Liq.Sales, v.Liq.Turnover, a.MinTurnover))
	}
	if v.Liq.MADRatio > a.MaxMADRatio {
		fails = append(fails, fmt.Sprintf("price dispersion %.0f%% above auto max %.0f%%", v.Liq.MADRatio*100, a.MaxMADRatio*100))
	}
	if v.Liq.Trend < a.MinTrend {
		fails = append(fails, fmt.Sprintf("trend %.2f below auto min %.2f", v.Liq.Trend, a.MinTrend))
	}
	if limits.MaxExitDays > 0 && v.ExpectedDays > limits.MaxExitDays {
		fails = append(fails, fmt.Sprintf("expected time to sell %s above max_exit_days %.1f",
			days(v.ExpectedDays), limits.MaxExitDays))
	}
	if v.ExitBasis == "median (sole ask)" {
		fails = append(fails, "no competing ask to price against")
	}
	return fails
}

// Score ranks simultaneous signals: bigger edge, faster market and larger
// ticket first, so the expensive liquid opportunity is not buried under dust.
func Score(v pricing.Valuation) float64 {
	if !v.Valid || v.Price <= 0 {
		return 0
	}
	return v.Edge * math.Min(v.Liq.Velocity, 5) * math.Log(1+v.Price)
}

func days(d float64) string {
	if math.IsInf(d, 1) {
		return "never"
	}
	if d < 1 {
		return fmt.Sprintf("%.0fh", d*24)
	}
	return fmt.Sprintf("%.1fd", d)
}
