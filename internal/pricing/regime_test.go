package pricing

import (
	"testing"
	"time"
)

// Production, 13 Aug. Vice Cream · Crystal, entry 3.156, was shown a Portals
// backdrop queue of 11 / 15 / 19 / 190 with "опора 15 · +375% к входу", and that
// 15 carried a fifth of the weight in price discovery. Pool Float · Giant Panda,
// entry 3.819, was shown 159.1 — the midpoint of a real 12.2 listing and a 306
// fantasy — and the card reported a 3082% divergence as though it were news
// about the market rather than about the data.
func TestNonsenseFromAVenueNeverReachesThePrice(t *testing.T) {
	book := bookOf(42, 3.156, 3.69, 3.8, 3.9)
	liq := liqOf(3.015, 8)

	v := evalPrice(t, 3.156, book, liq, 3.5, 26)
	v = WithCrossDepth(v, CrossMarket{Support: 15, Venues: 1, Asks: []float64{11, 15, 19, 190}})

	if !v.CrossImplausible {
		t.Fatal("a reference five times the executable market was accepted")
	}
	if v.CrossMarketSupport != 0 || v.CrossWeight != 0 {
		t.Errorf("support %.2f at weight %.2f — a rejected reference must carry none",
			v.CrossMarketSupport, v.CrossWeight)
	}
	// And having been rejected, it must not come back as a "disagreement" either.
	// The venues are not arguing with us; one of them is not describing this gift.
	if v.CrossDivergence > 0 {
		t.Errorf("divergence %.0f%% reported against a reference we threw away", v.CrossDivergence*100)
	}
	if v.FairValue > 5 {
		t.Errorf("fair value %.2f on a model whose live book starts at 3.69", v.FairValue)
	}
}

// The dangerous direction is the other one. A phantom ask *below* the market
// becomes the walkaway, and the exit is crushed against a price that does not
// exist — which is how a healthy position earns a "sell now" verdict.
func TestAPhantomCheapAskCannotCrushTheExit(t *testing.T) {
	book := bookOf(42, 3.27, 3.4, 3.45, 3.5)
	liq := liqOf(3.3, 20)

	v := evalPrice(t, 3.27, book, liq, 3.3, 20)
	v = WithCrossDepth(v, CrossMarket{Support: 3.4, Venues: 1, Asks: []float64{0.4, 3.4, 3.45}})

	if !v.CrossImplausible {
		t.Error("an ask at an eighth of the market was taken at face value")
	}
	if v.Walkaway < 3 {
		t.Errorf("walkaway %.3f came from the phantom rather than the market", v.Walkaway)
	}
	if v.FastExit < 3 {
		t.Errorf("fast exit %.3f was crushed against a price nobody is offering", v.FastExit)
	}
}

// Ordinary disagreement between venues must survive: the band exists to catch
// nonsense, not to throw away the corroboration the whole layer is for.
func TestOrdinaryVenueDisagreementIsStillBelieved(t *testing.T) {
	v := evalPrice(t, 3.0, bookOf(42, 3.0, 3.5, 3.55, 3.6), liqOf(3.4, 20), 3.5, 20)
	v = WithCrossDepth(v, CrossMarket{Support: 4.2, Venues: 2, Asks: []float64{4.2, 4.3, 4.4}})

	if v.CrossImplausible {
		t.Error("a venue 20% above the local book was thrown out; that is a real market difference")
	}
	if v.CrossMarketSupport != 4.2 {
		t.Errorf("support = %.2f, want it kept", v.CrossMarketSupport)
	}
}

func TestRegimeReadsTheRecentTapeAgainstTheWindow(t *testing.T) {
	falling := Liquidity{Sales: 12, Median: 4, Median7: 3.6, Trend: 0.9}
	calm := Liquidity{Sales: 12, Median: 4, Median7: 4.02, Trend: 1.005}
	rising := Liquidity{Sales: 12, Median: 4, Median7: 4.5, Trend: 1.125}
	thin := Liquidity{Sales: 2, Median: 4, Median7: 2, Trend: 0.5}

	for _, c := range []struct {
		name string
		liq  Liquidity
		want Regime
	}{
		{"falling", falling, RegimeFalling},
		{"calm", calm, RegimeNeutral},
		{"rising", rising, RegimeBull},
		{"too thin to call", thin, RegimeNeutral},
	} {
		if got := ReadRegime(c.liq); got != c.want {
			t.Errorf("%s: regime = %q, want %q", c.name, got, c.want)
		}
	}

	// In a falling market the older half of the window describes a level that is
	// gone, so the reference has to move towards the recent prints.
	ref := RegimeFalling.Reference(4, 3.6)
	if ref >= 4 || ref <= 3.6 {
		t.Errorf("falling reference = %.3f, want it pulled between 3.60 and 4.00", ref)
	}
	if RegimeNeutral.Reference(4, 3.6) != 4 {
		t.Error("a calm market must keep the longer window as its estimator")
	}
	if RegimeFalling.EdgeSurcharge() <= 0 {
		t.Error("a falling market has to cost extra edge")
	}
	if RegimeFalling.HoldLimit(4) >= 4 {
		t.Error("a falling market has to shorten the acceptable hold")
	}
}

// A discount is a gift when it is alone and the front of a slide when four other
// sellers walked in behind it. Both halves have to be present: fresh supply on
// its own is a busy model, a falling tape on its own is a cheap one.
func TestAdverseSelectionNeedsBothCrowdAndDirection(t *testing.T) {
	base := func() *Valuation {
		v := evalPrice(t, 3.0, bookOf(42, 3.0, 3.5, 3.55), liqOf(3.4, 20), 3.5, 20)
		return &v
	}

	quiet := base()
	quiet.Regime = RegimeFalling
	quiet.AssessCrowd(Crowd{Window: 15 * time.Minute})
	if quiet.AdverseSelection {
		t.Error("a falling model with nobody else listing is just a cheap model")
	}

	busy := base()
	busy.Regime = RegimeNeutral
	busy.AssessCrowd(Crowd{Window: 15 * time.Minute, Arrivals: 4, Cheapest: 3.4})
	if busy.AdverseSelection {
		t.Error("four arrivals all priced above us is a busy model, not a queue for the exit")
	}

	running := base()
	running.Regime = RegimeFalling
	running.AssessCrowd(Crowd{Window: 15 * time.Minute, Arrivals: 4, AtOrBelow: 3, Cheapest: 2.8})
	if !running.AdverseSelection {
		t.Fatal("three sellers undercutting us in fifteen minutes on a falling tape is the case this exists for")
	}
	if running.AdverseReason == "" {
		t.Error("the refusal has to say what it saw")
	}
}
