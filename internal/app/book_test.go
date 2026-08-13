package app

import (
	"testing"
	"time"

	"floorline/internal/pricing"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

func heldPosition(id int64, entry float64, age time.Duration, exit72 float64) positionAdvice {
	return positionAdvice{
		Position: store.Position{
			GiftID: id, BuyPrice: entry, ListPrice: entry * 1.1,
			BoughtAt: time.Now().Add(-age),
			Key:      tonnel.ModelKey{Name: "Snake Box", Model: "Bluebell"},
		},
		Val: pricing.Valuation{Valid: true, ExitIn72h: exit72, Liquidation: exit72 * .95},
	}
}

// Production: six positions costing 21.99 were headlined "оценка 40.5". The
// book's worth is what it would fetch, and mixing that with a theoretical value
// — or quietly folding the cash balance into the same number — turns the
// headline into something that cannot be checked against anything.
func TestBookIsMarkedAtWhatItWouldFetch(t *testing.T) {
	now := time.Now()
	ads := []positionAdvice{
		heldPosition(1, 4.0, time.Hour, 4.2),
		heldPosition(2, 6.0, time.Hour, 5.8),
	}
	b := summarise(ads, now)

	if b.Invested != 10 {
		t.Errorf("invested = %.2f, want 10", b.Invested)
	}
	if b.Mark72h != 10 {
		t.Errorf("mark = %.2f, want the 4.2 + 5.8 the book would actually fetch", b.Mark72h)
	}
	// Cash is a separate asset and must not silently inflate the mark.
	if b.Cash != 0 || b.CashKnown {
		t.Error("summarise invented a cash balance nobody supplied")
	}
}

// A position nobody could price is marked at what we are asking, not at zero:
// an unreachable model is not a loss.
func TestUnpricedPositionFallsBackToItsAsk(t *testing.T) {
	ad := positionAdvice{Position: store.Position{BuyPrice: 4, ListPrice: 4.5}}
	mark, fast := positionMark(ad)
	if mark != 4.5 || fast != 4.5 {
		t.Errorf("mark/fast = %.2f/%.2f, want the 4.50 ask", mark, fast)
	}
}

// +15% over twenty days is a worse use of the same money than +5% over two, and
// only a denominator with time in it can say so.
func TestCapitalEfficiencyPrefersFastMoneyToBigMoney(t *testing.T) {
	now := time.Now()
	slow := summarise([]positionAdvice{heldPosition(1, 10, 20*24*time.Hour, 11.5)}, now)
	fast := summarise([]positionAdvice{heldPosition(2, 10, 2*24*time.Hour, 10.5)}, now)

	if slow.Return() <= fast.Return() {
		t.Fatalf("fixture is wrong: the slow position should show the bigger raw return (%.3f vs %.3f)",
			slow.Return(), fast.Return())
	}
	if fast.CapitalEfficiency() <= slow.CapitalEfficiency() {
		t.Errorf("+5%% in two days (%.4f/day) ranks below +15%% in twenty (%.4f/day)",
			fast.CapitalEfficiency(), slow.CapitalEfficiency())
	}
}

// Capital past the flip horizon is not working capital, and the book has to say
// how much of it there is.
func TestStuckCapitalIsCountedSeparately(t *testing.T) {
	now := time.Now()
	b := summarise([]positionAdvice{
		heldPosition(1, 4, 2*time.Hour, 4.1),
		heldPosition(2, 6, 5*24*time.Hour, 6.1),
	}, now)

	if b.Stuck != 1 {
		t.Errorf("stuck = %d, want 1", b.Stuck)
	}
	if b.StuckCapital != 6 {
		t.Errorf("stuck capital = %.2f, want the 6 that has been sitting five days", b.StuckCapital)
	}
}
