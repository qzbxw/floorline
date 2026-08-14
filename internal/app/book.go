package app

import (
	"math"
	"time"

	"floorline/internal/store"
)

// The portfolio is not a list of independent trades, and reporting it as one is
// how nine positions appear while every individual decision looked reasonable.
// What the desk needs from the book as a whole is different from what it needs
// from any line in it: how much of the money could actually come back this week,
// how much is stuck, how much is riding on one collection, and whether the
// capital is earning its keep per day rather than per trade.

// stuckAfter is how long a flip has to sit before its capital counts as stuck
// rather than working. Three days is the horizon the desk trades on; past it the
// position is no longer the trade that was entered.
const stuckAfter = 72 * time.Hour

// bookSummary is the whole portfolio measured at once.
type bookSummary struct {
	// Invested is what the positions cost.
	Invested float64
	// Mark72h is what the book would fetch if every position were priced to be
	// gone inside three days. It is deliberately not fair value: a portfolio
	// marked at what its contents are theoretically worth is a number that cannot
	// be turned into money, and printing it next to "вложено" invites reading the
	// difference as profit.
	Mark72h float64
	// MarkFast is the same measure at the emergency horizon — everything sold
	// now, undercutting the market. It is the floor under the book.
	MarkFast float64
	// Cash is the free balance, kept separate from the gifts because they are not
	// the same asset and do not carry the same risk.
	Cash      float64
	CashKnown bool

	// TonDays is capital multiplied by the days it has been committed. It is the
	// denominator that turns a percentage into a rate.
	TonDays float64
	// OpenTonDays is the part of it belonging to positions still held, so the
	// open book's progress can be divided by the days that produced it rather
	// than by every day the desk has ever traded.
	OpenTonDays float64
	// Stuck and StuckCapital count the positions past the flip horizon.
	Stuck        int
	StuckCapital float64

	// Realised is profit already booked on closed cycles, supplied by the caller.
	Realised float64

	Collections map[string]float64
	Models      map[string]float64
}

// Return is the unrealised move on the book at the three-day horizon.
func (b bookSummary) Return() float64 {
	if b.Invested <= 0 {
		return 0
	}
	return b.Mark72h/b.Invested - 1
}

// CapitalEfficiency is profit per GRAM per day held.
//
// This is the metric a flipper is actually optimising and the one plain ROI
// hides: +15% over twenty days is a worse use of the same money than +5% over
// two, and only a denominator with time in it can say so. Unrealised progress
// counts, because capital committed to a position that has moved in our favour
// is doing its job whether or not it has been sold yet.
func (b bookSummary) CapitalEfficiency() float64 {
	if b.TonDays <= 0 {
		return 0
	}
	return (b.Mark72h - b.Invested + b.Realised) / b.TonDays
}

// Unrealised and UnrealisedRate split the open book out of that number.
//
// One figure over one denominator was not readable. The header printed
// "+2.93%/день" beside "51 GRAM·дней в работе", and dividing the open book's
// own progress by that denominator gives 1.4% — a reader who checks the
// arithmetic on screen finds it does not work, because the numerator silently
// included profit already banked on positions that closed days ago. Both facts
// are worth having; mixed into one ratio, neither can be verified.
func (b bookSummary) Unrealised() float64 { return b.Mark72h - b.Invested }

func (b bookSummary) UnrealisedRate() float64 {
	if b.OpenTonDays <= 0 {
		return 0
	}
	return b.Unrealised() / b.OpenTonDays
}

// RealisedRate is what the closed cycles actually returned per GRAM per day.
func (b bookSummary) RealisedRate() float64 {
	closed := b.TonDays - b.OpenTonDays
	if closed <= 0 {
		return 0
	}
	return b.Realised / closed
}

// summarise measures the whole book from the per-position advice.
func summarise(ads []positionAdvice, now time.Time) bookSummary {
	b := bookSummary{
		Collections: map[string]float64{},
		Models:      map[string]float64{},
	}
	for _, ad := range ads {
		p := ad.Position
		b.Invested += p.BuyPrice

		mark, fast := positionMark(ad)
		b.Mark72h += mark
		b.MarkFast += fast
		b.Collections[p.Key.Name] += mark
		b.Models[p.Key.ID()] += mark

		if held := heldFor(p, now); held > 0 {
			b.TonDays += p.BuyPrice * held.Hours() / 24
			b.OpenTonDays += p.BuyPrice * held.Hours() / 24
			if held >= stuckAfter {
				b.Stuck++
				b.StuckCapital += p.BuyPrice
			}
		}
	}
	return b
}

// positionMark is what one position is worth to the book at the three-day
// horizon, and at the emergency one.
//
// The fallbacks matter as much as the primary: a position we could not price is
// marked at what we are asking for it, and failing that at what it cost. Marking
// it at zero would make an unreachable model look like a loss, and marking it at
// fair value would make the book look like money we cannot get.
func positionMark(ad positionAdvice) (mark, fast float64) {
	p := ad.Position
	fallback := p.ListPrice
	if fallback <= 0 {
		fallback = p.BuyPrice
	}
	if !ad.Val.Valid {
		return fallback, fallback
	}
	mark = ad.Val.ExitIn72h
	if mark <= 0 {
		mark = ad.Val.FastExit
	}
	if mark <= 0 {
		mark = fallback
	}
	fast = ad.Val.Liquidation
	if fast <= 0 {
		fast = math.Min(mark, fallback)
	}
	return mark, fast
}

// heldFor is how long a position has been carried, guarding the rows whose
// acquisition time was never established.
func heldFor(p store.Position, now time.Time) time.Duration {
	if p.BoughtAt.IsZero() {
		return 0
	}
	held := now.Sub(p.BoughtAt)
	if held < 0 {
		return 0
	}
	return held
}
