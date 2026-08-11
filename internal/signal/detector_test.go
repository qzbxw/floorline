package signal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"floorline/internal/config"
	"floorline/internal/pricing"
	"floorline/internal/risk"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

var key = tonnel.ModelKey{Name: "Plush Pepe", Model: "Pink Diamond"}

// fakeBook serves a fixed ask ladder and counts how often it is asked, so the
// tests can prove the detector does not spend a request on hopeless candidates.
type fakeBook struct {
	prices []float64
	calls  int
	err    error
}

func (f *fakeBook) ModelBook(ctx context.Context, k tonnel.ModelKey, limit int) ([]tonnel.Gift, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]tonnel.Gift, 0, len(f.prices))
	for i, p := range f.prices {
		out = append(out, tonnel.Gift{
			GiftID: tonnel.FlexInt(100 + i),
			Name:   k.Name,
			Model:  k.Model + " (0.4%)",
			Price:  tonnel.Flex64(p),
			Asset:  tonnel.AssetGRAM,
		})
	}
	return out, nil
}

func testConfig() *config.Config {
	return &config.Config{
		TonnelFee:    0.005,
		Undercut:     0.01,
		LookbackDays: 14,
		BookCacheTTL: time.Millisecond,
		Sig: config.SignalGates{
			MinEdge: 0.05, MinVelocity: 1.0, MinSales: 10,
			MaxMADRatio: 0.35, MinTrend: 0.90, MinPrice: 1,
		},
		Auto: config.AutoGates{
			MinEdge: 0.10, MinVelocity: 2.0, MinSales: 20, MinTurnover: 0.6,
			MaxMADRatio: 0.25, MinTrend: 0.95, MaxDataAge: 5 * time.Minute,
		},
	}
}

type harness struct {
	det  *Detector
	st   *store.Store
	book *fakeBook
	cfg  *config.Config
}

func newHarness(t *testing.T, askPrices ...float64) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sig.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := testConfig()
	fb := &fakeBook{prices: askPrices}
	det := New(st, pricing.NewBookCache(fb, cfg.BookCacheTTL, 10), cfg)
	det.Coverage = func() time.Duration { return 14 * 24 * time.Hour }
	det.Warm = func() bool { return true }
	now := time.Now().UTC()
	if err := st.InsertGramQuotes(context.Background(), []store.GramQuote{
		{TS: now.Add(-time.Hour), USD: 1},
		{TS: now.Add(-15 * time.Minute), USD: 1},
		{TS: now, USD: 1},
	}); err != nil {
		t.Fatalf("seed GRAM quotes: %v", err)
	}

	return &harness{det: det, st: st, book: fb, cfg: cfg}
}

// seedSales writes `count` trades at `price` spread over the lookback window,
// cycling through `distinct` different gifts so turnover can be controlled.
func (h *harness) seedSales(t *testing.T, price float64, count, distinct int) {
	t.Helper()
	now := time.Now()
	sales := make([]tonnel.Sale, 0, count)
	for i := 0; i < count; i++ {
		sales = append(sales, tonnel.Sale{
			GiftID:    tonnel.FlexInt(9000 + i),
			GiftName:  key.Name,
			Model:     key.Model + " (0.4%)",
			Price:     tonnel.Flex64(price),
			GiftNum:   tonnel.FlexInt(int64(i%maxInt(distinct, 1)) + 1),
			Timestamp: tonnel.FlexTime{Time: now.Add(-time.Duration(i) * 6 * time.Hour)},
		})
	}
	if _, err := h.st.InsertSales(context.Background(), sales); err != nil {
		t.Fatalf("seed sales: %v", err)
	}
}

func (h *harness) seedStat(t *testing.T, floor float64, supply int) {
	t.Helper()
	err := h.st.ReplaceModelStats(context.Background(),
		[]tonnel.ModelStat{{Key: key, Floor: floor, Supply: supply, Rarity: 0.4}}, time.Now())
	if err != nil {
		t.Fatalf("seed stat: %v", err)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// candidateID matches the first ask the fake book serves, so the candidate is
// genuinely part of its own order book — exactly as it is in production. Getting
// this wrong would let the listing be priced against itself.
const candidateID = 100

func gift(id int64, price float64) tonnel.Gift {
	return tonnel.Gift{
		GiftID: tonnel.FlexInt(id),
		Name:   key.Name,
		Model:  key.Model + " (0.4%)",
		Price:  tonnel.Flex64(price),
		Asset:  tonnel.AssetGRAM,
	}
}

func defaultLimits() risk.Limits {
	l := risk.DefaultLimits()
	l.MaxTicket = 10000
	l.DailyBudget = 100000
	return l
}

// A clean opportunity: deeply liquid model, plenty of room above the entry.
func TestSignalAndAutoPassForALiquidMispricing(t *testing.T) {
	h := newHarness(t, 800, 1200, 1250)
	h.seedSales(t, 1200, 40, 40)
	h.seedStat(t, 800, 12)

	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil {
		t.Fatal("a clearly profitable listing produced no decision")
	}
	if !dec.Signal {
		t.Fatalf("signal gate failed: %v", dec.SignalFails)
	}
	if !dec.Auto {
		t.Errorf("auto gate failed: %v", dec.AutoFails)
	}
	if dec.Score <= 0 {
		t.Errorf("score = %v, want positive", dec.Score)
	}
}

// The trap: a listing far below the median, with a competing ask right above.
func TestNoSignalWhenTheNextAskKillsTheEdge(t *testing.T) {
	h := newHarness(t, 900, 905, 910)
	h.seedSales(t, 1200, 40, 10)
	h.seedStat(t, 900, 30)

	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 900), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec != nil && dec.Signal {
		t.Errorf("signalled a listing whose only exit loses money: edge %.3f", dec.Val.Edge)
	}
}

// Illiquidity must block auto-buy even when the arithmetic looks great: a huge
// discount on something that trades twice a month is inventory, not profit.
func TestIlliquidModelIsNotAutoBought(t *testing.T) {
	h := newHarness(t, 500, 1200)
	h.seedSales(t, 1200, 16, 14) // unique gifts clear signal, not the auto gate
	h.seedStat(t, 500, 5)

	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 500), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil || !dec.Signal {
		t.Fatalf("expected a signal, got %+v", dec)
	}
	if dec.Auto {
		t.Error("a model with 16 trades in 14 days must not be bought unattended")
	}
	if !containsSubstring(dec.AutoFails, "сделок") {
		t.Errorf("auto failures = %v, want one about the trade count", dec.AutoFails)
	}
}

// Repeated flips of a few physical gifts must not manufacture a signal at all.
func TestWashTradeGuardBlocksAutoBuy(t *testing.T) {
	h := newHarness(t, 800, 1200, 1250)
	h.seedSales(t, 1200, 40, 2) // 40 trades, but only two different gifts
	h.seedStat(t, 800, 12)

	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec != nil && dec.Signal {
		t.Fatalf("two gifts behind forty prints manufactured a signal: %+v", dec)
	}
}

func TestWarmUpBlocksAutoBuy(t *testing.T) {
	h := newHarness(t, 800, 1200, 1250)
	h.seedSales(t, 1200, 40, 40)
	h.seedStat(t, 800, 12)
	h.det.Warm = func() bool { return false }

	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec.Auto {
		t.Error("auto-buy must be held back until the history is warm")
	}
	if !dec.Signal {
		t.Error("warm-up should not suppress the alert itself")
	}
}

func TestGramVolatilityBlocksAutoButKeepsManualSignal(t *testing.T) {
	h := newHarness(t, 800, 1200, 1250)
	h.cfg.Auto.MaxGramMove15m = .03
	now := time.Now().UTC()
	if err := h.st.InsertGramQuotes(context.Background(), []store.GramQuote{{TS: now, USD: 1.1}}); err != nil {
		t.Fatal(err)
	}
	h.seedSales(t, 1200, 40, 40)
	h.seedStat(t, 800, 12)
	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 800), defaultLimits(), now)
	if err != nil || dec == nil || !dec.Signal {
		t.Fatalf("manual signal = %+v, err=%v", dec, err)
	}
	if dec.Auto || !containsSubstring(dec.AutoFails, "GRAM сходил") {
		t.Fatalf("volatile GRAM must block auto only: %+v", dec.AutoFails)
	}
}

func TestSlowExitBlocksAutoBuy(t *testing.T) {
	h := newHarness(t, 800, 1200, 1210, 1220, 1230)
	h.seedSales(t, 1200, 30, 30) // ~2.1 trades/day, all different gifts
	h.seedStat(t, 800, 12)

	limits := defaultLimits()
	limits.MaxExitDays = 0.5

	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("baseline evaluate: %v", err)
	}
	if !dec.Auto {
		t.Fatalf("baseline should auto-buy, failures: %v", dec.AutoFails)
	}

	// Same listing, stricter patience: now the queue of near-priced sellers
	// pushes the expected exit past the limit.
	dec2, err := h.det.Evaluate(context.Background(), gift(candidateID, 800), limits, time.Now())
	if err != nil {
		t.Fatalf("strict evaluate: %v", err)
	}
	if dec2.Auto {
		t.Errorf("expected the exit-time limit to block auto-buy, expected days %.2f", dec2.Val.ExpectedDays)
	}
	if !containsSubstring(dec2.AutoFails, "дольше лимита") {
		t.Errorf("auto failures = %v, want the exit-time limit", dec2.AutoFails)
	}
}

// A low historical median must not short-circuit live price discovery. That
// optimisation used to hide exactly the stale-history opportunities the exit
// model is meant to identify.
func TestLowMedianStillChecksTheLiveBook(t *testing.T) {
	h := newHarness(t, 1000, 1100)
	h.seedSales(t, 1000, 40, 10)
	h.seedStat(t, 1000, 12)

	dec, err := h.det.Evaluate(context.Background(), gift(1, 1000), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil {
		t.Fatal("the live book was never allowed to challenge stale history")
	}
	if h.book.calls != 1 {
		t.Errorf("book was fetched %d times, want one price-discovery read", h.book.calls)
	}
}

func TestUntradableRowsAreSkippedEntirely(t *testing.T) {
	h := newHarness(t, 500, 1200)
	h.seedSales(t, 1200, 40, 10)
	h.seedStat(t, 500, 12)

	bundle := gift(-1, 500)
	premarket := gift(2, 500)
	premarket.Premarket = true
	tgMarket := gift(3, 500)
	tgMarket.TelegramMarketplace = true
	usdt := gift(4, 500)
	usdt.Asset = "USDT"
	dust := gift(5, 0.001)

	for name, g := range map[string]tonnel.Gift{
		"bundle": bundle, "premarket": premarket, "telegram marketplace": tgMarket,
		"usdt": usdt, "dust": dust,
	} {
		dec, err := h.det.Evaluate(context.Background(), g, defaultLimits(), time.Now())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if dec != nil {
			t.Errorf("%s listing produced a decision; its price does not mean what it looks like", name)
		}
	}
	if h.book.calls != 0 {
		t.Errorf("untradable rows triggered %d book lookups", h.book.calls)
	}
}

func TestOwnListingIsSkippedBeforeBookLookup(t *testing.T) {
	h := newHarness(t, 800, 1200)
	h.det.OwnerID = func() int64 { return 777 }
	g := gift(candidateID, 800)
	g.Seller = 777

	dec, err := h.det.Evaluate(context.Background(), g, defaultLimits(), time.Now())
	if err != nil || dec != nil {
		t.Fatalf("own listing produced decision=%+v err=%v", dec, err)
	}
	if h.book.calls != 0 {
		t.Fatalf("own listing reached the order book: %d calls", h.book.calls)
	}
}

func TestDedupeAllowsAPriceDrop(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200, 1250)
	h.seedSales(t, 1200, 40, 40)
	h.seedStat(t, 800, 12)

	dec, err := h.det.Evaluate(ctx, gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil || dec == nil || !dec.Signal {
		t.Fatalf("first evaluation: %+v (%v)", dec, err)
	}
	if _, err := h.st.InsertSignal(ctx, store.SignalRow{
		TS: time.Now(), Kind: KindBuy, GiftID: candidateID, Key: key, Price: 800,
	}); err != nil {
		t.Fatalf("record signal: %v", err)
	}

	again, err := h.det.Evaluate(ctx, gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("repeat evaluation: %v", err)
	}
	if again != nil {
		t.Error("the same listing at the same price must not signal twice")
	}

	cheaper, err := h.det.Evaluate(ctx, gift(candidateID, 700), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("cheaper evaluation: %v", err)
	}
	if cheaper == nil || !cheaper.Signal {
		t.Error("a relist at a lower price is new news and must signal again")
	}
}

func TestMuteSuppressesTheAlertNotTheEvaluation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200, 1250)
	h.seedSales(t, 1200, 40, 40)
	h.seedStat(t, 800, 12)

	if err := h.st.SetMute(ctx, key.ID(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("mute: %v", err)
	}

	dec, err := h.det.Evaluate(ctx, gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil || !dec.Signal {
		t.Fatal("a mute must not change whether the listing is a signal")
	}
	if !dec.Suppressed {
		t.Error("a muted model must not produce a notification")
	}
	if !dec.Auto {
		t.Errorf("a mute is about noise, not about trading; auto failures: %v", dec.AutoFails)
	}
}

func TestGatesForExplainsAnAlreadySignalledListing(t *testing.T) {
	h := newHarness(t)
	v := pricing.Valuation{Valid: false, Reason: "no exit reference"}
	fails, autoFails := h.det.GatesFor(v, defaultLimits())
	if len(fails) != 1 || fails[0] != "no exit reference" {
		t.Errorf("fails = %v, want the valuation reason", fails)
	}
	if autoFails != nil {
		t.Errorf("auto failures = %v, want none for an unvaluable listing", autoFails)
	}
}

func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
