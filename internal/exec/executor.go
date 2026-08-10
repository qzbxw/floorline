// Package exec turns a decision into an actual purchase, and immediately puts
// the result back on the market.
package exec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"floorline/internal/config"
	"floorline/internal/pricing"
	"floorline/internal/risk"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

// Trader is the subset of the marketplace API the executor needs.
type Trader interface {
	BuyGift(ctx context.Context, giftID int64, price float64) (*tonnel.Result, error)
	ListForSale(ctx context.Context, giftID int64, price float64) (*tonnel.Result, error)
	MyGifts(ctx context.Context, listed bool, page, limit int) ([]tonnel.Gift, error)
}

// Outcome describes what actually happened.
type Outcome struct {
	GiftID    int64
	Key       tonnel.ModelKey
	Bought    bool
	Listed    bool
	BuyPrice  float64
	ListPrice float64
	// Note carries anything the operator must read: why we did not relist, or
	// why an attempt was skipped.
	Note string
}

// Executor performs purchases under the risk manager's supervision.
type Executor struct {
	api   Trader
	st    *store.Store
	books *pricing.BookCache
	rm    *risk.Manager
	cfg   *config.Config
}

// New builds an Executor.
func New(api Trader, st *store.Store, books *pricing.BookCache, rm *risk.Manager, cfg *config.Config) *Executor {
	return &Executor{api: api, st: st, books: books, rm: rm, cfg: cfg}
}

// ErrAlreadyAttempted means another attempt already owns this gift id.
var ErrAlreadyAttempted = errors.New("purchase already attempted for this listing")

// Buy purchases a listing and, on success, immediately lists it again.
//
// Idempotency comes first: the attempt is claimed in the database *before* the
// request goes out, so a double-tapped button, a retry or a crash mid-flight
// can never turn into two purchases of the same gift.
//
// source is "auto" or "manual" and is recorded on the resulting position.
func (e *Executor) Buy(ctx context.Context, val pricing.Valuation, gift tonnel.Gift, source string, now time.Time) (*Outcome, error) {
	giftID := gift.GiftID.Int()
	price := val.Price
	out := &Outcome{GiftID: giftID, Key: val.Key, BuyPrice: price}

	claimed, err := e.st.ClaimBuy(ctx, giftID, price, now)
	if err != nil {
		return out, fmt.Errorf("claim purchase: %w", err)
	}
	if !claimed {
		return out, ErrAlreadyAttempted
	}

	res, buyErr := e.api.BuyGift(ctx, giftID, price)
	switch {
	case buyErr == nil && res.OK():
		out.Bought = true

	case buyErr == nil:
		// The server answered with a business rejection: gone, or repriced.
		// res can be nil when the body was literally `null`, so never touch it
		// directly — a panic here would strand a claimed purchase.
		msg := "no reason given"
		if res != nil && res.Message != "" {
			msg = res.Message
		}
		_ = e.st.FinishBuy(ctx, giftID, "failed", msg)
		e.rm.RecordFailure(ctx)
		out.Note = "rejected by Tonnel: " + msg
		return out, nil

	default:
		var ae *tonnel.APIError
		if errors.As(buyErr, &ae) {
			// A clean HTTP-level rejection means the purchase definitely did
			// not happen, so there is nothing to reconcile.
			_ = e.st.FinishBuy(ctx, giftID, "failed", ae.Error())
			if ae.IsBlocked() {
				e.rm.Pause(5*time.Minute, "anti-bot block on the write host")
				_ = e.rm.Disarm(ctx, "anti-bot block while buying")
			} else {
				e.rm.RecordFailure(ctx)
			}
			return out, buyErr
		}

		// A transport failure is ambiguous: the request may have landed. Ask
		// the marketplace what we own rather than firing a second purchase.
		owned, checkErr := e.owns(ctx, giftID)
		if checkErr != nil {
			_ = e.st.FinishBuy(ctx, giftID, "pending", "outcome unknown: "+buyErr.Error())
			return out, fmt.Errorf("purchase outcome unknown for gift %d (%v); ownership check also failed: %w", giftID, buyErr, checkErr)
		}
		if !owned {
			_ = e.st.FinishBuy(ctx, giftID, "failed", buyErr.Error())
			e.rm.RecordFailure(ctx)
			return out, buyErr
		}
		out.Bought = true
		out.Note = "transport error, but reconciliation confirmed the purchase"
	}

	_ = e.st.FinishBuy(ctx, giftID, "bought", "")
	e.rm.RecordSuccess()
	if err := e.rm.Commit(ctx, price, now); err != nil {
		return out, fmt.Errorf("bought gift %d but failed to book the spend: %w", giftID, err)
	}

	pos := store.Position{
		GiftID:   giftID,
		GiftNum:  gift.GiftNum.Int(),
		Key:      val.Key,
		Backdrop: tonnel.BaseAttr(gift.Backdrop),
		Symbol:   tonnel.BaseAttr(gift.Symbol),
		BuyPrice: price,
		BoughtAt: now,
		Status:   store.StatusOpen,
		Source:   source,
	}
	if err := e.st.UpsertPosition(ctx, pos); err != nil {
		return out, fmt.Errorf("bought gift %d but failed to record the position: %w", giftID, err)
	}

	// We just removed the cheapest ask; anything cached is now wrong.
	e.books.Invalidate(val.Key)

	listPrice, note, err := e.Relist(ctx, giftID, val.Key, price, now)
	out.Note = joinNotes(out.Note, note)
	if err != nil {
		return out, fmt.Errorf("bought gift %d for %.2f but relisting failed: %w", giftID, price, err)
	}
	if listPrice > 0 {
		out.Listed = true
		out.ListPrice = listPrice
	}
	return out, nil
}

// Relist prices an owned gift against the current book and puts it up for sale.
//
// It returns a zero price with an explanatory note when the market has moved
// enough that listing would lock in a loss — in that case the position is left
// unlisted for the operator to decide on, rather than dumped automatically.
func (e *Executor) Relist(ctx context.Context, giftID int64, key tonnel.ModelKey, entry float64, now time.Time) (float64, string, error) {
	limits := e.rm.Limits()
	floorPrice := entry * (1 + limits.MinMarkup)

	book, err := e.books.Get(ctx, key)
	if err != nil {
		return 0, "", fmt.Errorf("re-read order book: %w", err)
	}
	sales, err := e.st.SalesSince(ctx, key, now.Add(-time.Duration(e.cfg.LookbackDays)*24*time.Hour))
	if err != nil {
		return 0, "", fmt.Errorf("re-read trade history: %w", err)
	}
	liq := pricing.ComputeLiquidity(sales, now,
		time.Duration(e.cfg.LookbackDays)*24*time.Hour,
		time.Duration(e.cfg.LookbackDays)*24*time.Hour)

	val := pricing.Evaluate(pricing.Input{
		GiftID: giftID,
		Key:    key,
		Price:  entry,
		Book:   book,
		Liq:    liq,
		Params: pricing.Params{Fee: e.cfg.TonnelFee, Undercut: e.cfg.Undercut},
	})
	if !val.Valid {
		return 0, "not listed: " + val.Reason, nil
	}

	target := roundDown2(val.Exit)
	if target < floorPrice {
		return 0, fmt.Sprintf(
			"not listed: the market moved — selling now would need %.2f, below your floor of %.2f (entry %.2f + %.0f%%)",
			target, floorPrice, entry, limits.MinMarkup*100), nil
	}

	res, err := e.api.ListForSale(ctx, giftID, target)
	if err != nil {
		return 0, "", fmt.Errorf("list for sale at %.2f: %w", target, err)
	}
	if res != nil && !res.OK() {
		return 0, "", fmt.Errorf("list for sale at %.2f rejected: %s", target, res.Message)
	}

	if err := e.st.SetPositionListed(ctx, giftID, target, now); err != nil {
		return target, "", fmt.Errorf("listed at %.2f but failed to record it: %w", target, err)
	}
	e.books.Invalidate(key)
	return target, "", nil
}

// owns reports whether the account currently holds the gift, checking both the
// unlisted and listed inventories.
func (e *Executor) owns(ctx context.Context, giftID int64) (bool, error) {
	for _, listed := range []bool{false, true} {
		gifts, err := e.api.MyGifts(ctx, listed, 1, 30)
		if err != nil {
			return false, err
		}
		for i := range gifts {
			if gifts[i].GiftID.Int() == giftID {
				return true, nil
			}
		}
	}
	return false, nil
}

// roundDown2 truncates to two decimals so a listing always lands strictly below
// the competing ask it is meant to undercut.
func roundDown2(v float64) float64 {
	return math.Floor(v*100) / 100
}

func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}
