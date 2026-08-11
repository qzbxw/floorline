package pricing

import (
	"math"
	"sort"
	"time"

	"floorline/internal/store"
)

type RatePoint struct {
	TS  time.Time
	USD float64
}

type FXContext struct {
	CurrentUSD    float64
	Move15m       float64
	Move1h        float64
	ExpectedFloor float64
	FloorLag      float64 // actual/FX-adjusted expected - 1; negative means stale-cheap
	QuoteAt       time.Time
	Valid         bool
}

// NormalizeSalesForRate keeps FX as a mild reference, not as the owner of the
// gift price. Gifts trade in GRAM and collections do not instantly track USD,
// so a single historical print may move by at most five percent.
func NormalizeSalesForRate(sales []store.SaleRow, rates []RatePoint, currentUSD float64) ([]store.SaleRow, float64) {
	out := append([]store.SaleRow(nil), sales...)
	if currentUSD <= 0 || len(rates) == 0 || len(sales) == 0 {
		return out, 0
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i].TS.Before(rates[j].TS) })
	matched := 0
	for i := range out {
		idx := sort.Search(len(rates), func(j int) bool { return rates[j].TS.After(out[i].TS) }) - 1
		if idx < 0 || rates[idx].USD <= 0 || out[i].TS.Sub(rates[idx].TS) > 2*time.Hour {
			continue
		}
		factor := rates[idx].USD / currentUSD
		if factor < .95 {
			factor = .95
		} else if factor > 1.05 {
			factor = 1.05
		}
		out[i].Price *= factor
		matched++
	}
	return out, float64(matched) / float64(len(out))
}

func ComputeFXContext(now time.Time, currentFloor float64, current, at15, at1h RatePoint, floor1h float64) FXContext {
	f := FXContext{CurrentUSD: current.USD, QuoteAt: current.TS}
	if current.USD <= 0 || current.TS.IsZero() || now.Sub(current.TS) > 5*time.Minute {
		return f
	}
	f.Valid = true
	target15 := now.Add(-15 * time.Minute)
	target1h := now.Add(-time.Hour)
	valid15 := at15.USD > 0 && !at15.TS.IsZero() && absDuration(at15.TS.Sub(target15)) <= 10*time.Minute
	valid1h := at1h.USD > 0 && !at1h.TS.IsZero() && absDuration(at1h.TS.Sub(target1h)) <= 15*time.Minute
	if valid15 {
		f.Move15m = current.USD/at15.USD - 1
	}
	if valid1h {
		f.Move1h = current.USD/at1h.USD - 1
	}
	if currentFloor > 0 && floor1h > 0 && valid1h {
		factor := at1h.USD / current.USD
		if factor < .95 {
			factor = .95
		} else if factor > 1.05 {
			factor = 1.05
		}
		f.ExpectedFloor = floor1h * factor
		if f.ExpectedFloor > 0 {
			f.FloorLag = currentFloor/f.ExpectedFloor - 1
		}
	}
	if math.IsNaN(f.FloorLag) || math.IsInf(f.FloorLag, 0) {
		f.FloorLag = 0
	}
	return f
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
