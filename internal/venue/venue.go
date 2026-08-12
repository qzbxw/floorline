// Package venue is the marketplace-agnostic view of a listing.
//
// Tonnel is not the only place a mispriced gift shows up, and the desk has been
// watching Portals and MRKT price the same models for weeks without being able
// to act on either. This package is what makes acting possible: one Listing
// type, one Venue interface, and a per-marketplace adapter behind it.
//
// The purchase request itself is deliberately isolated in a single function per
// venue, marked and empty until the real payload has been captured from the
// mini app's own traffic. Guessing the shape of a call that moves money is not
// something to do from documentation that does not exist.
package venue

import (
	"context"
	"errors"
	"fmt"

	"floorline/internal/tonnel"
)

// ErrBuyNotWired is returned by a venue whose purchase call has not been
// captured yet. It is a distinct error so the UI can say precisely that, rather
// than reporting a generic failure that looks like a network problem.
var ErrBuyNotWired = errors.New("покупка на этой площадке ещё не подключена")

// Listing is one gift for sale, wherever it is listed.
type Listing struct {
	// Venue names the marketplace. ID is that marketplace's own identifier —
	// an integer on Tonnel, a UUID on Portals — kept as a string so no venue
	// has to squeeze its ids into another's shape.
	Venue string
	ID    string
	// GiftNum is the gift's physical identity and the only field that means the
	// same thing everywhere. It is how the same item is recognised across
	// venues, and how a purchase is reconciled afterwards.
	GiftNum int64
	Key     tonnel.ModelKey

	Backdrop, Symbol string
	Price            float64 // gross ask, in GRAM
	Seller           string
}

// Receipt is what a venue reports after a purchase attempt.
type Receipt struct {
	OK bool
	// Spent is what actually left the balance, when the venue reports it.
	// Zero means the caller must reconcile against the balance itself.
	Spent   float64
	Message string
}

// Venue is a marketplace the desk can read and buy on.
type Venue interface {
	Name() string
	Enabled() bool
	// BuyFee is added on top of the displayed ask to get the real cost.
	BuyFee() float64
	// Book returns the cheap end of one model's sell queue, ascending.
	Book(ctx context.Context, key tonnel.ModelKey, backdrop, symbol string, limit int) ([]Listing, error)
	// Buy purchases a specific listing at a specific price. A venue that
	// disagrees about the price must refuse rather than fill at its own.
	Buy(ctx context.Context, l Listing, price float64) (*Receipt, error)
	// Owned lists what the account holds here, so inventory stays honest
	// across marketplaces.
	Owned(ctx context.Context) ([]Listing, error)
	// Balance is the spendable GRAM on this venue. Balances do not move between
	// marketplaces, so each one funds its own purchases.
	Balance(ctx context.Context) (float64, error)
}

// Seller is implemented by venues we can also list on. Only Tonnel qualifies
// today: it charges no sale commission, so moving a gift elsewhere to sell it
// costs more than it can recover.
type Seller interface {
	List(ctx context.Context, id string, price float64) error
	Cancel(ctx context.Context, id string) error
}

// Registry holds the configured venues and answers questions across all of them.
type Registry struct {
	venues []Venue
}

func NewRegistry(vs ...Venue) *Registry {
	r := &Registry{}
	for _, v := range vs {
		if v != nil && v.Enabled() {
			r.venues = append(r.venues, v)
		}
	}
	return r
}

// Names lists the configured venues, for /status.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.venues))
	for _, v := range r.venues {
		out = append(out, v.Name())
	}
	return out
}

// Find returns the venue with the given name.
func (r *Registry) Find(name string) (Venue, bool) {
	if r == nil {
		return nil, false
	}
	for _, v := range r.venues {
		if v.Name() == name {
			return v, true
		}
	}
	return nil, false
}

// Buyable reports which venues can actually complete a purchase right now.
// A venue whose buy call has not been captured yet is readable but not
// tradeable, and the difference has to be visible rather than discovered at
// the moment money is supposed to move.
func (r *Registry) Buyable(ctx context.Context) (ready, pending []string) {
	if r == nil {
		return nil, nil
	}
	for _, v := range r.venues {
		if _, err := v.Buy(ctx, Listing{Venue: v.Name()}, 0); errors.Is(err, ErrBuyNotWired) {
			pending = append(pending, v.Name())
			continue
		}
		ready = append(ready, v.Name())
	}
	return ready, pending
}

// CheapestAcross returns the cheapest listing for a model on any venue, which
// is the price a buyer would take instead of ours.
func (r *Registry) CheapestAcross(ctx context.Context, key tonnel.ModelKey, backdrop, symbol string) (Listing, bool) {
	var best Listing
	found := false
	for _, v := range r.venues {
		rows, err := v.Book(ctx, key, backdrop, symbol, 1)
		if err != nil || len(rows) == 0 {
			continue
		}
		if !found || rows[0].Price < best.Price {
			best, found = rows[0], true
		}
	}
	return best, found
}

// validate is the shared precondition every venue applies before spending.
func validate(l Listing, price float64) error {
	if l.ID == "" {
		return fmt.Errorf("%s: у лота нет идентификатора", l.Venue)
	}
	if price <= 0 {
		return fmt.Errorf("%s: цена покупки должна быть больше нуля", l.Venue)
	}
	if l.Price > 0 && l.Price != price {
		return fmt.Errorf("%s: цена уехала с %.3f на %.3f", l.Venue, price, l.Price)
	}
	return nil
}
