package pricing

import (
	"math"
	"testing"
	"time"

	"floorline/internal/store"
)

func TestNormalizeSalesForRate(t *testing.T) {
	now := time.Now()
	sales := []store.SaleRow{{TS: now.Add(-time.Hour), Price: 10}, {TS: now.Add(-48 * time.Hour), Price: 10}}
	rates := []RatePoint{{TS: now.Add(-2 * time.Hour), USD: 2}}
	got, cov := NormalizeSalesForRate(sales, rates, 1)
	if math.Abs(got[0].Price-10.5) > .001 || got[1].Price != 10 || cov != .5 {
		t.Fatalf("got=%+v coverage=%v", got, cov)
	}
}
func TestFXFloorLag(t *testing.T) {
	now := time.Now()
	f := ComputeFXContext(now, 90, RatePoint{TS: now, USD: 1}, RatePoint{TS: now.Add(-15 * time.Minute), USD: 1.1}, RatePoint{TS: now.Add(-time.Hour), USD: 2}, 100)
	if f.ExpectedFloor != 105 || math.Abs(f.FloorLag-(90.0/105-1)) > .001 {
		t.Fatalf("fx=%+v", f)
	}
}

func TestFXContextRejectsStaleReferencePoints(t *testing.T) {
	now := time.Now()
	f := ComputeFXContext(now, 90, RatePoint{TS: now, USD: 1}, RatePoint{TS: now.Add(-2 * time.Hour), USD: 2}, RatePoint{TS: now.Add(-3 * time.Hour), USD: 2}, 100)
	if f.Move15m != 0 || f.ExpectedFloor != 0 {
		t.Fatalf("stale points affected context: %+v", f)
	}
}
