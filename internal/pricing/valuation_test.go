package pricing

import (
	"math"
	"testing"
	"time"

	"floorline/internal/tonnel"
)

var testKey = tonnel.ModelKey{Name: "Plush Pepe", Model: "Pink Diamond"}

// bookOf builds a book from prices; the first price belongs to the candidate.
func bookOf(candidateID int64, prices ...float64) *Book {
	b := &Book{Key: testKey, FetchedAt: time.Now()}
	for i, p := range prices {
		id := int64(1000 + i)
		if i == 0 {
			id = candidateID
		}
		b.Asks = append(b.Asks, Ask{GiftID: id, Price: p})
	}
	return b
}

func liqOf(median float64, sales int) Liquidity {
	return Liquidity{
		Sales: sales, Sellers: sales, Buyers: sales,
		Median: median, Median7: median, Trend: 1,
		Velocity: float64(sales) / 14,
		LastSale: time.Now(),
	}
}

const (
	testFee      = 0.005
	testUndercut = 0.01
)

func evalPrice(t *testing.T, price float64, book *Book, liq Liquidity, floor float64, supply int) Valuation {
	t.Helper()
	return Evaluate(Input{
		GiftID: 42,
		Key:    testKey,
		Price:  price,
		Book:   book,
		Liq:    liq,
		Floor:  floor,
		Supply: supply,
		Params: Params{Fee: testFee, Undercut: testUndercut, Window: 14 * 24 * time.Hour},
	})
}

// The exit price must be measured against the *next* ask, not the one being
// bought. Buying the floor removes it from the book, so the floor it was
// "20% below" is not a price anyone can sell into.
func TestExitPricesAgainstTheNextAskNotTheOneBeingBought(t *testing.T) {
	book := bookOf(42, 1000, 1050, 1100)
	v := evalPrice(t, 1000, book, liqOf(1100, 40), 1000, 20)

	if !v.Valid {
		t.Fatalf("valuation is invalid: %s", v.Reason)
	}
	if v.CompetingAsk != 1050 {
		t.Errorf("competing ask = %v, want 1050 (the second ask, not our own)", v.CompetingAsk)
	}
	want := 1050 * (1 - testUndercut)
	if math.Abs(v.Exit-want) > 1e-9 {
		t.Errorf("exit = %v, want %v", v.Exit, want)
	}
	if v.ExitBasis != "undercut" {
		t.Errorf("exit basis = %q, want undercut", v.ExitBasis)
	}
}

// The trade that every other tool would take and lose money on: a listing well
// below the median, but with a competing ask right on top of it.
func TestNoEdgeWhenTheNextAskIsRightAbove(t *testing.T) {
	// Median says 1100, so a naive "22% below the median" reading says buy.
	book := bookOf(42, 900, 905, 910)
	v := evalPrice(t, 900, book, liqOf(1100, 40), 900, 30)

	if !v.Valid {
		t.Fatalf("valuation is invalid: %s", v.Reason)
	}
	if v.Edge >= 0 {
		t.Errorf("edge = %.4f, want negative: the only way out is undercutting 905, which loses money", v.Edge)
	}
	// A discount-to-floor reading would look great here, which is the point.
	if v.DiscountToFloor != 0 {
		t.Errorf("discount to floor = %v, want 0 for a listing at the floor", v.DiscountToFloor)
	}
}

// The mirror case: a genuinely mispriced listing with real room above it.
func TestRealEdgeWhenTheBookIsThinAboveUs(t *testing.T) {
	book := bookOf(42, 800, 1200, 1250)
	v := evalPrice(t, 800, book, liqOf(1180, 40), 800, 15)

	if !v.Valid {
		t.Fatalf("valuation is invalid: %s", v.Reason)
	}
	// Undercutting 1200 gives 1188, which is above the 1180 median, so the
	// conservative rule must fall back to the median.
	if v.ExitBasis != "median" {
		t.Errorf("exit basis = %q, want median (the cheaper of the two references)", v.ExitBasis)
	}
	if math.Abs(v.Exit-1180) > 1e-9 {
		t.Errorf("exit = %v, want 1180", v.Exit)
	}
	wantNet := 1180*(1-testFee) - 800
	if math.Abs(v.Net-wantNet) > 1e-9 {
		t.Errorf("net = %v, want %v", v.Net, wantNet)
	}
	if v.Edge < 0.4 {
		t.Errorf("edge = %.3f, want a large positive edge", v.Edge)
	}
}

// The exit is capped by the median even when the book would allow more, because
// asks are wishes and trades are facts.
func TestExitIsCappedByTheMedianTrade(t *testing.T) {
	book := bookOf(42, 500, 5000)
	v := evalPrice(t, 500, book, liqOf(600, 30), 500, 10)

	if v.ExitBasis != "median" {
		t.Errorf("exit basis = %q, want median: one fantasy ask must not set our exit", v.ExitBasis)
	}
	if v.Exit != 600 {
		t.Errorf("exit = %v, want 600", v.Exit)
	}
}

func TestSoleAskFallsBackToTheMedian(t *testing.T) {
	book := bookOf(42, 700) // only our own listing
	v := evalPrice(t, 700, book, liqOf(900, 20), 700, 1)

	if v.HasCompetingAsk {
		t.Error("a book containing only our own listing has no competing ask")
	}
	if v.ExitBasis != "median (sole ask)" {
		t.Errorf("exit basis = %q, want the sole-ask marker", v.ExitBasis)
	}
	if v.Exit != 900 {
		t.Errorf("exit = %v, want the median 900", v.Exit)
	}
}

func TestNoReferenceIsInvalid(t *testing.T) {
	book := bookOf(42, 700)
	v := evalPrice(t, 700, book, Liquidity{Trend: 1}, 0, 0)

	if v.Valid {
		t.Error("with no competing ask and no trade history there is nothing to price against")
	}
	if v.Reason == "" {
		t.Error("an invalid valuation must explain itself")
	}
}

func TestZeroPriceIsInvalid(t *testing.T) {
	v := evalPrice(t, 0, bookOf(42, 0, 100), liqOf(100, 10), 100, 5)
	if v.Valid {
		t.Error("a listing with no price cannot be valued")
	}
}

func TestFeeReducesTheEdge(t *testing.T) {
	book := bookOf(42, 1000, 1500)
	liq := liqOf(1500, 40)

	free := Evaluate(Input{GiftID: 42, Key: testKey, Price: 1000, Book: book, Liq: liq,
		Params: Params{Fee: 0, Undercut: testUndercut}})
	taxed := Evaluate(Input{GiftID: 42, Key: testKey, Price: 1000, Book: book, Liq: liq,
		Params: Params{Fee: 0.10, Undercut: testUndercut}})

	if taxed.Edge >= free.Edge {
		t.Errorf("a 10%% fee must reduce the edge: free %.3f, taxed %.3f", free.Edge, taxed.Edge)
	}
	if taxed.Exit != free.Exit {
		t.Error("the fee changes the proceeds, not the exit price")
	}
}

// Sellers stacked just above the exit will undercut straight back; that is the
// price-war risk the card has to show.
func TestCompetitorsNearCountsTheClusterAboveOurExit(t *testing.T) {
	book := bookOf(42, 1000, 1200, 1210, 1220, 1400)
	v := evalPrice(t, 1000, book, liqOf(1300, 40), 1000, 30)

	// Exit is 1200*0.99 = 1188; the 5% band above it reaches 1247.4.
	if v.CompetitorsNear != 3 {
		t.Errorf("competitors near exit = %d, want 3 (1200, 1210, 1220)", v.CompetitorsNear)
	}
	if v.ExpectedDays <= 0 || math.IsInf(v.ExpectedDays, 1) {
		t.Errorf("expected days = %v, want a finite positive estimate", v.ExpectedDays)
	}
}

func TestExpectedDaysIsInfiniteWithoutVelocity(t *testing.T) {
	liq := liqOf(1000, 10)
	liq.Velocity = 0
	v := evalPrice(t, 500, bookOf(42, 500, 1000), liq, 500, 10)

	if !math.IsInf(v.ExpectedDays, 1) {
		t.Errorf("expected days = %v, want +Inf when nothing ever trades", v.ExpectedDays)
	}
	if !math.IsInf(v.DaysOfSupply, 1) {
		t.Errorf("days of supply = %v, want +Inf", v.DaysOfSupply)
	}
}

func TestDiscountToFloorIsDisplayOnly(t *testing.T) {
	book := bookOf(42, 780, 1000)
	v := evalPrice(t, 780, book, liqOf(1000, 30), 1000, 12)

	if math.Abs(v.DiscountToFloor-0.22) > 1e-9 {
		t.Errorf("discount to floor = %.4f, want 0.22", v.DiscountToFloor)
	}
	// The naive read — "buy at 780, sell at the 1000 floor" — is more optimistic
	// than the truth, because selling means undercutting the next ask.
	naive := (v.Floor*(1-testFee) - v.Price) / v.Price
	if v.Edge >= naive {
		t.Errorf("edge %.4f must be below the naive sell-at-floor edge %.4f", v.Edge, naive)
	}
}

func TestBookExcludesBundlesAndPremarket(t *testing.T) {
	gifts := []tonnel.Gift{
		{GiftID: 1, Price: 100},
		{GiftID: -2, Price: 5},                  // bundle: one price for a whole pack
		{GiftID: 3, Price: 10, Premarket: true}, // not a deliverable gift
		{GiftID: 4, Price: 50, TelegramMarketplace: true},
		{GiftID: 5, Price: 90},
	}
	b := NewBook(testKey, gifts, time.Now())

	if b.Len() != 2 {
		t.Fatalf("book has %d asks, want 2 tradable ones", b.Len())
	}
	if b.Asks[0].Price != 90 {
		t.Errorf("book is not sorted: first ask %v, want 90", b.Asks[0].Price)
	}
	if best, ok := b.BestExcluding(5); !ok || best != 100 {
		t.Errorf("BestExcluding(5) = (%v, %v), want (100, true)", best, ok)
	}
}
