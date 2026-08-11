package app

import "testing"

func TestNumKeepsReferralPrecisionForSmallGRAMAmounts(t *testing.T) {
	if got := num(3.80895); got != "3.809" {
		t.Errorf("num(3.80895) = %q, want 3.809", got)
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
