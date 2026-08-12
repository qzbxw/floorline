package app

import (
	"strings"
	"testing"

	"floorline/internal/pricing"
)

func TestNumKeepsReferralPrecisionForSmallGRAMAmounts(t *testing.T) {
	if got := num(3.80895); got != "3.809" {
		t.Errorf("num(3.80895) = %q, want 3.809", got)
	}
}

func TestPassLineNamesTheEconomicReason(t *testing.T) {
	v := pricing.Valuation{
		Cost: 3.316, FastExit: 3.228,
		HasCompetingAsk: true, AskGap1: .01,
		CrossMarketSupport: 3.20,
	}
	got := passLine(v, []string{"скорость 0.43/день ниже 0.50"})
	for _, want := range []string{"PASS:", "реальный вход 3.316 выше быстрого выхода 3.228"} {
		if !strings.Contains(got, want) {
			t.Errorf("pass line %q does not contain %q", got, want)
		}
	}
	// Whatever actually rejected the listing has to survive into the text. The
	// old version dropped it whenever one of its own invented reasons fired.
	if !strings.Contains(got, "скорость 0.43/день ниже 0.50") {
		t.Errorf("pass line %q dropped the gate that actually failed", got)
	}
}

// Two of the reasons the block used to print corresponded to no gate at all,
// and printing them pushed the real failure out of the three-line budget.
func TestPassReasonsDoNotInventChecksThatNoGateMakes(t *testing.T) {
	v := pricing.Valuation{
		Cost: 3.0, FastExit: 3.4, // profitable: nothing economic to complain about
		HasCompetingAsk: true, AskGap1: .01, // a tight gap is not a rejection
		CrossMarketSupport: 2.9, // cheaper elsewhere is not a rejection either
	}
	reasons := passReasons(v, []string{"всего 3 сделок за 14д, нужно 6"})
	if len(reasons) != 1 || reasons[0] != "всего 3 сделок за 14д, нужно 6" {
		t.Errorf("reasons = %q, want only the gate that actually failed", reasons)
	}
}

func TestCapitalPressureOnlyAppearsBelowTheReserve(t *testing.T) {
	for _, tc := range []struct {
		balance float64
		known   bool
		reserve float64
		want    float64
	}{
		{30, true, 20, 0},
		{10, true, 20, .5},
		{0, false, 20, 0},
		{0, true, 0, 0},
	} {
		if got := capitalPressure(tc.balance, tc.known, tc.reserve); got != tc.want {
			t.Errorf("pressure(%v,%v,%v) = %v, want %v", tc.balance, tc.known, tc.reserve, got, tc.want)
		}
	}
}
