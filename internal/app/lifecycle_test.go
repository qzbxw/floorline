package app

import (
	"context"
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
