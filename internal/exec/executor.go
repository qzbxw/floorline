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
	UserID() int64
	GiftData(ctx context.Context, giftID int64) (*tonnel.Gift, error)
	Balance(ctx context.Context) (*tonnel.Balance, error)
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
	AskPrice  float64 // displayed price before the purchase referral
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
var ErrAlreadyAttempted = errors.New("этот лот уже пытались купить — второй раз не полезу")

// Buy purchases a listing and, on success, immediately lists it again.
//
// Idempotency comes first: the attempt is claimed in the database *before* the
// request goes out, so a double-tapped button, a retry or a crash mid-flight
// can never turn into two purchases of the same gift.
//
// source is "auto" or "manual" and is recorded on the resulting position.
func (e *Executor) Buy(ctx context.Context, val pricing.Valuation, gift tonnel.Gift, source string, now time.Time) (*Outcome, error) {
	giftID := gift.GiftID.Int()
	quotePrice := val.Price
	out := &Outcome{GiftID: giftID, Key: val.Key, AskPrice: quotePrice, BuyPrice: quotePrice}
	if giftID <= 0 || val.GiftID != giftID || val.Key != gift.Key() || quotePrice <= 0 {
		return out, fmt.Errorf("данные покупки не сходятся для гифта %d", giftID)
	}

	claimed, err := e.st.ClaimBuy(ctx, giftID, quotePrice, now)
	if err != nil {
		return out, fmt.Errorf("не смог застолбить покупку: %w", err)
	}
	if !claimed {
		return out, ErrAlreadyAttempted
	}
	release := true
	defer func() {
		if release {
			_ = e.st.ReleaseBuy(context.Background(), giftID)
		}
	}()

	// A fresh balance is required before unattended money moves. It both proves
	// that the account can be read now and gives us an exact debit to reconcile
	// after the purchase, including referral charges not present in the ask.
	before, err := e.api.Balance(ctx)
	if err != nil {
		return out, fmt.Errorf("не прочитал баланс прямо перед покупкой: %w", err)
	}
	if before == nil || before.GRAM <= 0 {
		return out, errors.New("перед покупкой не вижу положительный баланс GRAM")
	}
	expectedCost := quotePrice * (1 + math.Max(e.cfg.TonnelFee, 0))
	if before.GRAM < expectedCost {
		return out, fmt.Errorf("на балансе %.3f, а с комиссией спишется %.3f", before.GRAM, expectedCost)
	}

	// This is the final quote. Nothing except the exact-price BuyGift request is
	// allowed between it and the write, keeping the stale-feed window minimal.
	fresh, err := e.api.GiftData(ctx, giftID)
	if err != nil {
		return out, fmt.Errorf("re-read gift immediately before buy: %w", err)
	}
	if why := validateFreshListing(fresh, giftID, val.Key, quotePrice, e.api.UserID()); why != "" {
		return out, errors.New(why)
	}

	res, buyErr := e.api.BuyGift(ctx, giftID, quotePrice)
	release = false // the write was attempted; its claim must survive every outcome
	switch {
	case buyErr == nil && res.OK():
		out.Bought = true

	case buyErr == nil && res.Rejected():
		// The server answered with a business rejection: gone, or repriced.
		// res can be nil when the body was literally `null`, so never touch it
		// directly — a panic here would strand a claimed purchase.
		msg := "no reason given"
		if res != nil && res.Message != "" {
			msg = res.Message
		}
		_ = e.st.FinishBuy(ctx, giftID, "failed", msg)
		e.rm.RecordFailure(ctx)
		out.Note = "Tonnel отклонил покупку: " + msg
		return out, nil

	case buyErr == nil:
		owned, checkErr := e.owns(ctx, giftID)
		if checkErr != nil {
			_ = e.st.FinishBuy(ctx, giftID, "pending", "ambiguous response; ownership unknown")
			return out, fmt.Errorf("ambiguous purchase response for gift %d; ownership check failed: %w", giftID, checkErr)
		}
		if !owned {
			_ = e.st.FinishBuy(ctx, giftID, "failed", "ambiguous response; not found in inventory")
			e.rm.RecordFailure(ctx)
			return out, errors.New("ambiguous purchase response; gift not found in inventory")
		}
		out.Bought = true
		out.Note = "ответ был мутный, но гифт уже в нашем инвентаре — покупка подтверждена"

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
		out.Note = "сеть отвалилась, но гифт уже в нашем инвентаре — покупка подтверждена"
	}

	_ = e.st.FinishBuy(ctx, giftID, "bought", "")
	e.rm.RecordSuccess()
	actualCost := expectedCost
	if after, balanceErr := e.api.Balance(ctx); balanceErr == nil && after != nil {
		if debit := before.GRAM - after.GRAM; debit >= quotePrice && debit > 0 {
			actualCost = debit
		} else {
			out.Note = joinNotes(out.Note, "дельта баланса выглядит криво; беру консервативную цену с комиссией")
		}
	} else {
		out.Note = joinNotes(out.Note, "баланс после покупки не прочитался; беру консервативную цену с комиссией")
	}
	if actualCost > expectedCost+1e-6 {
		out.Note = joinNotes(out.Note, fmt.Sprintf("КРИТИЧНО: реально списалось %.3f вместо ожидаемых %.3f", actualCost, expectedCost))
		_ = e.rm.Disarm(context.Background(), "фактическое списание превысило ожидаемую цену")
	}
	out.BuyPrice = actualCost
	if err := e.rm.Commit(ctx, actualCost, now); err != nil {
		out.Note = joinNotes(out.Note, "КРИТИЧНО: купили, но расход не записался: "+err.Error())
		_ = e.rm.Disarm(context.Background(), "не удалось записать расход после покупки")
	}

	pos := store.Position{
		GiftID:      giftID,
		GiftNum:     gift.GiftNum.Int(),
		Key:         val.Key,
		Backdrop:    tonnel.BaseAttr(gift.Backdrop),
		Symbol:      tonnel.BaseAttr(gift.Symbol),
		ModelRarity: gift.ModelRarity.Float(), BackdropRarity: gift.BackdropRarity.Float(), SymbolRarity: gift.SymbolRarity.Float(),
		BuyPrice:   actualCost,
		CostSource: "floorline", CostConfidence: 1,
		BoughtAt: now,
		Status:   store.StatusOpen,
		Source:   source,
	}
	if err := e.st.UpsertPosition(ctx, pos); err != nil {
		out.Note = joinNotes(out.Note, "КРИТИЧНО: купили, но позиция не записалась: "+err.Error())
		_ = e.rm.Disarm(context.Background(), "не удалось записать позицию после покупки")
	}
	if err := e.st.RecordPositionEvent(ctx, giftID, "acquired", 0, actualCost, "куплено Floorline ("+source+")", now); err != nil {
		out.Note = joinNotes(out.Note, "событие позиции не записалось: "+err.Error())
	}

	// We just removed the cheapest ask; anything cached is now wrong.
	e.books.Invalidate(val.Key)

	listPrice, note, err := e.relist(ctx, giftID, val.Key, actualCost, now, val)
	out.Note = joinNotes(out.Note, note)
	if listPrice > 0 {
		out.Listed = true
		out.ListPrice = listPrice
	}
	if err != nil {
		return out, fmt.Errorf("гифт %d куплен за %.2f, но переставить не вышло: %w", giftID, actualCost, err)
	}
	if out.Listed {
		_ = e.st.RecordPositionEvent(ctx, giftID, "listed", 0, listPrice, "выставлен сразу после покупки", now)
	}
	return out, nil
}

func validateFreshListing(g *tonnel.Gift, giftID int64, key tonnel.ModelKey, price float64, ownerID int64) string {
	if g == nil || g.GiftID.Int() != giftID {
		return "перед покупкой Tonnel вернул уже другой гифт"
	}
	if g.Key() != key {
		return "перед покупкой поменялась коллекция или модель"
	}
	if g.Price.Float() != price {
		return fmt.Sprintf("перед покупкой цена уехала с %.2f на %.2f", price, g.Price.Float())
	}
	if ownerID != 0 && g.Seller.Int() == ownerID {
		return "это наш собственный лот — покупать его не буду"
	}
	if g.Seller.Int() == 0 {
		return "перед покупкой пропал ID продавца"
	}
	if g.IsBundle() || g.Premarket.Bool() || g.TelegramMarketplace.Bool() || g.Refunded.Bool() || g.Buyer != nil {
		return "лот уже нельзя нормально купить и получить"
	}
	if g.Asset != "" && g.Asset != tonnel.AssetGRAM {
		return "у лота поменялась валюта расчёта"
	}
	if g.Status != "" && g.Status != "forsale" {
		return "лот уже снят с продажи"
	}
	return ""
}

// Relist prices an owned gift against the current book and puts it up for sale.
//
// It returns a zero price with an explanatory note when the market has moved
// enough that listing would lock in a loss — in that case the position is left
// unlisted for the operator to decide on, rather than dumped automatically.
func (e *Executor) Relist(ctx context.Context, giftID int64, key tonnel.ModelKey, entry float64, now time.Time) (float64, string, error) {
	return e.relist(ctx, giftID, key, entry, now, pricing.Valuation{})
}

func (e *Executor) relist(ctx context.Context, giftID int64, key tonnel.ModelKey, entry float64, now time.Time, prior pricing.Valuation) (float64, string, error) {
	limits := e.rm.Limits()
	floorPrice := entry * (1 + limits.MinMarkup)

	book, err := e.books.Get(ctx, key)
	if err != nil {
		return 0, "", fmt.Errorf("не перечитал стакан: %w", err)
	}
	sales, err := e.st.SalesSince(ctx, key, now.Add(-time.Duration(e.cfg.LookbackDays)*24*time.Hour))
	if err != nil {
		return 0, "", fmt.Errorf("не перечитал историю сделок: %w", err)
	}
	liq := pricing.ComputeLiquidity(sales, now,
		time.Duration(e.cfg.LookbackDays)*24*time.Hour,
		time.Duration(e.cfg.LookbackDays)*24*time.Hour)

	val := pricing.Evaluate(pricing.Input{
		GiftID:    giftID,
		OwnerID:   e.api.UserID(),
		Key:       key,
		Price:     entry,
		Cost:      entry,
		Book:      book,
		Liq:       liq,
		Backdrop:  prior.Backdrop,
		Symbol:    prior.Symbol,
		Attribute: prior.Attribute,
		Params:    pricing.Params{Fee: e.cfg.TonnelFee, Undercut: e.cfg.Undercut},
	})
	if prior.CrossMarketSupport > 0 {
		val = pricing.WithCrossMarket(val, prior.CrossMarketSupport)
	}
	if !val.Valid {
		return 0, "не выставил: " + val.Reason, nil
	}

	target := roundDown2(val.Exit)
	if target < floorPrice {
		return 0, fmt.Sprintf(
			"не выставил: рынок уехал — быстрый выход %.2f ниже твоего минимума %.2f (вход %.2f + %.0f%%)",
			target, floorPrice, entry, limits.MinMarkup*100), nil
	}

	return e.ListAt(ctx, giftID, key, target, entry, now)
}

// ListAt applies the same no-loss invariant to an externally computed target
// (for example the attribute-aware portfolio adviser).
func (e *Executor) ListAt(ctx context.Context, giftID int64, key tonnel.ModelKey, target, entry float64, now time.Time) (float64, string, error) {
	floorPrice := entry * (1 + e.rm.Limits().MinMarkup)
	target = roundDown2(target)
	if target < floorPrice {
		return 0, fmt.Sprintf("не выставил: цель %.2f ниже входа с наценкой %.2f", target, floorPrice), nil
	}
	res, err := e.api.ListForSale(ctx, giftID, target)
	if err != nil {
		return 0, "", fmt.Errorf("не выставил по %.2f: %w", target, err)
	}
	if res != nil && !res.OK() {
		return 0, "", fmt.Errorf("Tonnel не принял листинг по %.2f: %s", target, res.Message)
	}

	if err := e.st.SetPositionListed(ctx, giftID, target, now); err != nil {
		return target, "", fmt.Errorf("выставил по %.2f, но не записал это в базу: %w", target, err)
	}
	e.books.Invalidate(key)
	return target, "", nil
}

// owns reports whether the account currently holds the gift, checking both the
// unlisted and listed inventories.
func (e *Executor) owns(ctx context.Context, giftID int64) (bool, error) {
	for _, listed := range []bool{false, true} {
		for page := 1; ; page++ {
			gifts, err := e.api.MyGifts(ctx, listed, page, 30)
			if err != nil {
				return false, err
			}
			for i := range gifts {
				if gifts[i].GiftID.Int() == giftID {
					return true, nil
				}
			}
			if len(gifts) < 30 {
				break
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
