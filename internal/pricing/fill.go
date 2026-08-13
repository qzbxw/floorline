package pricing

import (
	"math"
	"sort"
)

// This file answers the question a flipper actually has. Not "what is this
// worth" — that is fair_value and it is the easier half — but "what will I get
// out at, and how likely am I to get out at all before the money is needed
// somewhere else".
//
// The engine used to answer it with a single number of days, printed to one
// decimal: "~1.6д", "~2.8д", "~4.7д". That precision was fiction twice over. It
// came from dividing a queue by a fourteen-day average trade rate, which is a
// mean and not a schedule; and the queue it divided was the whole standing
// supply of the model rather than the offers standing in front of the price we
// were actually asking. Forty gifts listed and two of them under our exit is not
// a forty-deep queue — it is a two-deep one, and the other thirty-eight are
// irrelevant until we raise our ask above them.
//
// So the day count is replaced by a survival curve. Buyers for a model arrive at
// some rate; each one takes the cheapest offer on any screen; we are sold when
// enough of them have arrived to clear everyone standing cheaper than us. That
// is a Poisson counting process, and it produces exactly the three numbers worth
// printing:
//
//	P(fill 24h) = 34%   P(fill 72h) = 71%   P(fill 7d) = 89%
//
// The same machinery answers the pricing question in reverse. Rather than one
// "fast exit", there is an executable price *per horizon*: the most we can ask
// and still expect to be gone inside 24 hours, inside 72, inside a week. That is
// the number a purchase should be judged against, and it is not fair value.

const (
	// fillHorizons are the three waits worth quoting: tomorrow, the weekend, the
	// week. Anything beyond that is not a flip and the gates reject it anyway.
	day24 = 1.0
	day72 = 3.0
	day7d = 7.0
	// executableConfidence is how likely a fill has to be before a price counts
	// as executable within a horizon. A coin flip is the honest bar: above it the
	// price is more likely than not to be gone in time.
	executableConfidence = 0.5
	// maxQueueAhead caps the queue length fed to the Poisson tail. Past this the
	// answer is "no" and the factorials stop being worth computing.
	maxQueueAhead = 40
)

// FillCurve is the chance of being sold within each horizon, and the queue that
// decides it.
type FillCurve struct {
	In24h, In72h, In7d float64
	// QueueAhead is the effective queue: the offers a buyer takes before ours,
	// plus an allowance for the sellers standing just above us who will undercut
	// before the horizon is out. It is what the probabilities are computed from.
	//
	// The allowance is not a fudge, it is the difference between a snapshot and a
	// wait. Priced to undercut everything, we are first in line *at this instant*
	// by construction — so a model reading only the current queue would report
	// every fast exit as equally quick, which is how "cheapest on screen" turns
	// into a promise nobody can keep. The sellers a hair above us are the ones
	// who take that place back, and over three days about half of them do.
	QueueAhead int
	// Cheaper is the part of the queue that is genuinely already in front: offers
	// at or under our price right now.
	Cheaper int
	// Undercutters is the count sitting just above us, before the share that is
	// expected to move under us is applied.
	Undercutters int
	// BuyersPerDay is the arrival rate the curve was built on.
	BuyersPerDay float64
}

// Median is the wait at which a fill becomes more likely than not, in days, or
// +Inf when it never does inside a week. It exists so the gates and the ranking
// keep a single comparable number without the card having to print one.
func (f FillCurve) Median() float64 {
	switch {
	case f.BuyersPerDay <= 0:
		return math.Inf(1)
	case f.In24h >= .5:
		return day24
	case f.In72h >= .5:
		return day72
	case f.In7d >= .5:
		return day7d
	default:
		return math.Inf(1)
	}
}

const (
	// undercutBand is how close above us a seller has to be to be treated as
	// competition for the same buyer rather than as part of the book above.
	undercutBand = 0.05
	// undercutShare is the fraction of those sellers expected to price under us
	// before a multi-day horizon is out. Half is the honest guess and it is worth
	// stating as a guess: it is the one number here not measured from data, and
	// it is the first thing the outcome journal should be used to calibrate.
	undercutShare = 0.5
)

// fillCurve computes the survival curve for a price, given the merged ladder of
// competing offers and the model's buyer arrival rate.
func fillCurve(ladder []float64, price, buyersPerDay float64) FillCurve {
	c := FillCurve{BuyersPerDay: buyersPerDay}
	if price <= 0 {
		return c
	}
	c.Cheaper = queueAhead(ladder, price)
	c.Undercutters = countBand(ladder, price, price*(1+undercutBand))
	c.QueueAhead = c.Cheaper + int(math.Round(undercutShare*float64(c.Undercutters)))
	if buyersPerDay <= 0 {
		return c
	}
	c.In24h = fillProbability(buyersPerDay*day24, c.QueueAhead)
	c.In72h = fillProbability(buyersPerDay*day72, c.QueueAhead)
	c.In7d = fillProbability(buyersPerDay*day7d, c.QueueAhead)
	return c
}

// countBand counts offers strictly above lo and no higher than hi.
func countBand(ladder []float64, lo, hi float64) int {
	n := 0
	for _, p := range ladder {
		if p > lo+1e-9 && p <= hi+1e-9 {
			n++
		}
	}
	return n
}

// queueAhead counts the offers a buyer reaches before ours.
//
// Offers at the same price count as ahead of us. Two identical asks are a
// coin toss the marketplace resolves by its own ordering rules, and assuming we
// win it is how a queue of equals turns into a promise of an instant sale.
func queueAhead(ladder []float64, price float64) int {
	if price <= 0 {
		return 0
	}
	n := 0
	for _, p := range ladder {
		if p > 0 && p <= price+1e-9 {
			n++
		}
	}
	return n
}

// fillProbability is P(at least k+1 buyers arrive), for a Poisson process of
// mean `mean` over the horizon. k is the queue standing in front of us: every
// one of them absorbs a buyer before we see one.
func fillProbability(mean float64, queueAhead int) float64 {
	if mean <= 0 {
		return 0
	}
	if queueAhead < 0 {
		queueAhead = 0
	}
	if queueAhead > maxQueueAhead {
		return 0
	}
	// P(N >= k+1) = 1 - P(N <= k), summed term by term so no factorial is ever
	// materialised — at k = 40 that would overflow long before the sum does.
	term := math.Exp(-mean)
	cdf := term
	for i := 1; i <= queueAhead; i++ {
		term *= mean / float64(i)
		cdf += term
	}
	return clamp(1-cdf, 0, 1)
}

// executableAt is the highest price we can ask and still be more likely than not
// to be gone within the horizon.
//
// This is the counterpart to fair value and the one a purchase is judged
// against. Fair value asks what the thing is worth; this asks what the market
// will actually pay us inside a deadline. A gift can be worth 8 and have a
// 72-hour executable exit of 5.1, and when the entry is 4.9 the eight is not a
// fact about this trade — it is a fact about a trade somebody else will make.
//
// The search walks the competing ladder rather than the price axis, because the
// probability only changes where the queue changes: every rung we price above
// puts one more seller in front of us. Undercutting a rung by the configured
// margin is what buys the place in front of it.
func executableAt(ladder []float64, floorPrice, ceiling, buyersPerDay, undercut, horizonDays float64) float64 {
	if buyersPerDay <= 0 || floorPrice <= 0 {
		return 0
	}
	best := 0.0
	consider := func(price float64) {
		if price < floorPrice || (ceiling > 0 && price > ceiling) || price <= best {
			return
		}
		if fillCurveAt(ladder, price, buyersPerDay, horizonDays) >= executableConfidence {
			best = price
		}
	}
	// The floor of the search is the price that undercuts everything, and the
	// rungs above it are the prices that give up one place in the queue each.
	consider(floorPrice)
	for _, rung := range ladder {
		if rung > 0 {
			consider(rung * (1 - undercut))
		}
	}
	if ceiling > 0 {
		consider(ceiling)
	}
	return best
}

// fillCurveAt is the probability of a fill at one price over one horizon, using
// the same effective queue the published curve does. Kept separate so the
// executable-price search cannot drift away from the number on the card.
func fillCurveAt(ladder []float64, price, buyersPerDay, horizonDays float64) float64 {
	c := fillCurve(ladder, price, buyersPerDay)
	return fillProbability(buyersPerDay*horizonDays, c.QueueAhead)
}

// mergedLadder is every competing offer, ours excluded, restated as the Tonnel
// ask that would cost a buyer the same, ascending.
//
// One ladder rather than two is the point. Our buyer does not know or care which
// marketplace an offer is on, so a queue split across venues is still one queue,
// and the engine has to count it as one to say anything true about a fill.
func mergedLadder(local []Ask, cross []float64, fee float64) []float64 {
	out := make([]float64, 0, len(local)+len(cross))
	for _, a := range local {
		if a.Price > 0 {
			out = append(out, a.Price)
		}
	}
	for _, p := range cross {
		if eq := TonnelEquivalent(p, fee); eq > 0 {
			out = append(out, eq)
		}
	}
	sort.Float64s(out)
	return out
}
