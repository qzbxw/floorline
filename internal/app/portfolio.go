package app

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"floorline/internal/bot"
	"floorline/internal/pricing"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

type positionAdvice struct {
	Position        store.Position
	Val             pricing.Valuation
	Action, Reason  string
	CrossReference  float64
	CrossDivergence float64
}

func (a *App) advisePosition(ctx context.Context, p store.Position, now time.Time) positionAdvice {
	ad := positionAdvice{Position: p, Action: "REVIEW"}
	if p.BuyPrice <= 0 {
		ad.Action = "SET COST"
		ad.Reason = "entry price is unknown"
		return ad
	}
	g := tonnel.Gift{GiftID: tonnel.FlexInt(p.GiftID), Name: p.Key.Name, Model: p.Key.Model, Backdrop: p.Backdrop, Symbol: p.Symbol, Price: tonnel.Flex64(p.BuyPrice), Asset: tonnel.AssetGRAM}
	v, err := a.priceGift(ctx, g, now)
	ad.Val = v
	if err != nil || !v.Valid {
		if err != nil {
			ad.Reason = err.Error()
		} else {
			ad.Reason = v.Reason
		}
		return ad
	}
	limits := a.rm.Limits()
	switch {
	case v.Net < 0 || (limits.MaxExitDays > 0 && v.ExpectedDays > limits.MaxExitDays):
		ad.Action = "REDUCE/EXIT"
		ad.Reason = "capital is tied up or the conservative exit is below cost; manual confirmation required"
	case p.ListPrice <= 0 || math.Abs(p.ListPrice/v.Exit-1) >= .02:
		ad.Action = "RELIST"
		ad.Reason = "current ask differs from the best risk-adjusted exit"
	default:
		ad.Action = "HOLD"
		ad.Reason = "ask is within 2% of the recommended exit"
	}
	if a.cross.Enabled() {
		qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		quotes := a.cross.QuotesForGift(qctx, v.Key.Name, v.Key.Model, v.Backdrop, v.Symbol)
		cancel()
		var refs []float64
		for _, q := range quotes {
			if q.NetReference() > 0 {
				refs = append(refs, q.NetReference())
			}
		}
		if len(refs) > 0 {
			sort.Float64s(refs)
			ad.CrossReference = refs[len(refs)/2]
			ad.CrossDivergence = math.Abs(ad.CrossReference/v.Exit - 1)
			if ad.CrossDivergence > .30 {
				ad.Reason += fmt.Sprintf("; external ask-book divergence %.0f%% — verify manually", ad.CrossDivergence*100)
				if ad.Action == "HOLD" || ad.Action == "RELIST" {
					ad.Action = "REVIEW"
				}
			}
		}
	}
	return ad
}

func (a *App) adviceText(ctx context.Context, giftID int64) string {
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil {
		return "Could not read position: " + bot.Esc(err.Error())
	}
	if p == nil {
		return "Position not found. Inventory is imported by the inventory poller."
	}
	ad := a.advisePosition(ctx, *p, time.Now())
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s — %s</b>\n", ad.Action, bot.Esc(p.Key.String()))
	if p.Backdrop != "" || p.Symbol != "" {
		fmt.Fprintf(&b, "%s · %s\n", bot.Esc(p.Backdrop), bot.Esc(p.Symbol))
	}
	fmt.Fprintf(&b, "Entry %s (%s, confidence %.0f%%) · ask %s\n", num(p.BuyPrice), bot.Esc(p.CostSource), p.CostConfidence*100, num(p.ListPrice))
	if p.BuyPrice > 0 {
		breakEven := p.BuyPrice / (1 - a.cfg.TonnelFee)
		fmt.Fprintf(&b, "Bought %s · held %s · break-even ask %s\n", p.BoughtAt.Format("02 Jan 2006 15:04"), dur(time.Since(p.BoughtAt)), num(breakEven))
		if p.ListPrice > 0 {
			fmt.Fprintf(&b, "Current markup %+.1f%% · net if ask fills %+.1f%%\n", (p.ListPrice/p.BuyPrice-1)*100, (p.ListPrice*(1-a.cfg.TonnelFee)/p.BuyPrice-1)*100)
		}
	}
	if ad.Val.Valid {
		v := ad.Val
		fmt.Fprintf(&b, "Fast exit %s in ~%s", num(v.FastExit), days(v.FastExpectedDays))
		if v.PatientExit > 0 {
			fmt.Fprintf(&b, " · patient %s in ~%s", num(v.PatientExit), days(v.PatientExpectedDays))
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "Recommended <b>%s</b> · net %s (%+.1f%%) · %.2f GRAM/day\n", num(v.Exit), num(v.Net), v.Edge*100, v.ScoreBreakdown.ProfitPerDay)
		fmt.Fprintf(&b, "Confidence %.0f%% · attribute premium %+.1f%% (%d exact sales)\n", v.Confidence*100, v.Attribute.Premium*100, v.Attribute.ExactSamples)
		if ad.CrossReference > 0 {
			fmt.Fprintf(&b, "External ask-depth net reference %s · divergence %.0f%%\n", num(ad.CrossReference), ad.CrossDivergence*100)
		}
	}
	if ad.Reason != "" {
		fmt.Fprintf(&b, "<i>%s</i>\n", bot.Esc(ad.Reason))
	}
	fmt.Fprintf(&b, "<code>/history %d</code>\n", giftID)
	return b.String()
}

func (a *App) positionHistoryText(ctx context.Context, giftID int64) string {
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil {
		return "Could not read position: " + bot.Esc(err.Error())
	}
	if p == nil {
		return "Position not found."
	}
	events, err := a.st.PositionEvents(ctx, giftID, 50)
	if err != nil {
		return "Could not read lifecycle: " + bot.Esc(err.Error())
	}
	marks, _ := a.st.PositionMarks(ctx, giftID, 12)
	cycles, _ := a.st.PositionTradesForGift(ctx, giftID)
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Position history · %s #%d</b>\nStatus <b>%s</b> · source %s\n", bot.Esc(p.Key.String()), p.GiftNum, bot.Esc(p.Status), bot.Esc(p.Source))
	fmt.Fprintf(&b, "Entry %s GRAM · %s · confidence %.0f%%\n", num(p.BuyPrice), bot.Esc(p.CostSource), p.CostConfidence*100)
	if p.ListPrice > 0 {
		fmt.Fprintf(&b, "Current ask %s GRAM · listed %s\n", num(p.ListPrice), ago(p.ListedAt))
	}
	if !p.MissingSince.IsZero() {
		fmt.Fprintf(&b, "⚠️ Missing from inventory since %s; sale not assumed.\n", p.MissingSince.Format("02 Jan 15:04"))
	}
	if p.SellPrice > 0 && p.BuyPrice > 0 {
		net := p.SellPrice*(1-a.cfg.TonnelFee) - p.BuyPrice
		fmt.Fprintf(&b, "Sold %s GRAM · realised %s (%+.1f%%)\n", num(p.SellPrice), num(net), net/p.BuyPrice*100)
	}
	if len(cycles) > 0 {
		b.WriteString("\n<b>Completed cycles</b>\n")
		for _, c := range cycles {
			net := c.SellPrice*(1-a.cfg.TonnelFee) - c.BuyPrice
			roi := 0.0
			if c.BuyPrice > 0 {
				roi = net / c.BuyPrice * 100
			}
			fmt.Fprintf(&b, "%s → %s · %s → %s GRAM · net %s (%+.1f%%)\n", c.BoughtAt.Format("02 Jan 06"), c.SoldAt.Format("02 Jan 06"), num(c.BuyPrice), num(c.SellPrice), num(net), roi)
		}
	}
	b.WriteString("\n<b>Lifecycle</b>\n")
	if len(events) == 0 {
		b.WriteString("No journal events yet (position predates lifecycle tracking).\n")
	}
	for _, e := range events {
		fmt.Fprintf(&b, "%s · <b>%s</b>", e.TS.Format("02 Jan 15:04"), bot.Esc(e.Kind))
		if e.OldPrice > 0 || e.NewPrice > 0 {
			fmt.Fprintf(&b, " · %s → %s", num(e.OldPrice), num(e.NewPrice))
		}
		if e.Detail != "" {
			fmt.Fprintf(&b, " · %s", bot.Esc(e.Detail))
		}
		b.WriteString("\n")
	}
	if len(marks) > 0 {
		b.WriteString("\n<b>Market/advice marks</b>\n")
		for _, m := range marks {
			fmt.Fprintf(&b, "%s · %s · ask %s · exit %s · edge %+.1f%% · floor %s", m.TS.Format("02 Jan 15:04"), bot.Esc(m.Action), num(m.AskPrice), num(m.RecommendedExit), m.Edge*100, num(m.ModelFloor))
			if m.ExternalRef > 0 {
				fmt.Fprintf(&b, " · markets %s", num(m.ExternalRef))
			}
			if m.GramUSD > 0 {
				fmt.Fprintf(&b, " · GRAM $%s", num(m.GramUSD))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (a *App) portfolioText(ctx context.Context) string {
	ps, err := a.st.OpenPositions(ctx)
	if err != nil {
		return "Could not read portfolio: " + bot.Esc(err.Error())
	}
	if len(ps) == 0 {
		return "Portfolio is empty. Inventory will be imported on the next inventory poll."
	}
	now := time.Now()
	ads := make([]positionAdvice, 0, len(ps))
	invested, nav, profitDay := 0.0, 0.0, 0.0
	collections := map[string]float64{}
	models := map[string]float64{}
	for _, p := range ps {
		ad := a.advisePosition(ctx, p, now)
		ads = append(ads, ad)
		invested += p.BuyPrice
		mark := p.ListPrice
		if ad.Val.Valid {
			mark = ad.Val.FastExit * (1 - a.cfg.TonnelFee)
			profitDay += math.Max(ad.Val.ScoreBreakdown.ProfitPerDay, 0)
		}
		nav += mark
		collections[p.Key.Name] += mark
		models[p.Key.ID()] += mark
	}
	if bal, ok := a.rm.Balance(); ok {
		nav += bal
	}
	sort.Slice(ads, func(i, j int) bool { return ads[i].Val.ScoreBreakdown.Total > ads[j].Val.ScoreBreakdown.Total })
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Portfolio · %d positions</b>\nConservative NAV %s GRAM · invested %s · expected %.2f GRAM/day\n", len(ps), num(nav), num(invested), profitDay)
	topCollection, topCollectionValue, topModel, topModelValue := "", 0.0, "", 0.0
	for k, v := range collections {
		if v > topCollectionValue {
			topCollection, topCollectionValue = k, v
		}
	}
	for k, v := range models {
		if v > topModelValue {
			topModel, topModelValue = k, v
		}
	}
	if nav > 0 {
		fmt.Fprintf(&b, "Largest collection %s %.0f%% · model %s %.0f%%\n", bot.Esc(topCollection), topCollectionValue/nav*100, bot.Esc(strings.ReplaceAll(topModel, "|", " · ")), topModelValue/nav*100)
	}
	for _, ad := range ads {
		p := ad.Position
		fmt.Fprintf(&b, "\n<b>%s</b> %s\n", ad.Action, bot.Esc(p.Key.String()))
		if ad.Val.Valid {
			fmt.Fprintf(&b, "ask %s → target %s · net %+.1f%% · ~%s\n", num(p.ListPrice), num(ad.Val.Exit), ad.Val.Edge*100, days(ad.Val.ExpectedDays))
			if ad.CrossReference > 0 {
				fmt.Fprintf(&b, "external ask-depth ref %s · divergence %.0f%%\n", num(ad.CrossReference), ad.CrossDivergence*100)
			}
		} else {
			fmt.Fprintf(&b, "%s\n", bot.Esc(ad.Reason))
		}
		fmt.Fprintf(&b, "<code>/advice %d</code>\n", p.GiftID)
	}
	return b.String()
}

func (a *App) setCostText(ctx context.Context, giftID int64, price float64) string {
	if price <= 0 {
		return "Cost must be positive."
	}
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil || p == nil {
		return "Position not found."
	}
	if err := a.st.SetCostBasis(ctx, giftID, price, "manual", 1); err != nil {
		return "Could not save cost: " + bot.Esc(err.Error())
	}
	_ = a.st.RecordPositionEvent(ctx, giftID, "cost_set", p.BuyPrice, price, "manual cost basis", time.Now())
	return fmt.Sprintf("✅ Cost for %s set to %s", bot.Esc(p.Key.String()), num(price))
}

func (a *App) exitAtText(ctx context.Context, giftID int64, price float64, confirm string) string {
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil || p == nil {
		return "Position not found."
	}
	if price <= 0 {
		return "Exit price must be positive."
	}
	if confirm != "CONFIRM" {
		return fmt.Sprintf("⚠️ This may realise a loss. Review <code>/advice %d</code>, then run exactly:\n<code>/exit %d %s CONFIRM</code>", giftID, giftID, num(price))
	}
	target := math.Floor(price*100) / 100
	res, err := a.api.ListForSale(ctx, giftID, target)
	if err != nil {
		return "Exit listing failed: " + bot.Esc(err.Error())
	}
	if res != nil && !res.OK() {
		return "Exit listing rejected: " + bot.Esc(res.Message)
	}
	now := time.Now()
	_ = a.st.SetPositionListed(ctx, giftID, target, now)
	_ = a.st.RecordReprice(ctx, giftID, p.ListPrice, target, "manual confirmed exit", now)
	_ = a.st.RecordPositionEvent(ctx, giftID, "repriced", p.ListPrice, target, "manual confirmed exit", now)
	a.books.Invalidate(p.Key)
	return fmt.Sprintf("✅ Confirmed exit listed at %s. Entry was %s.", num(target), num(p.BuyPrice))
}
