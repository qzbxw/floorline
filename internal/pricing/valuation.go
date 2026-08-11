package pricing

import (
	"math"
	"sort"
	"time"

	"floorline/internal/tonnel"
)

const (
	marketDisagreementLimit = 0.10
	// Waiting has to buy us something. A patient ask equal to fair value is
	// just the same exit with a slower fill, so it carries a modest 1.5% wait
	// premium and is still capped by a genuinely comparable live ask.
	patientWaitPremium = 0.015
	// depthGapLimit is how far one ask may sit above the previous one and still
	// belong to the same pool of liquidity. A book reading 4.21 / 4.21 / 10.20
	// is two real asks and one wish: the median of those three points is not a
	// price anyone can sell into, so the depth reference is capped this far
	// above the cheapest genuinely competing ask and the run of trusted depth
	// ends at the first larger jump.
	depthGapLimit = 0.20
	// depthWindow is how many of the cheapest external asks feed the reference.
	depthWindow = 3
	// minAsksBelowEntry is how many independent cheaper offers it takes to call
	// a listing overpriced rather than mispriced.
	minAsksBelowEntry = 2
)

// Params are the economics of a round trip.
type Params struct {
	// Fee is paid only on purchase. The seller receives the displayed ask and
	// Tonnel does not take the same fee from our sale a second time.
	Fee      float64
	Undercut float64
	Window   time.Duration
}

type Input struct {
	GiftID  int64
	OwnerID int64
	Key     tonnel.ModelKey
	Price   float64
	Cost    float64

	Book *Book
	Liq  Liquidity

	Floor      float64
	Supply     int
	Rarity     float64
	Backdrop   string
	Symbol     string
	Attribute  AttributeValue
	SnapshotAt time.Time
	Now        time.Time
	FX         FXContext
	Params     Params
}

// Valuation keeps four different questions separate. Liquidation is an
// emergency price, FastExit is the only price used for BUY, FairValue is price
// discovery, and PatientAsk is what can reasonably be listed and waited for.
type Valuation struct {
	Key    tonnel.ModelKey
	GiftID int64

	Price, Cost, Floor float64
	Supply             int
	Rarity             float64
	Backdrop, Symbol   string
	Attribute          AttributeValue

	CompetingAsk    float64
	HasCompetingAsk bool
	LiveDepth       float64
	// LiveDepthCount is the run of cheapest asks that form one pool of
	// liquidity, i.e. how many of them are reachable without stepping over a
	// hole in the book. It is not the raw ask count — see ExternalAsks.
	LiveDepthCount int
	ExternalAsks   int
	DepthPrice3    float64
	// DepthCapped records that the raw depth median sat above the cheapest ask
	// by more than depthGapLimit and was pulled back down to it.
	DepthCapped bool
	AskGap1     float64
	AskGap3     float64

	HistoryReference   float64
	CrossMarketSupport float64
	Cross              CrossMarket
	HistoryWeight      float64
	LiveWeight         float64
	CrossWeight        float64
	TraitWeight        float64

	Exit, Proceeds, Net, Edge float64
	ExitBasis                 string
	Liquidation               float64
	LiquidationBasis          string
	FastExit                  float64
	FairValue                 float64
	PatientAsk                float64
	PatientExit               float64 // compatibility alias for older callers
	BearCase                  float64

	Support            float64
	SupportGuarded     bool
	MarketDisagreement bool
	MarketDivergence   float64
	CrossDivergence    float64

	// AsksBelowEntry counts independent offers — ours excluded, every venue
	// included — that are already cheaper than what this listing costs us.
	AsksBelowEntry int
	// PricedAboveMarket is the hard verdict: no measured trait premium, several
	// cheaper offers in front of us, and a model that still wanted to price the
	// exit above our own entry. The exit was clamped and the trade is manual.
	PricedAboveMarket bool
	ExitCapped        string
	// CheaperAsks is how many live Tonnel asks undercut our own fast exit.
	CheaperAsks int

	DiscountToFloor     float64
	Liq                 Liquidity
	CompetitorsNear     int
	DaysOfSupply        float64
	ExpectedDays        float64
	FastExpectedDays    float64
	PatientExpectedDays float64
	ChosenExit          string
	Confidence          float64
	DataAge             time.Duration
	ScoreBreakdown      ScoreBreakdown
	FX                  FXContext

	Valid  bool
	Reason string
	input  Input
}

func Evaluate(in Input) Valuation {
	cost := in.Cost
	if cost <= 0 {
		cost = in.Price * (1 + math.Max(in.Params.Fee, 0))
	}
	v := Valuation{
		Key: in.Key, GiftID: in.GiftID, Price: in.Price, Cost: cost,
		Floor: in.Floor, Supply: in.Supply, Rarity: in.Rarity,
		Backdrop: in.Backdrop, Symbol: in.Symbol, Attribute: in.Attribute,
		Liq: in.Liq, FX: in.FX, input: in,
	}
	if in.Price <= 0 {
		v.Reason = "у листинга нет цены"
		return v
	}
	if in.Floor > 0 {
		v.DiscountToFloor = (in.Floor - in.Price) / in.Floor
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
			if age := in.Now.Sub(at); age > v.DataAge {
				v.DataAge = age
			}
		}
	}
	recompute(&v)
	return v
}

// CrossMarket is what other venues are showing for the same model: a robust
// price reference plus the actual queue behind it. The queue matters as much as
// the reference — five asks under our entry price on Portals mean our buyer can
// shop there instead, whatever Tonnel's own thin book suggests.
type CrossMarket struct {
	Support float64
	Asks    []float64 // merged across venues, ascending
	Venues  int
}

// WithCrossMarket promotes external depth from a footnote to a pricing input.
// It intentionally recomputes every dependent number, including BUY edge.
func WithCrossMarket(v Valuation, support float64) Valuation {
	return WithCrossDepth(v, CrossMarket{Support: support})
}

// WithCrossDepth is the full form: the reference and the queue behind it.
func WithCrossDepth(v Valuation, cm CrossMarket) Valuation {
	if cm.Support <= 0 {
		return v
	}
	v.Cross = cm
	v.CrossMarketSupport = cm.Support
	recompute(&v)
	return v
}

func recompute(v *Valuation) {
	in := v.input
	cost := v.Cost
	undercut := in.Params.Undercut
	if undercut < 0 || undercut >= 1 {
		undercut = 0
	}

	v.Valid, v.Reason = false, ""
	v.CompetingAsk, v.HasCompetingAsk = 0, false
	v.LiveDepth, v.DepthPrice3, v.AskGap1, v.AskGap3, v.LiveDepthCount = 0, 0, 0, 0, 0
	v.ExternalAsks, v.DepthCapped = 0, false
	v.AsksBelowEntry, v.CheaperAsks, v.PricedAboveMarket, v.ExitCapped = 0, 0, false, ""
	var external []Ask
	if in.Book != nil {
		external = in.Book.ExternalAsks(in.GiftID, in.OwnerID)
	}
	if len(external) > 0 {
		v.ExternalAsks = len(external)
		v.CompetingAsk, v.HasCompetingAsk = external[0].Price, true
		v.LiveDepthCount = contiguousDepth(external, depthWindow)
		if len(external) >= 2 {
			depth := medianFirst(external, depthWindow)
			// A hole in the book is not liquidity. Whatever the median of the
			// first three says, nothing proves the market clears more than
			// depthGapLimit above the cheapest genuinely competing ask, so that
			// is where the reference stops.
			if lid := external[0].Price * (1 + depthGapLimit); depth > lid {
				depth, v.DepthCapped = lid, true
			}
			v.LiveDepth = depth
		}
		v.DepthPrice3 = external[minInt(2, len(external)-1)].Price
		if v.Price > 0 {
			v.AskGap1 = math.Max(0, external[0].Price/v.Price-1)
			v.AskGap3 = math.Max(0, v.DepthPrice3/v.Price-1)
		}
		for _, a := range external {
			if a.Price > 0 && a.Price < cost {
				v.AsksBelowEntry++
			}
		}
	}
	for _, p := range v.Cross.Asks {
		if p > 0 && p < cost {
			v.AsksBelowEntry++
		}
	}

	// The snapshot floor may still be the candidate we are about to remove.
	// Liquidation therefore follows the next external ask; floor is only a
	// fallback when no book depth is available.
	liveFloor := v.CompetingAsk
	if liveFloor <= 0 {
		liveFloor = in.Floor
	}
	if liveFloor > 0 {
		v.Liquidation = liveFloor * (1 - undercut)
		v.LiquidationBasis = "живой стакан"
		if v.LiveDepthCount < 2 && in.Liq.Median > 0 && in.Liq.Median < v.Liquidation {
			v.Liquidation = in.Liq.Median
			v.LiquidationBasis = "история при тонком стакане"
		}
	}

	hw := historyWeight(in.Liq.DistinctGifts)
	if in.Liq.Median <= 0 {
		hw = 0
	}
	tw := 0.0
	if in.Attribute.Valid && in.Attribute.ExactSamples >= MinAttributeSamples && in.Attribute.Fair > 0 {
		tw = .05
	}
	cw := 0.0
	if v.CrossMarketSupport > 0 {
		cw = .20
	}
	lw := 0.0
	if v.LiveDepth > 0 {
		lw = math.Max(0, 1-hw-cw-tw)
	}
	if lw == 0 && hw+cw+tw == 0 {
		v.Reason = "нет нормальной опоры для выхода"
		return
	}

	// Sparse history is first pulled towards the live market and only then
	// allowed into the blend. Seven trades over three gifts cannot drag a live
	// 3.70 book down to a stale 3.26 median.
	v.HistoryReference = in.Liq.Median
	if in.Liq.Median > 0 && v.LiveDepth > 0 {
		trust := hw / .40
		v.HistoryReference = v.LiveDepth + (in.Liq.Median-v.LiveDepth)*trust
		v.MarketDivergence = math.Abs(in.Liq.Median/v.LiveDepth - 1)
		v.MarketDisagreement = v.MarketDivergence > marketDisagreementLimit
	}
	if v.CrossMarketSupport > 0 && v.LiveDepth > 0 {
		v.CrossDivergence = math.Abs(v.CrossMarketSupport/v.LiveDepth - 1)
	}

	// If the live source is missing, redistribute its weight rather than
	// silently losing part of the estimate.
	total := lw + hw + cw + tw
	if total <= 0 {
		v.Reason = "не из чего собрать цену выхода"
		return
	}
	lw, hw, cw, tw = lw/total, hw/total, cw/total, tw/total
	v.LiveWeight, v.HistoryWeight, v.CrossWeight, v.TraitWeight = lw, hw, cw, tw

	liveFast := v.LiveDepth
	if liveFast > 0 {
		liveFast *= 1 - undercut
	}
	v.FastExit = weighted(
		component{liveFast, lw}, component{v.HistoryReference, hw},
		component{v.CrossMarketSupport, cw}, component{in.Attribute.Fair, tw},
	)
	// History can pull a live price down, but cannot lift a fast exit through
	// the visible queue. External depth may raise that ceiling only when an
	// independent venue confirms it — and it works in both directions: a venue
	// with a real stack *below* the Tonnel book caps us instead of confirming
	// us, because that is where our buyer will go.
	ceiling := liveFast
	if v.CrossMarketSupport > 0 {
		crossFast := v.CrossMarketSupport * (1 - undercut)
		if len(v.Cross.Asks) >= 2 && crossFast < ceiling {
			ceiling = math.Max(math.Min(ceiling, crossFast), v.Liquidation)
		} else {
			ceiling = math.Max(ceiling, crossFast)
		}
	}
	if ceiling > 0 && v.FastExit > ceiling {
		v.FastExit = ceiling
	}
	v.FairValue = weighted(
		component{v.LiveDepth, lw}, component{v.HistoryReference, hw},
		component{v.CrossMarketSupport, cw}, component{in.Attribute.Fair, tw},
	)
	if v.FastExit <= 0 {
		v.Reason = "быстрый выход не посчитался"
		return
	}
	if v.Liquidation > 0 && v.FastExit < v.Liquidation {
		v.FastExit = v.Liquidation
	}

	// The overpriced-listing guard. Claiming we can get out above what we just
	// paid needs a reason, and "the model averaged its way there" is not one:
	// with no measured trait premium and several independent offers already
	// cheaper than our entry, we are the expensive ask, not the misprice. The
	// exit is pulled back to entry (never below the live liquidation price) and
	// the listing is flagged manual.
	if tw == 0 && v.AsksBelowEntry >= minAsksBelowEntry && cost > 0 && v.FastExit > cost {
		if capped := math.Max(v.Liquidation, cost); capped < v.FastExit {
			v.FastExit = capped
			v.PricedAboveMarket = true
			v.ExitCapped = "выход прижат ко входу: рынок дешевле нас"
		}
	}

	v.PatientAsk = math.Max(v.FastExit, v.FairValue*(1+patientWaitPremium))
	if in.Attribute.Valid && in.Attribute.ExactSamples >= MinAttributeSamples && in.Book != nil {
		if ask, ok := in.Book.BestAttributesExcluding(in.GiftID, in.OwnerID, in.Backdrop, in.Symbol); ok {
			v.PatientAsk = math.Max(math.Max(v.FastExit, v.FairValue), math.Min(v.PatientAsk, ask*(1-undercut)))
		}
	}
	v.PatientExit = v.PatientAsk
	if in.Liq.Trend > 0 && in.Liq.Trend < .98 {
		v.BearCase = math.Min(v.FastExit, v.FairValue*in.Liq.Trend)
	}

	v.Support = v.LiveDepth
	v.SupportGuarded = in.Liq.Median > 0 && v.LiveDepth > in.Liq.Median*(1+marketDisagreementLimit) && v.FastExit > in.Liq.Median
	v.Exit, v.ExitBasis, v.ChosenExit = v.FastExit, "быстрый выход", "быстрый"
	v.Proceeds = v.FastExit
	v.Net = v.Proceeds - v.Cost
	v.Edge = v.Net / v.Cost

	if in.Book != nil {
		v.CompetitorsNear = in.Book.CountBetween(v.FastExit, v.FastExit*1.05, in.GiftID, in.OwnerID)
		v.CheaperAsks = in.Book.CountBelow(v.FastExit, in.GiftID, in.OwnerID)
	}
	if in.Liq.Velocity > 0 {
		v.DaysOfSupply = float64(in.Supply) / in.Liq.Velocity
		v.FastExpectedDays = float64(1+v.CompetitorsNear) / in.Liq.Velocity
		v.ExpectedDays = v.FastExpectedDays
		share := math.Max(in.Attribute.ExactShare, .05)
		if !in.Attribute.Valid {
			share = .25
		}
		queue := 0
		if in.Book != nil {
			queue = in.Book.CountAttributesBetween(v.PatientAsk, v.PatientAsk*1.05, in.GiftID, in.OwnerID, in.Backdrop, in.Symbol)
		}
		v.PatientExpectedDays = float64(1+queue) / (in.Liq.Velocity * share)
	} else {
		v.DaysOfSupply, v.ExpectedDays, v.FastExpectedDays, v.PatientExpectedDays = math.Inf(1), math.Inf(1), math.Inf(1), math.Inf(1)
	}
	v.Confidence = confidence(in.Liq, in.Attribute)
	if v.MarketDisagreement {
		v.Confidence *= .65
	}
	v.Valid = true
	v.ScoreBreakdown = BuildScore(*v, 1)
}

type component struct{ value, weight float64 }

func weighted(cs ...component) float64 {
	sum, weight := 0.0, 0.0
	for _, c := range cs {
		if c.value <= 0 || c.weight <= 0 {
			continue
		}
		sum += c.value * c.weight
		weight += c.weight
	}
	if weight == 0 {
		return 0
	}
	return sum / weight
}

func historyWeight(distinct int) float64 {
	switch {
	case distinct < 5:
		return .10
	case distinct < 10:
		return .20
	case distinct < 20:
		return .30
	default:
		return .40
	}
}

// contiguousDepth counts how many of the cheapest asks belong to one pool of
// liquidity. The run ends at the first jump wider than depthGapLimit, because
// past a hole like 4.21 → 10.20 there is nothing to sell into.
func contiguousDepth(asks []Ask, limit int) int {
	n := minInt(limit, len(asks))
	for i := 1; i < n; i++ {
		if asks[i-1].Price <= 0 || asks[i].Price > asks[i-1].Price*(1+depthGapLimit) {
			return i
		}
	}
	return n
}

func medianFirst(asks []Ask, n int) float64 {
	if len(asks) == 0 {
		return 0
	}
	if n > len(asks) {
		n = len(asks)
	}
	prices := make([]float64, n)
	for i := 0; i < n; i++ {
		prices[i] = asks[i].Price
	}
	sort.Float64s(prices)
	if n%2 == 1 {
		return prices[n/2]
	}
	return (prices[n/2-1] + prices[n/2]) / 2
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
