package venue

import (
	"context"
	"fmt"
	"strconv"

	"floorline/internal/tonnel"
)

// TonnelAPI is the part of the Tonnel client this adapter needs.
type TonnelAPI interface {
	UserID() int64
	ModelBook(ctx context.Context, key tonnel.ModelKey, limit int) ([]tonnel.Gift, error)
	BuyGift(ctx context.Context, giftID int64, price float64) (*tonnel.Result, error)
	ListForSale(ctx context.Context, giftID int64, price float64) (*tonnel.Result, error)
	CancelSale(ctx context.Context, giftID int64) (*tonnel.Result, error)
	MyGifts(ctx context.Context, listed bool, page, limit int) ([]tonnel.Gift, error)
	Balance(ctx context.Context) (*tonnel.Balance, error)
}

// Tonnel presents the existing client through the venue interface.
//
// It is the only venue that also implements Seller: Tonnel charges no sale
// commission, so it is where inventory goes to be sold regardless of where it
// was bought.
type Tonnel struct {
	api TonnelAPI
	fee float64
}

func NewTonnel(api TonnelAPI, fee float64) *Tonnel { return &Tonnel{api: api, fee: fee} }

func (t *Tonnel) Name() string    { return "Tonnel" }
func (t *Tonnel) Enabled() bool   { return t != nil && t.api != nil }
func (t *Tonnel) BuyFee() float64 { return t.fee }

func (t *Tonnel) Book(ctx context.Context, key tonnel.ModelKey, backdrop, symbol string, limit int) ([]Listing, error) {
	gifts, err := t.api.ModelBook(ctx, key, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Listing, 0, len(gifts))
	for i := range gifts {
		g := &gifts[i]
		if g.IsBundle() || g.Premarket.Bool() || g.TelegramMarketplace.Bool() || g.Price.Float() <= 0 {
			continue
		}
		bd, sy := tonnel.BaseAttr(g.Backdrop), tonnel.BaseAttr(g.Symbol)
		if backdrop != "" && bd != backdrop {
			continue
		}
		if symbol != "" && sy != symbol {
			continue
		}
		out = append(out, Listing{
			Venue: t.Name(), ID: strconv.FormatInt(g.GiftID.Int(), 10),
			GiftNum: g.GiftNum.Int(), Key: g.Key(),
			Backdrop: bd, Symbol: sy, Price: g.Price.Float(),
			Seller: strconv.FormatInt(g.Seller.Int(), 10),
		})
	}
	return out, nil
}

func (t *Tonnel) Buy(ctx context.Context, l Listing, price float64) (*Receipt, error) {
	if l.ID == "" && price == 0 {
		// Capability probe: Tonnel's purchase call is wired, so answer without
		// touching the network.
		return &Receipt{OK: false, Message: "probe"}, nil
	}
	if err := validate(l, price); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(l.ID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("tonnel: некорректный id лота %q", l.ID)
	}
	res, err := t.api.BuyGift(ctx, id, price)
	if err != nil {
		return nil, err
	}
	r := &Receipt{OK: res.OK()}
	if res != nil {
		r.Message = res.Message
	}
	return r, nil
}

func (t *Tonnel) Owned(ctx context.Context) ([]Listing, error) {
	var out []Listing
	for _, listed := range []bool{false, true} {
		for page := 1; ; page++ {
			gifts, err := t.api.MyGifts(ctx, listed, page, 30)
			if err != nil {
				return nil, err
			}
			for i := range gifts {
				g := &gifts[i]
				out = append(out, Listing{
					Venue: t.Name(), ID: strconv.FormatInt(g.GiftID.Int(), 10),
					GiftNum: g.GiftNum.Int(), Key: g.Key(), Price: g.Price.Float(),
				})
			}
			if len(gifts) < 30 {
				break
			}
		}
	}
	return out, nil
}

func (t *Tonnel) Balance(ctx context.Context) (float64, error) {
	b, err := t.api.Balance(ctx)
	if err != nil {
		return 0, err
	}
	if b == nil {
		return 0, fmt.Errorf("tonnel: пустой ответ по балансу")
	}
	return b.GRAM, nil
}

// List implements Seller.
func (t *Tonnel) List(ctx context.Context, id string, price float64) error {
	giftID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("tonnel: некорректный id лота %q", id)
	}
	res, err := t.api.ListForSale(ctx, giftID, price)
	if err != nil {
		return err
	}
	if res != nil && !res.OK() {
		return fmt.Errorf("tonnel не принял листинг по %.2f: %s", price, res.Message)
	}
	return nil
}

// Cancel implements Seller.
func (t *Tonnel) Cancel(ctx context.Context, id string) error {
	giftID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("tonnel: некорректный id лота %q", id)
	}
	res, err := t.api.CancelSale(ctx, giftID)
	if err != nil {
		return err
	}
	if res != nil && !res.OK() {
		return fmt.Errorf("tonnel не дал снять лот: %s", res.Message)
	}
	return nil
}
