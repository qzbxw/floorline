// Package exec turns a decision into an actual purchase, and immediately puts
// the result back on the market.
package exec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
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
	// CancelSale withdraws one of our own listings. Tonnel has no "change the
	// price" call: listForSale only finds gifts that are not currently on sale,
	// so repricing means withdrawing first.
	CancelSale(ctx context.Context, giftID int64) (*tonnel.Result, error)
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

	// relisting stamps the last reprice attempt per gift. Repricing is not
	// idempotent the way a purchase is — it withdraws a live ask before placing
	// the new one — so overlapping attempts are kept out in process rather than
	// reconciled afterwards.
	relistMu  sync.Mutex
	relisting map[int64]time.Time
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
	// The seller comes from the listing we were handed, not from the re-read:
	// /api/giftData does not report one. See validateFreshListing.
	if why := validateFreshListing(fresh, giftID, val.Key, quotePrice, gift.Seller.Int(), e.api.UserID()); why != "" {
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

	// Buying and selling are separate decisions. With automatic selling off the
	// gift stays in inventory, unlisted, and the operator gets the price the
	// engine would have used — so turning the switch off costs information but
	// never costs the trade.
	if !e.rm.ResellEnabled() {
		out.Note = joinNotes(out.Note, e.suggestListing(ctx, giftID, val.Key, actualCost, now, val))
		return out, nil
	}

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

// validateFreshListing is the last check before money moves: the gift is re-read
// straight from the marketplace and anything unexpected aborts the purchase.
//
// What it can check is bounded by what /api/giftData actually returns, and that
// is less than it looks. The response carries no `gift_id` — the id is in the
// URL, so the answer is about that gift by construction — and its `seller` is
// always 0. Two earlier checks compared exactly those two fields, so both were
// unsatisfiable and every purchase, manual and unattended alike, was rejected
// before it was attempted. The desk had never once been able to buy.
//
// So identity comes from the URL, the seller comes from the listing that
// produced the candidate (pageGifts does report one), and everything the
// endpoint genuinely answers — model, price, status, settlement — is still
// checked strictly.
func validateFreshListing(g *tonnel.Gift, giftID int64, key tonnel.ModelKey, price float64, sellerID, ownerID int64) string {
	if g == nil {
		return "перед покупкой Tonnel не ответил про этот гифт"
	}
	// Only meaningful when the payload carries an id at all; today it does not.
	if id := g.GiftID.Int(); id != 0 && id != giftID {
		return "перед покупкой Tonnel вернул уже другой гифт"
	}
	if g.Key() != key {
		return "перед покупкой поменялась коллекция или модель"
	}
	if g.Price.Float() != price {
		return fmt.Sprintf("перед покупкой цена уехала с %.2f на %.2f", price, g.Price.Float())
	}
	// The own-lot guard uses whichever source actually knows the seller.
	if ownerID != 0 && (g.Seller.Int() == ownerID || sellerID == ownerID) {
		return "это наш собственный лот — покупать его не буду"
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
	target, note, err := e.listingTarget(ctx, giftID, key, entry, now, prior)
	if err != nil || target <= 0 {
		return 0, note, err
	}
	return e.ListAt(ctx, giftID, key, target, entry, now)
}

// relistQuiet is how long after a reprice the same gift refuses another one.
// Long enough to absorb a double tap, short enough not to obstruct a decision
// to move the price again.
const relistQuiet = 20 * time.Second

// claimRelist serialises reprices of one gift. The second return reports that
// another one is already in flight or too recent.
func (e *Executor) claimRelist(giftID int64, now time.Time) (func(), bool) {
	e.relistMu.Lock()
	defer e.relistMu.Unlock()
	if e.relisting == nil {
		e.relisting = make(map[int64]time.Time)
	}
	if at, ok := e.relisting[giftID]; ok && now.Sub(at) < relistQuiet {
		return func() {}, true
	}
	e.relisting[giftID] = now
	return func() {
		e.relistMu.Lock()
		defer e.relistMu.Unlock()
		// The stamp stays behind deliberately: the quiet window is measured
		// from the attempt, so a retry burst cannot restart it early.
		e.relisting[giftID] = time.Now()
	}, false
}

// suggestListing is the read-only half of a relist: it prices the gift and says
// what it would have asked, without touching the market. This is what the
// operator gets when automatic selling is switched off.
func (e *Executor) suggestListing(ctx context.Context, giftID int64, key tonnel.ModelKey, entry float64, now time.Time, prior pricing.Valuation) string {
	target, note, err := e.listingTarget(ctx, giftID, key, entry, now, prior)
	switch {
	case err != nil:
		return "ресейл выключен, и цену подсказать не смог: " + err.Error()
	case target <= 0:
		return "ресейл выключен. " + note
	default:
		return fmt.Sprintf("ресейл выключен — не выставляю. По текущему стакану поставил бы %.2f: /relist %d", target, giftID)
	}
}

// listingTarget prices an owned gift against the current book and returns the
// ask it would place. A zero price with a nil error means listing right now
// would lock in a loss; the note says why.
func (e *Executor) listingTarget(ctx context.Context, giftID int64, key tonnel.ModelKey, entry float64, now time.Time, prior pricing.Valuation) (float64, string, error) {
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
		// Carry the whole cross-market picture, not just the headline number:
		// the queue behind it is what caps an over-optimistic exit.
		cm := prior.Cross
		cm.Support = prior.CrossMarketSupport
		val = pricing.WithCrossDepth(val, cm)
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
	return target, "", nil
}

// ListAt applies the same no-loss invariant to an externally computed target
// (for example the attribute-aware portfolio adviser).
func (e *Executor) ListAt(ctx context.Context, giftID int64, key tonnel.ModelKey, target, entry float64, now time.Time) (float64, string, error) {
	floorPrice := entry * (1 + e.rm.Limits().MinMarkup)
	target = roundDown2(target)
	if target < floorPrice {
		return 0, fmt.Sprintf("не выставил: цель %.2f ниже входа с наценкой %.2f", target, floorPrice), nil
	}

	// One gift, one reprice at a time. A double-tapped /relist ran two
	// withdraw-and-list pairs back to back — four writes in a second on an
	// endpoint that throttles — so the second tap was itself what produced the
	// "try again in a minute" that left the gift off the market.
	//
	// The guard belongs here rather than in relist() because this is the one
	// function both paths pass through: /relist prices the gift in the app layer
	// and calls straight in.
	release, busy := e.claimRelist(giftID, now)
	if busy {
		return 0, "этот гифт уже переставляется — подожди пару секунд", nil
	}
	defer release()

	res, err := e.api.ListForSale(ctx, giftID, target)
	// Tonnel has no reprice call. listForSale searches the gifts that are *not*
	// currently on sale, so repricing an active listing answers "Gift not found"
	// — which is what every /relist on a listed position used to return. Withdraw
	// the old ask and place the new one.
	if isNotListable(res, err) {
		if why := e.withdraw(ctx, giftID); why != "" {
			return 0, "", fmt.Errorf("не переставил по %.2f: %s", target, why)
		}
		// Past this line the gift is off the market and the only acceptable
		// outcome is getting it back on. Tonnel throttles writes with an HTTP
		// 200 body reading "Please try again in a minute", and one attempt
		// against that left a position unlisted with a chat message telling the
		// operator to fix it by hand. Waiting out a throttle is cheap; a gift
		// sitting off the market until someone notices is not.
		res, err = e.relistWithRetry(ctx, giftID, target)
		if err != nil || (res != nil && !res.OK()) {
			detail := "неизвестная причина"
			if err != nil {
				detail = err.Error()
			} else if res != nil && res.Message != "" {
				detail = res.Message
			}
			return 0, "", fmt.Errorf(
				"КРИТИЧНО: снял старый аск, но новый по %.2f не встал (%s) — гифт %d сейчас без листинга, поставь руками",
				target, detail, giftID)
		}
	}
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

// relistBackoff is how long to keep trying to put a withdrawn gift back on the
// market. Tonnel's own wording is "try again in a minute", so the schedule
// covers rather more than a minute before giving up and asking for hands.
var relistBackoff = []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second, 45 * time.Second}

// relistWithRetry places the new ask, waiting out a temporary throttle.
//
// It retries only refusals the marketplace has explicitly marked as temporary.
// A genuine rejection — wrong price, gift not owned, session dead — is returned
// at once, because repeating it would neither help nor be honest about what
// went wrong.
func (e *Executor) relistWithRetry(ctx context.Context, giftID int64, target float64) (*tonnel.Result, error) {
	res, err := e.api.ListForSale(ctx, giftID, target)
	for _, wait := range relistBackoff {
		if err == nil && (res == nil || res.OK()) {
			return res, nil
		}
		if !retryableListing(res, err) {
			return res, err
		}
		select {
		case <-ctx.Done():
			return res, err
		case <-time.After(wait):
		}
		res, err = e.api.ListForSale(ctx, giftID, target)
	}
	return res, err
}

// retryableListing reports whether a failed listing is worth repeating. The
// refusal arrives either as an error or as a non-OK body, so both are checked.
func retryableListing(res *tonnel.Result, err error) bool {
	if err != nil {
		return tonnel.RateLimited(err)
	}
	if res == nil || res.OK() {
		return false
	}
	m := strings.ToLower(res.Message)
	return strings.Contains(m, "try again") || strings.Contains(m, "too many") || strings.Contains(m, "rate limit")
}

// isNotListable reports the specific rejection that means "this gift is already
// on sale", as opposed to a transport failure or a different refusal.
//
// Tonnel answers it with HTTP 200 and a body message rather than a status code,
// so it has to be matched on text. Anything else is left alone: withdrawing a
// listing in response to an error we did not understand would be a way to end
// up with nothing on the market.
func isNotListable(res *tonnel.Result, err error) bool {
	if err != nil {
		return strings.Contains(strings.ToLower(err.Error()), "gift not found")
	}
	return res != nil && !res.OK() && strings.Contains(strings.ToLower(res.Message), "not found")
}

// withdraw removes our current ask so a new one can be placed. It returns an
// explanation when the listing could not be withdrawn, and an empty string on
// success.
func (e *Executor) withdraw(ctx context.Context, giftID int64) string {
	res, err := e.api.CancelSale(ctx, giftID)
	if err != nil {
		return "не снял старый аск: " + err.Error()
	}
	if res != nil && !res.OK() {
		return "Tonnel не дал снять старый аск: " + res.Message
	}
	return ""
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
