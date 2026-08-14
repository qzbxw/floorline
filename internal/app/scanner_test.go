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

// The limit on a sweep is the operator's. A report that always returns the same
// eight rows out of a market of thousands cannot answer "show me everything
// that clears the bar".
func TestScanArgTakesACollectionALimitOrBoth(t *testing.T) {
	for _, tc := range []struct {
		in         string
		collection string
		limit      int
	}{
		{"", "", 0},
		{"25", "", 25},
		{"Plush Pepe", "Plush Pepe", 0},
		{"Plush Pepe 15", "Plush Pepe", 15},
		{"  Snake Box   7 ", "Snake Box", 7},
		{"Plush Pepe 0", "Plush Pepe 0", 0}, // not a limit, so not swallowed
		{"Plush Pepe -3", "Plush Pepe -3", 0},
	} {
		collection, limit := scanArg(tc.in)
		if collection != tc.collection || limit != tc.limit {
			t.Errorf("scanArg(%q) = %q/%d, want %q/%d", tc.in, collection, limit, tc.collection, tc.limit)
		}
	}
}

func TestRankCandidatesHonoursTheRequestedCut(t *testing.T) {
	in := make([]Candidate, 30)
	for i := range in {
		in[i].Score = float64(i)
	}
	if got := rankCandidates(in, 25); len(got) != 25 {
		t.Fatalf("kept %d of a requested 25", len(got))
	}
	if got := rankCandidates(in, 0); len(got) != scanKeep {
		t.Fatalf("kept %d with no request, want the default %d", len(got), scanKeep)
	}
	// Still ordered by conviction, whatever the cut.
	got := rankCandidates(in, 5)
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Fatal("candidates are not ranked best first")
		}
	}
}
