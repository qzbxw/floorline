package exec

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"floorline/internal/config"
	"floorline/internal/pricing"
	"floorline/internal/risk"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

var key = tonnel.ModelKey{Name: "Plush Pepe", Model: "Pink Diamond"}

// fakeAPI records every call and lets each one be scripted independently.
type fakeAPI struct {
	mu sync.Mutex

	bookPrices []float64
	owned      []int64
	quote      tonnel.Gift
	apiOwner   int64
	balance    float64
	buyFee     float64
	balanceErr error

	buyCalls  []buyCall
	listCalls []listCall

	buyResult  *tonnel.Result
	buyErr     error
	listResult *tonnel.Result
	listErr    error
}

type buyCall struct {
	giftID int64
	price  float64
}
type listCall struct {
	giftID int64
	price  float64
}

func (f *fakeAPI) UserID() int64 { return f.apiOwner }

func (f *fakeAPI) GiftData(_ context.Context, giftID int64) (*tonnel.Gift, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.quote
	if g.GiftID.Int() == 0 {
		g.GiftID = tonnel.FlexInt(giftID)
	}
	return &g, nil
}

func (f *fakeAPI) Balance(context.Context) (*tonnel.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.balanceErr != nil {
		return nil, f.balanceErr
	}
	return &tonnel.Balance{GRAM: f.balance}, nil
}

func (f *fakeAPI) BuyGift(ctx context.Context, giftID int64, price float64) (*tonnel.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buyCalls = append(f.buyCalls, buyCall{giftID, price})
	if f.buyResult != nil || f.buyErr != nil {
		return f.buyResult, f.buyErr
	}
	f.balance -= price * (1 + f.buyFee)
	// A successful purchase takes the lot off the market, so the relist that
	// follows must price against a book that no longer contains it.
	for i, p := range f.bookPrices {
		if p == price {
			f.bookPrices = append(f.bookPrices[:i:i], f.bookPrices[i+1:]...)
			break
		}
	}
	return &tonnel.Result{Status: "success"}, nil
}

func (f *fakeAPI) ListForSale(ctx context.Context, giftID int64, price float64) (*tonnel.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, listCall{giftID, price})
	if f.listResult == nil && f.listErr == nil {
		return &tonnel.Result{Status: "success"}, nil
	}
	return f.listResult, f.listErr
}

func (f *fakeAPI) MyGifts(ctx context.Context, listed bool, page, limit int) ([]tonnel.Gift, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if listed {
		return nil, nil
	}
	out := make([]tonnel.Gift, 0, len(f.owned))
	for _, id := range f.owned {
		out = append(out, tonnel.Gift{GiftID: tonnel.FlexInt(id)})
	}
	return out, nil
}

func (f *fakeAPI) ModelBook(ctx context.Context, k tonnel.ModelKey, limit int) ([]tonnel.Gift, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tonnel.Gift, 0, len(f.bookPrices))
	for i, p := range f.bookPrices {
		out = append(out, tonnel.Gift{
			GiftID: tonnel.FlexInt(200 + i),
			Name:   k.Name,
			Model:  k.Model + " (0.4%)",
			Price:  tonnel.Flex64(p),
			Asset:  tonnel.AssetGRAM,
		})
	}
	return out, nil
}

func (f *fakeAPI) buys() []buyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]buyCall(nil), f.buyCalls...)
}

func (f *fakeAPI) lists() []listCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]listCall(nil), f.listCalls...)
}

type harness struct {
	ex  *Executor
	api *fakeAPI
	st  *store.Store
	rm  *risk.Manager
}

func newHarness(t *testing.T, bookPrices ...float64) *harness {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "exec.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	rm, err := risk.New(ctx, st)
	if err != nil {
		t.Fatalf("risk manager: %v", err)
	}
	if err := rm.SetLimit(ctx, "min_markup", "0.03"); err != nil {
		t.Fatalf("set markup: %v", err)
	}

	cfg := &config.Config{TonnelFee: 0.005, Undercut: 0.01, LookbackDays: 14, BookCacheTTL: time.Nanosecond}
	api := &fakeAPI{
		bookPrices: bookPrices,
		balance:    10000,
		buyFee:     cfg.TonnelFee,
		quote: tonnel.Gift{GiftID: 1, Name: key.Name, Model: key.Model + " (0.4%)",
			Price: tonnel.Flex64(bookPrices[0]), Asset: tonnel.AssetGRAM, Seller: 999},
	}
	books := pricing.NewBookCache(api, cfg.BookCacheTTL, 10)

	return &harness{ex: New(api, st, books, rm, cfg), api: api, st: st, rm: rm}
}

func (h *harness) seedSales(t *testing.T, price float64, count int) {
	t.Helper()
	now := time.Now()
	sales := make([]tonnel.Sale, 0, count)
	for i := 0; i < count; i++ {
		sales = append(sales, tonnel.Sale{
			GiftID: tonnel.FlexInt(9000 + i), GiftName: key.Name, Model: key.Model + " (0.4%)",
			Price: tonnel.Flex64(price), GiftNum: tonnel.FlexInt(int64(i) + 1),
			Timestamp: tonnel.FlexTime{Time: now.Add(-time.Duration(i) * 6 * time.Hour)},
		})
	}
	if _, err := h.st.InsertSales(context.Background(), sales); err != nil {
		t.Fatalf("seed sales: %v", err)
	}
}

func valuation(price float64) pricing.Valuation {
	return pricing.Valuation{Key: key, GiftID: 1, Price: price, Valid: true}
}

func candidate(price float64) tonnel.Gift {
	return tonnel.Gift{GiftID: 1, Name: key.Name, Model: key.Model + " (0.4%)", Price: tonnel.Flex64(price)}
}

func TestBuyThenRelistAtTheUndercutPrice(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200, 1250)
	h.seedSales(t, 1200, 30)

	out, err := h.ex.Buy(ctx, valuation(800), candidate(800), "auto", time.Now())
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if !out.Bought {
		t.Fatalf("purchase did not register: %+v", out)
	}
	if !out.Listed {
		t.Fatalf("the gift was bought but never relisted: %s", out.Note)
	}

	// Fast exit blends the robust first-three depth with the sale history.
	if len(h.api.lists()) != 1 || h.api.lists()[0].price != 1207.65 {
		t.Errorf("list calls = %+v, want one at 1207.65", h.api.lists())
	}

	pos, err := h.st.GetPosition(ctx, 1)
	if err != nil || pos == nil {
		t.Fatalf("position not recorded: %v", err)
	}
	if pos.Status != store.StatusListed || pos.BuyPrice != 804 || pos.ListPrice != 1207.65 {
		t.Errorf("position = %+v, want listed at 1207.65 with an actual debit of 804", pos)
	}

	spend, _ := h.st.SpendToday(ctx, time.Now().UTC().Format("2006-01-02"))
	if spend.Spent != 804 || spend.Buys != 1 {
		t.Errorf("ledger = %+v, want actual debit 804 across one buy", spend)
	}
}

// A double-tapped Buy button, a retry, or a crashed restart must never turn
// into two purchases of the same listing.
func TestSecondAttemptOnTheSameListingIsRefused(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200)
	h.seedSales(t, 1200, 30)

	if _, err := h.ex.Buy(ctx, valuation(800), candidate(800), "manual", time.Now()); err != nil {
		t.Fatalf("first buy: %v", err)
	}
	_, err := h.ex.Buy(ctx, valuation(800), candidate(800), "manual", time.Now())
	if !errors.Is(err, ErrAlreadyAttempted) {
		t.Fatalf("second attempt error = %v, want ErrAlreadyAttempted", err)
	}
	if len(h.api.buys()) != 1 {
		t.Errorf("the marketplace was asked to buy %d times, want 1", len(h.api.buys()))
	}
}

func TestFinalQuoteChangeAbortsBeforeMoneyMoves(t *testing.T) {
	h := newHarness(t, 800, 1200)
	h.api.quote.Price = 801

	out, err := h.ex.Buy(context.Background(), valuation(800), candidate(800), "auto", time.Now())
	if err == nil || out.Bought {
		t.Fatalf("changed final quote was bought: out=%+v err=%v", out, err)
	}
	if len(h.api.buys()) != 0 {
		t.Fatal("BuyGift was called after the final quote changed")
	}
}

func TestFinalQuoteRejectsOwnListing(t *testing.T) {
	h := newHarness(t, 800, 1200)
	h.api.quote.Seller = 77
	h.api.apiOwner = 77

	out, err := h.ex.Buy(context.Background(), valuation(800), candidate(800), "auto", time.Now())
	if err == nil || out.Bought || len(h.api.buys()) != 0 {
		t.Fatalf("own listing reached buy: out=%+v err=%v calls=%v", out, err, h.api.buys())
	}
}

func TestPurchaseCostIncludesReferralButWireQuoteDoesNot(t *testing.T) {
	h := newHarness(t, 3.79, 10)
	h.seedSales(t, 10, 30)

	out, err := h.ex.Buy(context.Background(), valuation(3.79), candidate(3.79), "auto", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(out.BuyPrice-3.80895) > 1e-9 {
		t.Errorf("actual cost = %.8f, want 3.80895", out.BuyPrice)
	}
	if calls := h.api.buys(); len(calls) != 1 || calls[0].price != 3.79 {
		t.Errorf("wire buy calls = %+v, want exact ask 3.79", calls)
	}
}

func TestAmbiguousSuccessResponseRequiresOwnershipReconciliation(t *testing.T) {
	h := newHarness(t, 800, 1200)
	h.seedSales(t, 1200, 30)
	h.api.buyResult = &tonnel.Result{}
	h.api.owned = []int64{1}

	out, err := h.ex.Buy(context.Background(), valuation(800), candidate(800), "auto", time.Now())
	if err != nil || !out.Bought {
		t.Fatalf("owned gift was not reconciled: out=%+v err=%v", out, err)
	}
}

// Selling below entry plus the markup is a loss the operator did not sign up
// for, so the position is left unlisted and flagged instead.
func TestNoRelistWhenTheMarketMovedBelowTheFloorPrice(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 810) // the next ask collapsed to just above our entry
	h.seedSales(t, 805, 30)

	out, err := h.ex.Buy(ctx, valuation(800), candidate(800), "auto", time.Now())
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if !out.Bought {
		t.Fatal("the purchase should still have gone through")
	}
	if out.Listed {
		t.Error("the gift must not be relisted at a loss")
	}
	if len(h.api.lists()) != 0 {
		t.Errorf("list was called %d times, want none", len(h.api.lists()))
	}
	if !strings.Contains(out.Note, "рынок уехал") {
		t.Errorf("note = %q, want an explanation of the refusal", out.Note)
	}

	pos, _ := h.st.GetPosition(ctx, 1)
	if pos == nil || pos.Status != store.StatusOpen {
		t.Errorf("position = %+v, want an open, unlisted position", pos)
	}
}

func TestBusinessRejectionIsRecordedAndCountsAsAFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200)
	h.seedSales(t, 1200, 30)
	h.api.buyResult = &tonnel.Result{Status: "error", Message: "gift already sold"}

	out, err := h.ex.Buy(ctx, valuation(800), candidate(800), "auto", time.Now())
	if err != nil {
		t.Fatalf("a clean rejection should not be an error: %v", err)
	}
	if out.Bought {
		t.Error("a rejected purchase must not report success")
	}
	if !strings.Contains(out.Note, "already sold") {
		t.Errorf("note = %q, want the marketplace message", out.Note)
	}
	if pos, _ := h.st.GetPosition(ctx, 1); pos != nil {
		t.Error("a failed purchase must not create a position")
	}
	spend, _ := h.st.SpendToday(ctx, time.Now().UTC().Format("2006-01-02"))
	if spend.Spent != 0 {
		t.Errorf("a failed purchase charged %v to the budget", spend.Spent)
	}
}

// A transport failure is ambiguous: the request may well have landed. The
// executor must ask what we own instead of firing a second purchase.
func TestAmbiguousFailureReconcilesInsteadOfRetrying(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200)
	h.seedSales(t, 1200, 30)
	h.api.buyErr = errors.New("connection reset by peer")
	h.api.owned = []int64{1} // it actually went through

	out, err := h.ex.Buy(ctx, valuation(800), candidate(800), "auto", time.Now())
	if err != nil {
		t.Fatalf("reconciliation should recover: %v", err)
	}
	if !out.Bought {
		t.Fatal("reconciliation found the gift in our inventory but did not record the purchase")
	}
	if len(h.api.buys()) != 1 {
		t.Errorf("buy was attempted %d times, want exactly 1", len(h.api.buys()))
	}
	if pos, _ := h.st.GetPosition(ctx, 1); pos == nil {
		t.Error("the recovered purchase should have created a position")
	}
}

func TestAmbiguousFailureThatDidNotLandIsAFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200)
	h.seedSales(t, 1200, 30)
	h.api.buyErr = errors.New("connection reset by peer")
	h.api.owned = nil // it really did not go through

	out, err := h.ex.Buy(ctx, valuation(800), candidate(800), "auto", time.Now())
	if err == nil {
		t.Fatal("a purchase that did not happen must surface the error")
	}
	if out.Bought {
		t.Error("nothing was bought")
	}
	if pos, _ := h.st.GetPosition(ctx, 1); pos != nil {
		t.Error("no position should exist")
	}
}

// An anti-bot block on the write host means something bigger is wrong than one
// bad listing, so unattended buying stops until a human looks.
func TestBlockedWriteHostDisarms(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 800, 1200)
	h.seedSales(t, 1200, 30)

	if err := h.rm.SetLimit(ctx, "max_ticket", "1000"); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	if err := h.rm.SetLimit(ctx, "daily_budget", "5000"); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	if err := h.rm.Arm(ctx); err != nil {
		t.Fatalf("arm: %v", err)
	}

	h.api.buyErr = &tonnel.APIError{Op: "/api/buyGift/1", Status: 403, Message: "cloudflare challenge"}

	if _, err := h.ex.Buy(ctx, valuation(800), candidate(800), "auto", time.Now()); err == nil {
		t.Fatal("a 403 must be surfaced as an error")
	}
	if h.rm.Armed() {
		t.Error("an anti-bot block on the write host must disarm auto-buy")
	}
}

func TestRelistPricesAgainstTheLiveBook(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 1500, 1600)
	h.seedSales(t, 2000, 30)

	if err := h.st.UpsertPosition(ctx, store.Position{
		GiftID: 1, Key: key, BuyPrice: 1000, BoughtAt: time.Now(),
		Status: store.StatusOpen, Source: "manual",
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	price, note, err := h.ex.Relist(ctx, 1, key, 1000, time.Now())
	if err != nil {
		t.Fatalf("relist: %v", err)
	}
	if note != "" {
		t.Errorf("unexpected note: %s", note)
	}
	// Two live asks establish depth at 1550; the fast exit sits just under it.
	if price != 1534.5 {
		t.Errorf("list price = %v, want 1534.5", price)
	}

	pos, _ := h.st.GetPosition(ctx, 1)
	if pos.Status != store.StatusListed || pos.ListPrice != 1534.5 {
		t.Errorf("position = %+v, want listed at 1534.5", pos)
	}
}

// Prices are truncated, never rounded up, so an undercut always lands strictly
// below the ask it is meant to beat.
func TestListPriceIsTruncatedNotRounded(t *testing.T) {
	if got := roundDown2(1188.999); got != 1188.99 {
		t.Errorf("roundDown2(1188.999) = %v, want 1188.99", got)
	}
	if got := roundDown2(100); got != 100 {
		t.Errorf("roundDown2(100) = %v, want 100", got)
	}
}
