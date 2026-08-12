package pricing

import (
	"math"
	"strings"
	"testing"
	"time"

	"floorline/internal/tonnel"
)

// This file is the regression net for the largest pricing error the desk has
// made, reconstructed from the signals it actually sent on 12–13 August 2026.
//
// Every case below is a real card. The engine priced a fast exit off the median
// of the first three asks and applied the walkaway price only as a floor
// underneath it, so the exit routinely stood *behind* a queue: a price nobody
// could sell into quickly. Measured against what the same cards already printed
// as "слить сейчас" — and, for Jolly Chimp, against the price it actually sold
// at two hours later — the claimed edge was overstated by a median factor of
// two, and in two of these eight cases it had the wrong sign.
//
//	lot                       claimed   executable   claimed edge   real
//	Jolly Chimp · Tourist       7.326      6.93         +11.1%      sold 6.98
//	Lol Pop · Mirage            3.892      3.307        +17.4%        −0.3%
//	Lunar Snake · Blood Adder   3.96       3.465        +12.3%        −1.8%
//	Surge Board · Blåhaj        6.861      6.277        +10.8%        +1.4%
//	Timeless Book               4.346      4.158         +8.9%        +4.2%
//	Instant Ramen · Spicy Beef  3.95       3.762        +12.6%        +7.3%
//	Snoop Dogg · Groove Pup     5.119      5.049         +4.6%        +3.2%
//	Xmas Stocking · Frozen Moss 4.95       4.95         +58.4%      ~0 (median 2.97)
//
// The point of pinning these is that the failure was not visible as a crash or
// an obviously silly number. Each card was internally consistent and looked
// plausible; only the comparison against execution exposed it.

// goldenTicket is max_ticket as it stood when these cards were sent, so the
// size term ranks them the way the desk would actually have felt them.
const goldenTicket = 8.0

// goldenCase is one card from the log, with the market as it stood.
type goldenCase struct {
	name string
	// ask is the listed price; cost is derived with the Tonnel referral.
	ask float64
	// tonnelAsks is the competing queue, ours excluded, ascending.
	tonnelAsks []float64
	// crossAsks is the merged external queue; crossSupport is the robust
	// reference the venue layer derived from it. Empty means no venue answered.
	crossAsks    []float64
	crossSupport float64
	unreachable  int

	median   float64
	distinct int
	prints   int
	velocity float64
	floor    float64
	supply   int

	// wantExit is the price the desk could actually have got out at, and
	// wantPositive says whether the round trip made money at all.
	wantExit     float64
	wantPositive bool
	// wantCapReason, when set, must appear in the recorded cap explanation.
	wantCapReason string
}

func goldenCases() []goldenCase {
	return []goldenCase{
		{
			// Sold for 6.98 two hours after this card claimed 7.326. The 7.00 ask
			// standing in front is the whole story, and the engine had it.
			name: "Jolly Chimp · Tourist",
			ask:  6.56, tonnelAsks: []float64{7, 7.9, 8},
			crossAsks: []float64{7.39, 7.4, 7.428, 7.5, 8}, crossSupport: 7.4,
			median: 6.285, distinct: 10, prints: 19, velocity: .71,
			floor: 7, supply: 15,
			wantExit: 6.93, wantPositive: true,
			wantCapReason: "стакан",
		},
		{
			// Portals did not answer, so the local queue is the only bound — and
			// it says 5.10. Claimed +4.6%, worth +3.2%.
			name: "Snoop Dogg · Groove Pup",
			ask:  4.87, tonnelAsks: []float64{5.1, 5.2, 5.2}, unreachable: 1,
			median: 4.808, distinct: 8, prints: 11, velocity: .57,
			floor: 5, supply: 36,
			wantExit: 5.049, wantPositive: true,
		},
		{
			// The highest score the desk ever printed came from a book reading
			// 8 → 14.4 with one live price. Portals had five offers from 6.34,
			// and 6.34 is where our buyer goes.
			name: "Surge Board · Blåhaj",
			ask:  6.16, tonnelAsks: []float64{8, 14.4},
			crossAsks: []float64{6.34, 6.93, 7, 7.06, 7.06}, crossSupport: 6.93,
			median: 6.327, distinct: 12, prints: 16, velocity: .86,
			floor: 8, supply: 7,
			wantExit: 6.34 / 1.005 * (1 - testUndercut), wantPositive: true,
			wantCapReason: "площадк",
		},
		{
			// Claimed +12.3% on a 5.30 second ask above a hole, while Portals
			// opened at 3.50 — below our own entry. This one loses money.
			name: "Lunar Snake · Blood Adder",
			ask:  3.51, tonnelAsks: []float64{5.3, 5.65, 9},
			crossAsks: []float64{3.5, 4, 4, 4, 4}, crossSupport: 4,
			median: 2.995, distinct: 7, prints: 12, velocity: .5,
			floor: 3.51, supply: 13,
			wantExit: 3.5 / 1.005 * (1 - testUndercut), wantPositive: false,
			wantCapReason: "площадк",
		},
		{
			// Claimed +17.4% with the next ask 1.2% away. The card even printed
			// "слить сейчас 3.307" directly above the claim.
			name: "Lol Pop · Mirage",
			ask:  3.3, tonnelAsks: []float64{3.34, 3.7, 4.1}, unreachable: 1,
			median: 3.241, distinct: 7, prints: 10, velocity: .5,
			floor: 3.34, supply: 10,
			wantExit: 3.3066, wantPositive: false,
		},
		{
			name: "Timeless Book · Paleontology",
			ask:  3.97, tonnelAsks: []float64{5, 5, 5},
			crossAsks: []float64{4.2, 4.39, 4.4, 4.44, 4.5}, crossSupport: 4.39,
			median: 3.957, distinct: 7, prints: 9, velocity: .5,
			floor: 3.97, supply: 17,
			wantExit: 4.2 / 1.005 * (1 - testUndercut), wantPositive: true,
			wantCapReason: "площадк",
		},
		{
			name: "Instant Ramen · Spicy Beef",
			ask:  3.49, tonnelAsks: []float64{3.8, 3.99, 3.99},
			crossAsks: []float64{4.2, 4.49, 4.5, 4.5, 4.5}, crossSupport: 4.49,
			median: 3.108, distinct: 7, prints: 12, velocity: .5,
			floor: 3.49, supply: 16,
			wantExit: 3.762, wantPositive: true,
			wantCapReason: "стакан",
		},
		{
			// The queue said 5.00 and Portals said 6.00, but thirteen real trades
			// had a median of 2.97. A +58.4% claim off a 60% hole in the book is
			// the case the history clamp exists for.
			name: "Xmas Stocking · Frozen Moss",
			ask:  3.11, tonnelAsks: []float64{5, 5.2, 5.5},
			crossAsks: []float64{6}, crossSupport: 6,
			median: 2.972, distinct: 7, prints: 13, velocity: .5,
			floor: 3.11, supply: 22,
			wantExit: 2.972 * (1 + historyGapLimit), wantPositive: true,
			wantCapReason: "истории",
		},
	}
}

// goldenValuation rebuilds one card's market and prices it.
func (c goldenCase) valuation() Valuation {
	key := tonnel.ModelKey{Name: "Golden", Model: c.name}
	book := &Book{Key: key, FetchedAt: time.Now()}
	// Our own listing sits in the book alongside the competition, exactly as the
	// feed delivers it.
	book.Asks = append(book.Asks, Ask{GiftID: 42, Price: c.ask})
	for i, p := range c.tonnelAsks {
		book.Asks = append(book.Asks, Ask{GiftID: int64(1000 + i), Price: p})
	}

	liq := Liquidity{
		Prints: c.prints, Sales: c.distinct, DistinctGifts: c.distinct,
		Turnover: float64(c.distinct) / float64(c.prints),
		Median:   c.median, Median7: c.median, Trend: 1,
		Velocity: c.velocity, MADRatio: .03, LastSale: time.Now(),
	}

	v := Evaluate(Input{
		GiftID: 42, Key: key, Price: c.ask, Book: book, Liq: liq,
		Floor: c.floor, Supply: c.supply, Now: time.Now(),
		Params: Params{Fee: testFee, Undercut: testUndercut},
		// The desk's ticket limit on the day these cards were sent.
		TicketRef: goldenTicket,
	})
	if len(c.crossAsks) > 0 || c.unreachable > 0 {
		v = WithCrossDepth(v, CrossMarket{
			Support: c.crossSupport, Asks: c.crossAsks, Venues: 1,
			Unreachable: c.unreachable,
		})
	}
	return v
}

// TestGoldenExitsMatchWhatTheDeskCouldActuallyGet is the direct check on the
// fix: the fast exit has to be a price a buyer would take, not a blend that
// stands behind the queue.
func TestGoldenExitsMatchWhatTheDeskCouldActuallyGet(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			v := c.valuation()
			if !v.Valid {
				t.Fatalf("valuation invalid: %s", v.Reason)
			}
			if math.Abs(v.FastExit-c.wantExit) > 0.01 {
				t.Errorf("fast exit = %.4f, want %.4f (executable price this card could have got)",
					v.FastExit, c.wantExit)
			}
			if got := v.Edge > 0; got != c.wantPositive {
				t.Errorf("edge = %+.2f%% (positive=%v), want positive=%v — entry %.3f, exit %.3f",
					v.Edge*100, got, c.wantPositive, v.Cost, v.FastExit)
			}
			if c.wantCapReason != "" && !strings.Contains(v.ExitCapped, c.wantCapReason) {
				t.Errorf("cap reason = %q, want it to mention %q", v.ExitCapped, c.wantCapReason)
			}
			t.Logf("entry %.3f → exit %.3f (%+.1f%%) · score %.0f · %s",
				v.Cost, v.FastExit, v.Edge*100, v.ScoreBreakdown.Total, v.ExitCapped)
		})
	}
}

// TestGoldenExitNeverPricesThroughAStandingOffer is the invariant behind every
// case above, stated once: whatever the blend believes, we cannot sell quickly
// above an offer a buyer can take instead of ours.
func TestGoldenExitNeverPricesThroughAStandingOffer(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			v := c.valuation()
			best := math.Inf(1)
			if len(c.tonnelAsks) > 0 {
				best = c.tonnelAsks[0]
			}
			if len(c.crossAsks) > 0 {
				// An external ask costs its buyer the sticker price, while ours
				// costs them the Tonnel referral on top.
				best = math.Min(best, c.crossAsks[0]/(1+testFee))
			}
			if math.IsInf(best, 1) {
				t.Skip("no competing offer in this fixture")
			}
			if v.FastExit > best+1e-9 {
				t.Errorf("fast exit %.4f prices through a standing offer at %.4f", v.FastExit, best)
			}
			// The ladder must stay honest in the other direction too: a patient
			// ask that is cheaper than the fast one is not a slower option.
			if v.PatientAsk < v.FastExit-1e-9 {
				t.Errorf("patient %.4f below fast %.4f", v.PatientAsk, v.FastExit)
			}
			if v.Liquidation > v.FastExit+1e-9 {
				t.Errorf("liquidation %.4f above fast exit %.4f", v.Liquidation, v.FastExit)
			}
		})
	}
}

// TestGoldenScoreRanksTheOneTradeThatWorked is the check on the ranking rather
// than the pricing.
//
// Of these eight cards exactly one was taken, and it made money: Jolly Chimp,
// bought at 6.593 and sold at 6.98. The old score put it at 28 while giving 55
// ("отличный") to Surge Board, worth +0.9%, and 32 ("хороший") to Xmas
// Stocking, whose price the engine had to invent between a 60% hole in the book
// and a tape 40% below it. The score was not merely noisy, it was inverted —
// because a gappy book inflated the edge it was ranking.
func TestGoldenScoreRanksTheOneTradeThatWorked(t *testing.T) {
	scores := map[string]float64{}
	for _, c := range goldenCases() {
		scores[c.name] = c.valuation().ScoreBreakdown.Total
	}

	jolly := scores["Jolly Chimp · Tourist"]
	for _, worse := range []string{
		"Surge Board · Blåhaj",        // +0.9% behind a hole in the book
		"Xmas Stocking · Frozen Moss", // price invented between a hole and the tape
		"Snoop Dogg · Groove Pup",     // +3.2% with no venue answering
		"Lunar Snake · Blood Adder",   // loses money
		"Lol Pop · Mirage",            // loses money
	} {
		if scores[worse] >= jolly {
			t.Errorf("%s scores %.0f, at or above the one trade that actually worked (%.0f)",
				worse, scores[worse], jolly)
		}
	}
	// And the two that lose money must not be rankable at all.
	for _, dead := range []string{"Lunar Snake · Blood Adder", "Lol Pop · Mirage"} {
		if scores[dead] != 0 {
			t.Errorf("%s has a negative edge but scores %.0f", dead, scores[dead])
		}
	}
}

// TestGoldenFairValueStaysInsideTheExternalQueue pins the other half of the
// complaint: cards printed "фэйр 10.9" and "fair 6.85" for models whose whole
// external queue sat under 4. Discovery may disagree with the local book, but
// not with every venue at once.
func TestGoldenFairValueStaysInsideTheExternalQueue(t *testing.T) {
	for _, c := range goldenCases() {
		if len(c.crossAsks) < 2 {
			continue // one quote is not a queue, and cannot bound anything
		}
		t.Run(c.name, func(t *testing.T) {
			v := c.valuation()
			depth := c.crossAsks[minInt(depthWindow, len(c.crossAsks))-1] / (1 + testFee)
			if v.FairValue > depth+1e-9 {
				t.Errorf("fair %.4f prices through the external queue's third rung %.4f",
					v.FairValue, depth)
			}
			if v.PatientAsk > math.Max(depth, v.FastExit)+1e-9 {
				t.Errorf("patient %.4f prices through the external queue's third rung %.4f",
					v.PatientAsk, depth)
			}
		})
	}
}
