package pricing

import (
	"math"
	"time"

	"floorline/internal/tonnel"
)

// Params are the economics of a round trip.
type Params struct {
	// Fee is the fraction of the sale price that does not reach you. Tonnel
	// charges no commission, so this is the referral cut only (~0.5%) — which
	// is exactly why the entry bar here is set by liquidity and not by fees.
	Fee float64
	// Undercut is how far below the best competing ask we assume we must list
	// in order to be the one that sells.
	Undercut float64
	// Window is the trade-history lookback.
	Window time.Duration
}

// Input is everything needed to price one listing.
type Input struct {
	GiftID int64
	Key    tonnel.ModelKey
	Price  float64

	Book *Book
	Liq  Liquidity

	Floor  float64 // model floor from the full-market snapshot
	Supply int     // listed count for the model
	Rarity float64

	Params Params
}

// Valuation is the full opinion on a single listing.
type Valuation struct {
	Key    tonnel.ModelKey
	GiftID int64

	Price  float64
	Floor  float64
	Supply int
	Rarity float64

	// CompetingAsk is the cheapest ask that is NOT this listing.
	CompetingAsk    float64
	HasCompetingAsk bool

	Exit      float64 // the price we expect to actually sell at
	ExitBasis string  // which reference set the exit price
	Proceeds  float64 // exit net of fees
	Net       float64 // proceeds minus entry
	Edge      float64 // net / entry

	DiscountToFloor float64 // headline "-22%", for display only

	Liq             Liquidity
	CompetitorsNear int     // asks clustered just above our exit
	DaysOfSupply    float64 // listed supply divided by trade rate
	ExpectedDays    float64 // rough time to actually get filled

	Valid  bool
	Reason string // why the valuation is unusable, when it is
}

// Evaluate prices a single listing.
//
// The trigger every other tool gets wrong is the exit price. "20% below floor"
// is meaningless when the listing you are buying *is* the floor: the moment you
// own it, the price you have to beat is the next ask up. So the exit is modelled
// as undercutting the best *competing* ask, and then capped by what the model
// has actually been trading at — whichever is worse.
func Evaluate(in Input) Valuation {
	v := Valuation{
		Key:    in.Key,
		GiftID: in.GiftID,
		Price:  in.Price,
		Floor:  in.Floor,
		Supply: in.Supply,
		Rarity: in.Rarity,
		Liq:    in.Liq,
	}

	if in.Price <= 0 {
		v.Reason = "listing has no price"
		return v
	}
	if in.Floor > 0 {
		v.DiscountToFloor = (in.Floor - in.Price) / in.Floor
	}

	undercut := in.Params.Undercut
	if undercut < 0 || undercut >= 1 {
		undercut = 0
	}

	if in.Book != nil {
		v.CompetingAsk, v.HasCompetingAsk = in.Book.BestExcluding(in.GiftID)
	}
	med := in.Liq.Median

	switch {
	case v.HasCompetingAsk && med > 0:
		undercutPrice := v.CompetingAsk * (1 - undercut)
		if undercutPrice <= med {
			v.Exit, v.ExitBasis = undercutPrice, "undercut"
		} else {
			v.Exit, v.ExitBasis = med, "median"
		}
	case v.HasCompetingAsk:
		v.Exit, v.ExitBasis = v.CompetingAsk*(1-undercut), "undercut"
	case med > 0:
		// Nobody else is offering this model. We would be the only ask, so the
		// trade history is the only defensible reference.
		v.Exit, v.ExitBasis = med, "median (sole ask)"
	default:
		v.Reason = "no exit reference: neither a competing ask nor any trade history"
		return v
	}

	if v.Exit <= 0 {
		v.Reason = "computed exit price is not positive"
		return v
	}

	v.Proceeds = v.Exit * (1 - in.Params.Fee)
	v.Net = v.Proceeds - in.Price
	v.Edge = v.Net / in.Price

	// Sellers clustered just above our exit will undercut us straight back, so
	// this is the price-war risk, not the "wall to climb".
	if in.Book != nil {
		v.CompetitorsNear = in.Book.CountBetween(v.Exit, v.Exit*1.05, in.GiftID)
	}

	if in.Liq.Velocity > 0 {
		v.DaysOfSupply = float64(in.Supply) / in.Liq.Velocity
		v.ExpectedDays = float64(1+v.CompetitorsNear) / in.Liq.Velocity
	} else {
		v.DaysOfSupply = math.Inf(1)
		v.ExpectedDays = math.Inf(1)
	}

	v.Valid = true
	return v
}
