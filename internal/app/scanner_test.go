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

// The limit and the price band are both the operator's. A report that always
// returns the same eight rows cannot answer "show me everything that clears the
// bar", and one bounded by the free balance cannot answer "what is standing
// between three and five" — which is a question about the market, asked while
// planning rather than while buying.
func TestScanArgTakesACollectionALimitAndABand(t *testing.T) {
	for _, tc := range []struct {
		in         string
		collection string
		limit      int
		lo, hi     float64
		ranged     bool
	}{
		{in: ""},
		{in: "25", limit: 25},
		{in: "Plush Pepe", collection: "Plush Pepe"},
		{in: "Plush Pepe 15", collection: "Plush Pepe", limit: 15},
		{in: "  Snake Box   7 ", collection: "Snake Box", limit: 7},
		{in: "3-5", lo: 3, hi: 5, ranged: true},
		{in: "3.5-5", lo: 3.5, hi: 5, ranged: true},
		{in: "3,5-5", lo: 3.5, hi: 5, ranged: true},
		{in: "-5", hi: 5, ranged: true},
		{in: "3-", lo: 3, ranged: true},
		{in: "5-3", lo: 3, hi: 5, ranged: true}, // a typo, not an empty set
		{in: "Plush Pepe 3-5 25", collection: "Plush Pepe", limit: 25, lo: 3, hi: 5, ranged: true},
		{in: "3-5 Plush Pepe", collection: "Plush Pepe", lo: 3, hi: 5, ranged: true},
		// A bare number stays the result limit — that is what /scan already
		// took, and changing its meaning would silently narrow old habits.
		{in: "Plush Pepe 5", collection: "Plush Pepe", limit: 5},
	} {
		got := scanArg(tc.in)
		if got.Collection != tc.collection || got.Limit != tc.limit ||
			got.Lo != tc.lo || got.Hi != tc.hi || got.Ranged != tc.ranged {
			t.Errorf("scanArg(%q) = %+v, want %q/%d/%v-%v ranged=%v",
				tc.in, got, tc.collection, tc.limit, tc.lo, tc.hi, tc.ranged)
		}
	}
}

// The band is what decides which listings a sweep even looks at.
func TestBudgetBandBoundsTheBook(t *testing.T) {
	band := budget{Lo: 3, Hi: 5, Explicit: true}
	for _, tc := range []struct {
		price float64
		allow bool
		above bool
	}{
		{2.9, false, false}, // under the band: skipped, but the book goes on
		{3, true, false},
		{4.5, true, false},
		{5, true, false},
		{5.01, false, true}, // past the top: the ascending book can stop here
	} {
		if got := band.allows(tc.price); got != tc.allow {
			t.Errorf("allows(%v) = %v", tc.price, got)
		}
		if got := band.above(tc.price); got != tc.above {
			t.Errorf("above(%v) = %v", tc.price, got)
		}
	}
	// An open-ended band still bounds the end it was given.
	up := budget{Lo: 3, Explicit: true}
	if !up.allows(9999) || up.above(9999) {
		t.Error("an open top rejected a high price")
	}
	if up.allows(2.9) {
		t.Error("an open top ignored its floor")
	}
	// And no band at all admits everything.
	var none budget
	if !none.allows(0.01) || none.above(1e9) {
		t.Error("an empty band filtered something")
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

// The affordability gate is about the wallet, not about the trade. When the
// operator has deliberately asked past the wallet — an explicit price band —
// repeating it under every line answers a question they did not ask.
func TestAnExplicitBandDropsTheAffordabilityGate(t *testing.T) {
	fails := []string{
		"бюджет: лот стоит 8.00, а поднять сейчас можно максимум 5.00 (тикет и свободный баланс)",
		"скорость 0.20/день ниже 0.50",
	}
	got := withoutBudgetFail(fails)
	if len(got) != 1 || got[0] != fails[1] {
		t.Fatalf("kept %v, want only the reasons about the trade", got)
	}
	// Everything else survives untouched, including an empty list.
	if len(withoutBudgetFail(nil)) != 0 {
		t.Fatal("an empty list grew")
	}
	if len(withoutBudgetFail([]string{fails[1]})) != 1 {
		t.Fatal("a trade reason was dropped")
	}
}

func TestBudgetBandRenders(t *testing.T) {
	for _, tc := range []struct {
		band budget
		want string
	}{
		{budget{Lo: 3, Hi: 5}, "3 – 5"},
		{budget{Hi: 5}, "до 5"},
		{budget{Lo: 3}, "от 3"},
		{budget{}, ""},
	} {
		if got := tc.band.String(); got != tc.want {
			t.Errorf("%+v renders as %q, want %q", tc.band, got, tc.want)
		}
	}
}
