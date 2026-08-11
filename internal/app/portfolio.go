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
	"floorline/internal/risk"
	"floorline/internal/store"
	"floorline/internal/tonnel"
)

type positionAdvice struct {
	Position        store.Position
	Val             pricing.Valuation
	Action, Reason  string
	CrossReference  float64
	CrossDivergence float64
	// FloorGuarded records that a sell recommendation was withheld because the
	// live market contradicted it. It is not a normal hold and says so.
	FloorGuarded bool
}

// The verdicts shown on a position. They are compared as well as displayed, so
// they live here rather than as literals at each branch.
const (
	actHold    = "ДЕРЖИМ"
	actRelist  = "ПЕРЕСТАВИТЬ"
	actExit    = "СОКРАТИТЬ/ВЫЙТИ"
	actReview  = "ПРОВЕРИТЬ"
	actSetCost = "ЗАДАТЬ ВХОД"
)

func (a *App) advisePosition(ctx context.Context, p store.Position, now time.Time) positionAdvice {
	ad := positionAdvice{Position: p, Action: actReview}
	if p.BuyPrice <= 0 {
		ad.Action = actSetCost
		ad.Reason = "цена входа неизвестна"
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
	// The cross-market reference has to be in hand *before* the action is
	// decided. A target under the live floor means one thing when external ask
	// depth has dropped too, and the exact opposite when it has not.
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
		}
	}

	ask := p.ListPrice
	if ask <= 0 {
		ask = v.Price
	}
	check := risk.ExitCheck{Ask: ask, Target: v.Exit, Floor: v.Floor, ExternalRef: ad.CrossReference}
	contradicted, why := check.ContradictsMarket()

	limits := a.rm.Limits()
	switch {
	case contradicted:
		// Withhold the sell rather than downgrade it silently: every path out of
		// here is downward (the target sits under the floor, which sits at or
		// under our ask), so relisting into it is just a slower panic sell.
		ad.Action, ad.FloorGuarded = actHold, true
		ad.Reason = why + "; аск не трогаем — выходить только явным подтверждением /exit"
	case v.Net < 0 || (limits.MaxExitDays > 0 && v.ExpectedDays > limits.MaxExitDays):
		ad.Action = actExit
		ad.Reason = "деньги заморожены или консервативный выход ниже входа; нужно ручное подтверждение"
	case p.ListPrice <= 0 || math.Abs(p.ListPrice/v.Exit-1) >= .02:
		ad.Action = actRelist
		ad.Reason = "текущий аск расходится с лучшим выходом с поправкой на риск"
	default:
		ad.Action = actHold
		ad.Reason = "аск в пределах 2% от рекомендованного выхода"
	}
	if ad.CrossDivergence > .30 && !ad.FloorGuarded {
		ad.Reason += fmt.Sprintf("; расхождение с внешними стаканами %.0f%% — проверь руками", ad.CrossDivergence*100)
		if ad.Action == actHold || ad.Action == actRelist {
			ad.Action = actReview
		}
	}
	return ad
}

func (a *App) adviceText(ctx context.Context, giftID int64) string {
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil {
		return "Не смог прочитать позицию: " + bot.Esc(err.Error())
	}
	if p == nil {
		return "Позиция не найдена. Инвентарь подтягивает поллер инвентаря."
	}
	ad := a.advisePosition(ctx, *p, time.Now())
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s — %s</b>\n", ad.Action, bot.Esc(p.Key.String()))
	if p.Backdrop != "" || p.Symbol != "" {
		fmt.Fprintf(&b, "%s · %s\n", bot.Esc(p.Backdrop), bot.Esc(p.Symbol))
	}
	fmt.Fprintf(&b, "Вход %s (%s, конфа %.0f%%) · аск %s\n", num(p.BuyPrice), bot.Esc(p.CostSource), p.CostConfidence*100, num(p.ListPrice))
	if p.BuyPrice > 0 {
		breakEven := p.BuyPrice / (1 - a.cfg.TonnelFee)
		fmt.Fprintf(&b, "Куплено %s · держим %s · безубыток %s\n", dt(p.BoughtAt), dur(time.Since(p.BoughtAt)), num(breakEven))
		if p.ListPrice > 0 {
			fmt.Fprintf(&b, "Наценка %+.1f%% · нет если заберут %+.1f%%\n", (p.ListPrice/p.BuyPrice-1)*100, (p.ListPrice*(1-a.cfg.TonnelFee)/p.BuyPrice-1)*100)
		}
	}
	if ad.Val.Valid {
		v := ad.Val
		fmt.Fprintf(&b, "Быстрый выход %s за ~%s", num(v.FastExit), days(v.FastExpectedDays))
		if v.PatientExit > 0 {
			fmt.Fprintf(&b, " · терпеливый %s за ~%s", num(v.PatientExit), days(v.PatientExpectedDays))
		}
		b.WriteString("\n")
		if v.Liquidation > 0 {
			fmt.Fprintf(&b, "Слить прямо сейчас %s (%s)", num(v.Liquidation), bot.Esc(v.LiquidationBasis))
			if v.Floor > 0 {
				fmt.Fprintf(&b, " · живой флор %s", num(v.Floor))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Рекомендую <b>%s</b> · нет %s (%+.1f%%) · %.2f GRAM/день\n", num(v.Exit), num(v.Net), v.Edge*100, v.ScoreBreakdown.ProfitPerDay)
		if v.SupportGuarded {
			fmt.Fprintf(&b, "Выход удержан на живой поддержке %s — история сделок (медиана %s) ниже всех живых офферов\n", num(v.Support), num(v.Liq.Median))
		}
		fmt.Fprintf(&b, "Конфа %.0f%% · премия за атрибуты %+.1f%% (%d точных сделок)\n", v.Confidence*100, v.Attribute.Premium*100, v.Attribute.ExactSamples)
		if ad.CrossReference > 0 {
			fmt.Fprintf(&b, "Внешняя глубина асков %s · расхождение %.0f%%\n", num(ad.CrossReference), ad.CrossDivergence*100)
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
		return "Не смог прочитать позицию: " + bot.Esc(err.Error())
	}
	if p == nil {
		return "Позиция не найдена."
	}
	events, err := a.st.PositionEvents(ctx, giftID, 50)
	if err != nil {
		return "Не смог прочитать историю: " + bot.Esc(err.Error())
	}
	marks, _ := a.st.PositionMarks(ctx, giftID, 12)
	cycles, _ := a.st.PositionTradesForGift(ctx, giftID)
	var b strings.Builder
	fmt.Fprintf(&b, "<b>История позиции · %s #%d</b>\nСтатус <b>%s</b> · источник %s\n", bot.Esc(p.Key.String()), p.GiftNum, bot.Esc(p.Status), bot.Esc(p.Source))
	fmt.Fprintf(&b, "Вход %s GRAM · %s · конфа %.0f%%\n", num(p.BuyPrice), bot.Esc(p.CostSource), p.CostConfidence*100)
	if p.ListPrice > 0 {
		fmt.Fprintf(&b, "Текущий аск %s GRAM · выставлен %s\n", num(p.ListPrice), ago(p.ListedAt))
	}
	if !p.MissingSince.IsZero() {
		fmt.Fprintf(&b, "⚠️ Пропал из инвентаря с %s, продажу не засчитываю.\n", dt(p.MissingSince))
	}
	if p.SellPrice > 0 && p.BuyPrice > 0 {
		net := p.SellPrice*(1-a.cfg.TonnelFee) - p.BuyPrice
		fmt.Fprintf(&b, "Продано %s GRAM · зафиксировано %s (%+.1f%%)\n", num(p.SellPrice), num(net), net/p.BuyPrice*100)
	}
	if len(cycles) > 0 {
		b.WriteString("\n<b>Закрытые циклы</b>\n")
		for _, c := range cycles {
			net := c.SellPrice*(1-a.cfg.TonnelFee) - c.BuyPrice
			roi := 0.0
			if c.BuyPrice > 0 {
				roi = net / c.BuyPrice * 100
			}
			fmt.Fprintf(&b, "%s → %s · %s → %s GRAM · нет %s (%+.1f%%)\n", dt(c.BoughtAt), dt(c.SoldAt), num(c.BuyPrice), num(c.SellPrice), num(net), roi)
		}
	}
	b.WriteString("\n<b>Жизненный цикл</b>\n")
	if len(events) == 0 {
		b.WriteString("Событий пока нет (позиция старше, чем сам журнал).\n")
	}
	for _, e := range events {
		fmt.Fprintf(&b, "%s · <b>%s</b>", dt(e.TS), bot.Esc(e.Kind))
		if e.OldPrice > 0 || e.NewPrice > 0 {
			fmt.Fprintf(&b, " · %s → %s", num(e.OldPrice), num(e.NewPrice))
		}
		if e.Detail != "" {
			fmt.Fprintf(&b, " · %s", bot.Esc(e.Detail))
		}
		b.WriteString("\n")
	}
	if len(marks) > 0 {
		b.WriteString("\n<b>Отметки рынка и советов</b>\n")
		for _, m := range marks {
			fmt.Fprintf(&b, "%s · %s · аск %s · выход %s · эдж %+.1f%% · флор %s", dt(m.TS), bot.Esc(m.Action), num(m.AskPrice), num(m.RecommendedExit), m.Edge*100, num(m.ModelFloor))
			if m.ExternalRef > 0 {
				fmt.Fprintf(&b, " · площадки %s", num(m.ExternalRef))
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
		return "Не смог прочитать портфель: " + bot.Esc(err.Error())
	}
	if len(ps) == 0 {
		return "Портфель пуст. Инвентарь подтянется на следующем опросе."
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
	fmt.Fprintf(&b, "<b>Портфель · %d позиций</b>\nКонсервативная оценка %s GRAM · вложено %s · ожидаем %.2f GRAM/день\n", len(ps), num(nav), num(invested), profitDay)
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
		fmt.Fprintf(&b, "Больше всего в %s %.0f%% · модель %s %.0f%%\n", bot.Esc(topCollection), topCollectionValue/nav*100, bot.Esc(strings.ReplaceAll(topModel, "|", " · ")), topModelValue/nav*100)
	}
	for _, ad := range ads {
		p := ad.Position
		fmt.Fprintf(&b, "\n<b>%s</b> %s\n", ad.Action, bot.Esc(p.Key.String()))
		if ad.Val.Valid {
			fmt.Fprintf(&b, "аск %s → цель %s · нет %+.1f%% · ~%s\n", num(p.ListPrice), num(ad.Val.Exit), ad.Val.Edge*100, days(ad.Val.ExpectedDays))
			if ad.CrossReference > 0 {
				fmt.Fprintf(&b, "внешняя глубина асков %s · расхождение %.0f%%\n", num(ad.CrossReference), ad.CrossDivergence*100)
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
		return "Цена входа должна быть больше нуля."
	}
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil || p == nil {
		return "Позиция не найдена."
	}
	if err := a.st.SetCostBasis(ctx, giftID, price, "manual", 1); err != nil {
		return "Не смог сохранить цену: " + bot.Esc(err.Error())
	}
	_ = a.st.RecordPositionEvent(ctx, giftID, "cost_set", p.BuyPrice, price, "manual cost basis", time.Now())
	return fmt.Sprintf("✅ Вход для %s = %s", bot.Esc(p.Key.String()), num(price))
}

// exitConfirmed reports whether the operator typed the confirmation keyword.
// Both spellings are accepted so confirming a loss never requires switching the
// keyboard layout mid-command.
func exitConfirmed(confirm string) bool {
	return strings.EqualFold(confirm, "CONFIRM") || strings.EqualFold(confirm, "ПОДТВЕРЖДАЮ")
}

func (a *App) exitAtText(ctx context.Context, giftID int64, price float64, confirm string) string {
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil || p == nil {
		return "Позиция не найдена."
	}
	if price <= 0 {
		return "Цена выхода должна быть больше нуля."
	}
	if !exitConfirmed(confirm) {
		return fmt.Sprintf("⚠️ Так можно зафиксировать убыток. Глянь <code>/advice %d</code>, потом пришли ровно это:\n<code>/exit %d %s CONFIRM</code>", giftID, giftID, num(price))
	}
	target := math.Floor(price*100) / 100
	res, err := a.api.ListForSale(ctx, giftID, target)
	if err != nil {
		return "Не смог выставить на выход: " + bot.Esc(err.Error())
	}
	if res != nil && !res.OK() {
		return "Выход отклонён площадкой: " + bot.Esc(res.Message)
	}
	now := time.Now()
	_ = a.st.SetPositionListed(ctx, giftID, target, now)
	_ = a.st.RecordReprice(ctx, giftID, p.ListPrice, target, "manual confirmed exit", now)
	_ = a.st.RecordPositionEvent(ctx, giftID, "repriced", p.ListPrice, target, "manual confirmed exit", now)
	a.books.Invalidate(p.Key)
	return fmt.Sprintf("✅ Подтверждённый выход выставлен по %s. Вход был %s.", num(target), num(p.BuyPrice))
}
