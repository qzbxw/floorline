package app

import (
	"strings"
	"testing"

	"floorline/internal/pricing"
	"floorline/internal/tonnel"
)

// The sweep prices against the model-wide external queue — one venue read per
// model instead of one per listing, which is the only reason it can finish a
// pass at all. That approximation is fine for ranking and wrong for acting, so
// it has to be visible on the line.
func TestScanNoteAdmitsWhenThePriceIsOnlyModelWide(t *testing.T) {
	c := Candidate{
		Gift: tonnel.Gift{Backdrop: "Onyx Black", Symbol: "Star"},
		Val: pricing.Valuation{
			Backdrop: "Onyx Black", Symbol: "Star",
			CrossMarketSupport: 4.2, ExitFromCross: true, Walkaway: 4.18,
		},
	}
	note := scanMarketNote(c)
	if !strings.Contains(note, "по модели") {
		t.Errorf("note = %q, want it to say the price is not matched on traits", note)
	}

	c.Refined = true
	if note := scanMarketNote(c); strings.Contains(note, "по модели") {
		t.Errorf("refined candidate still claims a model-wide price: %q", note)
	}
}

func TestScanNoteLeadsWithAVenueThatDidNotAnswer(t *testing.T) {
	c := Candidate{Val: pricing.Valuation{
		CrossMarketSupport: 4.2,
		Cross:              pricing.CrossMarket{Unreachable: 1},
	}, Refined: true}
	note := scanMarketNote(c)
	if !strings.Contains(note, "не ответила") {
		t.Errorf("note = %q, want the unreachable venue named first", note)
	}
	if strings.Contains(note, "площадки 4.2") {
		t.Errorf("note = %q, must not quote a reference it could not fully read", note)
	}
}

func TestScanNoteCountsTheCompanyOurExitKeeps(t *testing.T) {
	c := Candidate{Val: pricing.Valuation{
		CrossMarketSupport: 4.2, CrossCompetitorsNear: 3,
	}, Refined: true}
	if note := scanMarketNote(c); !strings.Contains(note, "3 оффера") {
		t.Errorf("note = %q, want the external offers standing at our exit", note)
	}
}
