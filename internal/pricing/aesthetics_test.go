package pricing

import (
	"strings"
	"testing"
	"time"

	"floorline/internal/tonnel"
)

func TestAppraiseReadsTheLookTheFloorCannotSee(t *testing.T) {
	cases := []struct {
		name             string
		backdrop, symbol string
		giftNum          int64
		wantPremium      bool
		wantReason       string
	}{
		{"onyx backdrop", "Onyx Black", "Cricket Helmet", 123456, true, "тёмный фон"},
		{"plain backdrop", "Mustard", "Watching Sun", 367512, false, ""},
		{"mono cool", "Azure Blue", "Sapphire Drop", 174083, true, "моно"},
		{"mono is not any two colours", "Azure Blue", "Tomato", 174083, false, ""},
		{"low number", "Mustard", "Watching Sun", 42, true, "двузначный"},
		{"repdigit", "Mustard", "Watching Sun", 7777, true, "повтор"},
		{"round number", "Mustard", "Watching Sun", 10000, true, "круглый"},
		{"ordinary number", "Mustard", "Watching Sun", 342471, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Appraise(c.backdrop, c.symbol, c.giftNum)
			if a.Premium != c.wantPremium {
				t.Errorf("premium = %v, want %v (reasons: %v)", a.Premium, c.wantPremium, a.Reasons)
			}
			if c.wantReason != "" && !strings.Contains(strings.Join(a.Reasons, " "), c.wantReason) {
				t.Errorf("reasons %v do not mention %q", a.Reasons, c.wantReason)
			}
		})
	}
}

// Appearance is allowed to say "do not compare this to the plain ones". It is
// never allowed to say "so it is worth more" — only measured sales may move a
// price, and this test is what keeps the dictionary from leaking into one.
func TestAppearanceNeverMovesThePriceByItself(t *testing.T) {
	key := tonnel.ModelKey{Name: "Pet Snake", Model: "Black Mamba"}
	book := bookOf(42, 4.0, 4.5, 4.6, 4.7)
	liq := liqOf(4.4, 20)

	plain := Evaluate(Input{
		GiftID: 42, GiftNum: 342471, Key: key, Price: 4.0, Book: book, Liq: liq,
		Backdrop: "Mustard", Symbol: "Watching Sun",
		Params: Params{Fee: testFee, Undercut: testUndercut},
	})
	fancy := Evaluate(Input{
		GiftID: 42, GiftNum: 777, Key: key, Price: 4.0, Book: book, Liq: liq,
		Backdrop: "Onyx Black", Symbol: "Obsidian Shard",
		Params: Params{Fee: testFee, Undercut: testUndercut},
	})

	if !fancy.Appearance.Premium || plain.Appearance.Premium {
		t.Fatalf("fixture is wrong: plain %v fancy %v", plain.Appearance, fancy.Appearance)
	}
	if fancy.FastExit != plain.FastExit || fancy.FairValue != plain.FairValue {
		t.Errorf("appearance moved the price on its own: plain %.4f/%.4f fancy %.4f/%.4f",
			plain.FastExit, plain.FairValue, fancy.FastExit, fancy.FairValue)
	}
}

// Production complaint, 13 Aug: an Onyx gift was priced against the Portals
// queue for the ordinary model. Once the exit began keying on the cheapest
// external offer that stopped being a cosmetic problem — the ordinary queue
// became the number the trade was decided by, so the specimen with the valuable
// backdrop looked like it had no edge at all.
//
// The engine still refuses to invent a premium. What it must not do is present
// the ordinary-queue number as if it were an appraisal of this gift.
func TestPremiumSpecimenIsNotSilentlyPricedOffOrdinaryOnes(t *testing.T) {
	key := tonnel.ModelKey{Name: "Pet Snake", Model: "Black Mamba"}
	// The local book is gappy, so the external queue is what caps the exit.
	book := bookOf(42, 4.0, 9.0, 9.5)
	liq := liqOf(4.4, 20)
	in := Input{
		GiftID: 42, GiftNum: 342471, Key: key, Price: 4.0, Book: book, Liq: liq,
		Backdrop: "Onyx Black", Symbol: "Cricket Helmet",
		Params: Params{Fee: testFee, Undercut: testUndercut},
	}
	// The ordinary specimens of this model, five of them, well below the gift.
	ordinary := CrossMarket{Support: 4.6, Venues: 1, Asks: []float64{4.5, 4.6, 4.7, 4.8, 4.9}}

	loose := WithCrossDepth(Evaluate(in), ordinary)
	if !loose.ExitFromCross {
		t.Fatalf("fixture is wrong: the external queue must be what caps this exit")
	}
	if !loose.AppearanceUnpriced {
		t.Error("an Onyx specimen capped by the ordinary model queue was not flagged as unpriced")
	}

	// The same queue, matched on the backdrop, is a real comparable — so the
	// number stands on its own and no warning is needed.
	matched := ordinary
	matched.Comparable = true
	tight := WithCrossDepth(Evaluate(in), matched)
	if tight.AppearanceUnpriced {
		t.Error("a backdrop-matched queue is a genuine comparable and must not be flagged")
	}
	// And in neither case does the flag change the price: it is a statement
	// about what we know, not a licence to charge more.
	if tight.FastExit != loose.FastExit {
		t.Errorf("the comparability flag moved the exit: %.4f vs %.4f", tight.FastExit, loose.FastExit)
	}
}

// The ladder in market.QuotesForGift falls exact → backdrop → model. This pins
// the half of it that lives in pricing: a model-scope quote is still usable
// evidence, so a plain gift keeps its cap and the Surge Board class of error
// stays caught.
func TestOrdinaryGiftIsStillCappedByTheModelQueue(t *testing.T) {
	key := tonnel.ModelKey{Name: "Surge Board", Model: "Blåhaj"}
	v := Evaluate(Input{
		GiftID: 42, GiftNum: 19781, Key: key, Price: 6.16,
		Book: bookOf(42, 6.16, 8, 14.4), Liq: liqOf(6.327, 12),
		Backdrop: "Camo Green", Symbol: "Watermelon", Now: time.Now(),
		Params: Params{Fee: testFee, Undercut: testUndercut},
	})
	v = WithCrossDepth(v, CrossMarket{
		Support: 6.93, Venues: 1, Asks: []float64{6.34, 6.93, 7, 7.06, 7.06},
	})
	if v.Appearance.Premium {
		t.Fatalf("fixture is wrong: Camo Green / Watermelon is an ordinary specimen: %v", v.Appearance.Reasons)
	}
	if v.AppearanceUnpriced {
		t.Error("an ordinary gift must not claim its trait comparables are missing")
	}
	if want := 6.34 / (1 + testFee) * (1 - testUndercut); v.FastExit > want+1e-9 {
		t.Errorf("exit %.4f priced through the external queue at %.4f", v.FastExit, want)
	}
}
