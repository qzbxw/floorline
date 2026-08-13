package app

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"floorline/internal/config"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

func lifecycleApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &App{st: st, cfg: &config.Config{TonnelFee: .005}, alertCooldown: make(map[string]time.Time)}, st
}

func TestReconcileTracksManualRepriceAndUnlist(t *testing.T) {
	ctx := context.Background()
	a, st := lifecycleApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	key := tonnel.ModelKey{Name: "Snake Box", Model: "Bluebell"}
	if err := st.UpsertPosition(ctx, store.Position{GiftID: 7, GiftNum: 9, Key: key, BuyPrice: 10, BoughtAt: now.Add(-time.Hour), ListPrice: 12, ListedAt: now.Add(-time.Hour), Status: store.StatusListed, Source: "import"}); err != nil {
		t.Fatal(err)
	}
	g := tonnel.Gift{GiftID: 7, GiftNum: 9, Name: key.Name, Model: key.Model, Price: 13, Asset: tonnel.AssetGRAM}
	if err := a.reconcileOwned(ctx, g, true, now); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetPosition(ctx, 7)
	if p.ListPrice != 13 || p.Status != store.StatusListed {
		t.Fatalf("repriced=%+v", p)
	}
	events, _ := st.PositionEvents(ctx, 7, 10)
	if len(events) != 1 || events[0].Kind != "repriced" {
		t.Fatalf("events=%+v", events)
	}
	if err := a.reconcileOwned(ctx, g, false, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetPosition(ctx, 7)
	if p.ListPrice != 0 || p.Status != store.StatusOpen {
		t.Fatalf("unlisted=%+v", p)
	}
	events, _ = st.PositionEvents(ctx, 7, 10)
	if events[0].Kind != "unlisted" {
		t.Fatalf("events=%+v", events)
	}
}

func TestMissingPositionIsNotSoldUntilTradeConfirmsIt(t *testing.T) {
	ctx := context.Background()
	a, st := lifecycleApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	key := tonnel.ModelKey{Name: "Snake Box", Model: "Bluebell"}
	p := store.Position{GiftID: 8, GiftNum: 10, Key: key, BuyPrice: 10, BoughtAt: now.Add(-time.Hour), ListPrice: 12, ListedAt: now.Add(-30 * time.Minute), Status: store.StatusListed, Source: "manual"}
	if err := st.UpsertPosition(ctx, p); err != nil {
		t.Fatal(err)
	}
	a.closePosition(ctx, p, now)
	missing, _ := st.GetPosition(ctx, 8)
	if missing.Status != store.StatusMissing || missing.SellPrice != 0 {
		t.Fatalf("absence fabricated sale: %+v", missing)
	}
	if _, err := st.InsertSales(ctx, []tonnel.Sale{{GiftID: 8, GiftName: key.Name, Model: key.Model, Price: 12.5, Timestamp: tonnel.FlexTime{Time: now.Add(time.Minute)}}}); err != nil {
		t.Fatal(err)
	}
	a.closePosition(ctx, *missing, now.Add(2*time.Minute))
	sold, _ := st.GetPosition(ctx, 8)
	if sold.Status != store.StatusSold || sold.SellPrice != 12.5 {
		t.Fatalf("confirmed sale not closed: %+v", sold)
	}
}

// seedSale writes one trade of a physical gift into the tape.
func seedSale(t *testing.T, st *store.Store, giftID int64, key tonnel.ModelKey, price float64, at time.Time) {
	t.Helper()
	if _, err := st.InsertSales(context.Background(), []tonnel.Sale{{
		GiftID: tonnel.FlexInt(giftID), GiftNum: tonnel.FlexInt(giftID),
		GiftName: key.Name, Model: key.Model, Price: tonnel.Flex64(price),
		Timestamp: tonnel.FlexTime{Time: at},
	}}); err != nil {
		t.Fatalf("seed sale: %v", err)
	}
}

// Production, 13 Aug: a Fresh Socks bought by hand at 4.2 was imported at 3.8 —
// the price the *previous* owner had paid — and then reported as a +16.6%
// listing that was really break-even.
//
// The inventory poller sees a gift as ours before our own purchase reaches
// saleHistory, so the newest trade at that moment belongs to whoever held it
// before. That guess was written once at 85% confidence and never revisited.
func TestImportedCostBasisFollowsTheTapeInsteadOfFreezingTheWrongTrade(t *testing.T) {
	ctx := context.Background()
	a, st := lifecycleApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	key := tonnel.ModelKey{Name: "Fresh Socks", Model: "The Office"}
	g := tonnel.Gift{GiftID: 11, GiftNum: 11, Name: key.Name, Model: key.Model,
		Price: 4.46, Asset: tonnel.AssetGRAM}

	// The tape so far holds only the previous owner's purchase, four hours ago.
	seedSale(t, st, 11, key, 3.78, now.Add(-4*time.Hour))
	if err := a.reconcileOwned(ctx, g, true, now); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetPosition(ctx, 11)
	if p.CostConfidence >= .85 {
		t.Errorf("a guess from the tape was recorded at %.0f%% confidence", p.CostConfidence*100)
	}

	// Our own purchase lands in the tape a minute later. It is newer than what
	// we recorded, and we still hold the gift — so it can only be ours.
	seedSale(t, st, 11, key, 4.18, now.Add(-time.Minute))
	if err := a.reconcileOwned(ctx, g, true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetPosition(ctx, 11)
	if want := 4.18 * 1.005; math.Abs(p.BuyPrice-want) > 0.001 {
		t.Errorf("cost basis = %.3f, want %.3f — the newer trade is ours", p.BuyPrice, want)
	}
}

// An operator who has typed /cost has answered the question. Nothing the tape
// says afterwards may overwrite it.
func TestManualCostBasisIsNeverOverwrittenByTheTape(t *testing.T) {
	ctx := context.Background()
	a, st := lifecycleApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	key := tonnel.ModelKey{Name: "Fresh Socks", Model: "The Office"}

	if err := st.UpsertPosition(ctx, store.Position{
		GiftID: 12, GiftNum: 12, Key: key, BuyPrice: 4.2, BoughtAt: now.Add(-2 * time.Hour),
		CostSource: "manual", CostConfidence: 1, Status: store.StatusOpen, Source: "import",
	}); err != nil {
		t.Fatal(err)
	}
	seedSale(t, st, 12, key, 3.1, now.Add(-time.Minute))

	g := tonnel.Gift{GiftID: 12, GiftNum: 12, Name: key.Name, Model: key.Model, Asset: tonnel.AssetGRAM}
	if err := a.reconcileOwned(ctx, g, false, now); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetPosition(ctx, 12)
	if p.BuyPrice != 4.2 {
		t.Errorf("cost basis = %.3f, want the operator's 4.2", p.BuyPrice)
	}
}

// A gift held since before the bot existed has no acquisition in the tape we
// hold. Guessing from a months-old trade is worse than admitting it.
func TestAncientTradeIsNotMistakenForOurPurchase(t *testing.T) {
	ctx := context.Background()
	a, st := lifecycleApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	key := tonnel.ModelKey{Name: "Swag Bag", Model: "Choco Kush"}

	seedSale(t, st, 13, key, 2.5, now.Add(-30*24*time.Hour))
	g := tonnel.Gift{GiftID: 13, GiftNum: 13, Name: key.Name, Model: key.Model, Asset: tonnel.AssetGRAM}
	if err := a.reconcileOwned(ctx, g, false, now); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetPosition(ctx, 13)
	if p.BuyPrice != 0 || p.CostSource != "unknown" {
		t.Errorf("position = {buy %.3f, source %q}, want an admitted unknown", p.BuyPrice, p.CostSource)
	}
}

// Production, 13 Aug: "Продано — Snoop Dogg · Shower Cap. Вход 4.894 → выход
// 4.87. Нет −0.024 (−0.5%), держали 2с."
//
// The gift was bought at 4.87 (4.894 with the referral) and listed at 5.47. The
// "sale" was our own purchase read back off the tape: the marketplace stamps a
// trade when it books it, a second or two after we recorded sending the order,
// so "the newest trade after bought_at" matched it perfectly. The position was
// closed at a small loss while it was still on the market.
func TestOurOwnPurchaseIsNotBookedAsTheSale(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	p := store.Position{
		GiftID: 21, BuyPrice: 4.894, BoughtAt: now,
		ListPrice: 5.47, ListedAt: now, Status: store.StatusListed,
	}

	// The tape's record of our own purchase: the ask, booked two seconds late.
	if !ourAcquisition(p, store.Trade{Price: 4.87, TS: now.Add(2 * time.Second)}, .005) {
		t.Error("our own purchase was not recognised")
	}
	// The real exit, at the price we listed at.
	if ourAcquisition(p, store.Trade{Price: 5.47, TS: now.Add(3 * time.Minute)}, .005) {
		t.Error("a sale at our ask was mistaken for the purchase")
	}
	// Same price much later is a different trade — the gift came back and went
	// again — and must not be filtered out.
	if ourAcquisition(p, store.Trade{Price: 4.87, TS: now.Add(2 * time.Hour)}, .005) {
		t.Error("an unrelated later trade was filtered as the purchase")
	}
	// A position with no known entry cannot make this judgement, and must not
	// pretend to: filtering on a guess would hide real sales.
	blank := store.Position{GiftID: 22}
	if ourAcquisition(blank, store.Trade{Price: 4.87, TS: now}, .005) {
		t.Error("filtered a trade for a position with no recorded entry")
	}
}

// The end-to-end shape of the same bug: reconcile sees the gift gone, the tape
// holds only our purchase, and the position must go to `missing` rather than be
// booked as a sale.
func TestPositionWithOnlyItsPurchaseOnTapeGoesMissingNotSold(t *testing.T) {
	ctx := context.Background()
	a, st := lifecycleApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	key := tonnel.ModelKey{Name: "Snoop Dogg", Model: "Shower Cap"}

	if err := st.UpsertPosition(ctx, store.Position{
		GiftID: 21, GiftNum: 21, Key: key, BuyPrice: 4.894, BoughtAt: now,
		CostSource: "floorline", CostConfidence: 1,
		ListPrice: 5.47, ListedAt: now, Status: store.StatusListed, Source: "auto",
	}); err != nil {
		t.Fatal(err)
	}
	seedSale(t, st, 21, key, 4.87, now.Add(2*time.Second))

	p, _ := st.GetPosition(ctx, 21)
	a.closePosition(ctx, *p, now.Add(time.Minute))

	got, _ := st.GetPosition(ctx, 21)
	if got.Status == store.StatusSold {
		t.Fatalf("position closed at %.3f from its own purchase", got.SellPrice)
	}
	if got.Status != store.StatusMissing {
		t.Errorf("status = %q, want missing while the sale is unconfirmed", got.Status)
	}
}
