package pricing

import (
	"math"
	"testing"
	"time"

	"floorline/internal/store"
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
		Sales: sales, DistinctGifts: sales, Turnover: 1,
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

func TestSparseAttributesShrinkToModelMedian(t *testing.T) {
	sales := []store.SaleRow{{Price: 200, Backdrop: "Rare", Symbol: "Moon"}}
	a := ComputeAttributeValue(sales, "Rare", "Moon", 100)
	if a.Valid {
		t.Error("one exact trade must not make an attribute valuation valid")
	}
	if a.Premium >= .35 {
		t.Errorf("sparse premium was not shrunk: %.2f", a.Premium)
	}
}

// Production reported "attribute premium +0.6% from 2 exact sales" and priced
// against it. Two or three prints cannot tell a half-percent premium from
// nothing, so below the minimum sample count the premium must be exactly zero
// rather than merely small.
func TestTinySamplesProduceNoAttributePremiumAtAll(t *testing.T) {
	// Three exact sales, each a few percent off the model median — the shape of
	// the noise that produced the +0.6% / -0.7% / -0.5% readings.
	sales := []store.SaleRow{
		{Price: 3.32, Backdrop: "Azure Blue", Symbol: "Spades"},
		{Price: 3.24, Backdrop: "Azure Blue", Symbol: "Spades"},
		{Price: 3.30, Backdrop: "Azure Blue", Symbol: "Spades"},
	}
	a := ComputeAttributeValue(sales, "Azure Blue", "Spades", 3.28)
	if a.Premium != 0 {
		t.Errorf("premium = %+.4f from %d exact sales, want exactly 0", a.Premium, a.ExactSamples)
	}
	if a.Fair != 3.28 {
		t.Errorf("fair value = %.4f, want the untouched model median 3.28", a.Fair)
	}
	if a.Valid {
		t.Error("three prints must not qualify as attribute evidence")
	}
}

func TestPatientExitRequiresEvidenceAndCanWinOnProfitPerDay(t *testing.T) {
	sales := make([]store.SaleRow, 0, 40)
	for i := 0; i < 40; i++ {
		p := 100.0
		bd, sy := "Plain", "Dot"
		if i < 20 {
			p = 130
			bd, sy = "Gold", "Moon"
		}
		sales = append(sales, store.SaleRow{Price: p, Backdrop: bd, Symbol: sy})
	}
	attr := ComputeAttributeValue(sales, "Gold", "Moon", 100)
	if !attr.Valid || attr.Fair <= 100 {
		t.Fatalf("attribute value = %+v", attr)
	}
	v := Evaluate(Input{GiftID: 42, Key: testKey, Price: 70, Backdrop: "Gold", Symbol: "Moon", Attribute: attr, Book: bookOf(42, 70, 140), Liq: liqOf(100, 40), Params: Params{Fee: testFee, Undercut: testUndercut}})
	if v.PatientExit <= v.FastExit {
		t.Errorf("patient %.2f should exceed fast %.2f", v.PatientExit, v.FastExit)
	}
	if v.Confidence <= 0 {
		t.Error("confidence should be populated")
	}
}

// The three positions the desk actually held when it advised REDUCE/EXIT on all
// of them at targets below the live floor. Every number below is the one the
// operator read off the market, so a regression here is a regression that costs
// money: a target under the collection floor, while the bot's own cross-market
// reference sits at or above the current ask, is a panic sell.
func TestHeldPositionsAreNotPricedBelowTheLiveFloor(t *testing.T) {
	cases := []struct {
		name string
		// entry is the cost basis; ask is what we are currently listed at.
		entry, ask float64
		// floor is the live collection floor; asks are the competing offers.
		floor       float64
		competing   []float64
		median      float64 // trade-history median, i.e. the stale reference
		externalRef float64 // cross-market ask-depth reference
		breakEven   float64
	}{
		{
			name: "Snake Box Bluebell", entry: 3.09, ask: 3.29, floor: 3.28,
			competing: []float64{3.28, 3.30, 3.31}, median: 2.93,
			externalRef: 3.23, breakEven: 3.11,
		},
		{
			name: "Instant Ramen Cat Food", entry: 3.39, ask: 3.75, floor: 3.75,
			competing: []float64{3.80, 3.85, 3.90}, median: 3.14,
			externalRef: 3.91, breakEven: 3.41,
		},
		{
			name: "Swag Bag Choco Kush", entry: 4.89, ask: 5.03, floor: 5.03,
			competing: []float64{5.09, 5.15, 5.20}, median: 4.71,
			externalRef: 5.01, breakEven: 4.91,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			book := bookOf(42, append([]float64{c.ask}, c.competing...)...)
			v := evalPrice(t, c.entry, book, liqOf(c.median, 30), c.floor, 20)
			if !v.Valid {
				t.Fatalf("valuation is invalid: %s", v.Reason)
			}
			// The whole live book, floor included, is above the trade-history
			// median, so the median is stale and must not set the target.
			if v.Exit < c.floor*(1-.03) {
				t.Errorf("target %.4f is more than 3%% below the live floor %.2f — that is a panic sell, not an exit",
					v.Exit, c.floor)
			}
			if !v.SupportGuarded {
				t.Errorf("the live support guard did not engage: basis %q, exit %.4f, median %.2f",
					v.ExitBasis, v.Exit, c.median)
			}
			// A target under break-even is what produced REDUCE/EXIT in
			// production; with the guard the position is profitable to hold.
			if v.Exit <= c.breakEven {
				t.Errorf("target %.4f is at or below break-even %.2f", v.Exit, c.breakEven)
			}
			if v.Net <= 0 {
				t.Errorf("net %.4f must be positive, or the desk is told to sell at a loss", v.Net)
			}
			// The bot must not contradict its own cross-market input: when
			// external ask depth is at or above our ask, the exit cannot be
			// priced far under it.
			if c.externalRef >= c.ask*(1-.01) && v.Exit < c.externalRef*(1-.05) {
				t.Errorf("target %.4f is 5%% under the external ask-depth reference %.2f that the model itself reported",
					v.Exit, c.externalRef)
			}
			// The liquidation price is a separate, live-market number: what we
			// must ask to be the cheapest offer on screen right now.
			if v.Liquidation <= 0 || v.Liquidation > c.floor {
				t.Errorf("liquidation %.4f must be a positive price under the live floor %.2f", v.Liquidation, c.floor)
			}
			if v.Liquidation <= c.breakEven {
				t.Errorf("liquidation %.4f is below break-even %.2f; the position is not underwater against the live book",
					v.Liquidation, c.breakEven)
			}
		})
	}
}

// The mirror of the case above: a single ask far above a thin book must never be
// mistaken for support, or the guard becomes a way to inflate every valuation.
func TestLiveSupportNeedsTwoAgreeingSources(t *testing.T) {
	// One competing ask, and it is nowhere near the floor.
	lonely := evalPrice(t, 500, bookOf(42, 500, 5000), liqOf(600, 30), 500, 10)
	if lonely.SupportGuarded || lonely.Exit != 600 {
		t.Errorf("one fantasy ask must not overrule the median: exit %.2f, guarded %v", lonely.Exit, lonely.SupportGuarded)
	}
	// Book agrees with the floor, but only one seller holds the level.
	thin := evalPrice(t, 3.09, bookOf(42, 3.29, 3.28), liqOf(2.93, 30), 3.28, 10)
	if thin.SupportGuarded {
		t.Errorf("a single offer is a listing, not a support level: exit %.4f", thin.Exit)
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

func TestScoreRanksFastMicroEdgeAboveSlowLargeEdge(t *testing.T) {
	fast := Valuation{Valid: true, Edge: .03, Net: 3, ExpectedDays: .5, Confidence: .8, Liq: Liquidity{Sales: 20, MADRatio: .02}}
	slow := Valuation{Valid: true, Edge: .10, Net: 10, ExpectedDays: 8, Confidence: .8, Liq: Liquidity{Sales: 20, MADRatio: .02}}
	fastScore := BuildScore(fast, 1)
	slowScore := BuildScore(slow, 1)
	if fastScore.Total <= slowScore.Total {
		t.Fatalf("fast micro-edge score %.2f should beat slow large-edge %.2f", fastScore.Total, slowScore.Total)
	}
}

func TestScoreRejectsFragileSubPercentEdge(t *testing.T) {
	v := Valuation{Valid: true, Edge: .009, Net: .9, ExpectedDays: 1, Confidence: .9, Liq: Liquidity{Sales: 20, MADRatio: .10}, CompetitorsNear: 2}
	s := BuildScore(v, 1)
	if s.RiskAdjustedEdge != 0 || s.Total != 0 {
		t.Fatalf("uncertainty should consume fragile edge: %+v", s)
	}
}

func TestScorePenalizesStaleExpensiveFloorAndVolatileGram(t *testing.T) {
	base := Valuation{Valid: true, Edge: .05, Net: 5, ExpectedDays: 1, Confidence: .8, Liq: Liquidity{Sales: 20, MADRatio: .02}}
	calm := base
	calm.FX = FXContext{Valid: true, FloorLag: 0, Move15m: 0}
	risky := base
	risky.FX = FXContext{Valid: true, FloorLag: .12, Move15m: .04}
	if BuildScore(risky, 1).Total >= BuildScore(calm, 1).Total {
		t.Fatal("stale-expensive floor during GRAM volatility should reduce score")
	}
}
