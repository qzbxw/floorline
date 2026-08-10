package pricing

import (
	"math"
	"testing"
	"time"

	"floorline/internal/store"
)

func salesAt(now time.Time, entries ...struct {
	agoDays float64
	price   float64
	seller  int64
}) []store.SaleRow {
	out := make([]store.SaleRow, 0, len(entries))
	for i, e := range entries {
		out = append(out, store.SaleRow{
			GiftID: int64(i + 1),
			TS:     now.Add(-time.Duration(e.agoDays * float64(24*time.Hour))),
			Price:  e.price,
			Seller: e.seller,
			Buyer:  e.seller + 500,
		})
	}
	return out
}

type entry = struct {
	agoDays float64
	price   float64
	seller  int64
}

func TestComputeLiquidityBasics(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour

	sales := salesAt(now,
		entry{1, 100, 1}, entry{2, 110, 2}, entry{3, 90, 3},
		entry{8, 105, 1}, entry{10, 95, 4}, entry{12, 100, 5},
	)

	l := ComputeLiquidity(sales, now, window, window)

	if l.Sales != 6 {
		t.Errorf("sales = %d, want 6", l.Sales)
	}
	if l.Sales7 != 3 {
		t.Errorf("sales in the last 7 days = %d, want 3", l.Sales7)
	}
	if l.Sellers != 5 {
		t.Errorf("distinct sellers = %d, want 5", l.Sellers)
	}
	if l.Median != 100 {
		t.Errorf("median = %v, want 100", l.Median)
	}
	if math.Abs(l.Velocity-6.0/14) > 1e-9 {
		t.Errorf("velocity = %v, want %v", l.Velocity, 6.0/14)
	}
	if !l.Traded() {
		t.Error("a model with six trades must count as traded")
	}
}

func TestComputeLiquidityIgnoresTradesOutsideTheWindow(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour

	sales := salesAt(now, entry{1, 100, 1}, entry{30, 5000, 2})
	l := ComputeLiquidity(sales, now, window, window)

	if l.Sales != 1 {
		t.Errorf("sales = %d, want 1: the 30-day-old trade is outside the window", l.Sales)
	}
	if l.Median != 100 {
		t.Errorf("median = %v, want 100", l.Median)
	}
}

// While the database is still filling up, dividing by the nominal window would
// understate velocity for every model and make a healthy market look dead.
func TestVelocityUsesActualCoverageDuringWarmUp(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour
	sales := salesAt(now, entry{0.1, 100, 1}, entry{0.5, 100, 2}, entry{0.9, 100, 3}, entry{1.5, 100, 4})

	full := ComputeLiquidity(sales, now, window, window)
	partial := ComputeLiquidity(sales, now, window, 2*24*time.Hour)

	if math.Abs(full.Velocity-4.0/14) > 1e-9 {
		t.Errorf("velocity over a full window = %v, want %v", full.Velocity, 4.0/14)
	}
	if math.Abs(partial.Velocity-4.0/2) > 1e-9 {
		t.Errorf("velocity over two days of coverage = %v, want 2.0", partial.Velocity)
	}
	if partial.Velocity <= full.Velocity {
		t.Error("partial coverage must not understate velocity")
	}
}

// A single print on a fresh database must not read as an enormous trade rate.
func TestVelocityDenominatorIsAtLeastOneDay(t *testing.T) {
	now := time.Now()
	sales := salesAt(now, entry{0.01, 100, 1})

	l := ComputeLiquidity(sales, now, 14*24*time.Hour, 10*time.Minute)
	if l.Velocity != 1 {
		t.Errorf("velocity = %v, want 1 (one trade, denominator floored at one day)", l.Velocity)
	}
}

func TestDispersionAndTrend(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour

	steady := ComputeLiquidity(salesAt(now,
		entry{1, 100, 1}, entry{2, 100, 2}, entry{3, 100, 3}, entry{9, 100, 4},
	), now, window, window)
	if steady.MADRatio != 0 {
		t.Errorf("dispersion of identical prices = %v, want 0", steady.MADRatio)
	}
	if steady.Trend != 1 {
		t.Errorf("trend of a flat market = %v, want 1", steady.Trend)
	}

	falling := ComputeLiquidity(salesAt(now,
		entry{1, 80, 1}, entry{2, 80, 2}, entry{3, 80, 3},
		entry{10, 120, 4}, entry{11, 120, 5}, entry{12, 120, 6},
	), now, window, window)
	if falling.Trend >= 1 {
		t.Errorf("trend of a falling market = %v, want below 1", falling.Trend)
	}
	if falling.MADRatio == 0 {
		t.Error("a market that halved should show non-zero dispersion")
	}
}

// Two prints are not a trend, so the 7-day median must not swing the verdict.
func TestTrendNeutralWithTooFewRecentTrades(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour

	l := ComputeLiquidity(salesAt(now,
		entry{1, 50, 1}, entry{2, 50, 2},
		entry{10, 100, 3}, entry{11, 100, 4}, entry{12, 100, 5},
	), now, window, window)

	if l.Trend != 1 {
		t.Errorf("trend = %v, want a neutral 1 with only two recent trades", l.Trend)
	}
}

func TestEmptyHistory(t *testing.T) {
	l := ComputeLiquidity(nil, time.Now(), 14*24*time.Hour, 14*24*time.Hour)
	if l.Traded() {
		t.Error("an empty history must not count as traded")
	}
	if l.Velocity != 0 || l.Median != 0 {
		t.Errorf("empty history gave velocity %v median %v, want zeros", l.Velocity, l.Median)
	}
}

func TestMedianEvenAndOdd(t *testing.T) {
	if got := median([]float64{3, 1, 2}); got != 2 {
		t.Errorf("median of odd set = %v, want 2", got)
	}
	if got := median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("median of even set = %v, want 2.5", got)
	}
	if got := median(nil); got != 0 {
		t.Errorf("median of nothing = %v, want 0", got)
	}
}
