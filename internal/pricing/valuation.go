package pricing

import (
	"math"
	"time"

	"floorline/internal/tonnel"
)

// CrossDivergenceLimit is how far Tonnel and the other venues may disagree
// before a trade stops being something a machine should do on its own. It lives
// here because the detector, the portfolio adviser, the card renderer and the
// auto-reprice path all have to mean the same thing by "they disagree".
const CrossDivergenceLimit = 0.15

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
	// UndercutsEntry is the subset of those that sit *meaningfully* cheaper
	// (crossUndercutMargin below entry), i.e. too far away to be explained by
	// rounding or a fee difference between venues.
	UndercutsEntry int
	// Walkaway is the cheapest offer on any venue that a buyer could take
	// instead of ours. It is what actually bounds the exit, and it is the reason
	// a hole in one venue's book can no longer read as room to sell into.
	Walkaway      float64
	WalkawayVenue string
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
	// Unreachable counts venues that could not be read at all — a timeout or a
	// rejected session, as opposed to a venue with nothing listed. Losing this
	// data silently removes the cap that holds an optimistic exit down, so the
	// count travels with the quote and blocks unattended buying.
	Unreachable int
}

// WithCrossMarket promotes external depth from a footnote to a pricing input.
// It intentionally recomputes every dependent number, including BUY edge.
func WithCrossMarket(v Valuation, support float64) Valuation {
	return WithCrossDepth(v, CrossMarket{Support: support})
}

// WithCrossDepth is the full form: the reference and the queue behind it.
func WithCrossDepth(v Valuation, cm CrossMarket) Valuation {
	if cm.Support <= 0 {
		// Even with no usable price, the fact that a venue could not be reached
		// has to survive: it is what stops the desk from trading blind.
		v.Cross.Unreachable = cm.Unreachable
		return v
	}
	v.Cross = cm
	v.CrossMarketSupport = cm.Support
	recompute(&v)
	return v
}

// recompute is the exit-pricing pipeline. Every stage lives in exit.go and owns
// one question; this function is the order in which they are asked.
func recompute(v *Valuation) {
	in := v.input
	undercut := in.Params.Undercut
	if undercut < 0 || undercut >= 1 {
		undercut = 0
	}
	resetDerived(v)

	// These have to be checked on every pass, not once in Evaluate.
	// WithCrossDepth re-runs this whole pipeline on a Valuation that Evaluate
	// may already have rejected, and a delisted gift arrives here with a price
	// of zero — which turned Edge into Net/0 = +Inf and rendered a card reading
	// "BUY, +Inf%". The feed path never saw it because tradable() screens zero
	// prices first, but /val did.
	if in.Price <= 0 {
		v.Reason = "у листинга нет цены — скорее всего его уже сняли с продажи"
		return
	}
	if v.Cost <= 0 {
		v.Reason = "непонятна цена входа"
		return
	}

	external := readLocalBook(v, in)
	readWalkaway(v, external, v.Cost)
	setLiquidation(v, in, undercut)

	if !blendWeights(v, in) {
		return
	}
	if !priceExits(v, in, undercut) {
		return
	}
	clampOverpriced(v, v.Cost)
	buildLadder(v, in, undercut)

	settle(v, in)
}

// resetDerived clears everything recompute is about to rebuild. WithCrossDepth
// re-runs the whole pipeline on an already-populated Valuation, so a stale field
// left behind here would silently survive into the second pass.
func resetDerived(v *Valuation) {
	v.Valid, v.Reason = false, ""
	v.CompetingAsk, v.HasCompetingAsk = 0, false
	v.LiveDepth, v.DepthPrice3, v.AskGap1, v.AskGap3, v.LiveDepthCount = 0, 0, 0, 0, 0
	v.ExternalAsks, v.DepthCapped = 0, false
	v.AsksBelowEntry, v.UndercutsEntry, v.CheaperAsks = 0, 0, 0
	v.PricedAboveMarket, v.ExitCapped = false, ""
	v.Walkaway, v.WalkawayVenue = 0, ""
	v.Liquidation, v.LiquidationBasis = 0, ""
	v.MarketDivergence, v.MarketDisagreement, v.CrossDivergence = 0, false, 0
	v.BearCase = 0
}

// settle turns the priced ladder into the numbers a decision is made on.
func settle(v *Valuation, in Input) {
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
	expectedDays(v, in)

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

