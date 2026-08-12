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
		Params: Params{Fee: testFee, Undercut: testUndercut},
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
	// Fast exit uses robust depth (middle of the first three external asks), not
	// one brittle top ask. Here two asks produce depth 1075.
	want := 1075 * (1 - testUndercut)
	if math.Abs(v.Exit-want) > 1e-9 {
		t.Errorf("exit = %v, want %v", v.Exit, want)
	}
	if v.ExitBasis != "быстрый выход" {
		t.Errorf("exit basis = %q, want the fast blend", v.ExitBasis)
	}
}

func TestOwnAsksAreNotCompetingMarketDepth(t *testing.T) {
	const ownerID = int64(777)
	book := &Book{Key: testKey, FetchedAt: time.Now(), Asks: []Ask{
		{GiftID: 42, Seller: 123, Price: 800},
		{GiftID: 43, Seller: ownerID, Price: 1290},
		{GiftID: 44, Seller: ownerID, Price: 1300},
		{GiftID: 45, Seller: 456, Price: 1300},
	}}
	v := Evaluate(Input{
		GiftID: 42, OwnerID: ownerID, Key: testKey, Price: 800,
		Book: book, Liq: liqOf(1300, 40),
		Params: Params{Fee: testFee, Undercut: testUndercut},
	})
	if !v.Valid {
		t.Fatalf("valuation invalid: %s", v.Reason)
	}
	if v.CompetingAsk != 1300 {
		t.Errorf("competing ask = %v, want the first ask from another seller", v.CompetingAsk)
	}
	if v.CompetitorsNear != 1 {
		t.Errorf("own asks inflated the queue: %d", v.CompetitorsNear)
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
	if v.Exit <= 1180 || v.Exit > 1250*(1-testUndercut) {
		t.Errorf("fast exit %.2f must blend history with live depth without crossing the queue", v.Exit)
	}
	if v.Edge < 0.4 {
		t.Errorf("edge = %.3f, want a large positive edge", v.Edge)
	}
}

// The exit is capped by the median even when the book would allow more, because
// asks are wishes and trades are facts.
func TestOneFantasyAskCannotOverruleHistory(t *testing.T) {
	book := bookOf(42, 500, 5000)
	v := evalPrice(t, 500, book, liqOf(600, 30), 500, 10)

	if v.Exit != 600 {
		t.Errorf("one fantasy ask is not depth: exit = %v, want history 600", v.Exit)
	}
}

func TestSoleAskFallsBackToTheMedian(t *testing.T) {
	book := bookOf(42, 700) // only our own listing
	v := evalPrice(t, 700, book, liqOf(900, 20), 700, 1)

	if v.HasCompetingAsk {
		t.Error("a book containing only our own listing has no competing ask")
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

// Production, /val on a gift that had already been delisted: the price came
// back as zero, Evaluate correctly refused — and then WithCrossDepth re-ran the
// whole pipeline, which did not repeat the check. Edge became Net/0 and the
// card read "✅ BUY — проходит фильтр сигнала · +Inf%".
func TestDelistedGiftStaysInvalidThroughCrossMarket(t *testing.T) {
	v := evalPrice(t, 0, bookOf(42, 0, 5, 5, 5.2), liqOf(3.28, 11), 5, 14)
	v = WithCrossDepth(v, CrossMarket{Support: 3.76, Venues: 1, Asks: []float64{3.7, 3.76, 3.79}})

	if v.Valid {
		t.Fatalf("a delisted gift must stay unpriceable: exit %.3f edge %v", v.FastExit, v.Edge)
	}
	if v.Reason == "" {
		t.Error("an invalid valuation must explain itself")
	}
	if math.IsInf(v.Edge, 0) || math.IsNaN(v.Edge) {
		t.Errorf("edge = %v, want a finite number", v.Edge)
	}
	if s := BuildScore(v, 1); math.IsInf(s.Total, 0) || math.IsNaN(s.Total) {
		t.Errorf("score = %v, want a finite number", s.Total)
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

func TestPurchaseReferralIsAddedToAskExactlyOnce(t *testing.T) {
	book := bookOf(42, 3.79, 10)
	v := Evaluate(Input{GiftID: 42, Key: testKey, Price: 3.79, Book: book, Liq: liqOf(10, 20), Params: Params{Fee: .005}})
	if math.Abs(v.Cost-3.80895) > 1e-9 {
		t.Errorf("cost = %.8f, want 3.80895", v.Cost)
	}

	owned := Evaluate(Input{GiftID: 42, Key: testKey, Price: 3.79, Cost: 3.809, Book: book, Liq: liqOf(10, 20), Params: Params{Fee: .005}})
	if owned.Cost != 3.809 {
		t.Errorf("actual owned cost was charged referral twice: %.8f", owned.Cost)
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
	naive := (v.Floor - v.Cost) / v.Cost
	if v.Edge > naive+1e-9 {
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
	attr := AttributeValue{Valid: true, Fair: 130, Premium: .30, ExactSamples: 20, ExactShare: .5, Confidence: .8}
	v := Evaluate(Input{GiftID: 42, Key: testKey, Price: 70, Backdrop: "Gold", Symbol: "Moon", Attribute: attr, Book: bookOf(42, 70, 140, 145), Liq: liqOf(100, 40), Params: Params{Fee: testFee, Undercut: testUndercut}})
	if v.PatientExit < v.FastExit {
		t.Errorf("patient %.2f must never be below fast %.2f", v.PatientExit, v.FastExit)
	}
	if v.Confidence <= 0 {
		t.Error("confidence should be populated")
	}
}

func TestPatientAskPaysForWaitingAndBearCaseStaysSeparate(t *testing.T) {
	liq := liqOf(3.20, 20)
	liq.Trend = .95
	v := evalPrice(t, 3.10, bookOf(42, 3.10, 3.24, 3.25, 3.30), liq, 3.10, 10)
	if v.PatientAsk <= v.FairValue {
		t.Fatalf("patient %.3f must carry a wait premium over fair %.3f", v.PatientAsk, v.FairValue)
	}
	if v.BearCase <= 0 || v.BearCase >= v.PatientAsk {
		t.Fatalf("bear %.3f must be a separate downside scenario below patient %.3f", v.BearCase, v.PatientAsk)
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
			v = WithCrossMarket(v, c.externalRef)
			if !v.Valid {
				t.Fatalf("valuation is invalid: %s", v.Reason)
			}
			// The whole live book, floor included, is above the trade-history
			// median, so the median is stale and must not set the target.
			if v.Exit < c.floor*(1-.03) {
				t.Errorf("target %.4f is more than 3%% below the live floor %.2f — that is a panic sell, not an exit",
					v.Exit, c.floor)
			}
			if v.MarketDivergence > .10 && !v.SupportGuarded {
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
			if v.MarketDivergence > .10 && !v.MarketDisagreement {
				t.Errorf("history %.2f versus live depth %.2f must be manual-only", c.median, v.LiveDepth)
			}
			// The liquidation price is a separate, live-market number: what we
			// must ask to be the cheapest offer on screen right now.
			if v.Liquidation <= 0 || v.Liquidation > c.competing[0] {
				t.Errorf("liquidation %.4f must be a positive price under the next external ask %.2f", v.Liquidation, c.competing[0])
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
	if lonely.Exit != 600 || lonely.LiveDepth != 0 {
		t.Errorf("one fantasy ask must not become robust depth: exit %.2f, depth %.2f", lonely.Exit, lonely.LiveDepth)
	}
}

func TestSparseHistoryShrinksTowardLiveDepth(t *testing.T) {
	liq := liqOf(3.26, 7)
	liq.DistinctGifts = 3
	v := evalPrice(t, 3.20, bookOf(42, 3.20, 3.50, 3.69, 3.75), liq, 3.50, 10)
	if v.HistoryWeight > .10 || v.HistoryReference < 3.55 {
		t.Fatalf("sparse history got too much power: weight %.2f ref %.3f", v.HistoryWeight, v.HistoryReference)
	}
	if v.FastExit < 3.50 || v.PatientAsk < v.FastExit {
		t.Fatalf("broken exit ladder: liquidation %.3f fast %.3f patient %.3f", v.Liquidation, v.FastExit, v.PatientAsk)
	}
}

func TestHistoryWeightFollowsUniqueGiftCount(t *testing.T) {
	for _, tc := range []struct {
		distinct int
		want     float64
	}{{3, .10}, {7, .20}, {14, .30}, {25, .40}} {
		liq := liqOf(3.50, 40)
		liq.DistinctGifts = tc.distinct
		v := evalPrice(t, 3, bookOf(42, 3, 3.6, 3.7, 3.8), liq, 3, 10)
		if math.Abs(v.HistoryWeight-tc.want) > 1e-9 {
			t.Errorf("%d unique gifts gave history weight %.2f, want %.2f", tc.distinct, v.HistoryWeight, tc.want)
		}
	}
}

func TestCrossMarketDepthMovesPriceDiscoveryButDisagreementIsFlagged(t *testing.T) {
	v := evalPrice(t, 3.20, bookOf(42, 3.20, 3.75, 3.85, 3.90), liqOf(3.14, 30), 3.75, 10)
	v = WithCrossMarket(v, 3.91)
	if v.CrossWeight < .19 || v.FastExit <= 3.40 {
		t.Fatalf("external depth was left as a footnote: %+v", v)
	}
	if !v.MarketDisagreement || v.MarketDivergence < .10 {
		t.Fatal("Ramen-shaped history/live disagreement must be manual-only")
	}
}

func TestAskGapRewardsThinDepthAndTightQueueCannotFakeEdge(t *testing.T) {
	thin := evalPrice(t, 3.869, bookOf(42, 3.869, 4.221, 4.30, 4.35), liqOf(4.20, 20), 3.869, 10)
	tight := evalPrice(t, 4.49, bookOf(42, 4.49, 4.50, 4.51, 4.52), liqOf(4.80, 20), 4.49, 10)
	if thin.AskGap1 < .05 || thin.ScoreBreakdown.DepthFactor <= 1 {
		t.Fatalf("Jester-shaped gap was not rewarded: gap %.2f factor %.2f", thin.AskGap1, thin.ScoreBreakdown.DepthFactor)
	}
	if tight.Edge >= 0 || tight.ScoreBreakdown.DepthFactor >= 1 {
		t.Fatalf("tight queue still looks buyable: edge %.3f factor %.2f", tight.Edge, tight.ScoreBreakdown.DepthFactor)
	}
}

// Production, 12 Aug: Pet Snake / Black Mamba listed at 6 (6.03 all in) while
// the next Tonnel ask was 4.21 and Portals was showing 4 / 5 / 5 / 5.89 / 6.
// The engine still printed a 6.867 fast exit and a +13.9% edge, because the
// median of 4.21 / 7.9 / 10.20 pretended a hole in the book was liquidity, and
// because an external venue was only ever allowed to raise the ceiling.
func TestGappyBookCannotInventAnExitAboveTheEntry(t *testing.T) {
	liq := liqOf(3.846, 9)
	liq.MADRatio, liq.Trend, liq.Velocity = .15, 1.05, .6
	v := evalPrice(t, 6, bookOf(42, 6, 4.21, 7.9, 10.2), liq, 4.21, 13)
	v = WithCrossDepth(v, CrossMarket{Support: 5, Venues: 1, Asks: []float64{4, 5, 5, 5.89, 6}})

	if !v.Valid {
		t.Fatalf("valuation is invalid: %s", v.Reason)
	}
	if v.LiveDepthCount != 1 {
		t.Errorf("trusted depth = %d, want 1: 4.21 → 7.90 is a hole, not a second price point", v.LiveDepthCount)
	}
	if !v.DepthCapped || v.LiveDepth > 4.21*(1+depthGapLimit)+1e-9 {
		t.Errorf("depth %.3f (capped=%v) must be pulled back towards the 4.21 ask", v.LiveDepth, v.DepthCapped)
	}
	if v.FastExit > v.Cost {
		t.Errorf("fast exit %.3f above the %.3f entry with five cheaper asks in front of us", v.FastExit, v.Cost)
	}
	if v.Edge > 0 {
		t.Errorf("edge %.3f must not be positive here", v.Edge)
	}
	if v.AsksBelowEntry < 5 {
		t.Errorf("asks below entry = %d, want the 4.21 Tonnel ask plus 4 / 5 / 5 / 5.89 on Portals", v.AsksBelowEntry)
	}
	// The exit has to price *under* the one real ask in the book, not above the
	// hole behind it. Getting this right is what makes CheaperAsks zero here:
	// at 4.168 we genuinely would be the cheapest offer.
	if v.FastExit >= 4.21 {
		t.Errorf("fast exit %.3f must undercut the only real ask 4.21, not clear the hole above it", v.FastExit)
	}
	// Four independent offers sit more than 5% under our entry. That is a market
	// telling us the entry is wrong, and no gap in the local book overrides it.
	if v.UndercutsEntry < 4 {
		t.Errorf("undercuts of entry = %d, want the 4.21 ask plus 4 / 5 / 5 on Portals", v.UndercutsEntry)
	}
	if !v.PricedAboveMarket {
		t.Error("a market standing well under our entry must veto the trade outright")
	}
}

// A stale-high history and one expensive external venue must not blend their
// way to an exit above our own entry while the whole live book sits under it.
//
// This used to need the overpriced clamp to catch it. It no longer does: an
// expensive foreign venue can no longer raise the ceiling, so the exit stays
// pinned to the local queue and never gets above entry in the first place. The
// clamp is still asserted separately, below.
func TestBlendCannotPriceAboveEntryWhileTheBookIsCheaper(t *testing.T) {
	v := evalPrice(t, 5, bookOf(42, 5, 4.9, 4.95, 5), liqOf(8, 40), 4.9, 20)
	v = WithCrossDepth(v, CrossMarket{Support: 9, Venues: 1, Asks: []float64{9, 9.5}})

	if v.FastExit > v.Cost {
		t.Errorf("exit %.3f still above the %.3f entry", v.FastExit, v.Cost)
	}
	if v.Edge > 0 {
		t.Errorf("edge %.4f must not survive: the whole live book is under our entry", v.Edge)
	}
	// The ceiling is the live queue, so a 9.00 quote elsewhere buys us nothing.
	if v.FastExit > v.LiveDepth {
		t.Errorf("exit %.3f cleared the live depth %.3f on the strength of a foreign quote", v.FastExit, v.LiveDepth)
	}
}

// The clamp still has a job: when the local book is too thin to produce a
// ceiling at all, a stale history can blend straight past the entry while
// another venue's queue sits well under it.
func TestOverpricedClampFiresWhenTheLocalBookHasNoDepth(t *testing.T) {
	v := evalPrice(t, 5, bookOf(42, 5, 9), liqOf(8, 40), 9, 20)
	v = WithCrossDepth(v, CrossMarket{Support: 4, Venues: 1, Asks: []float64{3.9, 4.0}})

	if v.LiveDepth != 0 {
		t.Fatalf("one competing ask must not become depth: %.3f", v.LiveDepth)
	}
	if !v.PricedAboveMarket {
		t.Fatalf("guard did not fire: exit %.3f, entry %.3f, %d asks below entry", v.FastExit, v.Cost, v.AsksBelowEntry)
	}
	if v.FastExit > v.Cost {
		t.Errorf("exit %.3f still above the %.3f entry", v.FastExit, v.Cost)
	}
	if v.Edge > 0 {
		t.Errorf("edge %.4f must not survive the clamp", v.Edge)
	}
	if v.ExitCapped == "" {
		t.Error("a clamped exit must explain itself on the card")
	}
}

// The trait premium is the only thing that can justify pricing above the crowd,
// and it has to be measured, not assumed.
func TestMeasuredTraitPremiumSurvivesTheOverpricedGuard(t *testing.T) {
	attr := AttributeValue{Valid: true, Fair: 9, Premium: .5, ExactSamples: 20, ExactShare: .3, Confidence: .8}
	v := Evaluate(Input{
		GiftID: 42, Key: testKey, Price: 6, Backdrop: "Platinum", Symbol: "Khinkali", Attribute: attr,
		Book: bookOf(42, 6, 7.5, 8, 8.4), Liq: liqOf(7.5, 30),
		Params: Params{Fee: testFee, Undercut: testUndercut},
	})
	v = WithCrossDepth(v, CrossMarket{Support: 5, Venues: 1, Asks: []float64{4, 5, 5}})
	if v.PricedAboveMarket {
		t.Errorf("a 20-sample trait premium is evidence; the guard must not fire: exit %.3f cost %.3f", v.FastExit, v.Cost)
	}
}

// A venue with a real queue below us caps the exit instead of confirming it.
func TestCheaperExternalQueueCapsTheFastExit(t *testing.T) {
	base := evalPrice(t, 3, bookOf(42, 3, 4, 4.1, 4.2), liqOf(4, 30), 4, 20)
	capped := WithCrossDepth(base, CrossMarket{Support: 3.5, Venues: 1, Asks: []float64{3.4, 3.5, 3.6}})
	if capped.FastExit >= base.FastExit {
		t.Errorf("cheaper external depth must pull the exit down: %.3f vs %.3f", capped.FastExit, base.FastExit)
	}
	if capped.FastExit < capped.Liquidation-1e-9 {
		t.Errorf("the cap must never push the exit below the live liquidation price %.3f", capped.Liquidation)
	}
}

// Production, 12 Aug — the two signals that made the operator distrust the bot.
//
// Both have the same shape: a hole in the Tonnel book sitting far above a dense,
// agreeing queue on Portals. The old engine priced the exit into the hole and
// printed +9.5% and +41.9%. It could not do otherwise: the cross-market cap was
// floored at a liquidation price derived from that same hole, so the cap was
// arithmetically incapable of biting.
func TestLocalGapCannotOutvoteAnAgreeingExternalQueue(t *testing.T) {
	t.Run("Liberty Figure Deputy", func(t *testing.T) {
		// Ask 4.5 (4.522 all in), next Tonnel ask 5.00, Portals 3.95–4.00.
		liq := liqOf(3.764, 14)
		liq.Velocity = 1.0
		v := evalPrice(t, 4.5, bookOf(42, 4.5, 5, 5, 5.2), liq, 5, 28)
		v = WithCrossDepth(v, CrossMarket{Support: 3.97, Venues: 1, Asks: []float64{3.95, 3.97, 4, 4, 4}})

		if v.FastExit > 4.0 {
			t.Errorf("fast exit %.3f: Portals is stacked at 3.95–4.00, the Tonnel 5.00 is a hole", v.FastExit)
		}
		if v.Edge > 0 {
			t.Errorf("edge %+.1f%% — this trade lost money at 4.522 in", v.Edge*100)
		}
		if !v.PricedAboveMarket {
			t.Error("five external asks more than 5% under entry must veto the trade")
		}
	})

	t.Run("Ice Cream Beehive", func(t *testing.T) {
		// Ask 3.47 (3.487 all in), next Tonnel ask 5.00, Portals 3.69–3.80.
		// This one is a real buy — just a +5-7% one, not the +41.9% printed.
		liq := liqOf(3.229, 7)
		liq.Velocity = .5
		v := evalPrice(t, 3.47, bookOf(42, 3.47, 5, 5, 5), liq, 5, 14)
		v = WithCrossDepth(v, CrossMarket{Support: 3.7, Venues: 1, Asks: []float64{3.69, 3.7, 3.76, 3.79, 3.8}})

		if v.FastExit > 3.8 {
			t.Errorf("fast exit %.3f must respect the Portals queue at 3.69–3.80", v.FastExit)
		}
		if v.Edge <= 0 || v.Edge > .12 {
			t.Errorf("edge %+.1f%%, want a realistic single-digit one, not the 41.9%% that was printed", v.Edge*100)
		}
		// Nothing on Portals is below entry here, so the veto must stay quiet:
		// the guard has to separate a genuine discount from an overpriced lot.
		if v.PricedAboveMarket {
			t.Error("no external ask is under this entry; the veto must not fire")
		}
	})
}

// A price and the wait it implies have to agree. The two estimates come from
// independent formulas, and production printed the same number on both rungs
// with the patient one arriving three days sooner.
func TestPatientRungIsNeverQuickerThanTheFastOne(t *testing.T) {
	liq := liqOf(3.05, 14)
	liq.Velocity = 1.0
	v := evalPrice(t, 3.16, bookOf(42, 3.16, 3.5, 3.5, 3.55), liq, 3.5, 22)

	if v.PatientAsk < v.FastExit {
		t.Fatalf("patient %.3f below fast %.3f", v.PatientAsk, v.FastExit)
	}
	if v.PatientExpectedDays < v.FastExpectedDays {
		t.Errorf("patient fills in %.1fd but fast takes %.1fd — waiting for more money cannot be quicker",
			v.PatientExpectedDays, v.FastExpectedDays)
	}
	if SamePrice(v.PatientAsk, v.FastExit) && v.PatientExpectedDays != v.FastExpectedDays {
		t.Errorf("one price (%.3f) cannot have two different waits: %.1fd vs %.1fd",
			v.FastExit, v.FastExpectedDays, v.PatientExpectedDays)
	}
}

// The whole ladder, on every shape of book the engine sees.
func TestExitLadderIsMonotonic(t *testing.T) {
	cases := []struct {
		name  string
		price float64
		book  *Book
		liq   Liquidity
		cross *CrossMarket
	}{
		{"dense book", 3.10, bookOf(42, 3.10, 3.24, 3.25, 3.30), liqOf(3.20, 20), nil},
		{"gap above the floor", 6, bookOf(42, 6, 4.21, 7.9, 10.2), liqOf(3.846, 9),
			&CrossMarket{Support: 5, Venues: 1, Asks: []float64{4, 5, 5, 5.89, 6}}},
		{"stale-low history", 3.20, bookOf(42, 3.20, 3.75, 3.85, 3.90), liqOf(3.14, 30),
			&CrossMarket{Support: 3.91, Venues: 1, Asks: []float64{3.90, 3.95}}},
		{"cheaper elsewhere", 4.5, bookOf(42, 4.5, 5, 5, 5.2), liqOf(3.764, 14),
			&CrossMarket{Support: 3.97, Venues: 1, Asks: []float64{3.95, 3.97, 4, 4, 4}}},
		{"single competing ask", 700, bookOf(42, 700, 5000), liqOf(900, 20), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := evalPrice(t, c.price, c.book, c.liq, 0, 10)
			if c.cross != nil {
				v = WithCrossDepth(v, *c.cross)
			}
			if !v.Valid {
				t.Fatalf("valuation invalid: %s", v.Reason)
			}
			if v.Liquidation > v.FastExit+1e-9 {
				t.Errorf("liquidation %.4f above fast exit %.4f", v.Liquidation, v.FastExit)
			}
			if v.PatientAsk < v.FastExit-1e-9 {
				t.Errorf("patient %.4f below fast %.4f", v.PatientAsk, v.FastExit)
			}
			if v.PatientExpectedDays < v.FastExpectedDays {
				t.Errorf("patient %.1fd quicker than fast %.1fd", v.PatientExpectedDays, v.FastExpectedDays)
			}
		})
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
	if best, ok := b.BestExcluding(5, 0); !ok || best != 100 {
		t.Errorf("BestExcluding(5) = (%v, %v), want (100, true)", best, ok)
	}
}

// Production, 12 Aug: the highest score the desk ever printed was 136.5, on a
// book reading 8 → 14.4 with a single live ask under the hole. The gap was
// scored as room to sell into. A gap above one price is the hole itself.
func TestGapAboveASingleAskIsNotRewardedAsDepth(t *testing.T) {
	liq := liqOf(6.327, 16)
	liq.DistinctGifts, liq.Velocity = 12, .86

	// One real ask at 8, then a jump to 14.4 — the Blåhaj shape.
	gappy := evalPrice(t, 6.16, bookOf(42, 6.16, 8, 14.4, 15), liq, 8, 6)
	// The same edge, but with a genuine run of prices behind the first ask.
	solid := evalPrice(t, 6.16, bookOf(42, 6.16, 8, 8.3, 8.6), liq, 8, 6)

	if gappy.LiveDepthCount != 1 || !gappy.DepthCapped {
		t.Fatalf("fixture is wrong: depth count %d capped %v", gappy.LiveDepthCount, gappy.DepthCapped)
	}
	if gappy.ScoreBreakdown.DepthFactor >= 1 {
		t.Errorf("a hole scored a depth bonus of %.2f", gappy.ScoreBreakdown.DepthFactor)
	}
	if gappy.ScoreBreakdown.Total >= solid.ScoreBreakdown.Total {
		t.Errorf("gappy book scored %.1f against %.1f for a real queue",
			gappy.ScoreBreakdown.Total, solid.ScoreBreakdown.Total)
	}
}

// The score has to be readable at a glance and comparable between cards. The
// old one was an unbounded product that ranged from 9 to 136.
func TestScoreStaysOnAHundredPointScale(t *testing.T) {
	// A deliberately extreme case: huge edge, sells in hours, dense history.
	liq := liqOf(10, 60)
	liq.DistinctGifts, liq.Velocity, liq.MADRatio = 60, 12, .01
	v := evalPrice(t, 3, bookOf(42, 3, 9, 9.1, 9.2), liq, 9, 60)
	v = WithCrossDepth(v, CrossMarket{Support: 9, Venues: 2, Asks: []float64{9, 9.1, 9.2}})

	s := v.ScoreBreakdown
	if s.Total <= 0 || s.Total > 100 {
		t.Errorf("score %.1f is outside 0..100 on an excellent trade", s.Total)
	}
	if s.Quality <= 0 || s.Quality > 1 {
		t.Errorf("quality %.2f is outside 0..1", s.Quality)
	}
	// The top of the scale has to be reachable, or the wording on the card
	// ("отличный") is decoration for a band nothing ever lands in.
	if s.Total < 80 {
		t.Errorf("a near-perfect trade scored only %.1f; the scale is unusable", s.Total)
	}

	// And a dead model must sit at the bottom of the same scale: four prints
	// across two gifts, a fortnight to sell.
	dead := liqOf(4.25, 4)
	dead.DistinctGifts, dead.Velocity, dead.Turnover = 2, .14, .5
	slow := evalPrice(t, 4.6, bookOf(42, 4.6, 5, 5.2, 5.5), dead, 4.55, 16)
	if slow.ScoreBreakdown.Total > 10 {
		t.Errorf("a model trading twice a fortnight scored %.1f", slow.ScoreBreakdown.Total)
	}
}

// Evidence, not just price, has to move the ranking: the same trade priced
// without any cross-market corroboration must rank below one the other venues
// confirm, and far below neither is a venue that failed to answer.
func TestScoreRewardsCorroborationAndPunishesBlindness(t *testing.T) {
	liq := liqOf(3.1, 20)
	liq.DistinctGifts, liq.Velocity = 20, 1.5

	base := evalPrice(t, 3, bookOf(42, 3, 3.5, 3.55, 3.6), liq, 3.5, 20)
	confirmed := WithCrossDepth(base, CrossMarket{Support: 3.5, Venues: 2, Asks: []float64{3.5, 3.52, 3.55}})
	blind := WithCrossDepth(base, CrossMarket{Unreachable: 1})

	if confirmed.ScoreBreakdown.Total <= base.ScoreBreakdown.Total {
		t.Errorf("two agreeing venues scored %.1f, no better than pricing alone at %.1f",
			confirmed.ScoreBreakdown.Total, base.ScoreBreakdown.Total)
	}
	if blind.ScoreBreakdown.Total >= base.ScoreBreakdown.Total {
		t.Errorf("a venue that never answered scored %.1f, no worse than a clean read at %.1f",
			blind.ScoreBreakdown.Total, base.ScoreBreakdown.Total)
	}
}

// Eight tidy prints used to read as 81% confident. Sample size now scales the
// whole judgement rather than topping up a generous floor.
func TestConfidenceNeedsSamplesNotJustTidiness(t *testing.T) {
	thin := liqOf(3.02, 8)
	thin.DistinctGifts, thin.MADRatio, thin.Velocity = 8, .02, .57
	deep := liqOf(3.02, 40)
	deep.DistinctGifts, deep.MADRatio, deep.Velocity = 40, .02, 2.5

	thinConf := confidence(thin, AttributeValue{})
	deepConf := confidence(deep, AttributeValue{})

	if thinConf > .6 {
		t.Errorf("eight prints gave %.0f%% confidence", thinConf*100)
	}
	if deepConf <= thinConf {
		t.Errorf("forty prints (%.2f) must beat eight (%.2f)", deepConf, thinConf)
	}
}

// A model that barely trades must not outrank a liquid one on the strength of a
// flattering day estimate derived from a single competitor.
func TestIlliquidModelRanksBelowALiquidOneAtTheSameEdge(t *testing.T) {
	slow := liqOf(3.1, 8)
	slow.DistinctGifts, slow.Velocity = 8, .2
	fast := liqOf(3.1, 8)
	fast.DistinctGifts, fast.Velocity = 8, 3

	book := bookOf(42, 3, 3.5, 3.55, 3.6)
	if s, f := evalPrice(t, 3, book, slow, 3.5, 10), evalPrice(t, 3, book, fast, 3.5, 10); s.ScoreBreakdown.Total >= f.ScoreBreakdown.Total {
		t.Errorf("0.2 sales/day scored %.1f against %.1f for 3/day", s.ScoreBreakdown.Total, f.ScoreBreakdown.Total)
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
