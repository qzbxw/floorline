package pricing

import (
	"fmt"
	"math"
	"time"
)

// A market that is falling is not the same market with worse numbers in it. The
// engine applied one set of rules everywhere, and that is how a model whose
// prints were good a week ago and bad since kept reading as an opportunity: the
// fourteen-day median it was measured against still contained the good week.
//
// Three regimes, decided by what the recent tape does relative to the whole
// window, are enough to change the three things that should change: how much
// edge is worth demanding, how long a hold is acceptable, and how much the old
// half of the history still deserves to be believed.

// Regime is the state of a model's market.
type Regime string

const (
	RegimeBull    Regime = "растущий"
	RegimeNeutral Regime = "спокойный"
	RegimeFalling Regime = "падающий"
)

const (
	// fallingTrend is where the last week's median has dropped far enough below
	// the window's that the difference is a direction rather than noise.
	fallingTrend = 0.96
	bullTrend    = 1.05
	// regimeMinPrints is the tape needed before a trend is a trend. Below it the
	// market is neutral because we cannot see it, not because it is calm.
	regimeMinPrints = 5
)

// ReadRegime classifies a model's market from its own tape.
func ReadRegime(l Liquidity) Regime {
	if l.Sales < regimeMinPrints || l.Median <= 0 || l.Trend <= 0 {
		return RegimeNeutral
	}
	switch {
	case l.Trend < fallingTrend:
		return RegimeFalling
	case l.Trend > bullTrend:
		return RegimeBull
	default:
		return RegimeNeutral
	}
}

// EdgeSurcharge is what a regime adds to the edge a trade has to clear.
//
// Buying into a falling model means the exit is priced off a queue that will be
// cheaper by the time we reach it, so the margin has to cover the drift as well
// as the spread. Nothing is given back in a rising one: an upward move is not a
// reason to relax the only guard that survives being wrong about it.
func (r Regime) EdgeSurcharge() float64 {
	if r == RegimeFalling {
		return 0.02
	}
	return 0
}

// HoldLimit scales the acceptable time to exit. A falling market punishes every
// extra day twice — once in the price and once in the capital that could have
// been somewhere else — so the patience allowed shrinks with it.
func (r Regime) HoldLimit(base float64) float64 {
	if base <= 0 {
		return base
	}
	if r == RegimeFalling {
		return base * 0.6
	}
	return base
}

// RecencyWeight is how far the history reference is pulled from the window's
// median towards the last week's, in [0,1].
//
// In a falling market the old half of the window is describing a price level
// that no longer exists, and averaging it in is how the desk kept being told a
// stale model was cheap. In a calm one the longer window is the better estimator
// and is left alone.
func (r Regime) RecencyWeight() float64 {
	switch r {
	case RegimeFalling:
		return 0.7
	case RegimeBull:
		return 0.35
	default:
		return 0
	}
}

// Reference blends the window median with the recent one according to the
// regime. Zero inputs fall back to whichever number exists.
func (r Regime) Reference(median, median7 float64) float64 {
	switch {
	case median <= 0:
		return median7
	case median7 <= 0:
		return median
	}
	w := r.RecencyWeight()
	return median*(1-w) + median7*w
}

// String makes the regime printable on a card without a switch at every site.
func (r Regime) String() string { return string(r) }

// crossSanity is the band another venue's reference has to fall inside before it
// is allowed to move a price.
//
// The bounds are deliberately wide. Venues genuinely disagree — different fee
// structures, different audiences, different depth — and a band that fired on
// ordinary disagreement would throw away the corroboration the whole
// cross-market layer exists to provide. What it is here to catch is not
// disagreement but nonsense: a backdrop-matched queue quoting 15 for a model
// whose executable market is 3.16, or 159 for one trading at 3.8. Those are not
// prices, they are the result of a filter that did not apply or a seller who
// typed a number nobody will ever pay.
const (
	crossSanityHigh = 2.5
	crossSanityLow  = 0.4
)

// plausibleCross reports whether an external reference is close enough to the
// executable market to be believed, and by what multiple it missed.
func plausibleCross(support, anchor float64) (bool, float64) {
	if support <= 0 || anchor <= 0 {
		return true, 0 // nothing to check against; other guards handle it
	}
	ratio := support / anchor
	if ratio > crossSanityHigh || ratio < crossSanityLow {
		return false, ratio
	}
	return true, ratio
}

// Crowd is what the rest of the market did in the minutes around a listing. It
// is supplied by the caller because only the store can see arrivals over time.
type Crowd struct {
	// Window is how far back the arrivals were counted.
	Window time.Duration
	// Arrivals is how many other listings of this model appeared in it.
	Arrivals int
	// AtOrBelow is how many of them are priced at or under our candidate.
	AtOrBelow int
	// Cheapest is the lowest of those arrivals.
	Cheapest float64
}

const (
	// crowdedArrivals is how many sellers turning up at once stops being
	// coincidence. Two is a pair; three is people reacting to the same thing.
	crowdedArrivals = 3
	// undercuttingArrivals is the sharper signal: new stock arriving *below* the
	// lot we are about to buy. Even two of those means the cheap end is moving
	// down and we would be buying into the move rather than against it.
	undercuttingArrivals = 2
)

// AssessCrowd decides whether a discount is a mistake or the front of a slide,
// and writes the verdict onto the valuation.
//
// Both halves have to be present for it to fire. Fresh supply alone is a busy
// model, and a falling tape alone is a cheap model — neither is a reason to
// refuse. It is the two together that describe sellers competing to get out,
// which is the one situation where being the cheapest offer on screen is worth
// nothing: the screen will have a cheaper one on it within the hour.
func (v *Valuation) AssessCrowd(c Crowd) {
	v.AdverseSelection, v.AdverseReason = false, ""
	if c.Arrivals == 0 {
		return
	}
	falling := v.Regime == RegimeFalling
	undercut := c.AtOrBelow >= undercuttingArrivals ||
		(c.Cheapest > 0 && v.Price > 0 && c.Cheapest < v.Price)

	switch {
	case c.AtOrBelow >= undercuttingArrivals && (falling || c.Arrivals >= crowdedArrivals):
		v.AdverseSelection = true
		v.AdverseReason = fmt.Sprintf(
			"за %s в модели появилось %s дешевле или вровень с этим лотом — это не мисприс, а очередь на выход",
			shortDur(c.Window), pluralRU(c.AtOrBelow, "лот", "лота", "лотов"))
	case c.Arrivals >= crowdedArrivals && falling && undercut:
		v.AdverseSelection = true
		v.AdverseReason = fmt.Sprintf(
			"за %s выставилось %s этой модели, а лента идёт вниз — дешёвый лот здесь начало движения, а не подарок",
			shortDur(c.Window), pluralRU(c.Arrivals, "лот", "лота", "лотов"))
	}
}

// shortDur renders the arrival window the way a person would say it.
func shortDur(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%.0f минут", d.Minutes())
	}
	return fmt.Sprintf("%.0f ч", d.Hours())
}

// pluralRU picks the Russian noun form. The card layer has its own copy; this
// package needs one because the reason strings are built here, next to the
// evidence, rather than assembled from fragments upstream.
func pluralRU(n int, one, few, many string) string {
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

// executableAnchor is the local market's own answer to "what is this worth",
// used only to sanity-check the venues against something we trust.
//
// The cheapest genuinely competing ask comes first: it is live, it is on the
// venue we settle on, and somebody is standing behind it right now. The trade
// history is the fallback, and our own entry the last resort — a price we were
// willing to pay is weak evidence, but it is evidence, and having none at all
// would leave the gate unable to fire exactly when the venues are at their most
// unhinged.
func executableAnchor(v *Valuation, in Input) float64 {
	switch {
	case v.CompetingAsk > 0:
		return v.CompetingAsk
	case in.Liq.Median > 0:
		return in.Liq.Median
	case in.Floor > 0:
		return in.Floor
	default:
		return math.Max(v.Cost, 0)
	}
}
