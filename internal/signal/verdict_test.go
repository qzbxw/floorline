package signal

import (
	"strings"
	"testing"

	"floorline/internal/pricing"
)

// solid is a valuation whose three layers all hold, so each test can break
// exactly one of them and see the verdict follow the weakest.
func solid() pricing.Valuation {
	return pricing.Valuation{
		Valid:      true,
		Confidence: .62,
		Fill: pricing.FillCurve{
			BuyersPerDay: 2, QueueAhead: 1,
			In24h: .59, In72h: .93, In7d: .99,
		},
		ScoreBreakdown: pricing.ScoreBreakdown{Total: 41},
	}
}

// Production: "✅ BUY · скор 3/100 · данные 3%". That is not a display problem,
// it is a broken sentence — BUY is read as "the model is confident", so a BUY
// resting on three percent confidence is the interface asserting something the
// engine does not believe.
func TestBuyIsNeverStrongerThanTheEvidenceUnderIt(t *testing.T) {
	v := solid()
	v.Confidence = .03

	verdict, layers := Grade(v, nil)
	if verdict == VerdictBuy {
		t.Fatal("3% confidence produced a BUY")
	}
	if verdict != VerdictSpeculative {
		t.Errorf("verdict = %q, want %q — the price is fine, the evidence is not", verdict, VerdictSpeculative)
	}
	blocking := Blocking(layers)
	if len(blocking) != 1 || blocking[0].Name != "данные" {
		t.Fatalf("blocking layers = %+v, want just the data layer", blocking)
	}
	if !strings.Contains(blocking[0].Note, "3%") {
		t.Errorf("note = %q, want it to quote the confidence it objected to", blocking[0].Note)
	}
}

// The headline and the number underneath it have to agree. A 3/100 BUY is the
// card contradicting itself in two adjacent lines.
func TestAThreeOutOfHundredCannotBeABuy(t *testing.T) {
	v := solid()
	v.ScoreBreakdown.Total = 3

	if verdict, _ := Grade(v, nil); verdict == VerdictBuy {
		t.Errorf("score 3 still produced %q", verdict)
	}
}

// Liquidity is its own layer: a real edge on something that cannot be sold
// inside a flip's horizon is a trade for a human, not for the machine.
func TestUnsellableEdgeIsManualNotBuy(t *testing.T) {
	v := solid()
	v.Fill.In72h = .18
	v.Fill.QueueAhead = 6

	verdict, layers := Grade(v, nil)
	if verdict != VerdictManual {
		t.Errorf("verdict = %q, want %q", verdict, VerdictManual)
	}
	if b := Blocking(layers); len(b) == 0 || b[0].Name != "ликвидность" {
		t.Fatalf("blocking = %+v, want the liquidity layer", b)
	}
}

// Price failing outranks everything: there is nothing to speculate on.
func TestNoEdgeIsAPassWhateverElseHolds(t *testing.T) {
	verdict, _ := Grade(solid(), []string{"реальный вход выше быстрого выхода"})
	if verdict != VerdictPass {
		t.Errorf("verdict = %q, want %q", verdict, VerdictPass)
	}
	if verdict.Actionable() {
		t.Error("a PASS must not be worth a notification")
	}
}

func TestAllThreeLayersHoldingIsTheOnlyBuy(t *testing.T) {
	verdict, layers := Grade(solid(), nil)
	if verdict != VerdictBuy {
		t.Fatalf("verdict = %q with every layer intact: %+v", verdict, layers)
	}
	if len(Blocking(layers)) != 0 {
		t.Errorf("a BUY must have nothing holding it down: %+v", Blocking(layers))
	}
	if verdict.Mark() != "🟢" {
		t.Errorf("mark = %q", verdict.Mark())
	}
}

// A venue we could not read is missing evidence, not absent objection — and the
// verdict has to reflect that even when every price gate is happy.
func TestAnUnreadVenueDowngradesTheVerdict(t *testing.T) {
	v := solid()
	v.Cross.Unreachable = 1

	if verdict, _ := Grade(v, nil); verdict != VerdictSpeculative {
		t.Errorf("verdict = %q, want %q", verdict, VerdictSpeculative)
	}
}
