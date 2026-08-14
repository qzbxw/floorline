package app

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"floorline/internal/bot"
	"floorline/internal/market"
	"floorline/internal/pricing"
	"floorline/internal/signal"
	"floorline/internal/tonnel"
)

// renderCard builds the actionable signal message.
//
// It is deliberately about half the length of what it replaces. The old card
// printed every number the engine had — four exit rungs, two ask gaps, days of
// supply, the four blend weights, an FX line — and the complaint about it was
// not that any of it was wrong. It was that a card carrying thirty numbers
// cannot be read, so the two that decide the trade get the same glance as the
// twenty-eight that do not. Everything cut is still one tap away in /val, which
// is the view for arguing with the engine rather than acting on it.
//
// The order is: what this is, what it pays, why that number, what could go
// wrong, then the evidence. The first line is a link to the gift itself, which
// Telegram unfurls into a picture — on this market the picture is half the
// asset, and the desk was being asked to judge a Black Mamba from a price.
// It is now four lines and a title. The previous version was already a cut-down
// of something longer, and it was still a wall: history, cross-market, traits,
// FX, queue and regime all had a line, and the two facts that decide the trade
// got the same glance as the twenty that do not. Everything removed is one tap
// away in /val, which is the view for arguing with the engine rather than acting
// on it.
func (a *App) renderCard(ctx context.Context, dec *signal.Decision, note string) string {
	v := dec.Val
	g := dec.Gift
	var b strings.Builder

	if url := bot.NFTPreviewURL(v.Key.Name, g.GiftNum.Int()); url != "" {
		fmt.Fprintf(&b, "<a href=\"%s\">🖼</a> ", url)
	}
	fmt.Fprintf(&b, "<b>%s</b> · <i>%s</i>\n",
		bot.Esc(giftTitle(v.Key.Name, g.GiftNum.Int())), bot.Esc(v.Key.Model))

	b.WriteString(headlineBlock(dec))
	if risk := riskLine(v); risk != "" {
		b.WriteString(risk + "\n")
	}
	// One line, and only when something is actually holding the trade back. A
	// warning that appears on every card is not a warning.
	if note != "" {
		fmt.Fprintf(&b, "⛔️ %s\n", bot.Esc(note))
	}
	fmt.Fprintf(&b, "<code>/val %d</code>", g.GiftID.Int())
	if dec.Age > 0 {
		fmt.Fprintf(&b, " · <i>висит %s</i>", dur(dec.Age))
	}
	return b.String()
}

// renderCardFull is the long form, kept for /val and for the "подробнее" button:
// every number the engine has, in the order it was derived.
func (a *App) renderCardFull(ctx context.Context, dec *signal.Decision, note string) string {
	v := dec.Val
	g := dec.Gift
	var b strings.Builder

	if url := bot.NFTPreviewURL(v.Key.Name, g.GiftNum.Int()); url != "" {
		fmt.Fprintf(&b, "<a href=\"%s\">🖼</a> ", url)
	}
	fmt.Fprintf(&b, "⚡️ <b>%s</b>\n<i>%s</i>\n\n",
		bot.Esc(giftTitle(v.Key.Name, g.GiftNum.Int())), bot.Esc(v.Key.Model))

	b.WriteString(headlineBlock(dec))
	if line := exitLadderLine(v); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + a.reasonBlock(ctx, v))
	if risks := riskLines(v); len(risks) > 0 {
		b.WriteString("\n")
		for _, r := range risks {
			b.WriteString("⚠️ " + r + "\n")
		}
	}
	b.WriteString("\n" + a.evidenceBlock(ctx, v, g))

	if note != "" {
		fmt.Fprintf(&b, "\n⛔️ <b>Только руками:</b> %s\n", bot.Esc(note))
	}
	fmt.Fprintf(&b, "\n<code>гифт %d</code>", g.GiftID.Int())
	if dec.Age > 0 {
		fmt.Fprintf(&b, " · <i>висит %s</i>", dur(dec.Age))
	}
	return b.String()
}

// giftTitle is the collection and number as one name.
func giftTitle(collection string, giftNum int64) string {
	if giftNum <= 0 {
		return collection
	}
	return fmt.Sprintf("%s #%d", collection, giftNum)
}

// headlineBlock is the trade in two lines: the money, then how much the engine
// believes itself.
//
// The old card gave equal billing to four exit rungs — liquidate, fast, fair,
// patient. Only one of them is ever the decision, and printing the other three
// beside it invited reading the most flattering as the plan. The fast exit is
// the number this trade is judged on, so it is the only one here.
// The verdict leads, and it is the weakest of the three layers rather than the
// verdict of the price gates alone. "✅ BUY · скор 3/100 · данные 3%" was not a
// layout problem: BUY is read as "the model is confident", so a BUY resting on
// three percent confidence was the interface asserting something the engine did
// not believe. Now a trade whose evidence is thin says SPEC and says why.
func headlineBlock(dec *signal.Decision) string {
	v := dec.Val
	verdict := dec.Verdict
	if verdict == "" {
		verdict, _ = signal.Grade(v, dec.SignalFails)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b> %.0f/100 · данные %.0f%%\n",
		verdict.Mark(), verdict, v.ScoreBreakdown.Total, v.ScoreBreakdown.Quality*100)
	fmt.Fprintf(&b, "%s → %s · <b>%s</b> · чистыми %s\n",
		num(v.Cost), num(v.FastExit), pct(v.Edge), num(v.Net))
	fmt.Fprintf(&b, "%s\n", fillLine(v))
	// The queue the two lines above were computed from. Without it the card
	// asserts a price and a probability and offers nothing to check them
	// against — which is exactly the complaint: "не видно, пиздит или нет".
	if line := valuationDepthLine(v); line != "" {
		fmt.Fprintf(&b, "%s\n", line)
	}
	// Whichever layer is holding the verdict down, in its own words. This is the
	// line that used to be missing entirely: the card showed a downgraded verdict
	// and left the operator to guess which of a dozen numbers caused it.
	if blocking := signal.Blocking(dec.Layers); len(blocking) > 0 {
		fmt.Fprintf(&b, "└ %s: %s\n", blocking[0].Name, bot.Esc(blocking[0].Note))
	}
	return b.String()
}

// reasonBlock answers the one question the operator cannot check from a phone:
// where did that exit price come from. A number without its reason is a number
// nobody can disagree with, which is how a fast exit standing behind a queue
// went unchallenged for weeks.
func (a *App) reasonBlock(ctx context.Context, v pricing.Valuation) string {
	why := v.ExitCapped
	if why == "" {
		why = fmt.Sprintf("стакан %.0f%% · история %.0f%% · площадки %.0f%%",
			v.LiveWeight*100, v.HistoryWeight*100, v.CrossWeight*100)
	}
	return "📐 <b>Почему столько:</b> " + bot.Esc(why) + "\n"
}

// riskLine is the single worst thing about this trade, for the short card.
//
// One line, chosen by severity, because the compact card has room for exactly
// one and the operator should get the one that matters. The rest are in /val.
func riskLine(v pricing.Valuation) string {
	risks := riskLines(v)
	if len(risks) == 0 {
		return ""
	}
	line := "⚠️ " + risks[0]
	if extra := len(risks) - 1; extra > 0 {
		line += fmt.Sprintf(" · <i>ещё %d в /val</i>", extra)
	}
	return line
}

// riskLines are the things that would make this trade go wrong, and nothing
// else. A warning that fires on every card is not a warning.
// The order is severity, not the order the numbers were computed in, because
// the compact card shows only the first one. "The market is already cheaper than
// you paid" is the most decisive thing that can be true about a trade — it says
// the position is upside-down before any modelling — so it leads, ahead of the
// warnings about how well we can measure it.
func riskLines(v pricing.Valuation) []string {
	var out []string
	if v.AdverseSelection {
		out = append(out, v.AdverseReason)
	}
	if v.AsksBelowEntry > 0 {
		out = append(out, fmt.Sprintf("дешевле твоего входа стоит %s — рынок ниже нас",
			plural(v.AsksBelowEntry, "чужой аск", "чужих аска", "чужих асков")))
	}
	if v.CrossImplausible {
		out = append(out, "чужие площадки показывают цену, которой не бывает — в оценку они не пошли")
	}
	if v.AppearanceUnpriced {
		out = append(out, "считали по обычным экземплярам — по этим трейтам сравнить не с чем, реальная цена может быть выше")
	}
	if v.ExitInvented {
		out = append(out, fmt.Sprintf("стакан и сделки разошлись на %.0f%% — цена выхода это середина между ними, а не факт", v.MarketDivergence*100))
	} else if v.MarketDisagreement {
		out = append(out, fmt.Sprintf("история и стакан разъехались на %.0f%%", v.MarketDivergence*100))
	}
	if v.DepthCapped || (v.HasCompetingAsk && v.LiveDepthCount < 2) {
		out = append(out, fmt.Sprintf("дырявый стакан: %s → %s, живых цен подряд %d",
			num(v.CompetingAsk), num(v.DepthPrice3), v.LiveDepthCount))
	}
	if near := v.CompetitorsNear + v.CrossCompetitorsNear; v.AsksBelowEntry == 0 && near > 0 {
		// Counted across venues, because being undercut does not require the
		// other seller to be on Tonnel. The split is spelled out: sellers we can
		// see moving in our own book are a different problem from sellers on a
		// marketplace we cannot reprice against.
		line := fmt.Sprintf("%s в 5%% над выходом — подрежут",
			plural(near, "продавец", "продавца", "продавцов"))
		if v.CrossCompetitorsNear > 0 {
			line += fmt.Sprintf(" (из них %d на других площадках)", v.CrossCompetitorsNear)
		}
		out = append(out, line)
	}
	if v.Cross.Unreachable > 0 {
		out = append(out, plural(v.Cross.Unreachable, "площадка не ответила", "площадки не ответили", "площадок не ответили")+" — сравнивать не с чем")
	}
	// Getting stuck is the failure mode that costs the most and shows up least
	// in the arithmetic. Expected days counts only the sellers within a whisker
	// of our exit; this counts the whole standing queue against the flow, which
	// is what decides how long we sit there after one undercut. It is a warning
	// rather than a score term on purpose — the score already divides by
	// expected days, and charging the same fact twice is what made the old
	// ranking useless.
	if v.DaysOfSupply > crowdedDaysOfSupply && !math.IsInf(v.DaysOfSupply, 1) {
		out = append(out, fmt.Sprintf("саплай %d при %.1f продажи в день — очереди на %s, после андерката будешь сидеть",
			v.Supply, v.Liq.Velocity, days(v.DaysOfSupply)))
	}
	return out
}

// crowdedDaysOfSupply is where the standing queue stops being a market and
// starts being a warehouse: three weeks of flow already listed.
const crowdedDaysOfSupply = 21

// fillLine states the chance of being out, per horizon.
//
// It replaces a single expected-days figure printed to one decimal — "~1.6д",
// "~4.7д" — whose precision was fiction. That number was a fourteen-day average
// trade rate divided by a queue that counted the model's whole standing supply,
// so it was wrong in both halves and confident in neither. Three probabilities
// are less tidy and much harder to misread: nobody plans around "38%" the way
// they plan around "1.6 days".
func fillLine(v pricing.Valuation) string {
	f := v.Fill
	if f.BuyersPerDay <= 0 {
		return "продажа: модель не торгуется — оценить нечем"
	}
	line := fmt.Sprintf("продать: 24ч %s · 72ч %s · 7д %s",
		prob(f.In24h), prob(f.In72h), prob(f.In7d))
	switch {
	case f.Cheaper > 0:
		line += fmt.Sprintf(" · впереди %s", plural(f.Cheaper, "оффер", "оффера", "офферов"))
	case f.Undercutters > 0:
		// Worth naming separately: we are first right now, and these are the
		// people who take that place back.
		line += fmt.Sprintf(" · рядом %s", plural(f.Undercutters, "продавец", "продавца", "продавцов"))
	}
	return line
}

// exitLadderLine is the executable price per horizon: the most we can ask and
// still expect to be gone in time. It is the counterpart to fair value and the
// number a purchase is actually judged against.
func exitLadderLine(v pricing.Valuation) string {
	if v.ExitIn72h <= 0 && v.ExitIn7d <= 0 {
		return ""
	}
	part := func(label string, price float64) string {
		if price <= 0 {
			return label + " —"
		}
		return label + " " + num(price)
	}
	return fmt.Sprintf("выход по срокам: %s · %s · %s",
		part("24ч", v.ExitIn24h), part("72ч", v.ExitIn72h), part("7д", v.ExitIn7d))
}

// evidenceBlock is the market in four lines: the local queue, the other venues,
// the tape, and the look. One line each, because the operator is deciding
// whether to trust the headline, not recomputing it.
func (a *App) evidenceBlock(ctx context.Context, v pricing.Valuation, g tonnel.Gift) string {
	var b strings.Builder

	if v.HasCompetingAsk {
		fmt.Fprintf(&b, "📚 Tonnel: следующий %s · %s · саплай %d\n",
			num(v.CompetingAsk), plural(v.ExternalAsks, "аск", "аска", "асков"), v.Supply)
	} else {
		fmt.Fprintf(&b, "📚 Tonnel: чужих асков нет · саплай %d\n", v.Supply)
	}

	for _, line := range a.crossMarketLines(ctx, v) {
		b.WriteString("🌐 " + line + "\n")
	}

	// Trades and distinct gifts, both. One physical gift flipped repeatedly
	// makes a tape look busy, and the gap between these two numbers is the only
	// wash-trading signal the endpoint allows.
	fmt.Fprintf(&b, "📊 %s · %s / %dд · медиана %s · %.2f в день · последняя %s\n",
		plural(v.Liq.Prints, "сделка", "сделки", "сделок"),
		plural(v.Liq.DistinctGifts, "гифт", "гифта", "гифтов"), a.cfg.LookbackDays,
		num(v.Liq.Median), v.Liq.Velocity, ago(v.Liq.LastSale))

	if bd := tonnel.BaseAttr(g.Backdrop); bd != "" {
		fmt.Fprintf(&b, "🎨 %s · %s — %s\n",
			bot.Esc(bd), bot.Esc(tonnel.BaseAttr(g.Symbol)), bot.Esc(premiumNote(v)))
	}
	if len(v.Appearance.Reasons) > 0 {
		fmt.Fprintf(&b, "✨ %s\n", bot.Esc(strings.Join(v.Appearance.Reasons, " · ")))
	}
	return b.String()
}

// premiumNote states what the trades say about these traits, in a few words.
func premiumNote(v pricing.Valuation) string {
	switch {
	case v.Attribute.Valid && v.Attribute.ExactSamples >= pricing.MinAttributeSamples:
		return fmt.Sprintf("премия %+.1f%% по %d сделкам", v.Attribute.Premium*100, v.Attribute.ExactSamples)
	case v.Attribute.ExactSamples > 0:
		return fmt.Sprintf("премии нет, всего %d точных сделок", v.Attribute.ExactSamples)
	default:
		return "премии нет, точных сделок не было"
	}
}

// ---- shared card blocks -------------------------------------------------
//
// Both the signal card and /val are built from these, so the two views never
// drift into describing the same number two different ways.

func titleBlock(icon string, v pricing.Valuation, g tonnel.Gift) string {
	var b strings.Builder
	if url := bot.NFTPreviewURL(v.Key.Name, g.GiftNum.Int()); url != "" {
		fmt.Fprintf(&b, "<a href=\"%s\">🖼</a> ", url)
	}
	fmt.Fprintf(&b, "%s <b>%s</b>\n<i>%s</i>\n",
		icon, bot.Esc(giftTitle(v.Key.Name, g.GiftNum.Int())), bot.Esc(attrWithRarity(v.Key.Model, v.Rarity)))
	return b.String()
}

// entryBlock is the money going out. The listing price is not it — the referral
// fee is, and that is the number every later comparison uses.
func entryBlock(v pricing.Valuation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "💵 <b>Вход %s</b> · аск %s с комиссией\n", num(v.Cost), num(v.Price))
	if v.Floor > 0 {
		rel := "ниже"
		if v.DiscountToFloor < 0 {
			rel = "<b>выше</b>"
		}
		fmt.Fprintf(&b, "Флор модели %s — берём на %.0f%% %s флора\n", num(v.Floor), math.Abs(v.DiscountToFloor)*100, rel)
	}
	return b.String()
}

// exitBlock is the whole point of the bot: four prices that mean four different
// things, laid out as a ladder so they can never be read as one blurred number.
func exitBlock(v pricing.Valuation) string {
	var b strings.Builder
	b.WriteString("🎯 <b>Выход</b>\n")
	if !pricing.SamePrice(v.Liquidation, v.FastExit) {
		fmt.Fprintf(&b, "├ слить сейчас %s\n", num(v.Liquidation))
	}
	fmt.Fprintf(&b, "├ быстро <b>%s</b> за ~%s ← считаем по нему\n", num(v.FastExit), days(v.FastExpectedDays))
	fmt.Fprintf(&b, "├ фэйр %s", num(v.FairValue))
	// Discovery below execution is not a contradiction — it means the queue is
	// standing above what the tape has been paying — but printed bare, next to a
	// higher "fast", it reads as a bug. Say which is which.
	if v.FairValue < v.FastExit {
		b.WriteString(" <i>(ниже быстрого: стакан выше реальных сделок)</i>")
	}
	b.WriteString("\n")
	// A rung that collapsed onto the fast exit is not a second option, it is the
	// same listing at the same price. Printing it twice was what produced
	// "3.465 за 4д" directly above "3.465 за 16д".
	//
	// Neither is a rung worth a few hundredths at the same expected wait: that
	// is not a slower, dearer option, it is the fast exit with rounding noise on
	// it, and offering it as a choice invites taking the higher number for free.
	if worthWaiting(v) {
		fmt.Fprintf(&b, "└ терпеливо %s за ~%s\n", num(v.PatientAsk), days(v.PatientExpectedDays))
	}
	if v.BearCase > 0 {
		fmt.Fprintf(&b, "Если рынок поплывёт: %s\n", num(v.BearCase))
	}
	verdict := "➖"
	switch {
	case v.Edge > 0:
		verdict = "🟢"
	case v.Edge < 0:
		verdict = "🔴"
	}
	fmt.Fprintf(&b, "%s Итог <b>%s</b> (<b>%s</b>) · эдж с риском %.1f%%\n",
		verdict, num(v.Net), pct(v.Edge), v.ScoreBreakdown.RiskAdjustedEdge*100)
	// The score is worth nothing without the evidence behind it, so the two are
	// printed together: a 60 on solid data and a 60 on three prints and one live
	// ask are not the same signal.
	fmt.Fprintf(&b, "Скор <b>%.0f</b>/100 (%s) · доверие данным %.0f%%\n",
		v.ScoreBreakdown.Total, scoreVerdict(v.ScoreBreakdown.Total), v.ScoreBreakdown.Quality*100)
	if v.ExitCapped != "" {
		fmt.Fprintf(&b, "⚠️ %s\n", bot.Esc(v.ExitCapped))
	}
	return b.String()
}

// patientPremium is the smallest gain over the fast exit that makes waiting a
// real choice rather than a rounding difference.
const patientPremium = 0.015

// worthWaiting reports whether the patient rung is a genuinely different offer:
// meaningfully dearer, and slower for a measurable reason. Equal prices, or
// equal expected waits, mean there is one option here and printing two invites
// reading the higher number as free money.
func worthWaiting(v pricing.Valuation) bool {
	if v.FastExit <= 0 || pricing.SamePrice(v.PatientAsk, v.FastExit) {
		return false
	}
	if v.PatientAsk < v.FastExit*(1+patientPremium) {
		return false
	}
	return v.PatientExpectedDays > v.FastExpectedDays
}

// bookBlock describes the queue we would have to sell through. Depth that had
// to be cut says so out loud: a reference the engine distrusts is exactly the
// thing the operator must see.
func (a *App) bookBlock(v pricing.Valuation) string {
	var b strings.Builder
	b.WriteString("📚 <b>Стакан Tonnel</b>\n")
	if !v.HasCompetingAsk {
		b.WriteString("Чужих асков нет — сравнивать не с чем\n")
	} else {
		fmt.Fprintf(&b, "Следующий аск %s · глубина %s · всего %s\n",
			num(v.CompetingAsk), num(v.LiveDepth), plural(v.ExternalAsks, "аск", "аска", "асков"))
		if v.DepthCapped || v.LiveDepthCount < minInt(2, v.ExternalAsks) {
			fmt.Fprintf(&b, "⚠️ Дырявый стакан: %s → %s. Живых цен подряд %d, глубину подрезали\n",
				num(v.CompetingAsk), num(v.DepthPrice3), v.LiveDepthCount)
		} else {
			fmt.Fprintf(&b, "Гэп до следующего %.1f%% · до глубины-3 %.1f%%\n", v.AskGap1*100, v.AskGap3*100)
		}
		// Since the exit undercuts the cheapest competing ask, we are always
		// first in the queue at that price — so saying so is not news. What the
		// operator needs to know is whether getting there meant pricing under
		// the market itself, and how tight the sellers behind us are.
		switch {
		case v.AsksBelowEntry > 0:
			fmt.Fprintf(&b, "⚠️ Дешевле твоего входа стоит %s — рынок ниже нас\n",
				plural(v.AsksBelowEntry, "чужой аск", "чужих аска", "чужих асков"))
		case v.CompetitorsNear > 0:
			fmt.Fprintf(&b, "Выходишь первым, но %s в 5%% над выходом — риск андерката\n",
				plural(v.CompetitorsNear, "продавец", "продавца", "продавцов"))
		default:
			b.WriteString("Выходишь первым в очереди, никто не дышит в спину\n")
		}
	}
	fmt.Fprintf(&b, "Саплай %d · запаса %s\n", v.Supply, days(v.DaysOfSupply))
	return b.String()
}

func (a *App) historyBlock(v pricing.Valuation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 <b>История %dд</b>\n", a.cfg.LookbackDays)
	fmt.Fprintf(&b, "%d сделок · %d гифтов · %.2f в день · последняя %s\n",
		v.Liq.Prints, v.Liq.DistinctGifts, v.Liq.Velocity, ago(v.Liq.LastSale))
	fmt.Fprintf(&b, "Медиана %s · разброс %.0f%% · тренд %+.0f%%\n",
		num(v.Liq.Median), v.Liq.MADRatio*100, (v.Liq.Trend-1)*100)
	if v.MarketDisagreement {
		fmt.Fprintf(&b, "⚠️ История и живой стакан разъехались на %.0f%% — только руками\n", v.MarketDivergence*100)
	}
	return b.String()
}

func traitBlock(v pricing.Valuation, g tonnel.Gift) string {
	var b strings.Builder
	if bd := tonnel.BaseAttr(g.Backdrop); bd != "" {
		fmt.Fprintf(&b, "🎨 <b>Трейты</b>: %s · %s\n",
			bot.Esc(attrWithRarity(bd, g.BackdropRarity.Float())),
			bot.Esc(attrWithRarity(tonnel.BaseAttr(g.Symbol), g.SymbolRarity.Float())))
	}
	switch {
	case v.Attribute.Valid && v.Attribute.ExactSamples >= pricing.MinAttributeSamples:
		fmt.Fprintf(&b, "Премия %+.1f%% · выборка: фон %d / символ %d / точных %d\n",
			v.Attribute.Premium*100, v.Attribute.BackdropSamples, v.Attribute.SymbolSamples, v.Attribute.ExactSamples)
	case v.Attribute.ExactSamples > 0:
		fmt.Fprintf(&b, "Премии нет: всего %d точных сделок, в цену не лезут\n", v.Attribute.ExactSamples)
	default:
		b.WriteString("Премии нет: точных сделок по этим трейтам не было\n")
	}
	// The look, stated separately from the premium, because they answer
	// different questions: one is what this specimen is, the other is what the
	// tape can prove it is worth. The floor sees neither.
	if len(v.Appearance.Reasons) > 0 {
		fmt.Fprintf(&b, "✨ %s\n", bot.Esc(strings.Join(v.Appearance.Reasons, " · ")))
	}
	if v.AppearanceUnpriced {
		b.WriteString("⚠️ Выход посчитан по обычным экземплярам модели — по этим трейтам сравнить не с чем. Реальная цена может быть выше; смотри руками\n")
	}
	return b.String()
}

// mixLine explains how the exit price was made.
//
// When a clamp overwrote the blend wholesale, the weights are no longer how the
// price was made and printing them is a lie the operator has no way to catch.
// In that case the clamp is the answer to "where did this number come from".
func mixLine(v pricing.Valuation) string {
	if v.ExitCapped != "" {
		return fmt.Sprintf("⚖️ <b>Из чего цена</b>: не из блендинга — %s\n", bot.Esc(v.ExitCapped))
	}
	return fmt.Sprintf("⚖️ <b>Из чего цена</b>: стакан %.0f%% · история %.0f%% · площадки %.0f%% · трейты %.0f%%\n",
		v.LiveWeight*100, v.HistoryWeight*100, v.CrossWeight*100, v.TraitWeight*100)
}

// fxLine reports the coin the prices are denominated in.
//
// The floor lag is the part worth reading and the part nobody could read: it is
// how far the model's floor has drifted from where the GRAM move over the last
// hour says it should be. Negative means the floor has not caught up yet and
// gifts are cheap in real terms. "Лаг флора −0.2%" said none of that, so it now
// only appears when it is large enough to matter, and says which way it points.
func fxLine(v pricing.Valuation) string {
	if !v.FX.Valid {
		return ""
	}
	s := fmt.Sprintf("💱 GRAM/USDT %s · за 15м %s", num(v.FX.CurrentUSD), pct(v.FX.Move15m))
	if v.FX.ExpectedFloor > 0 && math.Abs(v.FX.FloorLag) >= .02 {
		if v.FX.FloorLag < 0 {
			s += fmt.Sprintf(" · флор отстал на %.0f%% — гифты дешевле, чем должны", -v.FX.FloorLag*100)
		} else {
			s += fmt.Sprintf(" · флор убежал на +%.0f%% вперёд курса", v.FX.FloorLag*100)
		}
	}
	return s + "\n"
}

// pct renders a signed percentage with the same typographic minus num uses, so
// a card never mixes "−1.139" with "-18.9%" two lines apart.
//
// A move too small to show at this precision prints as a flat zero. "−0.0%" is
// not a direction, and reading one costs a beat working out that nothing moved.
func pct(v float64) string {
	if p := v * 100; math.Abs(p) < 0.05 {
		return "0%"
	}
	s := fmt.Sprintf("%+.1f%%", v*100)
	return strings.Replace(s, "-", "−", 1)
}

// plural renders a Russian count with the right noun form, because "1 асков"
// in a trading alert reads like a bug in everything else too.
func plural(n int, one, few, many string) string {
	word := many
	switch mod100 := n % 100; {
	case mod100 >= 11 && mod100 <= 14:
	default:
		switch n % 10 {
		case 1:
			word = one
		case 2, 3, 4:
			word = few
		}
	}
	return fmt.Sprintf("%d %s", n, word)
}

// signed renders a GRAM amount with its sign always shown, for the places where
// the number is a change rather than a level.
func signed(v float64) string {
	if v > 0 {
		return "+" + num(v)
	}
	return num(v)
}

// prob renders a fill probability without pretending to a precision the market
// cannot support.
//
// The curve is a Poisson tail over a queue counted from a handful of standing
// offers and an arrival rate estimated from a fortnight of a thin market. It
// will happily return 0.000 and 1.000, and printing those as "0%" and "100%"
// states a certainty that nothing in the inputs justifies — a model with two
// buyers a week and nobody in front of us is not a guaranteed sale in seven
// days, it is an estimate with a wide mouth. The bands say what is actually
// known: very unlikely, very likely, and a number in between.
func prob(p float64) string {
	switch {
	case p <= 0.05:
		return "<5%"
	case p >= 0.95:
		return ">95%"
	default:
		return fmt.Sprintf("%.0f%%", p*100)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// renderValuation is the verbose form used by /val, including the gates the
// listing failed. This is the debugging view for the thresholds.
func (a *App) renderValuation(ctx context.Context, g tonnel.Gift, v pricing.Valuation, fails, autoFails []string) string {
	var b strings.Builder

	b.WriteString(titleBlock("🔍", v, g))
	if !v.Valid {
		fmt.Fprintf(&b, "\nОценить нельзя: %s\n", bot.Esc(v.Reason))
		return b.String()
	}

	// The verdict first. Everything under it is the argument for it.
	if len(fails) == 0 {
		b.WriteString("\n✅ <b>BUY</b> — проходит фильтр сигнала\n")
	} else {
		b.WriteString("\n" + passBlock(v, fails))
	}

	b.WriteString("\n" + entryBlock(v))
	b.WriteString("\n" + exitBlock(v))
	if v.HasCompetingAsk {
		fmt.Fprintf(&b, "Андеркат лучшего чужого аска %s → %s\n", num(v.CompetingAsk), num(v.CompetingAsk*(1-a.cfg.Undercut)))
	}
	fmt.Fprintf(&b, "Опоры: история после шринка %s · живая глубина %s · площадки %s\n",
		num(v.HistoryReference), num(v.LiveDepth), num(v.CrossMarketSupport))

	b.WriteString("\n" + a.bookBlock(v))
	if lines := a.crossMarketLines(ctx, v); len(lines) > 0 {
		b.WriteString("\n🌐 <b>Другие площадки · участвуют в оценке</b>\n")
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n" + a.historyBlock(v))
	fmt.Fprintf(&b, "Оборот %.2f · ожидаемая продажа ≈ %s\n", v.Liq.Turnover, days(v.ExpectedDays))
	if v.Liq.FXCoverage > 0 {
		fmt.Fprintf(&b, "Медиана пересчитана по GRAM/USD (покрытие %.0f%%, сырая %s)\n", v.Liq.FXCoverage*100, num(v.Liq.RawMedian))
	}

	b.WriteString("\n" + traitBlock(v, g))
	b.WriteString("\n" + mixLine(v))
	if v.FX.Valid {
		fmt.Fprintf(&b, "💱 GRAM/USD %s · 15м %+.1f%% · 1ч %+.1f%%", num(v.FX.CurrentUSD), v.FX.Move15m*100, v.FX.Move1h*100)
		if v.FX.ExpectedFloor > 0 {
			fmt.Fprintf(&b, " · FX-флор %s (лаг %+.1f%%)", num(v.FX.ExpectedFloor), v.FX.FloorLag*100)
		}
		b.WriteString("\n")
	}

	b.WriteString("\n<b>Фильтры</b>\n")
	if len(fails) == 0 {
		b.WriteString("✅ сигнал: проходит\n")
		if len(autoFails) == 0 {
			b.WriteString("✅ автобай: проходит\n")
		} else {
			for _, f := range autoFails {
				b.WriteString("⛔️ автобай: " + bot.Esc(f) + "\n")
			}
		}
	} else {
		for _, f := range fails {
			b.WriteString("❌ " + bot.Esc(f) + "\n")
		}
	}
	return b.String()
}

// statusLine is the one-line health summary used at startup.
func (a *App) statusLine() string {
	cov := a.Coverage()
	warm := "прогрев"
	if a.Warm() {
		warm = "готов"
	}
	armed := "выключен"
	if a.rm.Armed() {
		armed = "ВКЛЮЧЁН"
	}
	return fmt.Sprintf("история %s (%s) · автобай %s", dur(cov), warm, armed)
}

// ---- formatting helpers -------------------------------------------------

// num renders a GRAM amount: no decimals for large values, two for small ones,
// with thin thousands separators so four-digit prices stay readable.
func num(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "—"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var s string
	switch {
	case v >= 1000:
		s = group(strconv.FormatFloat(math.Round(v), 'f', 0, 64))
	case v >= 10:
		s = trimZeros(strconv.FormatFloat(v, 'f', 1, 64))
	default:
		s = trimZeros(strconv.FormatFloat(v, 'f', 3, 64))
	}
	if neg {
		return "−" + s
	}
	return s
}

func group(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	pre := n % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func trimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// days renders an expected duration in days.
func days(d float64) string {
	switch {
	case math.IsInf(d, 1) || math.IsNaN(d):
		return "никогда"
	case d < 1:
		return fmt.Sprintf("%.0fч", math.Max(d*24, 1))
	case d < 100:
		return fmt.Sprintf("%.1fд", d)
	default:
		return "100д+"
	}
}

// dur renders an elapsed duration compactly.
func dur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0с"
	case d < time.Minute:
		return fmt.Sprintf("%.0fс", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%.0fм", d.Minutes())
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fч", d.Hours())
	default:
		return fmt.Sprintf("%.1fд", d.Hours()/24)
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "никогда"
	}
	return dur(time.Since(t)) + " назад"
}

// ruMonths are the short month names used by dt, because time.Format only
// knows English ones.
var ruMonths = [...]string{"янв", "фев", "мар", "апр", "мая", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"}

// dt renders a timestamp as "02 авг 15:04".
func dt(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return fmt.Sprintf("%02d %s %02d:%02d", t.Day(), ruMonths[t.Month()-1], t.Hour(), t.Minute())
}

// crossMarketLines renders actual sell queues. The robust reference is the
// middle of the first three asks, so one abandoned bait floor cannot dominate.
//
// This is never a trade instruction. Getting a gift out of Tonnel's off-chain
// engine takes minutes to hours, so no spread shown here is executable. It is a
// sanity check: a Tonnel floor far away from every other venue means one of the
// numbers is wrong.
func (a *App) crossMarketLines(ctx context.Context, v pricing.Valuation) []string {
	if !a.cross.Enabled() {
		return nil
	}
	// Bound the wait: a slow venue must not hold up a time-sensitive card. The
	// budget has to cover a venue's own pacing, though — two rate-limited calls
	// per venue — or the comparison drops out exactly when several cards are
	// rendered together.
	qctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	quotes, unreachable := a.cross.QuotesForGift(qctx, v.Key.Name, v.Key.Model, v.Backdrop, v.Symbol)
	lines := make([]string, 0, 2*len(quotes)+1)
	if unreachable > 0 {
		lines = append(lines, fmt.Sprintf("⚠️ %s не ответил%s — сравнить не с чем",
			plural(unreachable, "площадка", "площадки", "площадок"),
			map[bool]string{true: "а", false: "и"}[unreachable == 1]))
	}
	for _, q := range quotes {
		lines = append(lines, fmt.Sprintf("<b>%s</b> %s%s", bot.Esc(q.Venue), askPreview(q.Asks, q.Floor), scopeNote(q.Scope)))

		detail := fmt.Sprintf("опора %s", num(q.Reference()))
		if q.Fee > 0 {
			detail += fmt.Sprintf(" (нет %s)", num(q.NetReference()))
		}
		if v.Cost > 0 {
			detail += fmt.Sprintf(" · %+.0f%% к входу", (q.Reference()/v.Cost-1)*100)
			// The count is the part that decides trades: one cheap ask is a
			// bait, four are a queue our buyer can walk to instead of us.
			if n := countBelow(q.Asks, v.Cost); n > 0 {
				detail += fmt.Sprintf(" · %s дешевле входа", plural(n, "аск", "аска", "асков"))
			}
		}
		lines = append(lines, detail)
	}
	return lines
}

// scopeNote translates the venue's matching scope into something readable: it
// decides whether the quote is about this exact gift or the whole model.
func scopeNote(scope string) string {
	switch scope {
	case market.ScopeExact:
		return " · точно по трейтам"
	case market.ScopeBackdrop:
		return " · по фону"
	case market.ScopeModel:
		// Worth saying out loud rather than hiding in a word: this queue is the
		// ordinary specimens of the model, and for anything whose value is its
		// backdrop that is a different asset.
		return " · <b>по модели, не по трейтам</b>"
	case market.ScopeFloor:
		return " · только флор"
	default:
		return ""
	}
}

func countBelow(prices []float64, limit float64) int {
	n := 0
	for _, p := range prices {
		if p > 0 && p < limit {
			n++
		}
	}
	return n
}

func askPreview(asks []float64, fallback float64) string {
	if len(asks) == 0 {
		return num(fallback) + " (только флор)"
	}
	n := len(asks)
	if n > 5 {
		n = 5
	}
	parts := make([]string, 0, n)
	for _, p := range asks[:n] {
		parts = append(parts, num(p))
	}
	return strings.Join(parts, " / ")
}

// passBlock is the verdict at the top of /val: the economic reasons to skip,
// one per line. Three of them read; a semicolon-joined paragraph of five does
// not, and this is the part the operator actually acts on.
func passBlock(v pricing.Valuation, fails []string) string {
	reasons := passReasons(v, fails)
	if len(reasons) > 3 {
		reasons = reasons[:3]
	}
	var b strings.Builder
	b.WriteString("🚫 <b>PASS — не берём</b>\n")
	for _, r := range reasons {
		b.WriteString("• " + bot.Esc(r) + "\n")
	}
	return b.String()
}

// passLine is the one-line form, kept for notifications that have no room for
// a block.
func passLine(v pricing.Valuation, fails []string) string {
	return "PASS: " + strings.Join(passReasons(v, fails), "; ") + "."
}

// passReasons states why a listing was skipped, most decisive first.
//
// Every line here has to correspond to something that actually rejected the
// trade. An earlier version invented two of its own — "no gap after the first
// ask" and "the other venues give no edge" — which are checked by no gate at
// all, and then only fell back to the real failure when none of the inventions
// fired. So the block could name three reasons while hiding the one that
// actually mattered.
func passReasons(v pricing.Valuation, fails []string) []string {
	parts := make([]string, 0, 5)
	// The economics first, restated plainly: the operator should not have to
	// infer "entry is above exit" from a risk-adjusted edge clamped to zero.
	// The gate says this better when it fires, so only stand in for it.
	if v.FastExit > 0 && v.Cost >= v.FastExit && !mentions(fails, "выше быстрого выхода") {
		parts = append(parts, fmt.Sprintf("реальный вход %s выше быстрого выхода %s", num(v.Cost), num(v.FastExit)))
	}
	// Then whatever actually rejected the listing, verbatim and before any
	// commentary. The block is trimmed to three lines, so a gate pushed below
	// the context lines is a gate the operator never reads.
	parts = append(parts, fails...)

	// Supporting context last: true, useful, but not why we said no.
	if v.PricedAboveMarket {
		parts = append(parts, fmt.Sprintf("дешевле входа %s стоит %s на всех площадках, премии за трейты нет",
			num(v.Cost), plural(v.AsksBelowEntry, "чужой аск", "чужих аска", "чужих асков")))
	}
	if v.DepthCapped {
		parts = append(parts, fmt.Sprintf("стакан дырявый (%s → %s), глубине верить нельзя", num(v.CompetingAsk), num(v.DepthPrice3)))
	}
	if v.MarketDisagreement {
		parts = append(parts, "история и живой стакан разъехались")
	}
	if v.CrossDivergence > pricing.CrossDivergenceLimit {
		parts = append(parts, "Tonnel и другие площадки спорят")
	}
	return parts
}

// scoreVerdict puts a word to the number, so the score is readable without
// remembering what counts as good on this scale.
//
// The bands are anchored on what this market actually produces once the exit is
// priced honestly, not on where a hundred-point scale looks tidy. Jolly Chimp —
// the one card in the 12 Aug log that was taken and made money — lands at 17,
// and everything else that night lands under 10. A band structure that called
// 17 "слабый" would be describing the desk's best trade of the day as noise.
func scoreVerdict(total float64) string {
	switch {
	case total >= 40:
		return "<b>редкая</b>"
	case total >= 25:
		return "<b>сильная</b>"
	case total >= 12:
		return "рабочая"
	case total > 0:
		return "слабая"
	default:
		return "мимо"
	}
}

// mentions reports whether any gate failure already makes a point, so the PASS
// block does not say the same thing twice in two different wordings.
func mentions(fails []string, substr string) bool {
	for _, f := range fails {
		if strings.Contains(f, substr) {
			return true
		}
	}
	return false
}

func attrWithRarity(name string, rarity float64) string {
	if name == "" {
		return "—"
	}
	if rarity <= 0 {
		return name
	}
	return fmt.Sprintf("%s (%.1f%%)", name, rarity)
}
