package pricing

import (
	"math"
	"time"

	"floorline/internal/tonnel"
)

const (
	// supportAgreement is how far above the snapshot floor the live book may
	// sit and still be describing the same market. A book far above the floor
	// means the floor is stale or was filtered out, so the two do not
	// corroborate each other.
	supportAgreement = 0.15
	// supportBand is the width of the shelf we look for at the support level.
	supportBand = 0.10
	// supportSellers is how many live offers must hold that shelf before it may
	// overrule the trade history. One offer is a listing; two is a market.
	supportSellers = 2
)

// liveSupport is the cheapest price the market is currently offering, returned
// only when the order book and the market snapshot independently agree on it.
// Two sources are required because this number is allowed to overrule realised
// trades, and a single stale ask must never be able to do that.
func liveSupport(in Input, v Valuation) (float64, bool) {
	if in.Book == nil || !v.HasCompetingAsk || in.Floor <= 0 {
		return 0, false
	}
	if v.CompetingAsk > in.Floor*(1+supportAgreement) {
		return 0, false
	}
	support := math.Min(in.Floor, v.CompetingAsk)
	if in.Book.CountBetween(support, support*(1+supportBand), in.GiftID) < supportSellers {
		return 0, false
	}
	return support, true
}

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

	Floor      float64 // model floor from the full-market snapshot
	Supply     int     // listed count for the model
	Rarity     float64
	Backdrop   string
	Symbol     string
	Attribute  AttributeValue
	SnapshotAt time.Time
	Now        time.Time
	FX         FXContext

	Params Params
}

// Valuation is the full opinion on a single listing.
type Valuation struct {
	Key    tonnel.ModelKey
	GiftID int64

	Price     float64
	Floor     float64
	Supply    int
	Rarity    float64
	Backdrop  string
	Symbol    string
	Attribute AttributeValue

	// CompetingAsk is the cheapest ask that is NOT this listing.
	CompetingAsk    float64
	HasCompetingAsk bool

	Exit      float64 // the price we expect to actually sell at
	ExitBasis string  // which reference set the exit price
	Proceeds  float64 // exit net of fees
	Net       float64 // proceeds minus entry
	Edge      float64 // net / entry

	// Liquidation is the "out today" price: undercut everything the market is
	// currently showing, the visible book and the snapshot floor alike. It is a
	// fact about live offers and is deliberately kept separate from Exit, which
	// is a modelled opinion built partly on trade history.
	Liquidation      float64
	LiquidationBasis string

	// Support is the cheapest price the live market is actually offering, and
	// is only populated when two independent readings agree on it: the
	// market-snapshot floor and a book with more than one seller holding that
	// shelf. SupportGuarded records that it had to overrule the trade history.
	Support        float64
	SupportGuarded bool

	DiscountToFloor float64 // headline "-22%", for display only

	Liq             Liquidity
	CompetitorsNear int     // asks clustered just above our exit
	DaysOfSupply    float64 // listed supply divided by trade rate
	ExpectedDays    float64 // rough time to actually get filled

	FastExit            float64
	FastExpectedDays    float64
	PatientExit         float64
	PatientExpectedDays float64
	ChosenExit          string // fast | patient
	Confidence          float64
	DataAge             time.Duration
	ScoreBreakdown      ScoreBreakdown
	FX                  FXContext

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
		Key:       in.Key,
		GiftID:    in.GiftID,
		Price:     in.Price,
		Floor:     in.Floor,
		Supply:    in.Supply,
		Rarity:    in.Rarity,
		Backdrop:  in.Backdrop,
		Symbol:    in.Symbol,
		Attribute: in.Attribute,
		Liq:       in.Liq,
		FX:        in.FX,
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

	undercutPrice := 0.0
	if v.HasCompetingAsk {
		undercutPrice = v.CompetingAsk * (1 - undercut)
	}
	// The liquidation price answers a different question from the exit: not
	// "what is this worth" but "what do I have to ask to be the cheapest offer
	// on screen right now". It undercuts every live reference we hold.
	switch {
	case v.HasCompetingAsk && in.Floor > 0:
		v.Liquidation = math.Min(v.CompetingAsk, in.Floor) * (1 - undercut)
		v.LiquidationBasis = "undercut of live ask depth"
	case v.HasCompetingAsk:
		v.Liquidation, v.LiquidationBasis = undercutPrice, "undercut of best competing ask"
	case in.Floor > 0:
		v.Liquidation, v.LiquidationBasis = in.Floor*(1-undercut), "undercut of the model floor"
	}

	switch {
	case v.HasCompetingAsk && med > 0:
		if undercutPrice <= med {
			v.Exit, v.ExitBasis = undercutPrice, "undercut"
		} else {
			v.Exit, v.ExitBasis = med, "median"
		}
	case v.HasCompetingAsk:
		v.Exit, v.ExitBasis = undercutPrice, "undercut"
	case med > 0:
		// Nobody else is offering this model. We would be the only ask, so the
		// trade history is the only defensible reference.
		v.Exit, v.ExitBasis = med, "median (sole ask)"
	default:
		v.Reason = "no exit reference: neither a competing ask nor any trade history"
		return v
	}

	// The median is a statistic about the past. When every live offer sits
	// above it — the order book and the snapshot floor both — the median is
	// stale rather than conservative, and pricing off it means listing under a
	// market that never actually dropped. That is how a held position gets a
	// "target" 11% below the floor it could sell at today.
	if support, ok := liveSupport(in, v); ok {
		v.Support = support
		if guard := math.Min(undercutPrice, support); v.Exit < guard {
			v.Exit, v.ExitBasis, v.SupportGuarded = guard, "live support", true
		}
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
	// A fast exit means "priced to be lifted against the book that is on screen
	// right now", i.e. just under live ask depth — not "the low end of a sparse
	// sale history". Without a corroborated support level there is no live depth
	// to anchor to and the modelled exit is all there is.
	v.FastExit = v.Exit
	if v.Support > 0 && undercutPrice > 0 {
		if anchored := math.Min(undercutPrice, v.Support); anchored > v.FastExit {
			v.FastExit = anchored
		}
	}
	v.FastExpectedDays = v.ExpectedDays
	if v.FastExit != v.Exit && in.Book != nil && in.Liq.Velocity > 0 {
		queue := in.Book.CountBetween(v.FastExit, v.FastExit*1.05, in.GiftID)
		v.FastExpectedDays = float64(1+queue) / in.Liq.Velocity
	}
	v.ChosenExit = "fast"
	v.Confidence = confidence(in.Liq, in.Attribute)

	// A patient exit is allowed only when attributes have evidence. It is
	// independently capped by the next genuinely comparable ask.
	if in.Attribute.Valid && in.Attribute.Fair > 0 {
		patient := in.Attribute.Fair
		if in.Book != nil {
			if ask, ok := in.Book.BestAttributesExcluding(in.GiftID, in.Backdrop, in.Symbol); ok {
				patient = math.Min(patient, ask*(1-undercut))
			}
		}
		share := math.Max(in.Attribute.ExactShare, 0.05)
		velocity := in.Liq.Velocity * share
		queue := 0
		if in.Book != nil {
			queue = in.Book.CountAttributesBetween(patient, patient*1.05, in.GiftID, in.Backdrop, in.Symbol)
		}
		if velocity > 0 {
			v.PatientExpectedDays = float64(1+queue) / velocity
		} else {
			v.PatientExpectedDays = math.Inf(1)
		}
		v.PatientExit = patient
		fastProfitDay := (v.FastExit*(1-in.Params.Fee) - in.Price) / math.Max(v.FastExpectedDays, .25)
		patientProfitDay := (patient*(1-in.Params.Fee) - in.Price) / math.Max(v.PatientExpectedDays, .25)
		if in.Attribute.Confidence >= .55 && patientProfitDay > fastProfitDay {
			v.Exit, v.ExpectedDays, v.ChosenExit = patient, v.PatientExpectedDays, "patient"
			v.ExitBasis = "attribute fair value"
			v.Proceeds = v.Exit * (1 - in.Params.Fee)
			v.Net = v.Proceeds - in.Price
			v.Edge = v.Net / in.Price
		}
	}
	if !in.Now.IsZero() {
		bookAt := time.Time{}
		if in.Book != nil {
			bookAt = in.Book.FetchedAt
		}
		for _, at := range []time.Time{in.SnapshotAt, bookAt, in.FX.QuoteAt} {
			if at.IsZero() {
				continue
			}
			age := in.Now.Sub(at)
			if age > v.DataAge {
				v.DataAge = age
			}
		}
	}
	v.Valid = true
	v.ScoreBreakdown = BuildScore(v, 1)
	return v
}
