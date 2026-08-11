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
	got := passLine(v, []string{"some detailed gate"})
	for _, want := range []string{"PASS:", "реальный вход 3.316 выше быстрого выхода 3.228", "гэпа нет", "площадки эджа не дают"} {
		if !strings.Contains(got, want) {
			t.Errorf("pass line %q does not contain %q", got, want)
		}
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
