package app

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"floorline/internal/bot"
	"floorline/internal/pricing"
	"floorline/internal/signal"
	"floorline/internal/tonnel"
)

// renderCard builds the actionable signal message.
//
// Every line answers a question a trader would otherwise have to open three
// tabs for: what am I paying, what will I realistically get out at, how sure
// are we, and how long will my money be stuck.
func (a *App) renderCard(ctx context.Context, dec *signal.Decision, note string) string {
	v := dec.Val
	g := dec.Gift
	var b strings.Builder

	title := bot.Esc(v.Key.Name)
	if n := g.GiftNum.Int(); n > 0 {
		title += fmt.Sprintf(" #%d", n)
	}
	fmt.Fprintf(&b, "⚡️ <b>%s</b> · %s\n", title, bot.Esc(attrWithRarity(v.Key.Model, v.Rarity)))

	fmt.Fprintf(&b, "<b>%s GRAM</b>", num(v.Price))
	if v.Floor > 0 {
		fmt.Fprintf(&b, "  ·  флор модели %s  ·  <b>%+.0f%%</b>", num(v.Floor), -v.DiscountToFloor*100)
	}
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "Выход <b>%s</b>  <i>(%s)</i>\n", num(v.Exit), bot.Esc(exitExplain(v)))
	fmt.Fprintf(&b, "Нет <b>%s</b> (%+.1f%%) после %.1f%% комиссии\n", num(v.Net), v.Edge*100, a.cfg.TonnelFee*100)
	if v.PatientExit > 0 {
		fmt.Fprintf(&b, "Быстро %s / %s · терпеливо %s / %s · выбрано <b>%s</b>\n", num(v.FastExit), days(v.FastExpectedDays), num(v.PatientExit), days(v.PatientExpectedDays), v.ChosenExit)
	}
	fmt.Fprintf(&b, "Скор %.1f · эдж с риском %.1f%% · конфа %.0f%%\n", v.ScoreBreakdown.Total, v.ScoreBreakdown.RiskAdjustedEdge*100, v.Confidence*100)
	if v.FX.Valid {
		fmt.Fprintf(&b, "GRAM/USDT %s · 15м %+.1f%%", num(v.FX.CurrentUSD), v.FX.Move15m*100)
		if v.FX.ExpectedFloor > 0 {
			fmt.Fprintf(&b, " · лаг флора %+.1f%%", v.FX.FloorLag*100)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Ликва %.1f сделок/день · %d сделок за %dд · %d разных гифтов\n",
		v.Liq.Velocity, v.Liq.Sales, a.cfg.LookbackDays, v.Liq.DistinctGifts)
	fmt.Fprintf(&b, "Медиана %s · разброс %.0f%% · тренд %+.0f%%\n",
		num(v.Liq.Median), v.Liq.MADRatio*100, (v.Liq.Trend-1)*100)

	if v.CompetitorsNear == 0 {
		b.WriteString("В 5% от твоего выхода никого → ты лучший аск\n")
	} else {
		fmt.Fprintf(&b, "%d продавцов в 5%% от твоего выхода (риск андерката)\n", v.CompetitorsNear)
	}
	fmt.Fprintf(&b, "Продажа ≈ %s · саплай %d (%s запаса)\n",
		days(v.ExpectedDays), v.Supply, days(v.DaysOfSupply))

	if bd := tonnel.BaseAttr(g.Backdrop); bd != "" {
		fmt.Fprintf(&b, "Фон %s · Символ %s\n",
			bot.Esc(attrWithRarity(bd, g.BackdropRarity.Float())),
			bot.Esc(attrWithRarity(tonnel.BaseAttr(g.Symbol), g.SymbolRarity.Float())))
	}
	if v.Attribute.Valid {
		fmt.Fprintf(&b, "Премия за атрибуты %+.1f%% · выборка фон %d / символ %d / точных %d\n", v.Attribute.Premium*100, v.Attribute.BackdropSamples, v.Attribute.SymbolSamples, v.Attribute.ExactSamples)
	}

	for _, line := range a.crossMarketLines(ctx, v) {
		b.WriteString(line + "\n")
	}

	if dec.Age > 0 {
		fmt.Fprintf(&b, "Лот висит %s\n", dur(dec.Age))
	}
	fmt.Fprintf(&b, "<code>гифт %d</code>\n", g.GiftID.Int())

	if note != "" {
		fmt.Fprintf(&b, "\n<i>%s</i>", bot.Esc(note))
	}
	return b.String()
}

// renderValuation is the verbose form used by /val, including the gates the
// listing failed. This is the debugging view for the thresholds.
func (a *App) renderValuation(ctx context.Context, g tonnel.Gift, v pricing.Valuation, fails, autoFails []string) string {
	var b strings.Builder

	title := bot.Esc(v.Key.Name)
	if n := g.GiftNum.Int(); n > 0 {
		title += fmt.Sprintf(" #%d", n)
	}
	fmt.Fprintf(&b, "🔍 <b>%s</b> · %s\n", title, bot.Esc(attrWithRarity(v.Key.Model, v.Rarity)))

	if !v.Valid {
		fmt.Fprintf(&b, "\nОценить нельзя: %s\n", bot.Esc(v.Reason))
		return b.String()
	}

	fmt.Fprintf(&b, "Цена <b>%s</b>", num(v.Price))
	if v.Floor > 0 {
		fmt.Fprintf(&b, " · флор модели %s (%+.0f%%)", num(v.Floor), -v.DiscountToFloor*100)
	}
	b.WriteString("\n\n<b>Модель выхода</b>\n")
	if v.HasCompetingAsk {
		fmt.Fprintf(&b, "Лучший чужой аск %s → андеркат %s\n", num(v.CompetingAsk), num(v.CompetingAsk*(1-a.cfg.Undercut)))
	} else {
		b.WriteString("Конкурентов нет — ты был бы единственным продавцом\n")
	}
	fmt.Fprintf(&b, "Медиана %d сделок: %s\n", v.Liq.Sales, num(v.Liq.Median))
	fmt.Fprintf(&b, "→ выход <b>%s</b> (%s)\n", num(v.Exit), bot.Esc(v.ExitBasis))
	fmt.Fprintf(&b, "→ на руки %s после %.1f%% комиссии\n", num(v.Proceeds), a.cfg.TonnelFee*100)
	fmt.Fprintf(&b, "→ нет <b>%s</b> (%+.1f%%)\n", num(v.Net), v.Edge*100)
	if v.PatientExit > 0 {
		fmt.Fprintf(&b, "Быстро %s за %s · терпеливо %s за %s · выбрано %s\n", num(v.FastExit), days(v.FastExpectedDays), num(v.PatientExit), days(v.PatientExpectedDays), v.ChosenExit)
	}
	fmt.Fprintf(&b, "Конфа %.0f%% · скор %.1f · эдж с риском %.1f%%\n", v.Confidence*100, v.ScoreBreakdown.Total, v.ScoreBreakdown.RiskAdjustedEdge*100)
	if v.FX.Valid {
		fmt.Fprintf(&b, "GRAM/USD %s · 15м %+.1f%% · 1ч %+.1f%%", num(v.FX.CurrentUSD), v.FX.Move15m*100, v.FX.Move1h*100)
		if v.FX.ExpectedFloor > 0 {
			fmt.Fprintf(&b, " · FX-флор %s (лаг %+.1f%%)", num(v.FX.ExpectedFloor), v.FX.FloorLag*100)
		}
		b.WriteString("\n")
	}
	if v.Liq.FXCoverage > 0 {
		fmt.Fprintf(&b, "Медиана %s GRAM с поправкой на GRAM/USD (покрытие %.0f%%; сырая %s)\n", num(v.Liq.Median), v.Liq.FXCoverage*100, num(v.Liq.RawMedian))
	}

	b.WriteString("\n<b>Ликва</b>\n")
	fmt.Fprintf(&b, "%.2f сделок/день · %d сделок за %dд · %d разных гифтов (оборот %.2f)\n",
		v.Liq.Velocity, v.Liq.Sales, a.cfg.LookbackDays, v.Liq.DistinctGifts, v.Liq.Turnover)
	fmt.Fprintf(&b, "Разброс %.0f%% · тренд %+.0f%% · последняя сделка %s\n",
		v.Liq.MADRatio*100, (v.Liq.Trend-1)*100, ago(v.Liq.LastSale))
	fmt.Fprintf(&b, "Саплай %d (%s запаса) · %d продавцов в 5%% от выхода\n",
		v.Supply, days(v.DaysOfSupply), v.CompetitorsNear)
	fmt.Fprintf(&b, "Ожидаемая продажа ≈ %s\n", days(v.ExpectedDays))

	if lines := a.crossMarketLines(ctx, v); len(lines) > 0 {
		b.WriteString("\n<b>Другие площадки</b> <i>(только справка — арбитража нет)</i>\n")
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n<b>Фильтры</b>\n")
	if len(fails) == 0 {
		b.WriteString("✅ проходит фильтр сигнала\n")
	} else {
		for _, f := range fails {
			b.WriteString("❌ " + bot.Esc(f) + "\n")
		}
	}
	if len(fails) == 0 {
		if len(autoFails) == 0 {
			b.WriteString("✅ проходит фильтр автобая\n")
		} else {
			for _, f := range autoFails {
				b.WriteString("⛔️ автобай: " + bot.Esc(f) + "\n")
			}
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
		s = trimZeros(strconv.FormatFloat(v, 'f', 2, 64))
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
	// Bound the wait: a slow venue must not hold up a time-sensitive card.
	qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	quotes := a.cross.QuotesForGift(qctx, v.Key.Name, v.Key.Model, v.Backdrop, v.Symbol)
	lines := make([]string, 0, len(quotes))
	for _, q := range quotes {
		line := fmt.Sprintf("%s аски %s", bot.Esc(q.Venue), askPreview(q.Asks, q.Floor))
		if q.Scope != "" {
			line += " [" + q.Scope + "]"
		}
		if ref := q.Reference(); ref > 0 && ref != q.Floor {
			line += fmt.Sprintf(" · глубина %s", num(ref))
		}
		if q.Fee > 0 {
			line += fmt.Sprintf(" (нет %s)", num(q.NetReference()))
		}
		if v.Price > 0 {
			line += fmt.Sprintf(" · %+.0f%% к входу", (q.Reference()/v.Price-1)*100)
		}
		lines = append(lines, line)
	}
	return lines
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

// exitExplain says in one clause where the exit price came from, so the number
// is never an opaque model output.
func exitExplain(v pricing.Valuation) string {
	switch v.ExitBasis {
	case "attribute fair value":
		return "сопоставимые сделки с поправкой на атрибуты"
	case "undercut":
		return fmt.Sprintf("андеркат следующего аска на %s", num(v.CompetingAsk))
	case "median":
		return fmt.Sprintf("медиана %d сделок — дешевле, чем андеркатить %s", v.Liq.Sales, num(v.CompetingAsk))
	default:
		return fmt.Sprintf("медиана %d сделок, конкурентов нет", v.Liq.Sales)
	}
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
