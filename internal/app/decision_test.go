package app

import (
	"context"
	"math"
	"strings"
	"testing"

	"floorline/internal/pricing"
	"floorline/internal/risk"
)

// Snake Box, from the 14 Aug book: +0.2% on the target, a one-in-a-hundred
// chance of selling inside a week, twelve sellers in front. The old engine
// called that ДЕРЖИМ because the target was above the entry. It is three GRAM
// doing nothing.
func TestDeadCapitalIsNotAHold(t *testing.T) {
	v := pricing.Valuation{
		Valid: true, Net: 0.017,
		Fill: pricing.FillCurve{In24h: 0, In72h: 0, In7d: .01, Cheaper: 12, BuyersPerDay: .3},
	}
	got := productivity(v, 3.09)
	if got >= deadCapital {
		t.Fatalf("productivity %.5f/day counts as live capital", got)
	}

	// Pool Float, same book: a real move with a high chance of landing.
	good := pricing.Valuation{
		Valid: true, Net: 0.633,
		Fill: pricing.FillCurve{In24h: .58, In72h: .92, In7d: 1, BuyersPerDay: 2},
	}
	if p := productivity(good, 3.819); p <= deadCapital {
		t.Fatalf("a position expected to clear +16%% in a day scores %.5f/day", p)
	}
}

// The horizon has to be the one the position is actually likely to sell in.
// Discounting a three-day trade over a week understates it; pretending a
// seven-day trade is a one-day one overstates it by the same factor.
func TestProductivityUsesTheHorizonTheFillImplies(t *testing.T) {
	quick := pricing.Valuation{Valid: true, Net: 1, Fill: pricing.FillCurve{In24h: .8, In72h: .9, In7d: 1}}
	slow := pricing.Valuation{Valid: true, Net: 1, Fill: pricing.FillCurve{In24h: .1, In72h: .2, In7d: .8}}
	pq, ps := productivity(quick, 10), productivity(slow, 10)
	if pq <= ps {
		t.Fatalf("a one-day fill (%.4f) does not beat a seven-day one (%.4f)", pq, ps)
	}
	// A trade that never fills earns nothing, whatever it would have paid.
	never := pricing.Valuation{Valid: true, Net: 5, Fill: pricing.FillCurve{}}
	if p := productivity(never, 10); p != 0 {
		t.Fatalf("a position that never sells scores %.4f", p)
	}
	if p := productivity(quick, 0); p != 0 {
		t.Fatalf("unknown cost basis produced a rate of %.4f", p)
	}
}

// A Poisson tail over a queue counted from a handful of offers will return
// exact zeroes and ones. Printing them as certainties is the interface claiming
// something the inputs cannot support.
func TestProbabilitiesDoNotClaimCertainty(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "<5%"}, {0.004, "<5%"}, {0.05, "<5%"},
		{0.34, "34%"}, {0.5, "50%"},
		{0.951, ">95%"}, {1, ">95%"},
	} {
		if got := prob(tc.in); got != tc.want {
			t.Errorf("prob(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// The book behind a verdict, in one line. Every recommendation rests on these
// numbers and none of them used to be on screen.
func TestDepthLineShowsTheQueueTheVerdictRestsOn(t *testing.T) {
	v := pricing.Valuation{
		Valid:    true,
		Walkaway: 4.559, WalkawayVenue: "площадки",
		Ladder: []float64{4.55, 4.7, 4.9, 5.2},
		Cross:  pricing.CrossMarket{Asks: []float64{4.6, 4.8}, BestBuyerCost: 4.6},
	}
	line := valuationDepthLine(v)
	for _, want := range []string{"4.559", "площадки", "4.55", "4.7", "4.9"} {
		if !strings.Contains(line, want) {
			t.Fatalf("depth line %q is missing %q", line, want)
		}
	}
	// Only three rungs: a queue nobody reads is no better than no queue.
	if strings.Contains(line, "5.2") {
		t.Fatalf("depth line %q printed the whole ladder", line)
	}
	if valuationDepthLine(pricing.Valuation{}) != "" {
		t.Fatal("an unpriced position produced a depth line")
	}
}

// The header used to print "+2.93%/день" beside "51 GRAM·дней в работе", and
// dividing the open book's own move by that denominator gave half the printed
// rate — because profit banked on closed cycles was hiding in the numerator.
// Each rate now stands over the days that produced it, so the arithmetic on
// screen can be checked.
func TestBookRatesDivideByTheDaysThatProducedThem(t *testing.T) {
	b := bookSummary{
		Invested: 24.255, Mark72h: 24.975,
		TonDays: 80, OpenTonDays: 50,
		Realised: 0.49,
	}
	if got, want := b.Unrealised(), 0.72; math.Abs(got-want) > 1e-9 {
		t.Fatalf("unrealised = %v, want %v", got, want)
	}
	if got, want := b.UnrealisedRate(), 0.72/50; math.Abs(got-want) > 1e-9 {
		t.Fatalf("open rate = %v, want the open move over the open days (%v)", got, want)
	}
	if got, want := b.RealisedRate(), 0.49/30; math.Abs(got-want) > 1e-9 {
		t.Fatalf("closed rate = %v, want the banked profit over the closed days (%v)", got, want)
	}
	// And the reader can now reproduce both from what is printed.
	if math.Abs(b.UnrealisedRate()*b.OpenTonDays-b.Unrealised()) > 1e-9 {
		t.Fatal("the open rate does not multiply back to the open move")
	}
}

// "поднять сейчас можно максимум 12.67 (тикет и свободный баланс)" against a
// balance of 16.7 reads like an accounting error. It is not — the reserve is 4
// — but two limits were named and neither number was shown, so there was no way
// to tell which one was biting.
func TestSpendableNamesTheLimitThatBinds(t *testing.T) {
	a := coolingApp(t)
	ctx := context.Background()
	rm, err := risk.New(ctx, a.st)
	if err != nil {
		t.Fatal(err)
	}
	a.rm = rm

	if err := rm.SetLimit(ctx, "max_ticket", "20"); err != nil {
		t.Fatal(err)
	}
	if err := rm.SetLimit(ctx, "min_balance_reserve", "4"); err != nil {
		t.Fatal(err)
	}
	rm.SetBalance(16.7)

	room, why, ok := a.spendableWhy()
	if !ok {
		t.Fatal("nothing known about what can be spent")
	}
	if math.Abs(room-12.7) > 1e-9 {
		t.Fatalf("room = %v, want the balance less the reserve", room)
	}
	for _, want := range []string{"16.7", "4"} {
		if !strings.Contains(why, want) {
			t.Fatalf("reason %q does not show %s", why, want)
		}
	}
	if strings.Contains(why, "сделку") {
		t.Fatalf("reason %q blames the ticket, which is not what binds here", why)
	}

	// And when the ticket really is the tighter one, it says so instead.
	rm.SetBalance(500)
	if _, why, _ = a.spendableWhy(); !strings.Contains(why, "сделку") {
		t.Fatalf("reason %q does not name the ticket limit", why)
	}
}
