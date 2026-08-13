package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"floorline/internal/tonnel"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

var key = tonnel.ModelKey{Name: "Plush Pepe", Model: "Pink Diamond"}

func TestUpsertListingsReportsNewAndCheaper(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now()

	gifts := []tonnel.Gift{
		{GiftID: 1, Name: "Plush Pepe", Model: "Pink Diamond (0.4%)", Price: 1000, Asset: "TON"},
		{GiftID: 2, Name: "Plush Pepe", Model: "Pink Diamond (0.4%)", Price: 1100, Asset: "TON"},
	}

	changes, err := st.UpsertListings(ctx, gifts, now)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if len(changes.New) != 2 || len(changes.Cheaper) != 0 {
		t.Fatalf("first upsert: new=%v cheaper=%v, want 2 new", changes.New, changes.Cheaper)
	}

	// Same prices again: nothing has changed.
	changes, err = st.UpsertListings(ctx, gifts, now.Add(time.Second))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if len(changes.New) != 0 || len(changes.Cheaper) != 0 {
		t.Errorf("unchanged upsert reported new=%v cheaper=%v", changes.New, changes.Cheaper)
	}

	// A relist at a lower price is a fresh opportunity even on a known gift id.
	gifts[1].Price = 900
	changes, err = st.UpsertListings(ctx, gifts, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	if len(changes.Cheaper) != 1 || changes.Cheaper[0] != 2 {
		t.Errorf("cheaper = %v, want [2]", changes.Cheaper)
	}
	if len(changes.Candidates()) != 1 {
		t.Errorf("candidates = %v, want one", changes.Candidates())
	}
}

func TestUpsertListingsStripsRaritySuffixes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	_, err := st.UpsertListings(ctx, []tonnel.Gift{{
		GiftID: 7, Name: "Plush Pepe",
		Model: "Pink Diamond (0.4%)", Backdrop: "Desert Sand (1.5%)", Symbol: "Astronaut (0.4%)",
		Price: 1000, Asset: "TON",
	}}, time.Now())
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var model, backdrop string
	var modelRarity float64
	err = st.DB().QueryRow(`SELECT model, backdrop, model_rarity FROM listings WHERE gift_id = 7`).
		Scan(&model, &backdrop, &modelRarity)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if model != "Pink Diamond" || backdrop != "Desert Sand" {
		t.Errorf("stored model %q backdrop %q, want the base names", model, backdrop)
	}
	if modelRarity != 0.4 {
		t.Errorf("model rarity = %v, want 0.4 parsed from the suffix", modelRarity)
	}
}

func TestInsertSalesIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ts := tonnel.FlexTime{Time: time.Now().Truncate(time.Second)}

	sales := []tonnel.Sale{
		{GiftID: 1, GiftName: "Plush Pepe", Model: "Pink Diamond (0.4%)", Price: 1000, Timestamp: ts},
		{GiftID: 2, GiftName: "Plush Pepe", Model: "Pink Diamond (0.4%)", Price: 1100, Timestamp: ts},
	}

	n, err := st.InsertSales(ctx, sales)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted %d, want 2", n)
	}

	n, err = st.InsertSales(ctx, sales)
	if err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	if n != 0 {
		t.Errorf("re-inserting the same trades reported %d new rows, want 0", n)
	}
}

func TestInsertSalesSkipsUnusableRows(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	n, err := st.InsertSales(ctx, []tonnel.Sale{
		{GiftID: 1, GiftName: "Plush Pepe", Price: 100},                                               // no timestamp
		{GiftID: 2, Price: 100, Timestamp: tonnel.FlexTime{Time: time.Now()}},                         // no collection
		{GiftID: 3, GiftName: "Plush Pepe", Price: 0, Timestamp: tonnel.FlexTime{Time: time.Now()}},   // no price
		{GiftID: 4, GiftName: "Plush Pepe", Price: 100, Timestamp: tonnel.FlexTime{Time: time.Now()}}, // good
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted %d rows, want only the one usable trade", n)
	}
}

func TestSalesSinceFiltersByModelAndTime(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	_, err := st.InsertSales(ctx, []tonnel.Sale{
		{GiftID: 1, GiftName: "Plush Pepe", Model: "Pink Diamond (0.4%)", Price: 100,
			Timestamp: tonnel.FlexTime{Time: now.Add(-1 * time.Hour)}},
		{GiftID: 2, GiftName: "Plush Pepe", Model: "Pink Diamond (0.4%)", Price: 200,
			Timestamp: tonnel.FlexTime{Time: now.Add(-40 * 24 * time.Hour)}},
		{GiftID: 3, GiftName: "Plush Pepe", Model: "Blue Steel (2%)", Price: 300,
			Timestamp: tonnel.FlexTime{Time: now.Add(-1 * time.Hour)}},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := st.SalesSince(ctx, key, now.Add(-14*24*time.Hour))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0].Price != 100 {
		t.Errorf("got %+v, want only the recent Pink Diamond trade", rows)
	}
}

// The claim is what makes a double-tapped Buy button safe.
func TestClaimBuyIsExclusive(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now()

	ok, err := st.ClaimBuy(ctx, 42, 100, now)
	if err != nil || !ok {
		t.Fatalf("first claim = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = st.ClaimBuy(ctx, 42, 100, now)
	if err != nil {
		t.Fatalf("second claim errored: %v", err)
	}
	if ok {
		t.Error("the same listing was claimed twice; a retry could double-buy")
	}

	if err := st.FinishBuy(ctx, 42, "bought", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	n, err := st.BuysSince(ctx, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("buys in the last minute = %d, want 1", n)
	}
}

func TestSpendLedgerAccumulates(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	const day = "2026-08-11"

	for i := 0; i < 3; i++ {
		if err := st.AddSpend(ctx, day, 10); err != nil {
			t.Fatalf("add spend: %v", err)
		}
	}
	d, err := st.SpendToday(ctx, day)
	if err != nil {
		t.Fatalf("read spend: %v", err)
	}
	if d.Spent != 30 || d.Buys != 3 {
		t.Errorf("ledger = %+v, want spent 30 across 3 buys", d)
	}

	other, err := st.SpendToday(ctx, "2026-08-12")
	if err != nil {
		t.Fatalf("read other day: %v", err)
	}
	if other.Spent != 0 {
		t.Errorf("a different day should start empty, got %+v", other)
	}
}

func TestGramQuotesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	quotes := []GramQuote{{TS: now.Add(-time.Hour), USD: 1.25}, {TS: now, USD: 1.5, Bid: 1.49, Ask: 1.51, Change24: .08}}
	if err := st.InsertGramQuotes(ctx, quotes); err != nil {
		t.Fatalf("insert quotes: %v", err)
	}
	latest, ok, err := st.LatestGramQuote(ctx)
	if err != nil || !ok || latest.USD != 1.5 || latest.Bid != 1.49 {
		t.Fatalf("latest = %+v, %v, %v", latest, ok, err)
	}
	at, ok, err := st.GramQuoteAt(ctx, now.Add(-30*time.Minute))
	if err != nil || !ok || at.USD != 1.25 {
		t.Fatalf("historical = %+v, %v, %v", at, ok, err)
	}
}

func TestPositionLifecycleJournalAndMarks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	p := Position{GiftID: 77, GiftNum: 7, Key: key, BuyPrice: 10, BoughtAt: now.Add(-time.Hour), Status: StatusListed, Source: "manual", ListPrice: 12, ListedAt: now}
	if err := st.UpsertPosition(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordPositionEvent(ctx, 77, "acquired", 0, 10, "manual", p.BoughtAt); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordPositionEvent(ctx, 77, "listed", 0, 12, "manual", now); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertPositionMark(ctx, 77, PositionMark{TS: now, EntryPrice: 10, AskPrice: 12, RecommendedExit: 11.5, ExternalRef: 11.8, GramUSD: 1.33, Edge: .144, Action: "HOLD"}); err != nil {
		t.Fatal(err)
	}
	events, err := st.PositionEvents(ctx, 77, 10)
	if err != nil || len(events) != 2 || events[0].Kind != "listed" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	marks, err := st.PositionMarks(ctx, 77, 10)
	if err != nil || len(marks) != 1 || marks[0].ExternalRef != 11.8 {
		t.Fatalf("marks=%+v err=%v", marks, err)
	}
	if err := st.SetPositionMissing(ctx, 77, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	open, _ := st.OpenPositions(ctx)
	if len(open) != 0 {
		t.Fatalf("missing position counted open: %+v", open)
	}
	tracked, _ := st.TrackedPositions(ctx)
	if len(tracked) != 1 || tracked[0].Status != StatusMissing || tracked[0].MissingSince.IsZero() {
		t.Fatalf("tracked=%+v", tracked)
	}
	if err := st.SetPositionUnlisted(ctx, 77); err != nil {
		t.Fatal(err)
	}
	p2, _ := st.GetPosition(ctx, 77)
	if p2.Status != StatusOpen || p2.ListPrice != 0 || !p2.MissingSince.IsZero() {
		t.Fatalf("reappeared=%+v", p2)
	}
}

func TestSaleLookupNeverReusesAcquisition(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := st.InsertSales(ctx, []tonnel.Sale{{GiftID: 88, GiftName: key.Name, Model: key.Model, Price: 10, Timestamp: tonnel.FlexTime{Time: now.Add(-time.Hour)}}, {GiftID: 88, GiftName: key.Name, Model: key.Model, Price: 12, Timestamp: tonnel.FlexTime{Time: now}}})
	if err != nil {
		t.Fatal(err)
	}
	// Newest first, and several of them: telling our own purchase apart from a
	// sale needs more than one candidate, because the marketplace stamps our
	// purchase a moment *after* we recorded making it.
	trades, err := st.SalesForGiftAfter(ctx, 88, now.Add(-30*time.Minute), 5)
	if err != nil || len(trades) != 1 || trades[0].Price != 12 {
		t.Fatalf("trades=%+v err=%v", trades, err)
	}
	trades, err = st.SalesForGiftAfter(ctx, 88, now.Add(-2*time.Hour), 5)
	if err != nil || len(trades) != 2 || trades[0].Price != 12 || trades[1].Price != 10 {
		t.Fatalf("trades=%+v err=%v", trades, err)
	}
	trades, err = st.SalesForGiftAfter(ctx, 88, now.Add(time.Minute), 5)
	if err != nil || len(trades) != 0 {
		t.Fatalf("future lookup trades=%+v err=%v", trades, err)
	}
}

func TestReacquisitionPreservesCompletedCycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	first := Position{GiftID: 99, Key: key, BuyPrice: 10, BoughtAt: now.Add(-2 * time.Hour), Status: StatusListed, Source: "manual", ListPrice: 12}
	if err := st.UpsertPosition(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPositionSold(ctx, 99, 12, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	second := Position{GiftID: 99, Key: key, BuyPrice: 20, BoughtAt: now, Status: StatusOpen, Source: "reacquired"}
	if err := st.UpsertPosition(ctx, second); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetPosition(ctx, 99)
	if current.BuyPrice != 20 || current.SellPrice != 0 || current.ListPrice != 0 || current.Status != StatusOpen || current.Source != "reacquired" {
		t.Fatalf("current=%+v", current)
	}
	cycles, err := st.PositionTradesForGift(ctx, 99)
	if err != nil || len(cycles) != 1 || cycles[0].BuyPrice != 10 || cycles[0].SellPrice != 12 {
		t.Fatalf("cycles=%+v err=%v", cycles, err)
	}
}

func TestMutesCoverCollectionAndModel(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now()

	muted, err := st.IsMuted(ctx, key, now)
	if err != nil || muted {
		t.Fatalf("nothing should be muted initially (%v, %v)", muted, err)
	}

	if err := st.SetMute(ctx, key.ID(), now.Add(time.Hour)); err != nil {
		t.Fatalf("mute model: %v", err)
	}
	if muted, _ := st.IsMuted(ctx, key, now); !muted {
		t.Error("the model mute did not take effect")
	}
	other := tonnel.ModelKey{Name: "Plush Pepe", Model: "Blue Steel"}
	if muted, _ := st.IsMuted(ctx, other, now); muted {
		t.Error("a model mute must not silence other models")
	}

	if err := st.SetMute(ctx, key.Name, now.Add(time.Hour)); err != nil {
		t.Fatalf("mute collection: %v", err)
	}
	if muted, _ := st.IsMuted(ctx, other, now); !muted {
		t.Error("a collection mute must silence every model in it")
	}

	// Expiry is checked against the supplied time, not stored state.
	if muted, _ := st.IsMuted(ctx, key, now.Add(2*time.Hour)); muted {
		t.Error("an expired mute must not silence anything")
	}
}

// Plain gift-id dedupe would swallow a relist that dropped its price.
func TestAlreadySignalledIsPriceAware(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if _, err := st.InsertSignal(ctx, SignalRow{
		TS: time.Now(), Kind: "buy", GiftID: 5, Key: key, Price: 1000,
	}); err != nil {
		t.Fatalf("insert signal: %v", err)
	}

	same, err := st.AlreadySignalled(ctx, 5, "buy", 1000)
	if err != nil || !same {
		t.Errorf("the same price should be treated as already signalled (%v, %v)", same, err)
	}
	higher, _ := st.AlreadySignalled(ctx, 5, "buy", 1200)
	if !higher {
		t.Error("a higher price is not new news")
	}
	cheaper, _ := st.AlreadySignalled(ctx, 5, "buy", 800)
	if cheaper {
		t.Error("a price drop must be allowed to signal again")
	}
}

func TestPositionLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	pos := Position{
		GiftID: 9, Key: key, BuyPrice: 1000, BoughtAt: now,
		Status: StatusOpen, Source: "auto",
	}
	if err := st.UpsertPosition(ctx, pos); err != nil {
		t.Fatalf("insert position: %v", err)
	}

	if n, _ := st.CountOpenPositions(ctx); n != 1 {
		t.Errorf("open positions = %d, want 1", n)
	}
	if err := st.SetPositionListed(ctx, 9, 1200, now.Add(time.Minute)); err != nil {
		t.Fatalf("list: %v", err)
	}

	got, err := st.GetPosition(ctx, 9)
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Status != StatusListed || got.ListPrice != 1200 {
		t.Errorf("position = %+v, want listed at 1200", got)
	}

	// A later reconciliation must not wipe the recorded listing.
	if err := st.UpsertPosition(ctx, Position{
		GiftID: 9, Key: key, BuyPrice: 1000, BoughtAt: now, Status: StatusListed, Source: "auto",
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, _ = st.GetPosition(ctx, 9)
	if got.ListPrice != 1200 {
		t.Errorf("re-upsert erased the list price: %+v", got)
	}

	if err := st.SetPositionSold(ctx, 9, 1180, now.Add(time.Hour)); err != nil {
		t.Fatalf("sell: %v", err)
	}
	if n, _ := st.CountOpenPositions(ctx); n != 0 {
		t.Errorf("open positions after the sale = %d, want 0", n)
	}
	closed, err := st.ClosedPositions(ctx, 10)
	if err != nil || len(closed) != 1 || closed[0].SellPrice != 1180 {
		t.Errorf("closed positions = %+v (%v)", closed, err)
	}

	last, err := st.LastBuyForModel(ctx, key)
	if err != nil {
		t.Fatalf("last buy: %v", err)
	}
	if !last.Equal(now.UTC()) {
		t.Errorf("last buy = %v, want %v", last, now.UTC())
	}
}

func TestImportedCostBasisAndOutcomeRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().Truncate(time.Second)
	_, err := st.InsertSales(ctx, []tonnel.Sale{{GiftID: 77, GiftName: "Plush Pepe", Model: "Pink Diamond (0.4%)", Price: 88, Timestamp: tonnel.FlexTime{Time: now}}})
	if err != nil {
		t.Fatal(err)
	}
	p, at, ok, err := st.AcquisitionForGift(ctx, 77)
	if err != nil || !ok || p != 88 || !at.Equal(now.UTC()) {
		t.Fatalf("acquisition=(%v,%v,%v,%v)", p, at, ok, err)
	}
	id, err := st.InsertSignal(ctx, SignalRow{TS: now, Kind: "buy", GiftID: 77, Key: key, Price: 80})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSignalOutcome(ctx, id, 24, now.Add(24*time.Hour), true, 88, 90, true); err != nil {
		t.Fatal(err)
	}
	need, err := st.SignalsNeedingOutcome(ctx, 24, now.Add(25*time.Hour))
	if err != nil || len(need) != 0 {
		t.Fatalf("outcome was not deduped: %v %v", need, err)
	}
}

func TestModelStatsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	stats := []tonnel.ModelStat{
		{Key: key, Floor: 1000, Supply: 12, Rarity: 0.4},
		{Key: tonnel.ModelKey{Name: "Plush Pepe", Model: "Blue Steel"}, Floor: 400, Supply: 50, Rarity: 2},
	}
	if err := st.ReplaceModelStats(ctx, stats, now); err != nil {
		t.Fatalf("write stats: %v", err)
	}

	got, err := st.ModelStat(ctx, key)
	if err != nil || got == nil {
		t.Fatalf("read stat: %v", err)
	}
	if got.Floor != 1000 || got.Supply != 12 {
		t.Errorf("stat = %+v, want floor 1000 supply 12", got)
	}

	rows, err := st.ModelsForCollection(ctx, "plush pepe")
	if err != nil {
		t.Fatalf("collection models: %v", err)
	}
	if len(rows) != 2 || rows[0].Floor != 400 {
		t.Errorf("models = %+v, want two rows cheapest first", rows)
	}

	missing, err := st.ModelStat(ctx, tonnel.ModelKey{Name: "Nope", Model: "Nope"})
	if err != nil || missing != nil {
		t.Errorf("an unknown model should return (nil, nil), got (%v, %v)", missing, err)
	}
}

func TestFloorHistoryLookup(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	older := []tonnel.ModelStat{{Key: key, Floor: 1500, Supply: 10}}
	if err := st.SnapshotModelHistory(ctx, older, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	newer := []tonnel.ModelStat{{Key: key, Floor: 1000, Supply: 10}}
	if err := st.SnapshotModelHistory(ctx, newer, now); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	f, ok, err := st.FloorAt(ctx, key, now.Add(-time.Hour))
	if err != nil || !ok {
		t.Fatalf("lookup: (%v, %v)", ok, err)
	}
	if f != 1500 {
		t.Errorf("floor an hour ago = %v, want 1500", f)
	}

	_, ok, _ = st.FloorAt(ctx, key, now.Add(-10*time.Hour))
	if ok {
		t.Error("a lookup before any snapshot should report no data")
	}
}

func TestKVRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	v, err := st.GetKV(ctx, "missing")
	if err != nil || v != "" {
		t.Errorf("missing key = (%q, %v), want empty", v, err)
	}
	if err := st.SetKV(ctx, "k", "one"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.SetKV(ctx, "k", "two"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	v, _ = st.GetKV(ctx, "k")
	if v != "two" {
		t.Errorf("value = %q, want two", v)
	}
}
