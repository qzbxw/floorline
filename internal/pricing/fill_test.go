package pricing

import (
	"math"
	"testing"
)

// The day count divided the model's whole standing supply by a fourteen-day
// average trade rate. Forty gifts listed and two of them under our price is a
// two-deep queue; the other thirty-eight are irrelevant until we ask more than
// they do.
func TestFillCountsTheQueueAtOurPriceNotTheWholeSupply(t *testing.T) {
	// Forty listings, but only two at or under 4.00.
	ladder := []float64{3.5, 3.9}
	for i := 0; i < 38; i++ {
		ladder = append(ladder, 9+float64(i))
	}

	c := fillCurve(ladder, 4.00, 1.0)
	if c.Cheaper != 2 {
		t.Errorf("cheaper = %d, want the 2 offers a buyer actually reaches first", c.Cheaper)
	}
	if c.QueueAhead > 3 {
		t.Errorf("queue = %d; the 38 listings priced far above us are not in front of anybody", c.QueueAhead)
	}
	if c.In7d <= c.In72h || c.In72h <= c.In24h {
		t.Errorf("survival curve must rise with the horizon: %.2f / %.2f / %.2f", c.In24h, c.In72h, c.In7d)
	}
}

// Priced to undercut everything we are first in line at this instant, by
// construction. A model reading only that would report every fast exit as
// equally quick — so the sellers a hair above us, the ones who take that place
// back, have to count for something.
func TestBeingCheapestNowIsNotBeingCheapestAllWeek(t *testing.T) {
	alone := fillCurve([]float64{9, 10}, 4.00, 1.0)
	crowded := fillCurve([]float64{4.02, 4.05, 4.10, 4.15}, 4.00, 1.0)

	if alone.Cheaper != 0 || crowded.Cheaper != 0 {
		t.Fatalf("fixture is wrong: both should be the cheapest offer right now (%d, %d)",
			alone.Cheaper, crowded.Cheaper)
	}
	if crowded.QueueAhead <= alone.QueueAhead {
		t.Errorf("four sellers stacked on our price gave queue %d, no worse than an empty book at %d",
			crowded.QueueAhead, alone.QueueAhead)
	}
	if crowded.In24h >= alone.In24h {
		t.Errorf("P(24h) crowded %.2f is not below alone %.2f", crowded.In24h, alone.In24h)
	}
}

// A model nobody trades cannot be sold at any price, and the curve has to say so
// rather than dividing by zero into a confident-looking number.
func TestNoBuyersMeansNoFill(t *testing.T) {
	c := fillCurve([]float64{5}, 4, 0)
	if c.In24h != 0 || c.In72h != 0 || c.In7d != 0 {
		t.Errorf("a model with no trade rate reported a fill chance: %+v", c)
	}
	if !math.IsInf(c.Median(), 1) {
		t.Errorf("median wait = %v, want infinity", c.Median())
	}
}

// The executable price is the counterpart to fair value: the most we can ask and
// still expect to be gone in time. It must rise with the horizon — patience buys
// price — and never exceed the ceiling discovery puts on it.
func TestExecutableExitRisesWithThePatienceAllowed(t *testing.T) {
	ladder := []float64{4.0, 4.2, 4.5, 5.0}
	const floorPrice, ceiling, buyers, undercut = 3.9, 4.8, 1.5, 0.01

	in24 := executableAt(ladder, floorPrice, ceiling, buyers, undercut, day24)
	in72 := executableAt(ladder, floorPrice, ceiling, buyers, undercut, day72)
	in7d := executableAt(ladder, floorPrice, ceiling, buyers, undercut, day7d)

	if in72 < in24 || in7d < in72 {
		t.Errorf("executable exits must not fall as the deadline loosens: %.3f / %.3f / %.3f", in24, in72, in7d)
	}
	if in7d > ceiling+1e-9 {
		t.Errorf("the week-long exit %.3f prices through discovery's ceiling %.3f", in7d, ceiling)
	}
	// Zero is a real answer and the one this fixture gives at a day: at 1.5
	// buyers a day with a seller on our shoulder, no price is more likely than
	// not to be gone by tomorrow. Saying so is the entire improvement over
	// printing "~1.6д" and letting the operator plan around it.
	if in24 != 0 {
		t.Errorf("24h exit = %.3f; at this arrival rate nothing should clear inside a day", in24)
	}
	// Give the model real flow and the day horizon becomes reachable.
	if fast := executableAt(ladder, floorPrice, ceiling, 6, undercut, day24); fast < floorPrice {
		t.Errorf("a model doing six sales a day still has no 24h exit (%.3f)", fast)
	}
}

// Our buyer does not know which marketplace an offer is on, so a queue split
// across venues is still one queue. External stickers are restated as the Tonnel
// ask that costs the same before they join it.
func TestMergedLadderIsOneQueueInBuyerTerms(t *testing.T) {
	local := []Ask{{Price: 4.10}, {Price: 4.30}}
	cross := []float64{4.00 * (1 + testFee), 4.20 * (1 + testFee)}

	got := mergedLadder(local, cross, testFee)
	want := []float64{4.00, 4.10, 4.20, 4.30}
	if len(got) != len(want) {
		t.Fatalf("ladder = %v, want %v", got, want)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-6 {
			t.Fatalf("ladder = %v, want %v (ascending, in Tonnel terms)", got, want)
		}
	}
}
