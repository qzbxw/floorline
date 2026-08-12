package pricing

import (
	"math"
	"sort"
)

// This file holds the exit-pricing pipeline. recompute in valuation.go is the
// list of stages; each stage below owns one question and writes only the fields
// that answer it.
//
// The ordering matters and is not arbitrary:
//
//	readLocalBook   what the Tonnel queue looks like
//	readWalkaway    the cheapest offer a buyer could take instead of ours,
//	                on ANY venue — this is what actually bounds our exit
//	setLiquidation  the price that makes us the cheapest offer on screen
//	blendWeights    how much each source of truth is trusted
//	priceExits      fast and fair, then every clamp
//	buildLadder     liquidation ≤ fast ≤ fair ≤ patient, and the wait each implies
//	settle          edge, queue position, days, confidence, score

const (
	// crossUndercutMargin is how far below our entry an external ask has to sit
	// before it counts as evidence that we are the expensive listing rather
	// than the mispriced one. Rounding and fee differences between venues are
	// worth a percent or two on their own.
	crossUndercutMargin = 0.05
	// minCrossUndercuts is how many such offers it takes to veto the trade
	// outright. Three independent venues-or-sellers pricing well under our
	// entry is a market, not a coincidence.
	minCrossUndercuts = 3
)

// readLocalBook measures the Tonnel queue: who competes with us, how much of
// the cheap end is one connected pool of liquidity, and how far the price has
// to travel to reach it.
func readLocalBook(v *Valuation, in Input) []Ask {
	var external []Ask
	if in.Book != nil {
		external = in.Book.ExternalAsks(in.GiftID, in.OwnerID)
	}
	if len(external) == 0 {
		return nil
	}

	v.ExternalAsks = len(external)
	v.CompetingAsk, v.HasCompetingAsk = external[0].Price, true
	v.LiveDepthCount = contiguousDepth(external, depthWindow)

	// A depth reference needs at least two competing offers; a single ask is a
	// wish, not a market. Beyond that, only the run of asks that forms one
	// connected pool of liquidity counts — a book reading 4.21 / 7.90 / 10.20
	// has one real price in it, and the median of all three is a number nobody
	// can sell into.
	if len(external) >= 2 {
		trusted := v.LiveDepthCount
		if trusted < 1 {
			trusted = 1
		}
		depth := medianFirst(external, trusted)
		// "Capped" means a hole cut the run short, not merely that the book is
		// short. Two contiguous asks are a thin book, not a gappy one, and the
		// auto-buy veto that reads this flag must tell the two apart.
		if trusted < minInt(depthWindow, len(external)) {
			v.DepthCapped = true
		}
		if lid := external[0].Price * (1 + depthGapLimit); depth > lid {
			depth, v.DepthCapped = lid, true
		}
		v.LiveDepth = depth
	}

	// DepthPrice3 is the third rung of the ladder. With fewer than three asks
	// there is no third rung, and reusing the first one would make AskGap3 a
	// duplicate of AskGap1 that then counts twice in the depth score.
	if len(external) >= depthWindow {
		v.DepthPrice3 = external[depthWindow-1].Price
	}
	if v.Price > 0 {
		v.AskGap1 = math.Max(0, external[0].Price/v.Price-1)
		if v.DepthPrice3 > 0 {
			v.AskGap3 = math.Max(0, v.DepthPrice3/v.Price-1)
		}
	}
	return external
}

// readWalkaway finds the cheapest offer, on any venue, that a buyer could take
// instead of ours, and counts how much of the market is already cheaper than
// what this listing costs us.
//
// This is the number the old engine did not have. It priced the exit against
// the Tonnel queue alone, so a hole in one venue's book read as room to sell
// into even while three other venues were quoting well below it.
func readWalkaway(v *Valuation, external []Ask, cost float64) {
	for _, a := range external {
		if a.Price > 0 && a.Price < cost {
			v.AsksBelowEntry++
		}
		if a.Price > 0 && a.Price < cost*(1-crossUndercutMargin) {
			v.UndercutsEntry++
		}
	}
	for _, p := range v.Cross.Asks {
		if p > 0 && p < cost {
			v.AsksBelowEntry++
		}
		if p > 0 && p < cost*(1-crossUndercutMargin) {
			v.UndercutsEntry++
		}
	}

	v.Walkaway = v.CompetingAsk
	if len(v.Cross.Asks) > 0 {
		if best := v.Cross.Asks[0]; best > 0 && (v.Walkaway <= 0 || best < v.Walkaway) {
			v.Walkaway = best
			v.WalkawayVenue = "площадки"
		}
	}
	if v.Walkaway > 0 && v.WalkawayVenue == "" {
		v.WalkawayVenue = "Tonnel"
	}
}

// setLiquidation prices the emergency exit: what we would have to ask to be the
// cheapest offer anyone can see right now.
//
// It is measured against the walkaway price rather than the Tonnel queue. That
// distinction is the whole fix: when liquidation came from the local book, a
// gap in that book lifted it above what other venues were charging, and because
// liquidation is a floor under every other exit it dragged the fast exit up
// with it — cancelling the cross-market cap that was supposed to catch exactly
// this case.
func setLiquidation(v *Valuation, in Input, undercut float64) {
	floor := v.Walkaway
	if floor <= 0 {
		floor = in.Floor
	}
	if floor <= 0 {
		return
	}
	v.Liquidation = floor * (1 - undercut)
	v.LiquidationBasis = "живой стакан"
	if v.WalkawayVenue == "площадки" {
		v.LiquidationBasis = "другие площадки дешевле"
	}
	// With no trusted depth the single visible ask may be a fantasy. Trades are
	// facts, so a lower history median wins.
	if v.LiveDepthCount < 2 && in.Liq.Median > 0 && in.Liq.Median < v.Liquidation {
		v.Liquidation = in.Liq.Median
		v.LiquidationBasis = "история при тонком стакане"
	}
}

// blendWeights decides how much each source of truth is trusted, and shrinks a
// sparse history towards the live market before letting it vote. It reports
// false when there is nothing to price against at all.
func blendWeights(v *Valuation, in Input) bool {
	hw := historyWeight(in.Liq.DistinctGifts)
	if in.Liq.Median <= 0 {
		hw = 0
	}
	tw := 0.0
	if hasTraitEvidence(in.Attribute) {
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
		return false
	}

	// Seven trades over three gifts cannot drag a live 3.70 book down to a stale
	// 3.26 median, so sparse history is pulled towards the live market first and
	// only then allowed into the blend.
	v.HistoryReference = in.Liq.Median
	if in.Liq.Median > 0 && v.LiveDepth > 0 {
		trust := hw / .40
		v.HistoryReference = v.LiveDepth + (in.Liq.Median-v.LiveDepth)*trust
	}
	// MarketDivergence is measured later, against the price we actually settle
	// on — see measureDivergence.
	if v.CrossMarketSupport > 0 && v.LiveDepth > 0 {
		v.CrossDivergence = math.Abs(v.CrossMarketSupport/v.LiveDepth - 1)
	}

	total := lw + hw + cw + tw
	if total <= 0 {
		v.Reason = "не из чего собрать цену выхода"
		return false
	}
	v.LiveWeight, v.HistoryWeight, v.CrossWeight, v.TraitWeight = lw/total, hw/total, cw/total, tw/total
	return true
}

// priceExits builds the fast and fair prices and then applies every clamp that
// stands between a blend and a price someone would actually pay.
func priceExits(v *Valuation, in Input, undercut float64) bool {
	liveFast := v.LiveDepth
	if liveFast > 0 {
		liveFast *= 1 - undercut
	}
	v.FastExit = weighted(
		component{liveFast, v.LiveWeight}, component{v.HistoryReference, v.HistoryWeight},
		component{v.CrossMarketSupport, v.CrossWeight}, component{in.Attribute.Fair, v.TraitWeight},
	)
	v.FairValue = weighted(
		component{v.LiveDepth, v.LiveWeight}, component{v.HistoryReference, v.HistoryWeight},
		component{v.CrossMarketSupport, v.CrossWeight}, component{in.Attribute.Fair, v.TraitWeight},
	)

	// The visible queue is the ceiling. History can pull a price down but must
	// not lift it through the offers standing in front of us.
	//
	// A cheaper venue lowers that ceiling, because that is where our buyer goes.
	// A more expensive one does NOT raise it: we sell into the Tonnel queue, and
	// another venue asking more is not evidence that our lot will fetch more.
	// Letting it raise the ceiling is how a single expensive foreign quote used
	// to manufacture an edge.
	ceiling := liveFast
	if ceiling > 0 && v.CrossMarketSupport > 0 && len(v.Cross.Asks) >= 2 {
		if crossFast := v.CrossMarketSupport * (1 - undercut); crossFast < ceiling {
			ceiling = crossFast
			v.ExitCapped = "выход прижат к стакану других площадок"
			v.ExitFromCross = true
		}
	}
	if ceiling > 0 && v.FastExit > ceiling {
		v.FastExit = ceiling
	}
	if v.FastExit <= 0 {
		v.Reason = "быстрый выход не посчитался"
		return false
	}
	// Being the cheapest offer on screen is always available to us, so the fast
	// exit is never worse than that. Liquidation is derived from the walkaway
	// price, so this can no longer lift the exit above what other venues charge.
	if v.Liquidation > 0 && v.FastExit < v.Liquidation {
		v.FastExit = v.Liquidation
	}
	return true
}

// clampOverpriced is the guard against claiming we can get out above what we
// just paid. That claim needs a reason, and "the model averaged its way there"
// is not one.
func clampOverpriced(v *Valuation, cost float64) {
	if v.TraitWeight > 0 || cost <= 0 {
		return // a measured trait premium is a real reason to price above the crowd
	}
	// The hard veto: several independent offers are not merely cheaper than our
	// entry but meaningfully cheaper. The local book cannot argue us out of that
	// however wide its own gap is.
	if v.UndercutsEntry >= minCrossUndercuts {
		v.PricedAboveMarket = true
		v.ExitCapped = "рынок стоит заметно ниже входа — локальный гэп это не отменяет"
		if v.FastExit > cost {
			v.FastExit = math.Max(v.Liquidation, cost)
		}
		return
	}
	if v.AsksBelowEntry >= minAsksBelowEntry && v.FastExit > cost {
		if capped := math.Max(v.Liquidation, cost); capped < v.FastExit {
			v.FastExit = capped
			v.PricedAboveMarket = true
			v.ExitCapped = "выход прижат ко входу: рынок дешевле нас"
		}
	}
}

// buildLadder derives the patient ask and enforces the one property the ladder
// has to have: a higher price cannot also be a faster sale.
func buildLadder(v *Valuation, in Input, undercut float64) {
	v.PatientAsk = math.Max(v.FastExit, v.FairValue*(1+patientWaitPremium))
	if hasTraitEvidence(in.Attribute) && in.Book != nil {
		if ask, ok := in.Book.BestAttributesExcluding(in.GiftID, in.OwnerID, in.Backdrop, in.Symbol); ok {
			v.PatientAsk = math.Max(math.Max(v.FastExit, v.FairValue), math.Min(v.PatientAsk, ask*(1-undercut)))
		}
	}
	v.PatientExit = v.PatientAsk
	if in.Liq.Trend > 0 && in.Liq.Trend < .98 {
		v.BearCase = math.Min(v.FastExit, v.FairValue*in.Liq.Trend)
	}
}

// expectedDays estimates how long each rung of the ladder takes to fill.
func expectedDays(v *Valuation, in Input) {
	if in.Liq.Velocity <= 0 {
		v.DaysOfSupply, v.ExpectedDays = math.Inf(1), math.Inf(1)
		v.FastExpectedDays, v.PatientExpectedDays = math.Inf(1), math.Inf(1)
		return
	}
	v.DaysOfSupply = float64(in.Supply) / in.Liq.Velocity
	v.FastExpectedDays = float64(1+v.CompetitorsNear) / in.Liq.Velocity
	v.ExpectedDays = v.FastExpectedDays

	// The patient ask waits for a buyer who wants this exact backdrop and
	// symbol, so it draws on a fraction of the model's flow.
	share := math.Max(in.Attribute.ExactShare, .05)
	if !in.Attribute.Valid {
		share = .25
	}
	queue := 0
	if in.Book != nil {
		queue = in.Book.CountAttributesBetween(v.PatientAsk, v.PatientAsk*1.05, in.GiftID, in.OwnerID, in.Backdrop, in.Symbol)
	}
	v.PatientExpectedDays = float64(1+queue) / (in.Liq.Velocity * share)

	// The two estimates come from independent formulas, which used to let the
	// ladder contradict itself in both directions. Neither is allowed:
	//
	// If the patient ask collapsed onto the fast exit, there is only one listing
	// at one price, so there is only one wait. Printing 3.465 twice with 4 days
	// against 16 is not two options, it is one option described wrongly.
	if SamePrice(v.PatientAsk, v.FastExit) {
		v.PatientExpectedDays = v.FastExpectedDays
		return
	}
	// And asking for more money can never fill sooner than taking what is
	// already on the table.
	if v.PatientExpectedDays < v.FastExpectedDays {
		v.PatientExpectedDays = v.FastExpectedDays
	}
}

// measureDivergence asks whether the trade history and the price we intend to
// sell at tell the same story.
//
// The anchor has to be the price we actually settled on, not the local order
// book. Those are different numbers whenever another venue caps the exit, and
// using the book produced exactly the wrong verdict: a Tonnel ask of 5.00 above
// a Portals queue at 4.00 made a 3.22 history look 36% adrift, when the history
// and the venue we were pricing against were 19% apart and the 5.00 was the
// outlier both of them disagreed with. The bot then treated its own discredited
// anchor as evidence against the trade.
func measureDivergence(v *Valuation, in Input) {
	anchor := v.FastExit
	if anchor <= 0 {
		anchor = v.LiveDepth
	}
	if in.Liq.Median <= 0 || anchor <= 0 {
		return
	}
	v.MarketDivergence = math.Abs(in.Liq.Median/anchor - 1)
	v.MarketDisagreement = v.MarketDivergence > marketDisagreementLimit
}

// hasTraitEvidence reports whether the attribute premium rests on enough prints
// to be allowed into a price at all.
func hasTraitEvidence(a AttributeValue) bool {
	return a.Valid && a.ExactSamples >= MinAttributeSamples && a.Fair > 0
}

// SamePrice reports whether two rungs of the ladder have collapsed onto each
// other, so the card can print one line instead of two contradictory ones.
func SamePrice(a, b float64) bool { return math.Abs(a-b) < 0.005 }

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
