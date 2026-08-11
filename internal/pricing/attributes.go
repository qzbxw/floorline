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
}

func BuildScore(v Valuation, portfolioFit float64) ScoreBreakdown {
	portfolioFit = clamp(portfolioFit, 0, 1)
	b := ScoreBreakdown{NetProfit: v.Net, ExpectedDays: v.ExpectedDays, Confidence: v.Confidence, PortfolioFit: portfolioFit, FXFactor: 1, DepthFactor: 1}
	b.SafetyBuffer = .005 + math.Min(.03, v.Liq.MADRatio*.15/math.Sqrt(math.Max(float64(v.Liq.Sales), 1))) + math.Min(.01, float64(v.CompetitorsNear)*.001)
	b.RiskAdjustedEdge = math.Max(v.Edge-b.SafetyBuffer, 0)
	b.FillProbability = 1 / (1 + math.Max(v.ExpectedDays, 0)/3)
	if v.FX.Valid {
		b.FXFactor = clamp(1-math.Max(v.FX.FloorLag, 0)*1.5+math.Max(-v.FX.FloorLag, 0)*.35, .5, 1.15)
		if math.Abs(v.FX.Move15m) > .02 {
			b.FXFactor *= .85
		}
	}
	switch {
	case v.AskGap1 >= .05 || v.AskGap3 >= .08:
		b.DepthFactor = 1.18
	case v.AskGap1 > 0 && v.AskGap1 < .02 && v.AskGap3 > 0 && v.AskGap3 < .04:
		b.DepthFactor = .72
		b.SafetyBuffer += .01
	}
	if v.MarketDisagreement {
		b.DepthFactor *= .45
	}
	if v.Valid && b.RiskAdjustedEdge > 0 && !math.IsInf(v.ExpectedDays, 1) {
		b.ProfitPerDay = v.Net / math.Max(v.ExpectedDays, .25)
		b.DailyROI = b.RiskAdjustedEdge / math.Max(v.ExpectedDays, .25)
		b.Total = b.DailyROI * 10000 * v.Confidence * portfolioFit * b.FillProbability * b.FXFactor * b.DepthFactor
	}
	return b
}

func confidence(l Liquidity, a AttributeValue) float64 {
	samples := math.Min(float64(l.Sales)/20, 1)
	turnover := clamp(l.Turnover, 0, 1)
	stability := clamp(1-l.MADRatio, 0, 1)
	trend := clamp(l.Trend, 0, 1)
	base := .2 + .3*samples + .2*turnover + .2*stability + .1*trend
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
