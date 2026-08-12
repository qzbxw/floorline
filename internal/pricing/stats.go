// Package pricing turns raw order-book and trade data into a tradeable opinion.
//
// The central idea is that a collection floor is noise. The tradable unit is
// (collection, model), and the only honest evidence of what one is worth is the
// price other people actually paid for it recently.
package pricing

import (
	"math"
	"sort"
	"time"

	"floorline/internal/store"
)

// Liquidity summarises how a model trades over the lookback window.
type Liquidity struct {
	Window   time.Duration // the window the caller asked for
	Coverage time.Duration // how much history we actually hold

	// Prints is every trade in the window, repeats included. It is the raw tape.
	Prints int
	// Sales is the deduplicated count: one trade per physical gift. Every
	// statistic that a repeated flip could inflate — velocity, median, MAD — is
	// built on this, not on Prints.
	Sales  int
	Sales7 int // deduplicated trades in the most recent 7 days
	// DistinctGifts is how many different physical gifts those trades involved.
	// The endpoint carries no counterparty identities, so this is the available
	// wash-trading signal: forty "trades" of the same gift_num is one item being
	// passed around, not a market.
	DistinctGifts int
	// Turnover is DistinctGifts / Prints. Near 1 means genuinely different items
	// changing hands; near 0 means the same one going in circles. It must be
	// measured against the raw tape: comparing distinct gifts to the already
	// deduplicated count would make it identically 1 and the guard inert.
	Turnover float64

	Velocity   float64 // trades per day, normalised to the history we really have
	Median     float64 // median price over the window
	RawMedian  float64 // unadjusted historical GRAM median
	FXCoverage float64 // share of sales normalized with a nearby GRAM/USD quote
	Median7    float64 // median price over the last 7 days
	MAD        float64 // median absolute deviation
	MADRatio   float64 // MAD / Median — dispersion, i.e. how quotable this model is
	Trend      float64 // Median7 / Median; < 1 means the model is bleeding

	LastSale time.Time
}

// Traded reports whether there is any usable trade evidence at all.
func (l Liquidity) Traded() bool { return l.Sales > 0 && l.Median > 0 }

// ComputeLiquidity derives the model statistics from stored trades.
//
// coverage is how far back the local history actually reaches. During warm-up
// that is less than the full window, and dividing by the nominal window would
// understate velocity for every model — making the bot look at a healthy market
// and see a dead one.
func ComputeLiquidity(sales []store.SaleRow, now time.Time, window, coverage time.Duration) Liquidity {
	l := Liquidity{Window: window, Coverage: coverage, Trend: 1}

	cut := now.Add(-window)
	cut7 := now.Add(-7 * 24 * time.Hour)

	// A physical gift may be flipped repeatedly. Treating every hop as an
	// independent print lets one item manufacture velocity and move the median.
	// Keep only its newest sale in the window; gift_num is the stable physical
	// identity, with gift_id as a conservative fallback for older rows.
	latest := make(map[int64]store.SaleRow, len(sales))

	for _, s := range sales {
		if s.Price <= 0 || s.TS.Before(cut) {
			continue
		}
		physicalID := s.GiftNum
		if physicalID == 0 {
			physicalID = s.GiftID
		}
		if physicalID == 0 {
			continue // unverifiable identity must not support unattended trading
		}
		l.Prints++
		if prev, ok := latest[physicalID]; !ok || s.TS.After(prev.TS) {
			latest[physicalID] = s
		}
	}

	prices := make([]float64, 0, len(latest))
	prices7 := make([]float64, 0, len(latest))
	for _, s := range latest {
		prices = append(prices, s.Price)
		if s.TS.After(l.LastSale) {
			l.LastSale = s.TS
		}
		if !s.TS.Before(cut7) {
			prices7 = append(prices7, s.Price)
		}
	}

	l.Sales = len(prices)
	l.Sales7 = len(prices7)
	l.DistinctGifts = len(latest)
	if l.Sales == 0 {
		return l
	}
	l.Turnover = float64(l.DistinctGifts) / float64(l.Prints)

	l.Median = median(prices)
	l.MAD = medianAbsDev(prices, l.Median)
	if l.Median > 0 {
		l.MADRatio = l.MAD / l.Median
	}

	// Seven days is only a meaningful trend signal with a few prints in it.
	if len(prices7) >= 3 {
		l.Median7 = median(prices7)
		if l.Median > 0 {
			l.Trend = l.Median7 / l.Median
		}
	} else {
		l.Median7 = l.Median
	}

	l.Velocity = float64(l.Sales) / effectiveDays(window, coverage)
	return l
}

// effectiveDays is the denominator for velocity: the window, shortened to the
// history we hold, and never below one day so a single print on a fresh
// database cannot read as a huge trade rate.
func effectiveDays(window, coverage time.Duration) float64 {
	d := window
	if coverage > 0 && coverage < window {
		d = coverage
	}
	days := d.Hours() / 24
	return math.Max(days, 1)
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func medianAbsDev(xs []float64, med float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	dev := make([]float64, len(xs))
	for i, x := range xs {
		dev[i] = math.Abs(x - med)
	}
	return median(dev)
}
