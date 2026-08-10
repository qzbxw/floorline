package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"floorline/internal/bot"
	"floorline/internal/pricing"
	"floorline/internal/tonnel"
)

// App implements bot.Backend. Handlers get finished, HTML-safe text back.
var _ bot.Backend = (*App)(nil)

// priceGift runs the full valuation for one listing, fetching whatever market
// data it needs. Shared by /val and the Buy button so both see the same numbers
// the detector saw.
func (a *App) priceGift(ctx context.Context, g tonnel.Gift, now time.Time) (pricing.Valuation, error) {
	key := g.Key()
	if key.Name == "" || key.Model == "" {
		return pricing.Valuation{Reason: "listing has no collection or model"}, nil
	}

	sales, err := a.st.SalesSince(ctx, key, now.Add(-a.window()))
	if err != nil {
		return pricing.Valuation{}, fmt.Errorf("load trade history: %w", err)
	}
	liq := pricing.ComputeLiquidity(sales, now, a.window(), a.Coverage())

	var floor, rarity float64
	var supply int
	if stat, err := a.st.ModelStat(ctx, key); err == nil && stat != nil {
		floor, supply, rarity = stat.Floor, stat.Supply, stat.Rarity
	}

	book, err := a.books.Get(ctx, key)
	if err != nil {
		return pricing.Valuation{}, fmt.Errorf("load order book: %w", err)
	}

	return pricing.Evaluate(pricing.Input{
		GiftID: g.GiftID.Int(),
		Key:    key,
		Price:  g.Price.Float(),
		Book:   book,
		Liq:    liq,
		Floor:  floor,
		Supply: supply,
		Rarity: rarity,
		Params: pricing.Params{Fee: a.cfg.TonnelFee, Undercut: a.cfg.Undercut, Window: a.window()},
	}), nil
}

// Status reports the health of every moving part.
func (a *App) Status(ctx context.Context) string {
	now := time.Now()
	var b strings.Builder

	b.WriteString("<b>Floorline</b>\n")
	fmt.Fprintf(&b, "Uptime %s\n", dur(now.Sub(a.startedAt)))

	armed := "🔴 disarmed"
	if a.rm.Armed() {
		armed = "🟢 ARMED"
	}
	fmt.Fprintf(&b, "Auto-buy %s", armed)
	if reason := a.rm.LastReason(); reason != "" {
		fmt.Fprintf(&b, " — %s", bot.Esc(reason))
	}
	b.WriteString("\n")
	if until := a.rm.DisabledUntil(); until.After(now) {
		fmt.Fprintf(&b, "Paused for another %s\n", dur(until.Sub(now)))
	}

	warm := "warming up"
	if a.Warm() {
		warm = "ready"
	}
	count, _ := a.st.CountSales(ctx)
	oldest, _ := a.st.OldestSaleTime(ctx)
	newest, _ := a.st.NewestSaleTime(ctx)
	fmt.Fprintf(&b, "\n<b>Data</b>\nHistory %s (%s) · %d trades stored\n", dur(a.Coverage()), warm, count)
	if !oldest.IsZero() {
		fmt.Fprintf(&b, "Oldest %s · newest %s\n", ago(oldest), ago(newest))
	}
	if cols, err := a.st.CollectionNames(ctx); err == nil {
		fmt.Fprintf(&b, "Collections tracked: %d\n", len(cols))
	}
	if last := a.api.LastSuccess(); !last.IsZero() {
		fmt.Fprintf(&b, "Last successful API call %s\n", ago(last))
	}
	if n := a.api.BlockedStreak(); n > 0 {
		fmt.Fprintf(&b, "⚠️ %d consecutive anti-bot rejections\n", n)
	}

	b.WriteString("\n<b>Pollers</b>\n")
	a.mu.RLock()
	names := make([]string, 0, len(a.pollers))
	for n := range a.pollers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ps := a.pollers[n]
		line := fmt.Sprintf("%-11s runs %d · last %s", n, ps.Runs, ago(ps.LastRun))
		if ps.LastErr != "" {
			line += " · ❌ " + truncate(ps.LastErr, 90)
		}
		b.WriteString(bot.Esc(line) + "\n")
	}
	a.mu.RUnlock()

	stats, _ := a.st.SignalStatsSince(ctx, now.Add(-24*time.Hour))
	fmt.Fprintf(&b, "\n<b>Last 24h</b>\n%d signals · %d sent · %d bought\n", stats.Total, stats.Sent, stats.Bought)

	if bal, ok := a.rm.Balance(); ok {
		fmt.Fprintf(&b, "\nBalance %s TON\n", num(bal))
	}
	return b.String()
}

// Floor shows a collection's models, or one model in detail.
func (a *App) Floor(ctx context.Context, collection, model string) string {
	name := a.resolveCollection(ctx, collection)
	if name == "" {
		return a.unknownCollection(ctx, collection)
	}

	if model == "" {
		rows, err := a.st.ModelsForCollection(ctx, name)
		if err != nil {
			return "Could not read the market snapshot: " + bot.Esc(err.Error())
		}
		if len(rows) == 0 {
			return "No models recorded for " + bot.Esc(name) + " yet."
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<b>%s</b> — %d models, cheapest first\n\n", bot.Esc(name), len(rows))
		for i, r := range rows {
			if i >= 25 {
				fmt.Fprintf(&b, "…and %d more\n", len(rows)-i)
				break
			}
			fmt.Fprintf(&b, "%-28s %8s  ×%d\n",
				bot.Esc(truncate(attrWithRarity(r.Key.Model, r.Rarity), 28)), num(r.Floor), r.Supply)
		}
		return "<pre>" + b.String() + "</pre>"
	}

	key := tonnel.ModelKey{Name: name, Model: tonnel.TitleCase(model)}
	stat, err := a.st.ModelStat(ctx, key)
	if err != nil {
		return "Could not read the market snapshot: " + bot.Esc(err.Error())
	}
	if stat == nil {
		if resolved, ok := a.resolveModel(ctx, name, model); ok {
			key = resolved
			stat, _ = a.st.ModelStat(ctx, key)
		}
	}
	if stat == nil {
		return fmt.Sprintf("No snapshot for %s. Try <code>/floor %s</code> to list its models.",
			bot.Esc(key.String()), bot.Esc(name))
	}

	book, err := a.books.Get(ctx, key)
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\nFloor %s · supply %d · rarity %.1f%%\n",
		bot.Esc(key.String()), num(stat.Floor), stat.Supply, stat.Rarity)

	if err == nil && book.Len() > 0 {
		b.WriteString("\nCheapest asks:\n")
		for i, ask := range book.Asks {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "  %s — <code>/val %d</code>\n", num(ask.Price), ask.GiftID)
		}
	}
	return b.String()
}

// BookText prints the ask ladder with cumulative depth.
func (a *App) BookText(ctx context.Context, collection, model string) string {
	key, msg := a.mustResolve(ctx, collection, model)
	if msg != "" {
		return msg
	}
	book, err := a.books.Get(ctx, key)
	if err != nil {
		return "Could not read the order book: " + bot.Esc(err.Error())
	}
	if book.Len() == 0 {
		return "Nothing listed for " + bot.Esc(key.String()) + "."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b> — ask ladder\n\n", bot.Esc(key.String()))
	var cum float64
	var rows strings.Builder
	best := book.Asks[0].Price
	for i, ask := range book.Asks {
		cum += ask.Price
		fmt.Fprintf(&rows, "%2d %8s  %+5.0f%%  cum %8s  #%d\n",
			i+1, num(ask.Price), (ask.Price/best-1)*100, num(cum), ask.GiftNum)
	}
	b.WriteString("<pre>" + rows.String() + "</pre>")

	within10 := book.CountBetween(best, best*1.1, 0)
	fmt.Fprintf(&b, "\n%d asks within 10%% of the floor.", within10)
	return b.String()
}

// Hist prints the real trade history for a model.
func (a *App) Hist(ctx context.Context, collection, model string) string {
	key, msg := a.mustResolve(ctx, collection, model)
	if msg != "" {
		return msg
	}
	now := time.Now()
	sales, err := a.st.SalesSince(ctx, key, now.Add(-a.window()))
	if err != nil {
		return "Could not read the trade history: " + bot.Esc(err.Error())
	}
	liq := pricing.ComputeLiquidity(sales, now, a.window(), a.Coverage())

	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b> — last %d days\n", bot.Esc(key.String()), a.cfg.LookbackDays)
	if liq.Sales == 0 {
		b.WriteString("\nNo recorded trades. This model is not liquid enough to flip.")
		return b.String()
	}
	fmt.Fprintf(&b, "%d trades · %.2f/day · %d sellers / %d buyers\n",
		liq.Sales, liq.Velocity, liq.Sellers, liq.Buyers)
	fmt.Fprintf(&b, "Median %s · 7d median %s · trend %+.0f%%\n",
		num(liq.Median), num(liq.Median7), (liq.Trend-1)*100)
	fmt.Fprintf(&b, "Dispersion %.0f%% · last trade %s\n", liq.MADRatio*100, ago(liq.LastSale))

	if stat, err := a.st.ModelStat(ctx, key); err == nil && stat != nil && stat.Floor > 0 {
		fmt.Fprintf(&b, "\nFloor %s is %+.0f%% versus the median trade.\n",
			num(stat.Floor), (stat.Floor/liq.Median-1)*100)
	}

	b.WriteString("\nRecent trades:\n<pre>")
	shown := 0
	for i := len(sales) - 1; i >= 0 && shown < 12; i-- {
		fmt.Fprintf(&b, "%8s   %s\n", num(sales[i].Price), sales[i].TS.Format("02 Jan 15:04"))
		shown++
	}
	b.WriteString("</pre>")
	return b.String()
}

// Val prices one listing in full, including the gates it fails.
func (a *App) Val(ctx context.Context, giftID int64) string {
	g, err := a.api.GiftData(ctx, giftID)
	if err != nil {
		return "Could not fetch that listing: " + bot.Esc(err.Error())
	}
	if g.GiftID.Int() == 0 {
		g.GiftID = tonnel.FlexInt(giftID)
	}

	now := time.Now()
	v, err := a.priceGift(ctx, *g, now)
	if err != nil {
		return "Could not value that listing: " + bot.Esc(err.Error())
	}

	dec, err := a.det.Evaluate(ctx, *g, a.rm.Limits(), now)
	var fails, autoFails []string
	if err == nil && dec != nil {
		fails, autoFails = dec.SignalFails, dec.AutoFails
	} else {
		// The detector skips listings it has already signalled, so fall back to
		// evaluating the gates directly rather than showing nothing.
		fails, autoFails = a.det.GatesFor(v, a.rm.Limits())
	}
	return a.renderValuation(ctx, *g, v, fails, autoFails)
}

// Positions lists open inventory with live marks.
func (a *App) Positions(ctx context.Context) string {
	positions, err := a.st.OpenPositions(ctx)
	if err != nil {
		return "Could not read positions: " + bot.Esc(err.Error())
	}
	if len(positions) == 0 {
		return "No open positions."
	}

	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%d open positions</b>\n", len(positions))
	for _, p := range positions {
		mark := 0.0
		if stat, err := a.st.ModelStat(ctx, p.Key); err == nil && stat != nil {
			mark = stat.Floor
		}
		fmt.Fprintf(&b, "\n<b>%s</b>\n", bot.Esc(p.Key.String()))
		fmt.Fprintf(&b, "entry %s · held %s · %s\n", num(p.BuyPrice), dur(now.Sub(p.BoughtAt)), p.Status)
		if p.ListPrice > 0 {
			fmt.Fprintf(&b, "asking %s\n", num(p.ListPrice))
		}
		if mark > 0 {
			line := fmt.Sprintf("floor %s", num(mark))
			if p.BuyPrice > 0 {
				net := mark*(1-a.cfg.TonnelFee) - p.BuyPrice
				line += fmt.Sprintf(" → unrealised %s (%+.1f%%)", num(net), net/p.BuyPrice*100)
			}
			b.WriteString(line + "\n")
		}
		if p.Note != "" {
			fmt.Fprintf(&b, "<i>%s</i>\n", bot.Esc(p.Note))
		}
		fmt.Fprintf(&b, "<code>/relist %d</code>\n", p.GiftID)
	}
	return b.String()
}

// PnL reports realised and unrealised profit, net of fees.
func (a *App) PnL(ctx context.Context) string {
	closed, err := a.st.ClosedPositions(ctx, 500)
	if err != nil {
		return "Could not read closed positions: " + bot.Esc(err.Error())
	}
	open, err := a.st.OpenPositions(ctx)
	if err != nil {
		return "Could not read open positions: " + bot.Esc(err.Error())
	}

	var realised, invested float64
	var wins, losses, unknown int
	for _, p := range closed {
		if p.BuyPrice <= 0 || p.SellPrice <= 0 {
			unknown++
			continue
		}
		net := p.SellPrice*(1-a.cfg.TonnelFee) - p.BuyPrice
		realised += net
		invested += p.BuyPrice
		if net >= 0 {
			wins++
		} else {
			losses++
		}
	}

	var unrealised, atRisk float64
	var unmarked int
	for _, p := range open {
		if p.BuyPrice <= 0 {
			unmarked++
			continue
		}
		atRisk += p.BuyPrice
		stat, err := a.st.ModelStat(ctx, p.Key)
		if err != nil || stat == nil || stat.Floor <= 0 {
			unmarked++
			continue
		}
		unrealised += stat.Floor*(1-a.cfg.TonnelFee) - p.BuyPrice
	}

	var b strings.Builder
	b.WriteString("<b>PnL</b> <i>(net of fees)</i>\n\n")
	fmt.Fprintf(&b, "Realised   <b>%s</b> TON over %d trades\n", num(realised), wins+losses)
	if invested > 0 {
		fmt.Fprintf(&b, "           %+.1f%% on %s deployed · %d win / %d loss\n",
			realised/invested*100, num(invested), wins, losses)
	}
	fmt.Fprintf(&b, "Unrealised <b>%s</b> TON across %d open\n", num(unrealised), len(open))
	fmt.Fprintf(&b, "At risk    %s TON\n", num(atRisk))
	fmt.Fprintf(&b, "\nTotal      <b>%s</b> TON\n", num(realised+unrealised))

	if unknown > 0 || unmarked > 0 {
		b.WriteString("\n<i>")
		if unknown > 0 {
			fmt.Fprintf(&b, "%d closed positions have no recorded prices. ", unknown)
		}
		if unmarked > 0 {
			fmt.Fprintf(&b, "%d open positions could not be marked (imported or no floor). ", unmarked)
		}
		b.WriteString("Those are excluded above.</i>\n")
	}
	fmt.Fprintf(&b, "\n<i>Marks use the model floor and a %.1f%% fee.</i>", a.cfg.TonnelFee*100)
	return b.String()
}

// BalanceText reports the account balance.
func (a *App) BalanceText(ctx context.Context) string {
	bal, err := a.api.Balance(ctx)
	if err != nil {
		return "Could not read the balance: " + bot.Esc(err.Error())
	}
	a.rm.SetBalance(bal.TON)

	var b strings.Builder
	fmt.Fprintf(&b, "<b>Balance</b>\nTON %s\n", num(bal.TON))
	if bal.USDT > 0 {
		fmt.Fprintf(&b, "USDT %s\n", num(bal.USDT))
	}
	if bal.Tonnel > 0 {
		fmt.Fprintf(&b, "TONNEL %s\n", num(bal.Tonnel))
	}
	return b.String()
}

// Relist reprices an owned gift against the current book.
func (a *App) Relist(ctx context.Context, giftID int64) string {
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil {
		return "Could not read that position: " + bot.Esc(err.Error())
	}
	if p == nil {
		return fmt.Sprintf("No position for gift %d. Check <code>/pos</code>.", giftID)
	}
	a.books.Invalidate(p.Key)

	price, note, err := a.ex.Relist(ctx, giftID, p.Key, p.BuyPrice, time.Now())
	if err != nil {
		return "Relisting failed: " + bot.Esc(err.Error())
	}
	if price <= 0 {
		return "Not relisted.\n" + bot.Esc(note)
	}
	net := price*(1-a.cfg.TonnelFee) - p.BuyPrice
	return fmt.Sprintf("✅ Listed <b>%s</b> at <b>%s</b>\nEntry %s → net %s if it fills (%+.1f%%)",
		bot.Esc(p.Key.String()), num(price), num(p.BuyPrice), num(net), net/p.BuyPrice*100)
}

// Arm enables unattended buying.
func (a *App) Arm(ctx context.Context) string {
	if err := a.rm.Arm(ctx); err != nil {
		return "❌ " + bot.Esc(err.Error())
	}
	if !a.Warm() {
		return "🟢 Auto-buy armed — but the trade history is still warming up, so nothing will be bought until it is ready.\n\n<pre>" +
			bot.Esc(a.rm.Describe(ctx, time.Now())) + "</pre>"
	}
	return "🟢 <b>Auto-buy armed.</b>\n\n<pre>" + bot.Esc(a.rm.Describe(ctx, time.Now())) + "</pre>"
}

// Disarm stops unattended buying.
func (a *App) Disarm(ctx context.Context) string {
	if err := a.rm.Disarm(ctx, "disarmed manually"); err != nil {
		return "❌ " + bot.Esc(err.Error())
	}
	return "🔴 Auto-buy disarmed."
}

// LimitsText shows the limits and today's usage.
func (a *App) LimitsText(ctx context.Context) string {
	return "<b>Limits</b>\n<pre>" + bot.Esc(a.rm.Describe(ctx, time.Now())) + "</pre>" +
		"\nChange one with <code>/limits set max_ticket 50</code>"
}

// SetLimit updates one limit.
func (a *App) SetLimit(ctx context.Context, key, value string) string {
	if err := a.rm.SetLimit(ctx, key, value); err != nil {
		return "❌ " + bot.Esc(err.Error())
	}
	return fmt.Sprintf("✅ %s = %s\n\n<pre>%s</pre>",
		bot.Esc(key), bot.Esc(value), bot.Esc(a.rm.Describe(ctx, time.Now())))
}

// Watch subscribes to a model.
func (a *App) Watch(ctx context.Context, collection, model string, maxPrice float64) string {
	key, msg := a.mustResolve(ctx, collection, model)
	if msg != "" {
		return msg
	}
	if err := a.st.AddWatch(ctx, key, maxPrice, time.Now()); err != nil {
		return "Could not save the watch: " + bot.Esc(err.Error())
	}
	if maxPrice > 0 {
		return fmt.Sprintf("👁 Watching <b>%s</b> under %s", bot.Esc(key.String()), num(maxPrice))
	}
	return fmt.Sprintf("👁 Watching <b>%s</b> at any price", bot.Esc(key.String()))
}

// Unwatch removes a subscription.
func (a *App) Unwatch(ctx context.Context, collection, model string) string {
	key := tonnel.ModelKey{Name: tonnel.TitleCase(collection), Model: tonnel.TitleCase(model)}
	if resolved, ok := a.resolveModel(ctx, key.Name, model); ok {
		key = resolved
	}
	if err := a.st.RemoveWatch(ctx, key); err != nil {
		return "Could not remove the watch: " + bot.Esc(err.Error())
	}
	return "Removed " + bot.Esc(key.String()) + " from the watchlist."
}

// Watchlist lists subscriptions with their current floors.
func (a *App) Watchlist(ctx context.Context) string {
	watches, err := a.st.Watches(ctx)
	if err != nil {
		return "Could not read the watchlist: " + bot.Esc(err.Error())
	}
	if len(watches) == 0 {
		return "Watchlist is empty. Add one with <code>/watch Plush Pepe / Pink Diamond 1200</code>"
	}
	var b strings.Builder
	b.WriteString("<b>Watchlist</b>\n")
	for _, w := range watches {
		line := "• " + bot.Esc(w.Key.String())
		if w.MaxPrice > 0 {
			line += fmt.Sprintf(" under %s", num(w.MaxPrice))
		}
		if stat, err := a.st.ModelStat(ctx, w.Key); err == nil && stat != nil && stat.Floor > 0 {
			line += fmt.Sprintf(" · floor %s", num(stat.Floor))
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// Mute silences alerts for a collection or a single model.
func (a *App) Mute(ctx context.Context, collection, model string, d time.Duration) string {
	scope, label := a.muteScope(ctx, collection, model)
	until := time.Now().Add(d)
	if err := a.st.SetMute(ctx, scope, until); err != nil {
		return "Could not set the mute: " + bot.Esc(err.Error())
	}
	return fmt.Sprintf("🔕 Muted <b>%s</b> for %s", bot.Esc(label), dur(d))
}

// Unmute clears a mute.
func (a *App) Unmute(ctx context.Context, collection, model string) string {
	scope, label := a.muteScope(ctx, collection, model)
	if err := a.st.ClearMute(ctx, scope); err != nil {
		return "Could not clear the mute: " + bot.Esc(err.Error())
	}
	return "🔔 Unmuted " + bot.Esc(label)
}

func (a *App) muteScope(ctx context.Context, collection, model string) (scope, label string) {
	name := a.resolveCollection(ctx, collection)
	if name == "" {
		name = tonnel.TitleCase(collection)
	}
	if model == "" {
		return name, name
	}
	key := tonnel.ModelKey{Name: name, Model: tonnel.TitleCase(model)}
	if resolved, ok := a.resolveModel(ctx, name, model); ok {
		key = resolved
	}
	return key.ID(), key.String()
}

// SetAuth replaces the Tonnel session and verifies it immediately, so a bad
// paste is reported now rather than as silent poller failures later.
func (a *App) SetAuth(ctx context.Context, authData string) string {
	prev := a.api.Auth()
	a.api.SetAuth(authData)

	if _, err := a.api.Balance(ctx); err != nil {
		a.api.SetAuth(prev)
		return "❌ That authData was rejected, keeping the old one:\n<code>" + bot.Esc(err.Error()) + "</code>"
	}
	if err := a.st.SetKV(ctx, "tonnel.auth", authData); err != nil {
		return "Session works, but saving it failed: " + bot.Esc(err.Error())
	}
	return fmt.Sprintf("✅ Tonnel session updated (user %d).", a.api.UserID())
}

// BuySignal executes the Buy button.
//
// A manual tap is an explicit override, so the auto-buy gates and the armed
// switch do not apply — but the price is re-read first, because the card may
// have been sitting in the chat for a while.
func (a *App) BuySignal(ctx context.Context, signalID int64) string {
	sig, err := a.st.GetSignal(ctx, signalID)
	if err != nil {
		return "Could not read that signal: " + bot.Esc(err.Error())
	}
	if sig == nil {
		return "That signal is gone."
	}

	g, err := a.api.GiftData(ctx, sig.GiftID)
	if err != nil {
		return "Could not re-read the listing: " + bot.Esc(err.Error())
	}
	if g.GiftID.Int() == 0 {
		g.GiftID = tonnel.FlexInt(sig.GiftID)
	}
	if g.Buyer != nil {
		return "Too late — that listing has already been bought."
	}

	price := g.Price.Float()
	if price <= 0 {
		return "That listing is no longer for sale."
	}
	if price > sig.Price {
		return fmt.Sprintf("Price moved up from %s to %s since the alert. Not buying — run <code>/val %d</code> if you still want it.",
			num(sig.Price), num(price), sig.GiftID)
	}

	now := time.Now()
	v, err := a.priceGift(ctx, *g, now)
	if err != nil {
		return "Could not value the listing before buying: " + bot.Esc(err.Error())
	}

	out, buyErr := a.ex.Buy(ctx, v, *g, "manual", now)
	a.reportPurchase(ctx, signalID, out, buyErr, false)
	return ""
}

// BookForSignal shows the ladder behind a card.
func (a *App) BookForSignal(ctx context.Context, signalID int64) string {
	sig, err := a.st.GetSignal(ctx, signalID)
	if err != nil || sig == nil {
		return "That signal is gone."
	}
	a.books.Invalidate(sig.Key)
	return a.BookText(ctx, sig.Key.Name, sig.Key.Model)
}

// MuteSignal silences the model behind a card.
func (a *App) MuteSignal(ctx context.Context, signalID int64, d time.Duration) string {
	sig, err := a.st.GetSignal(ctx, signalID)
	if err != nil || sig == nil {
		return "That signal is gone."
	}
	if err := a.st.SetMute(ctx, sig.Key.ID(), time.Now().Add(d)); err != nil {
		return "Could not set the mute: " + bot.Esc(err.Error())
	}
	return fmt.Sprintf("🔕 Muted <b>%s</b> for %s", bot.Esc(sig.Key.String()), dur(d))
}

// ---- name resolution ----------------------------------------------------

// resolveCollection matches user input against known collection names,
// tolerating case and partial names.
func (a *App) resolveCollection(ctx context.Context, input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	titled := tonnel.TitleCase(input)
	names, err := a.st.CollectionNames(ctx)
	if err != nil {
		return titled
	}
	for _, n := range names {
		if n == titled || strings.EqualFold(n, input) {
			return n
		}
	}
	var partial []string
	lower := strings.ToLower(input)
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), lower) {
			partial = append(partial, n)
		}
	}
	if len(partial) == 1 {
		return partial[0]
	}
	if len(names) == 0 {
		return titled // snapshot not loaded yet; let the API decide
	}
	return ""
}

// resolveModel matches user input against the models of a collection.
func (a *App) resolveModel(ctx context.Context, collection, input string) (tonnel.ModelKey, bool) {
	rows, err := a.st.ModelsForCollection(ctx, collection)
	if err != nil || len(rows) == 0 {
		return tonnel.ModelKey{}, false
	}
	lower := strings.ToLower(strings.TrimSpace(input))
	for _, r := range rows {
		if strings.EqualFold(r.Key.Model, input) {
			return r.Key, true
		}
	}
	var partial []tonnel.ModelKey
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Key.Model), lower) {
			partial = append(partial, r.Key)
		}
	}
	if len(partial) == 1 {
		return partial[0], true
	}
	return tonnel.ModelKey{}, false
}

// mustResolve resolves a collection/model pair or returns a ready error message.
func (a *App) mustResolve(ctx context.Context, collection, model string) (tonnel.ModelKey, string) {
	name := a.resolveCollection(ctx, collection)
	if name == "" {
		return tonnel.ModelKey{}, a.unknownCollection(ctx, collection)
	}
	key, ok := a.resolveModel(ctx, name, model)
	if ok {
		return key, ""
	}
	return tonnel.ModelKey{Name: name, Model: tonnel.TitleCase(model)},
		fmt.Sprintf("No model matching %q in %s. Run <code>/floor %s</code> to see them.",
			bot.Esc(model), bot.Esc(name), bot.Esc(name))
}

func (a *App) unknownCollection(ctx context.Context, input string) string {
	matches, err := a.st.SearchCollections(ctx, input, 8)
	if err != nil || len(matches) == 0 {
		return fmt.Sprintf("Unknown collection %q. The market snapshot may not have loaded yet — check <code>/status</code>.", bot.Esc(input))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches several collections:\n", bot.Esc(input))
	for _, m := range matches {
		b.WriteString("• " + bot.Esc(m) + "\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
