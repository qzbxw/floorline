package venue

import (
	"context"
	"errors"
	"testing"

	"floorline/internal/tonnel"
)

// A venue whose purchase call has not been captured yet must say so in a way
// the caller can recognise, not fail like a network blip. The desk shows this
// in /status, and the distinction is the difference between "try again" and
// "this cannot work until somebody captures the request".
func TestUnwiredVenuesReportThemselvesAsSuch(t *testing.T) {
	ctx := context.Background()
	hc, err := NewHTTPClient(5)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}

	for _, v := range []Venue{
		NewPortals(hc, nil, .02, HumanPace()),
		NewMRKT(hc, stubSession{}, .02, HumanPace()),
	} {
		l := Listing{Venue: v.Name(), ID: "abc", Price: 5}
		if _, err := v.Buy(ctx, l, 5); !errors.Is(err, ErrBuyNotWired) {
			t.Errorf("%s: buy error = %v, want ErrBuyNotWired", v.Name(), err)
		}
	}
}

// Buyable has to separate the venues that can execute from the ones that only
// read, without sending a purchase anywhere to find out.
func TestBuyableSeparatesReadyFromPending(t *testing.T) {
	hc, err := NewHTTPClient(5)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	r := NewRegistry(
		NewTonnel(stubTonnel{}, .005),
		NewPortals(hc, nil, .02, HumanPace()),
		NewMRKT(hc, stubSession{}, .02, HumanPace()),
	)

	ready, pending := r.Buyable(context.Background())
	if len(ready) != 1 || ready[0] != "Tonnel" {
		t.Errorf("ready = %v, want just Tonnel", ready)
	}
	if len(pending) != 2 {
		t.Errorf("pending = %v, want both unwired venues", pending)
	}
}

// The price check is the one thing standing between a stale card and paying
// whatever the venue currently asks.
func TestBuyRefusesWhenThePriceMoved(t *testing.T) {
	v := NewTonnel(stubTonnel{}, .005)
	l := Listing{Venue: "Tonnel", ID: "1", Price: 5.5}

	if _, err := v.Buy(context.Background(), l, 5); err == nil {
		t.Fatal("bought at a price the listing no longer carries")
	}
}

func TestDisabledVenuesAreDroppedFromTheRegistry(t *testing.T) {
	r := NewRegistry(NewTonnel(nil, .005), nil)
	if names := r.Names(); len(names) != 0 {
		t.Errorf("registry kept unusable venues: %v", names)
	}
}

type stubSession struct{}

func (stubSession) InitData(context.Context, string) (string, error) { return "stub", nil }
func (stubSession) Invalidate(string)                                {}

type stubTonnel struct{}

func (stubTonnel) UserID() int64 { return 1 }
func (stubTonnel) ModelBook(context.Context, tonnel.ModelKey, int) ([]tonnel.Gift, error) {
	return nil, nil
}
func (stubTonnel) BuyGift(context.Context, int64, float64) (*tonnel.Result, error) {
	return &tonnel.Result{Status: "success"}, nil
}
func (stubTonnel) ListForSale(context.Context, int64, float64) (*tonnel.Result, error) {
	return &tonnel.Result{Status: "success"}, nil
}
func (stubTonnel) CancelSale(context.Context, int64) (*tonnel.Result, error) {
	return &tonnel.Result{Status: "success"}, nil
}
func (stubTonnel) MyGifts(context.Context, bool, int, int) ([]tonnel.Gift, error) { return nil, nil }
func (stubTonnel) Balance(context.Context) (*tonnel.Balance, error) {
	return &tonnel.Balance{GRAM: 100}, nil
}
