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
		fmt.Fprintf(&b, "  ·  model floor %s  ·  <b>%+.0f%%</b>", num(v.Floor), -v.DiscountToFloor*100)
	}
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "Exit <b>%s</b>  <i>(%s)</i>\n", num(v.Exit), bot.Esc(exitExplain(v)))
	fmt.Fprintf(&b, "Net <b>%s</b> (%+.1f%%) after %.1f%% fee\n", num(v.Net), v.Edge*100, a.cfg.TonnelFee*100)
	if v.PatientExit > 0 {
		fmt.Fprintf(&b, "Fast %s / %s · patient %s / %s · chose <b>%s</b>\n", num(v.FastExit), days(v.FastExpectedDays), num(v.PatientExit), days(v.PatientExpectedDays), v.ChosenExit)
	}
	fmt.Fprintf(&b, "Score %.1f · risk edge %.1f%% · confidence %.0f%%\n", v.ScoreBreakdown.Total, v.ScoreBreakdown.RiskAdjustedEdge*100, v.Confidence*100)
	if v.FX.Valid {
		fmt.Fprintf(&b, "GRAM/USDT %s · 15m %+.1f%%", num(v.FX.CurrentUSD), v.FX.Move15m*100)
		if v.FX.ExpectedFloor > 0 {
			fmt.Fprintf(&b, " · floor lag %+.1f%%", v.FX.FloorLag*100)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Liquidity %.1f trades/day · %d trades in %dd · %d different gifts\n",
		v.Liq.Velocity, v.Liq.Sales, a.cfg.LookbackDays, v.Liq.DistinctGifts)
	fmt.Fprintf(&b, "Median %s · dispersion %.0f%% · trend %+.0f%%\n",
		num(v.Liq.Median), v.Liq.MADRatio*100, (v.Liq.Trend-1)*100)

	if v.CompetitorsNear == 0 {
		b.WriteString("Nobody within 5% of your exit → you become the best ask\n")
	} else {
		fmt.Fprintf(&b, "%d sellers within 5%% of your exit (undercut risk)\n", v.CompetitorsNear)
	}
	fmt.Fprintf(&b, "Time to sell ≈ %s · supply %d (%s of inventory)\n",
		days(v.ExpectedDays), v.Supply, days(v.DaysOfSupply))

	if bd := tonnel.BaseAttr(g.Backdrop); bd != "" {
		fmt.Fprintf(&b, "Backdrop %s · Symbol %s\n",
			bot.Esc(attrWithRarity(bd, g.BackdropRarity.Float())),
			bot.Esc(attrWithRarity(tonnel.BaseAttr(g.Symbol), g.SymbolRarity.Float())))
	}
	if v.Attribute.Valid {
		fmt.Fprintf(&b, "Attribute premium %+.1f%% · samples backdrop %d / symbol %d / exact %d\n", v.Attribute.Premium*100, v.Attribute.BackdropSamples, v.Attribute.SymbolSamples, v.Attribute.ExactSamples)
	}

	for _, line := range a.crossMarketLines(ctx, v) {
		b.WriteString(line + "\n")
	}

	if dec.Age > 0 {
		fmt.Fprintf(&b, "Listing seen %s ago\n", dur(dec.Age))
	}
	fmt.Fprintf(&b, "<code>gift %d</code>\n", g.GiftID.Int())

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
		fmt.Fprintf(&b, "\nCannot be valued: %s\n", bot.Esc(v.Reason))
		return b.String()
	}

	fmt.Fprintf(&b, "Price <b>%s</b>", num(v.Price))
	if v.Floor > 0 {
		fmt.Fprintf(&b, " · model floor %s (%+.0f%%)", num(v.Floor), -v.DiscountToFloor*100)
	}
	b.WriteString("\n\n<b>Exit model</b>\n")
	if v.HasCompetingAsk {
		fmt.Fprintf(&b, "Best competing ask %s → undercut %s\n", num(v.CompetingAsk), num(v.CompetingAsk*(1-a.cfg.Undercut)))
	} else {
		b.WriteString("No competing ask — you would be the only seller\n")
	}
	fmt.Fprintf(&b, "Median of %d trades: %s\n", v.Liq.Sales, num(v.Liq.Median))
	fmt.Fprintf(&b, "→ exit <b>%s</b> (%s)\n", num(v.Exit), bot.Esc(v.ExitBasis))
	fmt.Fprintf(&b, "→ proceeds %s after %.1f%% fee\n", num(v.Proceeds), a.cfg.TonnelFee*100)
	fmt.Fprintf(&b, "→ net <b>%s</b> (%+.1f%%)\n", num(v.Net), v.Edge*100)
	if v.PatientExit > 0 {
		fmt.Fprintf(&b, "Fast %s in %s · patient %s in %s · selected %s\n", num(v.FastExit), days(v.FastExpectedDays), num(v.PatientExit), days(v.PatientExpectedDays), v.ChosenExit)
	}
	fmt.Fprintf(&b, "Confidence %.0f%% · score %.1f · risk edge %.1f%%\n", v.Confidence*100, v.ScoreBreakdown.Total, v.ScoreBreakdown.RiskAdjustedEdge*100)
	if v.FX.Valid {
		fmt.Fprintf(&b, "GRAM/USD %s · 15m %+.1f%% · 1h %+.1f%%", num(v.FX.CurrentUSD), v.FX.Move15m*100, v.FX.Move1h*100)
		if v.FX.ExpectedFloor > 0 {
			fmt.Fprintf(&b, " · FX floor %s (%+.1f%% lag)", num(v.FX.ExpectedFloor), v.FX.FloorLag*100)
		}
		b.WriteString("\n")
	}
	if v.Liq.FXCoverage > 0 {
		fmt.Fprintf(&b, "Median %s GRAM normalized for GRAM/USD (%.0f%% coverage; raw %s)\n", num(v.Liq.Median), v.Liq.FXCoverage*100, num(v.Liq.RawMedian))
	}

	b.WriteString("\n<b>Liquidity</b>\n")
	fmt.Fprintf(&b, "%.2f trades/day · %d trades in %dd · %d different gifts (turnover %.2f)\n",
		v.Liq.Velocity, v.Liq.Sales, a.cfg.LookbackDays, v.Liq.DistinctGifts, v.Liq.Turnover)
	fmt.Fprintf(&b, "Dispersion %.0f%% · trend %+.0f%% · last trade %s\n",
		v.Liq.MADRatio*100, (v.Liq.Trend-1)*100, ago(v.Liq.LastSale))
	fmt.Fprintf(&b, "Supply %d (%s of inventory) · %d sellers within 5%% of exit\n",
		v.Supply, days(v.DaysOfSupply), v.CompetitorsNear)
	fmt.Fprintf(&b, "Expected time to sell ≈ %s\n", days(v.ExpectedDays))

	if lines := a.crossMarketLines(ctx, v); len(lines) > 0 {
		b.WriteString("\n<b>Other venues</b> <i>(reference only — no arbitrage)</i>\n")
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n<b>Gates</b>\n")
	if len(fails) == 0 {
		b.WriteString("✅ passes the signal gate\n")
	} else {
		for _, f := range fails {
			b.WriteString("❌ " + bot.Esc(f) + "\n")
		}
	}
	if len(fails) == 0 {
		if len(autoFails) == 0 {
			b.WriteString("✅ passes the auto-buy gate\n")
		} else {
			for _, f := range autoFails {
				b.WriteString("⛔️ auto: " + bot.Esc(f) + "\n")
			}
		}
	}
	return b.String()
}

// statusLine is the one-line health summary used at startup.
func (a *App) statusLine() string {
	cov := a.Coverage()
	warm := "warming up"
	if a.Warm() {
		warm = "ready"
	}
	armed := "disarmed"
	if a.rm.Armed() {
		armed = "ARMED"
	}
	return fmt.Sprintf("history %s (%s) · auto-buy %s", dur(cov), warm, armed)
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
		return "never"
	case d < 1:
		return fmt.Sprintf("%.0fh", math.Max(d*24, 1))
	case d < 100:
		return fmt.Sprintf("%.1fd", d)
	default:
		return "100d+"
	}
}

// dur renders an elapsed duration compactly.
func dur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%.0fm", d.Minutes())
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	default:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return dur(time.Since(t)) + " ago"
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
		line := fmt.Sprintf("%s asks %s", bot.Esc(q.Venue), askPreview(q.Asks, q.Floor))
		if q.Scope != "" {
			line += " [" + q.Scope + "]"
		}
		if ref := q.Reference(); ref > 0 && ref != q.Floor {
			line += fmt.Sprintf(" · depth ref %s", num(ref))
		}
		if q.Fee > 0 {
			line += fmt.Sprintf(" (net ref %s)", num(q.NetReference()))
		}
		if v.Price > 0 {
			line += fmt.Sprintf(" · %+.0f%% vs entry", (q.Reference()/v.Price-1)*100)
		}
		lines = append(lines, line)
	}
	return lines
}

func askPreview(asks []float64, fallback float64) string {
	if len(asks) == 0 {
		return num(fallback) + " (floor only)"
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
		return "attribute-adjusted comparable sales"
	case "undercut":
		return fmt.Sprintf("undercutting the next ask at %s", num(v.CompetingAsk))
	case "median":
		return fmt.Sprintf("median of %d trades — cheaper than undercutting %s", v.Liq.Sales, num(v.CompetingAsk))
	default:
		return fmt.Sprintf("median of %d trades, no competing ask", v.Liq.Sales)
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
