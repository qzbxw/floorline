package signal

import (
	"context"
	"math"
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

// seedSignal records a prior card for the same collection, as the batch and
// cooldown checks read them back.
func (h *harness) seedSignal(t *testing.T, giftID int64, model string, price float64, at time.Time) {
	t.Helper()
	_, err := h.st.InsertSignal(context.Background(), store.SignalRow{
		TS: at, Kind: KindBuy, GiftID: giftID,
		Key:   tonnel.ModelKey{Name: key.Name, Model: model},
		Price: price, Exit: price * 1.1, Edge: .1,
	})
	if err != nil {
		t.Fatalf("seed signal: %v", err)
	}
}

// Production, 12 Aug, 22:21–22:31: seven Lol Pop cards in ten minutes. Every
// one a different model — so no two shared the (collection, model) cooldown
// key — and every one priced at exactly 3.3. That is one seller emptying a
// collection, delivered as seven separate opportunities.
//
// It is not only noise. Several lots at one price is the absence of the
// scarcity the whole trade rests on: whatever we buy, our buyer can have the
// next one at the same number.
func TestOneSellerDumpingACollectionIsNotSevenOpportunities(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200, 1250)
	h.seedSales(t, 1200, 40, 40)
	h.seedStat(t, 800, 12)
	now := time.Now()

	// The same collection, three other models, all at our price, minutes ago.
	for i, model := range []string{"Lavender Ice", "Lychee Mint", "Wild Mango"} {
		h.seedSignal(t, int64(500+i), model, 800, now.Add(-time.Duration(i+1)*time.Minute))
	}

	dec, err := h.det.Evaluate(ctx, gift(candidateID, 800), defaultLimits(), now)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil {
		t.Fatal("the listing was dropped instead of being judged")
	}
	if dec.BatchPeers != 3 {
		t.Errorf("batch peers = %d, want the three lots already shown at this price", dec.BatchPeers)
	}
	if dec.Signal {
		t.Error("a fourth lot from the same dump was still pushed as a signal")
	}
	if !containsSubstring(dec.SignalFails, "распродажа одного продавца") {
		t.Errorf("the reason has to name the dump, got %v", dec.SignalFails)
	}
}

// The collection cooldown is about the chat, not about the trade: a second
// genuinely different model minutes after the first is still a real signal, it
// just does not get its own notification.
func TestASecondModelOfTheSameCollectionIsQuietButStillATrade(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200, 1250)
	h.seedSales(t, 1200, 40, 40)
	h.seedStat(t, 800, 12)
	now := time.Now()

	// One prior card, at a clearly different price so the batch rule stays out.
	h.seedSignal(t, 501, "Lavender Ice", 400, now.Add(-2*time.Minute))

	dec, err := h.det.Evaluate(ctx, gift(candidateID, 800), defaultLimits(), now)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil || !dec.Signal {
		t.Fatalf("a different model at a different price is a real signal: %+v", dec)
	}
	if dec.BatchPeers != 0 {
		t.Errorf("batch peers = %d, want none — the prior card was at another price", dec.BatchPeers)
	}
	if !dec.Suppressed {
		t.Error("a second card for the same collection within minutes must stay quiet")
	}
	if !dec.Auto {
		t.Errorf("the cooldown is about noise, not trading; auto failures: %v", dec.AutoFails)
	}
}

// Production, 12 Aug: a 53 GRAM Fine Pen was pushed as a signal while the
// ticket cap was 5.5 and the wallet held 25.1. Nobody could act on it, in
// either direction — that is not an opportunity, it is a 2am notification.
func TestUnaffordableLotIsNotASignal(t *testing.T) {
	h := newHarness(t, 800, 1200, 1250)
	h.seedSales(t, 1200, 40, 40)
	h.seedStat(t, 800, 12)
	h.det.Spendable = func() (float64, string, bool) { return 100, "баланс 104 − резерв 4", true }

	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil {
		t.Fatal("no decision at all")
	}
	if dec.Signal {
		t.Error("a lot 8x the spendable budget must not alert")
	}
	if !containsSubstring(dec.SignalFails, "поднять сейчас можно максимум") {
		t.Errorf("the refusal must name the budget: %v", dec.SignalFails)
	}

	// The same listing with money behind it is still a signal.
	h.det.Spendable = func() (float64, string, bool) { return 5000, "лимит на сделку 5000", true }
	dec, err = h.det.EvaluateFresh(context.Background(), gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !dec.Signal {
		t.Errorf("affordable and profitable, but rejected: %v", dec.SignalFails)
	}
}

// The overpriced-listing guard, end to end: a hole in the book plus a cheaper
// external queue must not reach the operator as a buy.
func TestOverpricedListingIsRejectedWithAnExplanation(t *testing.T) {
	h := newHarness(t, 6, 4.21, 7.9, 10.2)
	h.seedSales(t, 3.85, 9, 9)
	h.seedStat(t, 4.21, 13)
	h.cfg.Sig.MinPrice, h.cfg.Sig.MinSales, h.cfg.Sig.MinVelocity = 0.1, 5, 0.1
	h.det.CrossSupport = func(context.Context, pricing.Valuation) pricing.CrossMarket {
		return pricing.CrossMarket{Support: 5, Venues: 1, Asks: []float64{4, 5, 5, 5.89, 6}}
	}

	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 6), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil {
		t.Fatal("no decision at all")
	}
	if dec.Signal {
		t.Fatalf("6 GRAM behind a 4.21 ask and a 4 / 5 / 5 Portals queue must not be a signal: exit %.3f", dec.Val.FastExit)
	}
	// The hole is what used to invent the edge: 4.21 / 7.90 / 10.20 has one
	// price point of real liquidity, not three.
	if dec.Val.LiveDepthCount != 1 || !dec.Val.DepthCapped {
		t.Errorf("the gappy book was still trusted: depth %.3f over %d asks (capped=%v)",
			dec.Val.LiveDepth, dec.Val.LiveDepthCount, dec.Val.DepthCapped)
	}
	if dec.Val.AsksBelowEntry < 5 {
		t.Errorf("asks below entry = %d, want the Tonnel 4.21 plus the Portals queue", dec.Val.AsksBelowEntry)
	}
	if !containsSubstring(dec.SignalFails, "выше быстрого выхода") {
		t.Errorf("the refusal must say why: %v", dec.SignalFails)
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

// A flat edge bar prices two very different trades identically.
//
// Three percent on a model doing 2.5 deduplicated sales a day is a position you
// are out of by tomorrow. The same three percent on one selling twice a week is
// a fortnight of capital tied up for the price of a rounding error — and that
// is the trade the desk keeps getting stuck in. So the bar moves with how fast
// a mistake can be unwound.
func TestEdgeBarScalesWithHowFastYouCouldGetOut(t *testing.T) {
	const base = 0.045
	cases := []struct {
		name     string
		velocity float64
		want     float64
	}{
		{"liquid model gets a little relief", 2.57, base - liquidRelief},
		{"ordinary model pays the base", 1.4, base},
		{"thin model pays for its illiquidity", 0.5, base + thinPenalty},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := requiredEdge(base, pricing.Liquidity{Velocity: c.velocity}, pricing.RegimeNeutral)
			if math.Abs(got-c.want) > 1e-9 {
				t.Errorf("requiredEdge at %.2f/day = %.4f, want %.4f", c.velocity, got, c.want)
			}
			// The reason travels with the number, or the threshold in the
			// rejection message looks arbitrary.
			if liquidityNote(pricing.Liquidity{Velocity: c.velocity}) == "" {
				t.Error("the band has no explanation")
			}
		})
	}
	// The ordering is the property that matters: illiquidity is never cheaper.
	thin := requiredEdge(base, pricing.Liquidity{Velocity: .3}, pricing.RegimeNeutral)
	liquid := requiredEdge(base, pricing.Liquidity{Velocity: 3}, pricing.RegimeNeutral)
	if thin <= liquid {
		t.Errorf("a model selling twice a week (%.3f) must not be easier to clear than one selling three times a day (%.3f)", thin, liquid)
	}
}

// Production, 13 Aug: Liberty Figure / Peridot at 4.02 with a 4.158 exit —
// +3.4% raw, 2.5% after the risk buffer, with three sellers within 5% of the
// exit. The model is genuinely liquid, and it still is not worth 0.14 GRAM of
// NFT risk.
func TestThinEdgeOnALiquidModelIsStillRejected(t *testing.T) {
	h := newHarness(t, 800, 830, 840)
	h.seedSales(t, 800, 60, 40)
	h.seedStat(t, 830, 46)

	dec, err := h.det.Evaluate(context.Background(), gift(candidateID, 800), defaultLimits(), time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil {
		t.Fatal("the listing was dropped instead of being judged")
	}
	if dec.Signal {
		t.Errorf("a ~3%% edge was pushed as a signal: edge %.1f%% fails %v",
			dec.Val.Edge*100, dec.SignalFails)
	}
}

// The sweep of the standing book cannot afford a venue read per listing: the
// marketplaces are paced like a person tapping through a mini app, and a pass
// over forty models times five asks could not finish one sweep inside its own
// deadline. Every candidate then came back with the venues unreachable — which
// caps the score and blocks unattended buying — so the sweep hands in one quote
// per model and every listing in that book is priced against it.
func TestEvaluateWithCrossUsesTheSuppliedQuoteAndAsksNoVenue(t *testing.T) {
	h := newHarness(t, 4.00, 4.60, 4.62, 4.65)
	h.seedSales(t, 4.4, 30, 30)
	h.seedStat(t, 4.00, 40)

	fetches := 0
	h.det.CrossSupport = func(context.Context, pricing.Valuation) pricing.CrossMarket {
		fetches++
		return pricing.CrossMarket{Support: 9, Asks: []float64{9, 9.1}, Venues: 1}
	}

	cm := pricing.CrossMarket{Support: 4.3, Asks: []float64{4.3, 4.35, 4.4}, Venues: 2}
	dec, err := h.det.EvaluateWithCross(context.Background(), gift(candidateID, 4.00), risk.Limits{}, time.Now(), cm)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if dec == nil || !dec.Val.Valid {
		t.Fatalf("no valuation: %+v", dec)
	}
	if fetches != 0 {
		t.Errorf("the venues were queried %d times; the caller had already read them", fetches)
	}
	if dec.Val.CrossMarketSupport != 4.3 {
		t.Errorf("cross support = %.2f, want the supplied 4.30", dec.Val.CrossMarketSupport)
	}
	// The supplied queue has to bound the exit exactly as a freshly fetched one
	// would: 4.30 elsewhere costs a buyer less than our own 4.60 next ask.
	want := 4.30 / (1 + h.cfg.TonnelFee) * (1 - h.cfg.Undercut)
	if math.Abs(dec.Val.FastExit-want) > 0.005 {
		t.Errorf("fast exit = %.4f, want %.4f — the external queue must cap it", dec.Val.FastExit, want)
	}
	if !dec.Val.ExitFromCross {
		t.Error("the exit came from another venue's queue and does not say so")
	}
}
