package app

import (
	"strings"
	"testing"

	"floorline/internal/pricing"
	"floorline/internal/store"
)

// The book used to be sorted by score, which is a candidate-ranking number: it
// is zero for every position whose fast exit sits under its entry, and that is
// most of a portfolio most of the time. So the position bleeding money could
// land at the bottom of eight identical zeros.
func TestPortfolioLeadsWithWhatNeedsADecision(t *testing.T) {
	ads := []positionAdvice{
		{Position: store.Position{GiftID: 1, BuyPrice: 3}, Action: actHold},
		{Position: store.Position{GiftID: 2, BuyPrice: 4}, Action: actRelist},
		{Position: store.Position{GiftID: 3, BuyPrice: 5}, Action: actExit},
		{Position: store.Position{GiftID: 4, BuyPrice: 40}, Action: actRelist},
		{Position: store.Position{GiftID: 5, BuyPrice: 9}, Action: actReview},
	}
	sortByUrgency(ads)

	var order []int64
	for _, ad := range ads {
		order = append(order, ad.Position.GiftID)
	}
	want := []int64{3, 4, 2, 5, 1}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v — exits first, then the biggest relist, holds last", order, want)
		}
	}
}

// A position can be the cheapest lot on Tonnel and still be the fourth-cheapest
// offer a buyer sees. The comparison is made in buyer terms: an external sticker
// is restated as the Tonnel ask that costs the same, because otherwise an offer
// counts as cheaper purely through who charges the referral.
func TestCheaperElsewhereIsCountedInBuyerTerms(t *testing.T) {
	const fee = 0.005
	v := pricing.Valuation{Cross: pricing.CrossMarket{Asks: []float64{
		3.90,        // clearly under our 4.00 ask
		3.98,        // under it once the referral is taken off ours
		4.00 * 1.01, // dearer than us either way
	}}}

	if n := countCheaperElsewhere(v, 4.00, fee); n != 2 {
		t.Errorf("cheaper elsewhere = %d, want 2", n)
	}
	if n := countCheaperElsewhere(v, 0, fee); n != 0 {
		t.Errorf("with no ask of our own the count is meaningless, got %d", n)
	}
}

func TestPositionMarketNoteNamesTheOtherVenues(t *testing.T) {
	quiet := positionAdvice{CrossReference: 4.1, CrossDivergence: .02}
	if note := quiet.marketNote(); note != "" {
		t.Errorf("nothing to report, yet the note reads %q", note)
	}

	beaten := positionAdvice{CheaperElsewhere: 2, WalkawayVenue: "площадки", CrossReference: 3.8, CrossDivergence: .2}
	note := beaten.marketNote()
	if !strings.Contains(note, "2 оффера") {
		t.Errorf("note = %q, want it to count the cheaper offers", note)
	}
	if !strings.Contains(note, "расхождение") {
		t.Errorf("note = %q, want it to keep the divergence warning", note)
	}
}
