package pricing

import (
	"math"
	"testing"
	"time"

	"floorline/internal/store"
)

func salesAt(now time.Time, entries ...entry) []store.SaleRow {
	out := make([]store.SaleRow, 0, len(entries))
	for i, e := range entries {
		out = append(out, store.SaleRow{
			GiftID:  int64(i + 1),
			GiftNum: e.gift,
			TS:      now.Add(-time.Duration(e.agoDays * float64(24*time.Hour))),
			Price:   e.price,
		})
	}
	return out
}

// entry is one synthetic trade: how long ago, at what price, of which gift.
type entry = struct {
	agoDays float64
	price   float64
	gift    int64
}

func TestComputeLiquidityBasics(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour

	sales := salesAt(now,
		entry{1, 100, 1}, entry{2, 110, 2}, entry{3, 90, 3},
		entry{8, 105, 1}, entry{10, 95, 4}, entry{12, 100, 5},
	)

	l := ComputeLiquidity(sales, now, window, window)

	if l.Sales != 5 {
		t.Errorf("sales = %d, want 5 unique physical gifts", l.Sales)
	}
	if l.Sales7 != 3 {
		t.Errorf("sales in the last 7 days = %d, want 3", l.Sales7)
	}
	if l.DistinctGifts != 5 {
		t.Errorf("distinct gifts = %d, want 5", l.DistinctGifts)
	}
	if l.Median != 100 {
		t.Errorf("median = %v, want 100", l.Median)
	}
	if math.Abs(l.Velocity-5.0/14) > 1e-9 {
		t.Errorf("velocity = %v, want %v", l.Velocity, 5.0/14)
	}
	if !l.Traded() {
		t.Error("a model with six trades must count as traded")
	}
}

func TestRepeatedSalesOfOnePhysicalGiftCountOnce(t *testing.T) {
	now := time.Now()
	l := ComputeLiquidity(salesAt(now,
		entry{0.1, 70, 7}, entry{1, 100, 7}, entry{2, 100, 7},
		entry{3, 120, 8},
	), now, 14*24*time.Hour, 14*24*time.Hour)

	if l.Sales != 2 || l.DistinctGifts != 2 {
		t.Fatalf("liquidity = %+v, want two independent physical gifts", l)
	}
	if l.Median != 95 {
		t.Errorf("median = %v, want newest sale of gift 7 (70) and gift 8 (120)", l.Median)
	}
}

// Turnover is the only wash-trading signal available: the endpoint carries no
// counterparty identities. It has to be measured against the raw tape, because
// comparing distinct gifts against the already-deduplicated count makes it
// identically 1 and the AUTOBUY_MIN_TURNOVER gate unreachable.
func TestTurnoverMeasuresTheRawTapeNotTheDeduplicatedOne(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour

	// Four prints, but only two physical gifts changing hands.
	washed := ComputeLiquidity(salesAt(now,
		entry{1, 100, 7}, entry{2, 100, 7},
		entry{3, 120, 8}, entry{4, 120, 8},
	), now, window, window)

	if washed.Prints != 4 {
		t.Errorf("prints = %d, want the four raw trades", washed.Prints)
	}
	if washed.DistinctGifts != 2 {
		t.Errorf("distinct gifts = %d, want 2", washed.DistinctGifts)
	}
	if math.Abs(washed.Turnover-0.5) > 1e-9 {
		t.Errorf("turnover = %.3f, want 0.5 — two gifts behind four prints", washed.Turnover)
	}

	// A genuine market: every print is a different gift.
	clean := ComputeLiquidity(salesAt(now,
		entry{1, 100, 1}, entry{2, 100, 2}, entry{3, 120, 3}, entry{4, 120, 4},
	), now, window, window)

	if clean.Turnover != 1 {
		t.Errorf("turnover = %.3f, want 1 when every print is a different gift", clean.Turnover)
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

// The tape a flipper is denied by is almost never the collection's — it is the
// small sample belonging to one model inside it. Vice Cream empties every day
// and Berry Shake, one model of it, showed 0.71 sales a day and was refused.
func TestPeersLiftAThinModelTowardTheCollection(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour

	// Three prints of our own: an estimate with a big error bar, not a verdict.
	l := ComputeLiquidity(salesAt(now,
		entry{1, 100, 1}, entry{5, 100, 2}, entry{9, 100, 3},
	), now, window, window)
	own := l.Velocity

	// The collection around it: 140 different gifts across 10 models in the same
	// fortnight — one model of it sees 1 a day on average.
	p := ComputePeers(store.CollectionTape{Prints: 160, Sales: 140, Models: 10}, window, window)
	if math.Abs(p.PerModel-1) > 1e-9 {
		t.Fatalf("per-model rate = %.3f, want 1.00", p.PerModel)
	}

	l = WithPeerSupport(l, p)
	if !l.PeerSupported {
		t.Fatal("a three-print model in a liquid collection was left to its own tape")
	}
	if l.ModelVelocity != own {
		t.Errorf("model velocity = %.3f, want the untouched %.3f", l.ModelVelocity, own)
	}
	if l.Velocity <= own {
		t.Errorf("velocity = %.3f, want it lifted above the model's own %.3f", l.Velocity, own)
	}
	// The cap is the other half of the point: peers vouch, they do not testify.
	if l.Velocity > own*maxPeerLift+1e-9 {
		t.Errorf("velocity = %.3f, want no more than %.0f× the model's own %.3f", l.Velocity, maxPeerLift, own)
	}
}

// A model nobody trades is exactly the case the cascade must not paper over.
func TestPeersLendNothingToAModelWithNoTape(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour
	p := ComputePeers(store.CollectionTape{Prints: 160, Sales: 140, Models: 10}, window, window)

	one := WithPeerSupport(ComputeLiquidity(salesAt(now, entry{2, 100, 1}), now, window, window), p)
	if one.PeerSupported || one.Velocity != one.ModelVelocity {
		t.Errorf("a single print borrowed the collection's rate: %.3f vs own %.3f", one.Velocity, one.ModelVelocity)
	}
	none := WithPeerSupport(ComputeLiquidity(nil, now, window, window), p)
	if none.Velocity != 0 {
		t.Errorf("an untraded model was given %.3f sales a day", none.Velocity)
	}
}

// The collection has to be a market itself before it can vouch for anything.
func TestThinCollectionVouchesForNobody(t *testing.T) {
	window := 14 * 24 * time.Hour
	shallow := ComputePeers(store.CollectionTape{Prints: 12, Sales: 11, Models: 6}, window, window)
	if shallow.PerModel != 0 {
		t.Errorf("eleven trades produced a per-model rate of %.3f", shallow.PerModel)
	}
	narrow := ComputePeers(store.CollectionTape{Prints: 90, Sales: 80, Models: 2}, window, window)
	if narrow.PerModel != 0 {
		t.Errorf("two models produced a per-model rate of %.3f", narrow.PerModel)
	}
}

// Confidence is about the price, so peers move it much less than they move the
// trade rate — but "8% доверия" on a model with two prints inside a collection
// with two hundred is understating what is known.
func TestPeersAreDiscountedEvidenceForConfidence(t *testing.T) {
	now := time.Now()
	window := 14 * 24 * time.Hour
	thin := ComputeLiquidity(salesAt(now,
		entry{1, 100, 1}, entry{4, 100, 2},
	), now, window, window)

	alone := confidence(thin, AttributeValue{})
	supported := confidence(WithPeerSupport(thin,
		ComputePeers(store.CollectionTape{Prints: 240, Sales: 220, Models: 12}, window, window)), AttributeValue{})

	if supported <= alone {
		t.Errorf("confidence = %.2f with a deep collection, %.2f without", supported, alone)
	}
	if supported > .5 {
		t.Errorf("confidence = %.2f — peers alone must not make a two-print model a known price", supported)
	}
}
