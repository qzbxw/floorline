package pricing

import (
	"math"
	"sort"

	"floorline/internal/store"
)

// MinAttributeSamples is the smallest bucket allowed to move a price at all.
// Below it, a "premium" is the sampling noise of two or three prints: the
// n/(n+k) shrinkage still leaves a fraction of a percent on the table, and that
// fraction then travels into an exit price and out into an exit recommendation
// as though it were evidence. Under this many samples the premium is exactly
// zero and the model median stands.
const MinAttributeSamples = 5

const (
	// marginalPrior is the empirical-Bayes prior weight for the single-attribute
	// (backdrop-only, symbol-only) premiums.
	marginalPrior = 20.0
	// interactionPrior is heavier because the exact backdrop+symbol combination
	// is always the sparsest bucket and the easiest one to overfit.
	interactionPrior = 40.0
)

// AttributeValue is an empirical-Bayes premium for backdrop, symbol and their
// interaction. Sparse combinations shrink back to the model median.
type AttributeValue struct {
	Fair            float64
	Premium         float64
	BackdropSamples int
	SymbolSamples   int
	ExactSamples    int
	ExactShare      float64
	Confidence      float64
	Valid           bool
}

func ComputeAttributeValue(sales []store.SaleRow, backdrop, symbol string, modelMedian float64) AttributeValue {
	var out AttributeValue
	if modelMedian <= 0 || (backdrop == "" && symbol == "") || len(sales) == 0 {
		return out
	}
	var bd, sy, exact []float64
	for _, s := range sales {
		if s.Price <= 0 {
			continue
		}
		baseline := modelMedian
		if !s.TS.IsZero() {
			var nearby []float64
			for _, other := range sales {
				if other.Price > 0 && !other.TS.IsZero() && math.Abs(other.TS.Sub(s.TS).Hours()) <= 7*24 {
					nearby = append(nearby, other.Price)
				}
			}
			if len(nearby) >= 3 {
				baseline = medianFloat(nearby)
			}
		}
		if baseline <= 0 {
			continue
		}
		r := math.Log(s.Price / baseline)
		if backdrop != "" && s.Backdrop == backdrop {
			bd = append(bd, r)
		}
		if symbol != "" && s.Symbol == symbol {
			sy = append(sy, r)
		}
		if (backdrop == "" || s.Backdrop == backdrop) && (symbol == "" || s.Symbol == symbol) {
			exact = append(exact, r)
		}
	}
	bp := shrunkMedian(bd, marginalPrior)
	sp := shrunkMedian(sy, marginalPrior)
	interaction := 0.0
	if len(exact) >= MinAttributeSamples {
		interaction = (medianFloat(exact) - bp - sp) * float64(len(exact)) / (float64(len(exact)) + interactionPrior)
	}
	logPremium := clamp(bp+sp+interaction, math.Log(.65), math.Log(1.35))
	out.BackdropSamples, out.SymbolSamples, out.ExactSamples = len(bd), len(sy), len(exact)
	out.ExactShare = float64(len(exact)) / float64(len(sales))
	sample := math.Min(float64(len(sales))/20, 1)
	out.Confidence = clamp(.35*sample+.2*reliability(len(bd), marginalPrior)+.2*reliability(len(sy), marginalPrior)+.25*reliability(len(exact), interactionPrior), 0, 1)
	out.Premium = math.Exp(logPremium) - 1
	out.Fair = modelMedian * math.Exp(logPremium)
	out.Valid = len(bd) >= MinAttributeSamples || len(sy) >= MinAttributeSamples || len(exact) >= MinAttributeSamples
	return out
}

type ScoreBreakdown struct {
	NetProfit        float64 `json:"net_profit"`
	ExpectedDays     float64 `json:"expected_days"`
	Confidence       float64 `json:"confidence"`
	PortfolioFit     float64 `json:"portfolio_fit"`
	ProfitPerDay     float64 `json:"profit_per_day"`
	Total            float64 `json:"total"`
	RiskAdjustedEdge float64 `json:"risk_adjusted_edge"`
	SafetyBuffer     float64 `json:"safety_buffer"`
	DailyROI         float64 `json:"daily_roi"`
	FillProbability  float64 `json:"fill_probability"`
	FXFactor         float64 `json:"fx_factor"`
	DepthFactor      float64 `json:"depth_factor"`
	// CrossFactor is how much the other venues corroborate this trade, and
	// LiquidityFactor how much the model actually trades. Both used to be absent
	// from the score entirely.
	CrossFactor     float64 `json:"cross_factor"`
	LiquidityFactor float64 `json:"liquidity_factor"`
	// Quality is every trust multiplier collapsed into one number in [0,1], so
	// the card can say "how good is the evidence" separately from "how good is
	// the price".
	Quality float64 `json:"quality"`
}

// scoreHalfPoint is the daily risk-adjusted return that scores 50 out of 100
// on perfect evidence. 3%/day is a very good flip on this market, so the scale
// saturates where reality does.
const scoreHalfPoint = 0.03

// BuildScore turns a valuation into a single number in 0..100.
//
// The old score was an unbounded product that ran from 9 to 136 with no
// interpretable meaning, and it was dominated by 1/ExpectedDays — so a thin,
// badly-evidenced lot with an optimistic day count outranked a solid one. Worse,
// it *rewarded* the exact pathology the pricing engine exists to catch: a wide
// gap above the cheapest ask scored a 1.18 bonus as "room to sell into" even
// when that gap was a hole with a single real price under it. The highest score
// the desk ever produced, 136.5, was a book reading 8 → 14.4 with one live ask.
//
// The shape now is deliberately two-part: how good the price is, multiplied by
// how much the evidence deserves to be believed.
func BuildScore(v Valuation, portfolioFit float64) ScoreBreakdown {
	portfolioFit = clamp(portfolioFit, 0, 1)
	b := ScoreBreakdown{
		NetProfit: v.Net, ExpectedDays: v.ExpectedDays, Confidence: v.Confidence,
		PortfolioFit: portfolioFit, FXFactor: 1, DepthFactor: 1, CrossFactor: 1, LiquidityFactor: 1,
	}
	b.SafetyBuffer = .005 + math.Min(.03, v.Liq.MADRatio*.15/math.Sqrt(math.Max(float64(v.Liq.Sales), 1))) + math.Min(.01, float64(v.CompetitorsNear)*.001)
	b.FillProbability = 1 / (1 + math.Max(v.ExpectedDays, 0)/3)

	if v.FX.Valid {
		b.FXFactor = clamp(1-math.Max(v.FX.FloorLag, 0)*1.5+math.Max(-v.FX.FloorLag, 0)*.35, .5, 1.15)
		if math.Abs(v.FX.Move15m) > .02 {
			b.FXFactor *= .85
		}
	}

	b.DepthFactor = depthFactor(v, &b)
	b.CrossFactor = crossFactor(v)
	b.LiquidityFactor = liquidityFactor(v.Liq)

	// Every penalty must land before this line: the risk-adjusted edge is what
	// the gates read, so a buffer added afterwards would only decorate the
	// recorded breakdown.
	b.RiskAdjustedEdge = math.Max(v.Edge-b.SafetyBuffer, 0)

	b.Quality = evidenceQuality(v, b, portfolioFit)

	if v.Valid && b.RiskAdjustedEdge > 0 && !math.IsInf(v.ExpectedDays, 1) {
		b.ProfitPerDay = v.Net / math.Max(v.ExpectedDays, .25)
		b.DailyROI = b.RiskAdjustedEdge / math.Max(v.ExpectedDays, .5)
		// Saturating so that an implausible day estimate cannot run away with
		// the ranking: doubling an already-excellent daily return is worth a few
		// points, not a multiple.
		price := b.DailyROI / (b.DailyROI + scoreHalfPoint)
		b.Total = clamp(100*price*b.Quality, 0, 100)
	}
	return b
}

// evidenceQuality is how much the trade deserves to be believed, in [0,1].
//
// Multiplying every trust factor together was wrong twice over. Six numbers
// each below one collapse towards zero, so an ordinary well-evidenced trade
// scored like a broken one: a +9.5% flip on ten distinct gifts with Portals
// confirming came out at 7% quality and five points out of a hundred.
//
// And it double-counted. History disagreeing with the book discounted
// confidence, discounted depth, and showed up a third time through
// cross-market divergence — one fact, three penalties, compounding.
//
// So the dimensions are averaged rather than multiplied. A geometric mean still
// punishes a genuinely broken dimension hard (one near-zero term drags the
// whole thing down) without letting four merely-imperfect ones bottom out. The
// disagreement discount is then applied exactly once, on top.
func evidenceQuality(v Valuation, b ScoreBreakdown, portfolioFit float64) float64 {
	dims := []float64{
		clamp(v.Confidence, .01, 1),      // how much history there is
		clamp(b.DepthFactor, .01, 1.18),  // whether there is a queue to sell into
		clamp(b.CrossFactor, .01, 1.05),  // whether other venues corroborate
		clamp(b.LiquidityFactor, .01, 1), // whether the model actually trades
	}
	product := 1.0
	for _, d := range dims {
		product *= d
	}
	q := math.Pow(product, 1/float64(len(dims)))

	// One fact, one penalty. The history and the price we settled on telling
	// different stories is a reason for a human to look, and it is already a
	// hard auto-buy gate; here it costs rank once rather than three times.
	if v.MarketDisagreement {
		q *= .75
	}
	// These two are about us and the wider market rather than about the
	// evidence, so they stay multiplicative.
	return clamp(q*b.FXFactor*portfolioFit, 0, 1)
}

// depthFactor judges the queue we would have to sell through.
//
// A gap only counts as room to sell into when there is a real run of prices
// under it. Above a single ask the same gap is the hole itself — that is what
// used to earn the highest score the desk ever printed.
//
// The judgement is about whichever book the exit actually rests on. When
// another venue's queue caps the price, a hole in the local book is no longer
// load-bearing and penalising it is penalising a number we did not use.
func depthFactor(v Valuation, b *ScoreBreakdown) float64 {
	if v.ExitFromCross {
		// Priced off the external queue; its own depth is what matters, and
		// crossFactor already scores that.
		return 1
	}
	f := 1.0
	switch {
	case v.DepthCapped || (v.HasCompetingAsk && v.LiveDepthCount < 2):
		// One live price and then a jump. The exit is a guess about a market
		// nobody is currently making.
		f = .45
		b.SafetyBuffer += .01
	case v.AskGap1 >= .05 || v.AskGap3 >= .08:
		f = 1.18
	case v.AskGap1 < .02 && v.AskGap3 < .04:
		f = .72
		b.SafetyBuffer += .01
	}
	if !v.HasCompetingAsk {
		f = math.Min(f, .5) // nothing to price against but history
	}
	return f
}

// crossFactor is how much the rest of the market corroborates the trade.
//
// Cross-market depth is what bounds the exit, so its absence is not neutral. A
// venue that could not be read at all is worse than a venue with nothing
// listed: we are guessing where we could have been checking.
func crossFactor(v Valuation) float64 {
	if v.Cross.Unreachable > 0 {
		return .5
	}
	if v.CrossMarketSupport <= 0 {
		return .8 // priced on Tonnel alone
	}
	f := 1.0
	if v.Cross.Venues >= 2 {
		f = 1.05 // two independent venues agreeing is real corroboration
	}
	// Disagreement is already a manual-only gate; here it also costs rank, in
	// proportion to how far apart the venues are.
	if v.CrossDivergence > 0 {
		f *= clamp(1-v.CrossDivergence, .4, 1)
	}
	return f
}

// liquidityFactor is the model's actual trade rate.
//
// Expected days already divides by velocity, but that only says how long one
// sale should take — not how much to trust the estimate. A model printing twice
// a week can produce a flattering day count off a single competitor, and the
// ranking used to take it at face value.
func liquidityFactor(l Liquidity) float64 {
	if l.Velocity <= 0 {
		return .3
	}
	return clamp(l.Velocity/(l.Velocity+.6), .3, 1)
}

// confidence is how much the trade history deserves to be believed.
//
// It used to start at 0.2 and collect another 0.2 for turnover regardless of
// sample size, so eight prints could read as 81% confident. Sample size now
// scales the whole thing: with nothing traded there is nothing to be confident
// about, however tidy the few prints look.
func confidence(l Liquidity, a AttributeValue) float64 {
	n := float64(l.DistinctGifts)
	if n <= 0 {
		return 0
	}
	// Half weight at 8 distinct gifts, 0.71 at 20, 0.83 at 40.
	sample := n / (n + 8)
	turnover := clamp(l.Turnover, 0, 1)
	stability := clamp(1-l.MADRatio, 0, 1)
	trend := clamp(l.Trend, 0, 1)

	base := sample * (.45 + .2*turnover + .25*stability + .1*trend)
	if a.Valid {
		base = .8*base + .2*a.Confidence
	}
	return clamp(base, 0, 1)
}

func shrunkMedian(xs []float64, prior float64) float64 {
	if len(xs) < MinAttributeSamples {
		return 0
	}
	return medianFloat(xs) * float64(len(xs)) / (float64(len(xs)) + prior)
}
func reliability(n int, prior float64) float64 { return float64(n) / (float64(n) + prior) }
func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	y := append([]float64(nil), xs...)
	sort.Float64s(y)
	n := len(y)
	if n%2 == 1 {
		return y[n/2]
	}
	return (y[n/2-1] + y[n/2]) / 2
}
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
