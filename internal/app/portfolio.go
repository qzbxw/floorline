package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
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
	CapitalPressure float64
	// CheaperElsewhere is how many offers on other venues undercut our own ask,
	// restated as the Tonnel ask that would cost a buyer the same. A position can
	// be the cheapest thing on Tonnel and still be the expensive one on screen,
	// and until this was counted the desk had no way to see that: the undercut
	// watcher, the relist target and this view all read the local book alone.
	CheaperElsewhere int
	// WalkawayVenue names where the cheapest competing offer actually is.
	WalkawayVenue string
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

// positionBudget is how long any one position may take to price.
//
// Every position costs a book fetch and a cross-market lookup, and those are
// network calls against a marketplace that is sometimes slow and sometimes
// throttled. Run one after another under a single request deadline, six
// positions could not finish inside it — so the last ones came back as "не
// прочитал историю сделок: context deadline exceeded" and the whole view took
// the better part of a minute to appear.
const positionBudget = 12 * time.Second

// advisePositions prices a whole book at once.
//
// Concurrently, because the calls are independent and the alternative is a
// latency that grows with the size of the portfolio; and with a budget each,
// because one unreachable model must cost its own line rather than every line
// below it. A position that runs out of time comes back as an honest "не
// успел", which is a different statement from "this cannot be valued".
func (a *App) advisePositions(ctx context.Context, ps []store.Position, now time.Time) []positionAdvice {
	out := make([]positionAdvice, len(ps))
	var wg sync.WaitGroup
	// Enough to make the view feel instant, few enough not to burst through the
	// client's rate limiter — which would turn parallelism into throttling.
	sem := make(chan struct{}, 4)
	for i, p := range ps {
		wg.Add(1)
		go func(i int, p store.Position) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pctx, cancel := context.WithTimeout(ctx, positionBudget)
			defer cancel()
			out[i] = a.advisePosition(pctx, p, now)
		}(i, p)
	}
	wg.Wait()
	return out
}

func (a *App) advisePosition(ctx context.Context, p store.Position, now time.Time) positionAdvice {
	ad := positionAdvice{Position: p, Action: actReview}
	if p.BuyPrice <= 0 {
		ad.Action = actSetCost
		ad.Reason = "цена входа неизвестна"
		return ad
	}
	g := tonnel.Gift{GiftID: tonnel.FlexInt(p.GiftID), Name: p.Key.Name, Model: p.Key.Model, Backdrop: p.Backdrop, Symbol: p.Symbol, Price: tonnel.Flex64(p.BuyPrice), Asset: tonnel.AssetGRAM}
	v, err := a.pricePosition(ctx, g, p.BuyPrice, now)
	ad.Val = v
	if err != nil || !v.Valid {
		switch {
		case err != nil && errors.Is(err, context.DeadlineExceeded), err != nil && errors.Is(err, context.Canceled):
			// "context deadline exceeded" tells the operator nothing about
			// their position; it tells them about our plumbing. Say which it is.
			ad.Reason = "не успел опросить рынок — Tonnel отвечает медленно, попробуй обновить"
		case err != nil:
			ad.Reason = err.Error()
		default:
			ad.Reason = v.Reason
		}
		return ad
	}
	ad.CrossReference = v.CrossMarketSupport
	ad.CrossDivergence = v.CrossDivergence
	ad.WalkawayVenue = v.WalkawayVenue

	ask := p.ListPrice
	if ask <= 0 {
		ask = v.Price
	}
	ad.CheaperElsewhere = countCheaperElsewhere(v, ask, a.cfg.TonnelFee)
	check := risk.ExitCheck{Ask: ask, Target: v.Exit, Floor: v.Floor, ExternalRef: ad.CrossReference}
	contradicted, why := check.ContradictsMarket()

	limits := a.rm.Limits()
	bal, balanceKnown := a.rm.Balance()
	ad.CapitalPressure = capitalPressure(bal, balanceKnown, limits.MinBalanceReserve)
	slow := limits.MaxExitDays > 0 && v.ExpectedDays > limits.MaxExitDays
	switch {
	case v.MarketDisagreement:
		ad.Action = actReview
		ad.Reason = fmt.Sprintf("рынок спорит сам с собой на %.0f%% — аск не трогаем, сначала проверяем руками", v.MarketDivergence*100)
	case contradicted:
		// Withhold the sell rather than downgrade it silently: every path out of
		// here is downward (the target sits under the floor, which sits at or
		// under our ask), so relisting into it is just a slower panic sell.
		ad.Action, ad.FloorGuarded = actHold, true
		ad.Reason = why + "; аск не трогаем — выходить только явным подтверждением /exit"
	case v.Net < 0:
		ad.Action = actExit
		ad.Reason = "быстрый выход ниже входа; минус фиксируем только руками"
	case slow && ad.CapitalPressure > 0:
		ad.Action = actExit
		ad.Reason = fmt.Sprintf("позиция долгая, а запас кэша просел на %.0f%%; выход только с ручным подтверждением", ad.CapitalPressure*100)
	case slow:
		ad.Action = actHold
		ad.Reason = "позиция долгая, но свободного кэша хватает — паниковать и лить во флор незачем"
	case p.ListPrice <= 0 || math.Abs(p.ListPrice/v.Exit-1) >= .02:
		ad.Action = actRelist
		ad.Reason = "текущий аск расходится с лучшим выходом с поправкой на риск"
	default:
		ad.Action = actHold
		ad.Reason = "аск в пределах 2% от рекомендованного выхода"
	}
	if ad.CrossDivergence > pricing.CrossDivergenceLimit && !ad.FloorGuarded {
		ad.Reason += fmt.Sprintf("; расхождение с внешними стаканами %.0f%% — проверь руками", ad.CrossDivergence*100)
		if ad.Action == actHold || ad.Action == actRelist {
			ad.Action = actReview
		}
	}
	return ad
}

// countCheaperElsewhere counts the offers on other venues that a buyer would
// reach for before ours.
//
// The comparison is made in the unit the buyer compares — money leaving their
// wallet — so an external sticker is restated as the Tonnel ask that costs the
// same. Matching gross prices across venues would count an offer as cheaper
// purely because of who charges the referral.
func countCheaperElsewhere(v pricing.Valuation, ask, fee float64) int {
	if ask <= 0 {
		return 0
	}
	n := 0
	for _, p := range v.Cross.Asks {
		if eq := pricing.TonnelEquivalent(p, fee); eq > 0 && eq < ask {
			n++
		}
	}
	return n
}

// marketNote is the one line about the rest of the market that a position's
// verdict rests on.
func (ad positionAdvice) marketNote() string {
	var parts []string
	if ad.CheaperElsewhere > 0 {
		parts = append(parts, fmt.Sprintf("дешевле нас на других площадках: %s",
			plural(ad.CheaperElsewhere, "оффер", "оффера", "офферов")))
	} else if ad.WalkawayVenue == "площадки" {
		parts = append(parts, "самый дешёвый оффер сейчас не на Tonnel")
	}
	if ad.CrossReference > 0 && ad.CrossDivergence > .10 {
		parts = append(parts, fmt.Sprintf("площадки на %s, расхождение %.0f%%",
			num(ad.CrossReference), ad.CrossDivergence*100))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

func capitalPressure(balance float64, known bool, desiredReserve float64) float64 {
	if !known || desiredReserve <= 0 {
		return 0
	}
	return math.Max(0, 1-balance/desiredReserve)
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
		breakEven := p.BuyPrice
		fmt.Fprintf(&b, "Куплено %s · держим %s · безубыток %s\n", dt(p.BoughtAt), dur(time.Since(p.BoughtAt)), num(breakEven))
		if p.ListPrice > 0 {
			fmt.Fprintf(&b, "Наценка %+.1f%% · нет если заберут %+.1f%%\n", (p.ListPrice/p.BuyPrice-1)*100, (p.ListPrice/p.BuyPrice-1)*100)
		}
	}
	if ad.Val.Valid {
		v := ad.Val
		fmt.Fprintf(&b, "Слить сейчас %s · быстрый выход %s за ~%s\n", num(v.Liquidation), num(v.FastExit), days(v.FastExpectedDays))
		// Only a genuinely higher ask is a second option worth naming.
		if pricing.SamePrice(v.PatientAsk, v.FastExit) {
			fmt.Fprintf(&b, "Фэйр %s · ждать смысла нет, терпеливый аск тот же\n", num(v.FairValue))
		} else {
			fmt.Fprintf(&b, "Фэйр %s · терпеливый аск %s за ~%s\n", num(v.FairValue), num(v.PatientAsk), days(v.PatientExpectedDays))
		}
		if v.BearCase > 0 {
			fmt.Fprintf(&b, "Если рынок поплывёт: %s\n", num(v.BearCase))
		}
		fmt.Fprintf(&b, "Рекомендую <b>%s</b> · нет %s (%+.1f%%) · %.2f GRAM/день\n", num(v.Exit), num(v.Net), v.Edge*100, v.ScoreBreakdown.ProfitPerDay)
		if v.SupportGuarded {
			fmt.Fprintf(&b, "Выход удержан на живой поддержке %s — история сделок (медиана %s) ниже всех живых офферов\n", num(v.Support), num(v.Liq.Median))
		}
		if v.Attribute.Valid && v.Attribute.ExactSamples >= pricing.MinAttributeSamples {
			fmt.Fprintf(&b, "Конфа %.0f%% · трейты %+.1f%% (%d точных сделок)\n", v.Confidence*100, v.Attribute.Premium*100, v.Attribute.ExactSamples)
		} else {
			fmt.Fprintf(&b, "Конфа %.0f%% · по трейтам данных мало, в цену не лезут\n", v.Confidence*100)
		}
		if ad.CrossReference > 0 {
			fmt.Fprintf(&b, "Внешняя глубина асков %s · расхождение %.0f%%\n", num(ad.CrossReference), ad.CrossDivergence*100)
		}
		// Which venue the exit is actually measured against, and who stands in
		// front of us there. A position can be the cheapest lot on Tonnel and the
		// fourth-cheapest offer a buyer sees.
		if v.Walkaway > 0 {
			fmt.Fprintf(&b, "Ближайший чужой оффер %s (%s)\n", num(v.Walkaway), bot.Esc(v.WalkawayVenue))
		}
		if ad.CheaperElsewhere > 0 {
			fmt.Fprintf(&b, "⚠️ Дешевле нашего аска на других площадках: %s\n",
				plural(ad.CheaperElsewhere, "оффер", "оффера", "офферов"))
		}
		if v.Cross.Unreachable > 0 {
			fmt.Fprintf(&b, "⚠️ %s не ответил%s — сравнивал только по Tonnel\n",
				plural(v.Cross.Unreachable, "площадка", "площадки", "площадок"),
				map[bool]string{true: "а", false: "и"}[v.Cross.Unreachable == 1])
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
		net := p.SellPrice - p.BuyPrice
		fmt.Fprintf(&b, "Продано %s GRAM · зафиксировано %s (%+.1f%%)\n", num(p.SellPrice), num(net), net/p.BuyPrice*100)
	}
	if len(cycles) > 0 {
		b.WriteString("\n<b>Закрытые циклы</b>\n")
		for _, c := range cycles {
			net := c.SellPrice - c.BuyPrice
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
	ads := a.advisePositions(ctx, ps, now)
	book := summarise(ads, now)
	book.Cash, book.CashKnown = a.rm.Balance()
	// Closed cycles count towards how hard the capital has been working, but not
	// towards what the book is worth: that money is already in the balance and
	// adding it again would double it.
	if closed, err := a.st.PositionTrades(ctx, 500); err == nil {
		for _, c := range closed {
			if c.BuyPrice > 0 && c.SellPrice > 0 {
				book.Realised += c.SellPrice - c.BuyPrice
				book.TonDays += c.BuyPrice * math.Max(c.SoldAt.Sub(c.BoughtAt).Hours(), 0) / 24
			}
		}
	}
	sortByUrgency(ads)

	var b strings.Builder
	// What the book is worth is what it would fetch, not what it is theoretically
	// valued at, and the two numbers are kept apart because mixing them is how a
	// portfolio of six positions costing 22 came to be headlined "оценка 40.5".
	// The mark is the 72-hour executable exit: the price the whole book could
	// realistically be out of inside a normal working horizon.
	fmt.Fprintf(&b, "💼 <b>%s</b>\n", plural(len(ps), "позиция", "позиции", "позиций"))
	fmt.Fprintf(&b, "Вложено %s → слить за 72ч %s (%+.1f%%)\n",
		num(book.Invested), num(book.Mark72h), book.Return()*100)
	if book.CashKnown {
		fmt.Fprintf(&b, "Кэш %s · всего %s\n", num(book.Cash), num(book.Mark72h+book.Cash))
	}
	// Capital efficiency, not ROI. +15% over twenty days is worse than +5% over
	// two, and only a number with time in its denominator can say so.
	if book.TonDays > 0 {
		fmt.Fprintf(&b, "Отдача %+.2f%%/день на вложенный GRAM · %.0f GRAM·дней в работе\n",
			book.CapitalEfficiency()*100, book.TonDays)
	}
	if book.StuckCapital > 0 {
		fmt.Fprintf(&b, "⏳ %s зависло дольше 72ч (%s)\n",
			num(book.StuckCapital), plural(book.Stuck, "позиция", "позиции", "позиций"))
	}
	nav := book.Mark72h + book.Cash
	collections, models := book.Collections, book.Models
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
	// Concentration only matters when it is concentrated. Below a third of the
	// book in one place it is a line of noise on every refresh.
	if nav > 0 && topCollectionValue/nav > .33 {
		fmt.Fprintf(&b, "⚠️ %.0f%% книги в одной коллекции — %s\n", topCollectionValue/nav*100, bot.Esc(topCollection))
	} else if nav > 0 && topModelValue/nav > .25 {
		fmt.Fprintf(&b, "⚠️ %.0f%% книги в одной модели — %s\n", topModelValue/nav*100,
			bot.Esc(strings.ReplaceAll(topModel, "|", " · ")))
	}

	for _, ad := range ads {
		p := ad.Position
		fmt.Fprintf(&b, "\n%s <b>%s</b>\n", actionMark(ad.Action), bot.Esc(p.Key.String()))
		if !ad.Val.Valid {
			fmt.Fprintf(&b, "   <i>%s</i>\n", bot.Esc(ad.Reason))
			continue
		}
		// Entry → target, and the percentage between exactly those two.
		//
		// This line used to read "4.43 → 4.95 · +17.3%", where 4.43 was the ask,
		// 4.95 the target and the percentage was measured from the entry — three
		// numbers from two different comparisons, so the arithmetic on screen
		// did not work and the position looked mis-valued. The ask belongs here
		// too, but as its own fact.
		fmt.Fprintf(&b, "   вход %s → цель %s · <b>%+.1f%%</b>\n",
			num(p.BuyPrice), num(ad.Val.Exit), ad.Val.Edge*100)
		fmt.Fprintf(&b, "   %s\n", fillLine(ad.Val))
		if p.ListPrice > 0 {
			line := fmt.Sprintf("   стоим %s", num(p.ListPrice))
			if !pricing.SamePrice(p.ListPrice, ad.Val.Exit) {
				line += fmt.Sprintf(" · переставить на %s", num(ad.Val.Exit))
			}
			if costIsProvisional(&p) {
				line += " · <i>вход угадан, задай /cost</i>"
			}
			b.WriteString(line + "\n")
		} else if costIsProvisional(&p) {
			b.WriteString("   <i>вход угадан по ленте сделок, задай /cost</i>\n")
		}
		if note := ad.marketNote(); note != "" {
			fmt.Fprintf(&b, "   <i>%s</i>\n", bot.Esc(note))
		}
	}
	return b.String()
}

// sortByUrgency puts the positions that need a decision at the top.
//
// The book used to be sorted by score, which is a candidate-ranking number: it
// is zero for anything whose fast exit sits under its entry, and that is most of
// a portfolio most of the time. So the sort key was zero for nearly every line
// and the order was whatever the database happened to return — the position
// bleeding money could land at the bottom of eight.
//
// What the operator needs first is the position that requires an action, and
// among those the one with the most money on it: a decision about 40 GRAM is
// worth reading before the same decision about 3.
func sortByUrgency(ads []positionAdvice) {
	rank := func(ad positionAdvice) int {
		switch ad.Action {
		case actExit:
			return 0
		case actRelist:
			return 1
		case actReview, actSetCost:
			return 2
		default: // actHold — nothing to do, and that is the point
			return 3
		}
	}
	sort.SliceStable(ads, func(i, j int) bool {
		if ri, rj := rank(ads[i]), rank(ads[j]); ri != rj {
			return ri < rj
		}
		return ads[i].Position.BuyPrice > ads[j].Position.BuyPrice
	})
}

// actionMark puts a light next to a recommendation, so a book of eight
// positions can be read by colour before it is read by word.
func actionMark(action string) string {
	switch action {
	case actHold:
		return "🟢 " + action
	case actRelist:
		return "🟡 " + action
	case actExit:
		return "🔴 " + action
	default:
		// Review and set-cost are not verdicts about the position, they are the
		// desk saying it cannot form one yet.
		return "⚪️ " + action
	}
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
