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
	// a listing overpriced rather than mispriced. One is enough: a single
	// genuinely cheaper offer is where our buyer goes, whatever the local book
	// wishes.
	minAsksBelowEntry = 1
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
	GiftNum int64
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
	// TicketRef is the largest single purchase the desk allows, used only to
	// judge whether a trade is big enough to be worth a slot. Zero disables the
	// size term entirely, which is what every caller that is pricing an owned
	// position rather than ranking a candidate wants.
	TicketRef float64
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
	Appearance         Appearance
	// AppearanceUnpriced records that this specimen looks like it should trade
	// above the plain ones, and that nothing on any venue was comparable enough
	// to say by how much. The exit stays the conservative one — we do not invent
	// a premium — but the operator is told the number is a floor on the truth
	// rather than an estimate of it.
	AppearanceUnpriced bool

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
	// ExitInvented records that neither the queue nor the tape could be
	// believed on its own, so the exit is an interpolation the engine had to
	// make rather than a price anyone has quoted or paid.
	//
	// It has to be remembered separately because the clamp erases its own
	// evidence: once the exit is pulled into the band the history supports, the
	// measured divergence falls below the threshold that triggered it, and a
	// score reading only that number would rank the invented price as if it had
	// been observed.
	ExitInvented bool

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
	// ExitFromCross records that another venue's queue, not the local book, is
	// what bounds the exit. When it does, a hole in the local book is no longer
	// something the price depends on, and must not be scored as if it were.
	ExitFromCross bool
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
	v.Appearance = Appraise(in.Backdrop, in.Symbol, in.GiftNum)
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
	Asks    []float64 // merged across venues, ascending, in each venue's own gross terms
	Venues  int
	// BestBuyerCost is what a buyer actually pays for the cheapest external
	// offer. Today that is the displayed ask — Portals and MRKT charge their
	// sellers, not their buyers — but the bound has to be stated in the unit
	// the buyer compares, because our own side is not fee-free: a Tonnel ask
	// costs its buyer the referral on top. Keeping the field in buyer terms is
	// what lets a venue that does charge buyers be added without moving the
	// comparison back onto mismatched stickers.
	BestBuyerCost float64
	// DepthBuyerCost is the same measure for the third rung of the merged
	// queue: the price a patient seller has to outlast, rather than the one a
	// fast seller has to undercut.
	DepthBuyerCost float64
	// Comparable reports that the cheapest external offer was matched on the
	// attributes that move the price, not merely on the model. A model-wide
	// queue is the ordinary specimens of that model, and pricing an Onyx Black
	// gift against them is comparing two different assets.
	Comparable bool
	// Unreachable counts venues that could not be read at all — a timeout or a
	// rejected session, as opposed to a venue with nothing listed. Losing this
	// data silently removes the cap that holds an optimistic exit down, so the
	// count travels with the quote and blocks unattended buying.
	Unreachable int
}

// fillBuyerCosts derives the buyer-cost bounds from the raw queue when the
// caller did not supply them.
//
// Every producer of a CrossMarket should set them, because only the caller
// knows each venue's buyer fee. But a caller that forgets would otherwise lose
// the cross-market cap entirely and silently — turning the one guard that holds
// an optimistic exit down into a no-op. Falling back to the gross asks keeps
// the cap in place and merely gives up the fee correction.
func (cm *CrossMarket) fillBuyerCosts() {
	if len(cm.Asks) == 0 {
		return
	}
	if cm.BestBuyerCost <= 0 {
		cm.BestBuyerCost = cm.Asks[0]
	}
	if cm.DepthBuyerCost <= 0 {
		cm.DepthBuyerCost = cm.Asks[minInt(depthWindow, len(cm.Asks))-1]
	}
}

// WithCrossMarket promotes external depth from a footnote to a pricing input.
// It intentionally recomputes every dependent number, including BUY edge.
func WithCrossMarket(v Valuation, support float64) Valuation {
	return WithCrossDepth(v, CrossMarket{Support: support})
}

// WithCrossDepth is the full form: the reference and the queue behind it.
func WithCrossDepth(v Valuation, cm CrossMarket) Valuation {
	cm.fillBuyerCosts()
	if cm.Support <= 0 {
		// No usable price, but "a venue could not be reached" is itself an input:
		// it blocks unattended buying and costs the trade rank, so it has to be
		// recorded *and* fed back through the score.
		if cm.Unreachable == v.Cross.Unreachable {
			return v
		}
		v.Cross.Unreachable = cm.Unreachable
		recompute(&v)
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
	readWalkaway(v, external, v.Cost, in.Params.Fee)
	setLiquidation(v, in, undercut)

	if !blendWeights(v, in) {
		return
	}
	if !priceExits(v, in, undercut) {
		return
	}
	clampToHistory(v, in)
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
	v.PricedAboveMarket, v.ExitCapped, v.ExitFromCross = false, "", false
	v.AppearanceUnpriced = false
	v.Walkaway, v.WalkawayVenue = 0, ""
	v.Liquidation, v.LiquidationBasis = 0, ""
	v.MarketDivergence, v.MarketDisagreement, v.CrossDivergence = 0, false, 0
	v.ExitInvented = false
	v.BearCase = 0
}

// settle turns the priced ladder into the numbers a decision is made on.
func settle(v *Valuation, in Input) {
	measureDivergence(v, in)
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

	// Confidence is about the trade history alone: how many independent prints
	// there are and how tidy they look. It used to also absorb a disagreement
	// penalty, which then reappeared in the depth factor and again through
	// cross-market divergence — one fact charged three times. The discount now
	// lives in exactly one place, evidenceQuality.
	v.Confidence = confidence(in.Liq, in.Attribute)
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
