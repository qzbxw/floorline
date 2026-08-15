package app

import (
	"context"
	"errors"
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

// priceGift runs the full valuation for one listing, fetching whatever market
// data it needs. Shared by /val and the Buy button so both see the same numbers
// the detector saw.
func (a *App) priceGift(ctx context.Context, g tonnel.Gift, now time.Time) (pricing.Valuation, error) {
	return a.priceGiftWithCost(ctx, g, 0, a.rm.Limits().MaxTicket, now)
}

// pricePosition values something the desk already owns.
//
// The difference from priceGift is the ticket reference, and it is deliberate.
// The size term exists to stop a 0.15-GRAM profit outranking a 5-GRAM one when
// the desk is choosing what to buy; applied to a position, it is answering a
// question nobody asked — the capital is already committed, and a small holding
// is not a worse holding for being small. Left in, it silently ranked the
// portfolio by lot size.
func (a *App) pricePosition(ctx context.Context, g tonnel.Gift, cost float64, now time.Time) (pricing.Valuation, error) {
	return a.priceGiftWithCost(ctx, g, cost, 0, now)
}

func (a *App) priceGiftWithCost(ctx context.Context, g tonnel.Gift, cost, ticketRef float64, now time.Time) (pricing.Valuation, error) {
	key := g.Key()
	if key.Name == "" || key.Model == "" {
		return pricing.Valuation{Reason: "у лота нет коллекции или модели"}, nil
	}

	attrDays := a.cfg.AttributeLookbackDays
	if attrDays < a.cfg.LookbackDays {
		attrDays = a.cfg.LookbackDays
	}
	since := now.Add(-time.Duration(attrDays) * 24 * time.Hour)
	sales, err := a.st.SalesSince(ctx, key, since)
	if err != nil {
		return pricing.Valuation{}, fmt.Errorf("не прочитал историю сделок: %w", err)
	}
	rawLiq := pricing.ComputeLiquidity(sales, now, a.window(), a.Coverage())
	fxSales, fxCoverage, _ := a.normalizeGramSales(ctx, sales, since)
	liq := pricing.ComputeLiquidity(fxSales, now, a.window(), a.Coverage())
	liq.RawMedian, liq.FXCoverage = rawLiq.Median, fxCoverage
	// The collection this model sits in, on the same terms the detector reads it,
	// so /val and the Buy button cannot disagree with the card that offered the
	// trade about how fast the thing sells.
	if tape, err := a.st.CollectionTapeSince(ctx, key.Name, now.Add(-a.window())); err == nil {
		liq = pricing.WithPeerSupport(liq, pricing.ComputePeers(tape, a.window(), a.Coverage()))
	}

	var floor, rarity float64
	var supply int
	var snapshotAt time.Time
	var fxContext pricing.FXContext
	if stat, err := a.st.ModelStat(ctx, key); err == nil && stat != nil {
		floor, supply, rarity = stat.Floor, stat.Supply, stat.Rarity
		snapshotAt = stat.TS
		fxContext = a.fxForModel(ctx, key, floor, now)
	}

	book, err := a.books.Get(ctx, key)
	if err != nil {
		return pricing.Valuation{}, fmt.Errorf("не прочитал стакан: %w", err)
	}

	v := pricing.Evaluate(pricing.Input{
		GiftID: g.GiftID.Int(), GiftNum: g.GiftNum.Int(), OwnerID: a.api.UserID(),
		Key:      key,
		Price:    g.Price.Float(),
		Cost:     cost,
		Book:     book,
		Liq:      liq,
		Floor:    floor,
		Supply:   supply,
		Rarity:   rarity,
		Backdrop: tonnel.BaseAttr(g.Backdrop), Symbol: tonnel.BaseAttr(g.Symbol),
		Attribute:  pricing.ComputeAttributeValue(fxSales, tonnel.BaseAttr(g.Backdrop), tonnel.BaseAttr(g.Symbol), liq.Median),
		SnapshotAt: snapshotAt, Now: now, FX: fxContext,
		Params:    pricing.Params{Fee: a.cfg.TonnelFee, Undercut: a.cfg.Undercut},
		TicketRef: ticketRef,
	})
	// Unconditionally, not only when a venue answered. A venue we could not
	// read is itself an input — it removes the cap that holds an optimistic
	// exit down — and guarding this on Support > 0 threw that fact away: the
	// card printed "1 площадка не ответила" while the valuation behind it had
	// no idea, so neither the score nor the auto-buy gate ever saw it.
	v = pricing.WithCrossDepth(v, a.crossMarketDepth(ctx, v))
	return v, nil
}

// Status reports the health of every moving part.
func (a *App) statusText(ctx context.Context) string {
	now := time.Now()
	var b strings.Builder

	fmt.Fprintf(&b, "🔄 <b>Статус</b> · аптайм %s\n\n", dur(now.Sub(a.startedAt)))

	// The switches live on /autobuy and are only summarised here — one line, not
	// a repeat of that whole panel. The mode is read from the risk manager
	// rather than from config: shadow moved into the database when it became a
	// button, and reading the startup value here would have reported "боевой"
	// while the desk was recording.
	switch {
	case !a.rm.Armed():
		b.WriteString("🔴 автобай выключен")
		if reason := a.rm.LastReason(); reason != "" {
			fmt.Fprintf(&b, " — %s", bot.Esc(reason))
		}
	case a.rm.ShadowMode():
		b.WriteString("🌑 shadow — считает, но не покупает")
	default:
		b.WriteString("🟢 торгует сам")
	}
	if a.rm.ResellEnabled() {
		b.WriteString(" · ♻️ ресейл")
	}
	b.WriteString("\n")
	if until := a.rm.DisabledUntil(); until.After(now) {
		fmt.Fprintf(&b, "⏸ на паузе ещё %s\n", dur(until.Sub(now)))
	}

	n, first, _ := a.st.CalibrationStats(ctx)
	fmt.Fprintf(&b, "Калибровка %d/%d", n, a.cfg.CalibrationMinSignals)
	if !first.IsZero() {
		fmt.Fprintf(&b, " · %s из %dд", dur(now.Sub(first)), a.cfg.CalibrationMinDays)
	}
	if a.rm.CalibrationWaived() {
		b.WriteString(" · снята вручную")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s\n", bot.Esc(a.sessionLine()))
	fmt.Fprintf(&b, "%s\n", bot.Esc(a.venuesLine()))
	fmt.Fprintf(&b, "%s\n", bot.Esc(a.scanLine()))

	warm := "прогрев"
	if a.Warm() {
		warm = "готов"
	}
	count, _ := a.st.CountSales(ctx)
	oldest, _ := a.st.OldestSaleTime(ctx)
	newest, _ := a.st.NewestSaleTime(ctx)
	fmt.Fprintf(&b, "\n<b>Данные</b>\nИстория %s (%s) · %d сделок в базе\n", dur(a.Coverage()), warm, count)
	if !oldest.IsZero() {
		fmt.Fprintf(&b, "Самая старая %s · свежая %s\n", ago(oldest), ago(newest))
	}
	if cols, err := a.st.CollectionNames(ctx); err == nil {
		fmt.Fprintf(&b, "Коллекций отслеживаем: %d\n", len(cols))
	}
	if q, ok, _ := a.st.LatestGramQuote(ctx); ok {
		fmt.Fprintf(&b, "GRAM/USDT %s · котировка %s\n", num(q.USD), ago(q.TS))
	} else {
		b.WriteString("⚠️ Курс GRAM/USDT недоступен\n")
	}
	b.WriteString(bot.Esc(a.streamLine()) + "\n")
	if last := a.api.LastSuccess(); !last.IsZero() {
		fmt.Fprintf(&b, "Последний удачный запрос к API %s\n", ago(last))
	}
	if n := a.api.BlockedStreak(); n > 0 {
		fmt.Fprintf(&b, "⚠️ %d отказов антибота подряд\n", n)
	}
	b.WriteString(a.routesBlock())
	a.mu.RLock()
	cool := a.coolUntil
	a.mu.RUnlock()
	if left := time.Until(cool); left > 0 {
		fmt.Fprintf(&b, "❄️ Остужаю Tonnel ещё %s — приватные эндпоинты не дёргаю, пробую раз в %s\n", dur(left), dur(coolProbeEvery))
	}
	if venues := a.cross.Venues(); len(venues) > 0 {
		fmt.Fprintf(&b, "Кроссмаркет: %s\n", strings.Join(venues, ", "))
	} else {
		b.WriteString("Кроссмаркет: не настроен\n")
	}

	b.WriteString("\n<b>Поллеры</b>\n")
	a.mu.RLock()
	names := make([]string, 0, len(a.pollers))
	for n := range a.pollers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ps := a.pollers[n]
		line := fmt.Sprintf("%-11s запусков %d · последний %s", n, ps.Runs, ago(ps.LastRun))
		if ps.LastErr != "" {
			line += " · ❌ " + truncate(ps.LastErr, 90)
		}
		b.WriteString(bot.Esc(line) + "\n")
	}
	a.mu.RUnlock()

	stats, _ := a.st.SignalStatsSince(ctx, now.Add(-24*time.Hour))
	fmt.Fprintf(&b, "\n<b>За 24 часа</b>\n%d сигналов · %d отправлено · %d куплено\n", stats.Total, stats.Sent, stats.Bought)

	if bal, ok := a.rm.Balance(); ok {
		fmt.Fprintf(&b, "\nБаланс %s GRAM\n", num(bal))
	}
	return b.String()
}

// Floor shows a collection's models, or one model in detail.
func (a *App) floorText(ctx context.Context, collection, model string) string {
	name := a.resolveCollection(ctx, collection)
	if name == "" {
		return a.unknownCollection(ctx, collection)
	}

	if model == "" {
		rows, err := a.st.ModelsForCollection(ctx, name)
		if err != nil {
			return "Не смог прочитать снапшот рынка: " + bot.Esc(err.Error())
		}
		if len(rows) == 0 {
			return "По " + bot.Esc(name) + " моделей пока нет."
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<b>%s</b> — %d моделей, сначала дешёвые\n\n", bot.Esc(name), len(rows))
		for i, r := range rows {
			if i >= 25 {
				fmt.Fprintf(&b, "…и ещё %d\n", len(rows)-i)
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
		return "Не смог прочитать снапшот рынка: " + bot.Esc(err.Error())
	}
	if stat == nil {
		if resolved, ok := a.resolveModel(ctx, name, model); ok {
			key = resolved
			stat, _ = a.st.ModelStat(ctx, key)
		}
	}
	if stat == nil {
		return fmt.Sprintf("Нет снапшота по %s. Открой <code>/floor %s</code> — там список моделей.",
			bot.Esc(key.String()), bot.Esc(name))
	}

	book, err := a.books.Get(ctx, key)
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\nФлор %s · саплай %d · рарность %.1f%%\n",
		bot.Esc(key.String()), num(stat.Floor), stat.Supply, stat.Rarity)

	if err == nil && book.Len() > 0 {
		b.WriteString("\nСамые дешёвые аски:\n")
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
func (a *App) bookText(ctx context.Context, collection, model string) string {
	key, msg := a.mustResolve(ctx, collection, model)
	if msg != "" {
		return msg
	}
	book, err := a.books.Get(ctx, key)
	if err != nil {
		return "Не смог прочитать стакан: " + bot.Esc(err.Error())
	}
	if book.Len() == 0 {
		return "По " + bot.Esc(key.String()) + " ничего не выставлено."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b> — стакан\n\n", bot.Esc(key.String()))
	var cum float64
	var rows strings.Builder
	best := book.Asks[0].Price
	for i, ask := range book.Asks {
		cum += ask.Price
		fmt.Fprintf(&rows, "%2d %8s  %+5.0f%%  накопл %8s  #%d\n",
			i+1, num(ask.Price), (ask.Price/best-1)*100, num(cum), ask.GiftNum)
	}
	b.WriteString("<pre>" + rows.String() + "</pre>")

	within10 := book.CountBetween(best, best*1.1, 0, 0)
	fmt.Fprintf(&b, "\n%d асков в пределах 10%% от флора.", within10)
	if a.cross.Enabled() {
		qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		quotes := a.cross.Quotes(qctx, key.Name, key.Model)
		cancel()
		if len(quotes) > 0 {
			b.WriteString("\n\n<b>Очереди на других площадках</b>\n")
			for _, q := range quotes {
				fmt.Fprintf(&b, "%s: %s · глубина %s", bot.Esc(q.Venue), askPreview(q.Asks, q.Floor), num(q.Reference()))
				if q.Fee > 0 {
					fmt.Fprintf(&b, " · нет %s", num(q.NetReference()))
				}
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// Hist prints the real trade history for a model.
func (a *App) histText(ctx context.Context, collection, model string) string {
	key, msg := a.mustResolve(ctx, collection, model)
	if msg != "" {
		return msg
	}
	now := time.Now()
	sales, err := a.st.SalesSince(ctx, key, now.Add(-a.window()))
	if err != nil {
		return "Не смог прочитать историю сделок: " + bot.Esc(err.Error())
	}
	liq := pricing.ComputeLiquidity(sales, now, a.window(), a.Coverage())

	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b> — за %d дней\n", bot.Esc(key.String()), a.cfg.LookbackDays)
	if liq.Sales == 0 {
		b.WriteString("\nСделок нет. Модель неликвид — флипать нечего.")
		return b.String()
	}
	fmt.Fprintf(&b, "%d сделок · %.2f/день · %d разных гифтов (оборот %.2f)\n",
		liq.Sales, liq.Velocity, liq.DistinctGifts, liq.Turnover)
	fmt.Fprintf(&b, "Медиана %s · за 7д %s · тренд %+.0f%%\n",
		num(liq.Median), num(liq.Median7), (liq.Trend-1)*100)
	fmt.Fprintf(&b, "Разброс %.0f%% · последняя сделка %s\n", liq.MADRatio*100, ago(liq.LastSale))

	if stat, err := a.st.ModelStat(ctx, key); err == nil && stat != nil && stat.Floor > 0 {
		fmt.Fprintf(&b, "\nФлор %s — это %+.0f%% к медиане сделок.\n",
			num(stat.Floor), (stat.Floor/liq.Median-1)*100)
	}

	b.WriteString("\nПоследние сделки:\n<pre>")
	shown := 0
	for i := len(sales) - 1; i >= 0 && shown < 12; i-- {
		fmt.Fprintf(&b, "%8s   %s\n", num(sales[i].Price), sales[i].TS.Format("02 Jan 15:04"))
		shown++
	}
	b.WriteString("</pre>")
	return b.String()
}

// Val prices one listing in full, including the gates it fails.
// slowMarketHint turns a plumbing failure into something the operator can act
// on.
//
// "Не смог достать этот лот: context deadline exceeded" names a Go primitive
// and nothing else — not what failed, not whether to retry, not whether the
// gift exists. Two of those three answers are usually available.
func slowMarketHint(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "Tonnel не ответил вовремя. Он сейчас режет запросы — попробуй ещё раз через минуту."
	case tonnel.RateLimited(err):
		return "Tonnel режет запросы. Подожди и повтори."
	default:
		return bot.Esc(err.Error())
	}
}

func (a *App) valText(ctx context.Context, giftID int64) string {
	g, err := a.api.GiftData(ctx, giftID)
	if err != nil {
		return "Не смог достать этот лот.\n" + slowMarketHint(err)
	}
	if g.GiftID.Int() == 0 {
		g.GiftID = tonnel.FlexInt(giftID)
	}

	now := time.Now()
	v, err := a.priceGift(ctx, *g, now)
	if err != nil {
		return "Не смог оценить лот.\n" + slowMarketHint(err)
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

// PnL reports realised and unrealised profit, net of fees.
func (a *App) pnlText(ctx context.Context) string {
	closed, err := a.st.PositionTrades(ctx, 500)
	if err != nil {
		return "Не смог прочитать закрытые позиции: " + bot.Esc(err.Error())
	}
	open, err := a.st.OpenPositions(ctx)
	if err != nil {
		return "Не смог прочитать открытые позиции: " + bot.Esc(err.Error())
	}

	var realised, invested float64
	var wins, losses, unknown int
	// The dollar leg is reconstructed from the stored GRAM quotes rather than
	// from a rate saved on each trade, so it covers every position already in
	// the book instead of only the ones bought after this was written.
	var realisedUSD, investedUSD float64
	var fxKnown int
	for _, p := range closed {
		if p.BuyPrice <= 0 || p.SellPrice <= 0 {
			unknown++
			continue
		}
		net := p.SellPrice - p.BuyPrice
		realised += net
		invested += p.BuyPrice
		if net >= 0 {
			wins++
		} else {
			losses++
		}
		buyFX, okBuy, _ := a.st.GramQuoteAt(ctx, p.BoughtAt)
		sellFX, okSell, _ := a.st.GramQuoteAt(ctx, p.SoldAt)
		if okBuy && okSell && buyFX.USD > 0 && sellFX.USD > 0 {
			realisedUSD += p.SellPrice*sellFX.USD - p.BuyPrice*buyFX.USD
			investedUSD += p.BuyPrice * buyFX.USD
			fxKnown++
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
		ad := a.advisePosition(ctx, p, time.Now())
		if !ad.Val.Valid || ad.Val.FastExit <= 0 {
			unmarked++
			continue
		}
		unrealised += ad.Val.FastExit - p.BuyPrice
	}
	tracked, _ := a.st.TrackedPositions(ctx)
	missing, missingCost := 0, 0.0
	for _, p := range tracked {
		if p.Status == store.StatusMissing {
			missing++
			missingCost += p.BuyPrice
		}
	}

	var b strings.Builder
	b.WriteString("<b>PnL</b> <i>(за вычетом комиссий)</i>\n\n")
	fmt.Fprintf(&b, "Зафиксировано  <b>%s</b> GRAM за %d сделок\n", num(realised), wins+losses)
	if invested > 0 {
		fmt.Fprintf(&b, "               %+.1f%% на %s вложенных · %d в плюс / %d в минус\n",
			realised/invested*100, num(invested), wins, losses)
	}
	fmt.Fprintf(&b, "Бумажный       <b>%s</b> GRAM по %d открытым\n", num(unrealised), len(open))
	fmt.Fprintf(&b, "В риске        %s GRAM\n", num(atRisk))
	if missing > 0 {
		fmt.Fprintf(&b, "Подвисло       %s GRAM по %d пропавшим гифтам (не считаем проданными)\n", num(missingCost), missing)
	}
	fmt.Fprintf(&b, "\nИтого          <b>%s</b> GRAM\n", num(realised+unrealised))

	b.WriteString(a.usdLeg(ctx, realised, realisedUSD, investedUSD, fxKnown, len(closed)-unknown))

	if unknown > 0 || unmarked > 0 {
		b.WriteString("\n<i>")
		if unknown > 0 {
			fmt.Fprintf(&b, "У %d закрытых позиций нет цен в базе. ", unknown)
		}
		if unmarked > 0 {
			fmt.Fprintf(&b, "%d открытых не удалось оценить (импорт или нет флора). ", unmarked)
		}
		b.WriteString("Они выше не учтены.</i>\n")
	}
	fmt.Fprintf(&b, "\n<i>Открытые считаем по быстрому выходу из живого стакана; во входе уже сидит %.1f%% комиссии покупателя.</i>", a.cfg.TonnelFee*100)
	return b.String()
}

// usdLeg reports the second profit-and-loss curve.
//
// Trading alpha and the money in your pocket are different numbers, and on this
// market they routinely disagree: a hundred GRAM turned into a hundred and
// fifteen is +15% of genuine edge, and if GRAM fell from $1.34 to $1.05 over
// the same weeks it is $134 turned into $121. One curve says the bot works, the
// other says the month lost money, and both are true. Reporting only the first
// is how a losing quarter reads as a winning one.
func (a *App) usdLeg(ctx context.Context, realised, realisedUSD, investedUSD float64, priced, total int) string {
	if priced == 0 {
		return "\n<i>Курс на моменты сделок неизвестен — долларовую кривую посчитать не из чего.</i>\n"
	}
	var b strings.Builder
	b.WriteString("\n<b>В долларах</b> <i>(по курсу на моменты сделок)</i>\n")
	fmt.Fprintf(&b, "Зафиксировано  <b>$%.2f</b>", realisedUSD)
	if investedUSD > 0 {
		fmt.Fprintf(&b, " · %+.1f%% на $%.2f вложенных", realisedUSD/investedUSD*100, investedUSD)
	}
	b.WriteString("\n")

	// The gap between the two curves is the exchange rate's contribution, and
	// it is the number worth reading: it says how much of the result was the
	// bot and how much was the coin.
	if cur, ok, _ := a.st.LatestGramQuote(ctx); ok && cur.USD > 0 {
		if fx := realisedUSD - realised*cur.USD; math.Abs(fx) >= 0.01 {
			verdict := "курс сработал в плюс"
			if fx < 0 {
				verdict = "курс съел часть прибыли"
			}
			fmt.Fprintf(&b, "Из них курс    <b>$%.2f</b> — %s\n", fx, verdict)
		}
	}
	if priced < total {
		fmt.Fprintf(&b, "<i>%d из %d сделок без котировки — в долларовую кривую не вошли.</i>\n", total-priced, total)
	}
	return b.String()
}

// BalanceText reports the account balance.
func (a *App) balanceText(ctx context.Context) string {
	bal, err := a.api.Balance(ctx)
	if err != nil {
		return "Не смог прочитать баланс: " + bot.Esc(err.Error())
	}
	a.rm.SetBalance(bal.GRAM)

	var b strings.Builder
	fmt.Fprintf(&b, "<b>Баланс</b>\nGRAM %s\n", num(bal.GRAM))
	if bal.USDT > 0 {
		fmt.Fprintf(&b, "USDT %s\n", num(bal.USDT))
	}
	if bal.Tonnel > 0 {
		fmt.Fprintf(&b, "TONNEL %s\n", num(bal.Tonnel))
	}
	return b.String()
}

// Relist reprices an owned gift against the current book.
func (a *App) relistText(ctx context.Context, giftID int64) string {
	p, err := a.st.GetPosition(ctx, giftID)
	if err != nil {
		return "Не смог прочитать позицию: " + bot.Esc(err.Error())
	}
	if p == nil {
		return fmt.Sprintf("По гифту %d позиции нет. Смотри <code>/pos</code>.", giftID)
	}
	a.books.Invalidate(p.Key)
	if p.BuyPrice <= 0 {
		return "Цена входа неизвестна. Задай её: <code>/cost " + fmt.Sprint(giftID) + " 4.25</code>."
	}
	now := time.Now()
	ad := a.advisePosition(ctx, *p, now)
	if !ad.Val.Valid {
		return "Не смог посчитать безопасную цену: " + bot.Esc(ad.Reason)
	}
	price, note, err := a.ex.ListAt(ctx, giftID, p.Key, ad.Val.Exit, p.BuyPrice, now)
	if err != nil {
		return "Релист не прошёл: " + bot.Esc(err.Error())
	}
	if price <= 0 {
		return "Не переставил.\n" + bot.Esc(note)
	}
	_ = a.st.RecordReprice(ctx, giftID, p.ListPrice, price, "manual portfolio recommendation", now)
	_ = a.st.RecordPositionEvent(ctx, giftID, "repriced", p.ListPrice, price, "manual portfolio recommendation", now)
	net := price - p.BuyPrice
	return fmt.Sprintf("✅ Выставил <b>%s</b> по <b>%s</b>\nВход %s → нет %s если заберут (%+.1f%%)",
		bot.Esc(p.Key.String()), num(price), num(p.BuyPrice), num(net), net/p.BuyPrice*100)
}

// blocker is one thing standing between the desk and an unattended purchase.
type blocker struct {
	// clear is true when this one is satisfied; warn marks a soft one that the
	// operator may knowingly overrule.
	clear, warn bool
	name, state string
}

// autoBlockers is the honest answer to "why has this not bought anything".
//
// It exists because the answer used to be unobtainable. /arm reported success,
// the bot then bought nothing, and the reason appeared as one line of small
// print at the bottom of every card — "включён shadow-режим" — with nothing
// saying what that was, why it was on, or how to change it. Two of the four
// gates below were reachable only by editing .env and restarting.
func (a *App) autoBlockers(ctx context.Context) []blocker {
	l := a.rm.Limits()
	out := make([]blocker, 0, 5)

	switch {
	case l.MaxTicket <= 0 || l.DailyBudget <= 0:
		out = append(out, blocker{name: "лимиты", state: "не заданы — нужен max_ticket и daily_budget"})
	default:
		out = append(out, blocker{clear: true, name: "лимиты",
			state: fmt.Sprintf("тикет %s · день %s", num(l.MaxTicket), num(l.DailyBudget))})
	}

	if a.rm.Armed() {
		out = append(out, blocker{clear: true, name: "покупка", state: "включена"})
	} else {
		reason := "выключена"
		if r := a.rm.LastReason(); r != "" {
			reason += " — " + r
		}
		out = append(out, blocker{name: "покупка", state: reason})
	}

	if a.Warm() {
		out = append(out, blocker{clear: true, name: "история", state: dur(a.Coverage()) + ", прогрета"})
	} else {
		out = append(out, blocker{name: "история", state: "ещё прогревается (" + dur(a.Coverage()) + ")"})
	}

	if a.rm.ShadowMode() {
		out = append(out, blocker{name: "shadow-режим", state: "включён — только записываю, что купил бы"})
	} else {
		out = append(out, blocker{clear: true, name: "shadow-режим", state: "выключен"})
	}

	n, first, _ := a.st.CalibrationStats(ctx)
	sample := fmt.Sprintf("%d/%d сигналов", n, a.cfg.CalibrationMinSignals)
	if !first.IsZero() {
		sample += fmt.Sprintf(" · %s из %dд", dur(time.Since(first)), a.cfg.CalibrationMinDays)
	}
	switch {
	case a.calibrated(ctx):
		out = append(out, blocker{clear: true, name: "калибровка", state: sample})
	case a.rm.CalibrationWaived():
		out = append(out, blocker{clear: true, warn: true, name: "калибровка",
			state: sample + " — снята вручную, риск выше"})
	default:
		out = append(out, blocker{warn: true, name: "калибровка", state: sample})
	}
	return out
}

// autoPanelText renders the switches and the checklist together, because the
// question "is it on" and the question "will it actually do anything" have had
// different answers for two days.
func (a *App) autoPanelText(ctx context.Context) string {
	blockers := a.autoBlockers(ctx)
	blocking := 0
	for _, bl := range blockers {
		if !bl.clear {
			blocking++
		}
	}

	var b strings.Builder
	switch {
	case blocking == 0:
		b.WriteString("🟢 <b>Автобай готов</b> — покупает сам\n\n")
	case a.rm.Armed():
		fmt.Fprintf(&b, "🟡 <b>Включён, но не купит</b> — %s\n\n",
			plural(blocking, "барьер", "барьера", "барьеров"))
	default:
		b.WriteString("🔴 <b>Автобай выключен</b>\n\n")
	}

	for _, bl := range blockers {
		mark := "🔴"
		switch {
		case bl.clear && bl.warn:
			mark = "🟡"
		case bl.clear:
			mark = "✅"
		case bl.warn:
			mark = "🟡"
		}
		fmt.Fprintf(&b, "%s %s — <i>%s</i>\n", mark, bl.name, bot.Esc(bl.state))
	}

	if a.rm.ResellEnabled() {
		b.WriteString("✅ ресейл — <i>сам выставляет купленное</i>\n")
	} else {
		b.WriteString("🔴 ресейл — <i>покупает, но не выставляет</i>\n")
	}
	return b.String()
}

// Arm enables unattended buying.
func (a *App) armText(ctx context.Context) string {
	if err := a.rm.Arm(ctx); err != nil {
		return "❌ " + bot.Esc(err.Error()) + "\n\n" + a.autoPanelText(ctx)
	}
	return a.autoPanelText(ctx)
}

// Disarm stops unattended buying.
func (a *App) disarmText(ctx context.Context) string {
	if err := a.rm.Disarm(ctx, "выключен вручную"); err != nil {
		return "❌ " + bot.Esc(err.Error()) + "\n\n" + a.autoPanelText(ctx)
	}
	return a.autoPanelText(ctx)
}

// setShadowText switches recording-only mode.
func (a *App) setShadowText(ctx context.Context, on bool) string {
	if err := a.rm.SetShadowMode(ctx, on); err != nil {
		return "❌ " + bot.Esc(err.Error())
	}
	return a.autoPanelText(ctx)
}

// waiveCalibrationText accepts, or reinstates, the scoring sample requirement.
func (a *App) waiveCalibrationText(ctx context.Context, on bool) string {
	if err := a.rm.SetCalibrationWaived(ctx, on); err != nil {
		return "❌ " + bot.Esc(err.Error())
	}
	return a.autoPanelText(ctx)
}

// resellText shows or changes whether the bot may sell on its own.
//
// Buying and selling are separate switches: catching a mispriced lot and
// deciding what to ask for it are different decisions, and the second one is
// the one worth keeping by hand while the exit model is still being trusted.
func (a *App) resellText(ctx context.Context, arg string) string {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "on", "вкл", "1":
		if err := a.rm.SetResell(ctx, true); err != nil {
			return "❌ " + bot.Esc(err.Error())
		}
	case "off", "выкл", "0":
		if err := a.rm.SetResell(ctx, false); err != nil {
			return "❌ " + bot.Esc(err.Error())
		}
	case "":
		// Showing the panel is the answer: the switch and its current state
		// belong in one place.
	default:
		return "Не понял. Ожидаю <code>/resell on</code> или <code>/resell off</code>."
	}
	return a.autoPanelText(ctx)
}

// LimitsText shows the limits and today's usage.
func (a *App) limitsText(ctx context.Context) string {
	return "<b>Лимиты</b>\n<pre>" + bot.Esc(a.rm.Describe(ctx, time.Now())) + "</pre>" +
		"\nПоменять: <code>/limits set max_ticket 50</code>"
}

// SetLimit updates one limit.
func (a *App) setLimitText(ctx context.Context, key, value string) string {
	if err := a.rm.SetLimit(ctx, key, value); err != nil {
		return "❌ " + bot.Esc(err.Error())
	}
	return fmt.Sprintf("✅ %s = %s\n\n<pre>%s</pre>",
		bot.Esc(key), bot.Esc(value), bot.Esc(a.rm.Describe(ctx, time.Now())))
}

// Watch subscribes to a model.
func (a *App) watchText(ctx context.Context, collection, model string, maxPrice float64) string {
	key, msg := a.mustResolve(ctx, collection, model)
	if msg != "" {
		return msg
	}
	if err := a.st.AddWatch(ctx, key, maxPrice, time.Now()); err != nil {
		return "Не смог сохранить подписку: " + bot.Esc(err.Error())
	}
	if maxPrice > 0 {
		return fmt.Sprintf("👁 Слежу за <b>%s</b> дешевле %s", bot.Esc(key.String()), num(maxPrice))
	}
	return fmt.Sprintf("👁 Слежу за <b>%s</b> по любой цене", bot.Esc(key.String()))
}

// Unwatch removes a subscription.
func (a *App) unwatchText(ctx context.Context, collection, model string) string {
	key := tonnel.ModelKey{Name: tonnel.TitleCase(collection), Model: tonnel.TitleCase(model)}
	if resolved, ok := a.resolveModel(ctx, key.Name, model); ok {
		key = resolved
	}
	if err := a.st.RemoveWatch(ctx, key); err != nil {
		return "Не смог удалить подписку: " + bot.Esc(err.Error())
	}
	return "Убрал " + bot.Esc(key.String()) + " из вотчлиста."
}

// Watchlist lists subscriptions with their current floors.
func (a *App) watchlistText(ctx context.Context) string {
	watches, err := a.st.Watches(ctx)
	if err != nil {
		return "Не смог прочитать вотчлист: " + bot.Esc(err.Error())
	}
	if len(watches) == 0 {
		return "Вотчлист пуст. Добавь: <code>/watch Plush Pepe / Pink Diamond 1200</code>"
	}
	var b strings.Builder
	b.WriteString("<b>Вотчлист</b>\n")
	for _, w := range watches {
		line := "• " + bot.Esc(w.Key.String())
		if w.MaxPrice > 0 {
			line += fmt.Sprintf(" дешевле %s", num(w.MaxPrice))
		}
		if stat, err := a.st.ModelStat(ctx, w.Key); err == nil && stat != nil && stat.Floor > 0 {
			line += fmt.Sprintf(" · флор %s", num(stat.Floor))
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// Mute silences alerts for a collection or a single model.
func (a *App) muteText(ctx context.Context, collection, model string, d time.Duration) string {
	scope, label := a.muteScope(ctx, collection, model)
	until := time.Now().Add(d)
	if err := a.st.SetMute(ctx, scope, until); err != nil {
		return "Не смог поставить мьют: " + bot.Esc(err.Error())
	}
	return fmt.Sprintf("🔕 Замьютил <b>%s</b> на %s", bot.Esc(label), dur(d))
}

// Unmute clears a mute.
func (a *App) unmuteText(ctx context.Context, collection, model string) string {
	scope, label := a.muteScope(ctx, collection, model)
	if err := a.st.ClearMute(ctx, scope); err != nil {
		return "Не смог снять мьют: " + bot.Esc(err.Error())
	}
	return "🔔 Размьютил " + bot.Esc(label)
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
func (a *App) setAuthText(ctx context.Context, authData string) string {
	prev := a.api.Auth()
	a.api.SetAuth(authData)

	if _, err := a.api.Balance(ctx); err != nil {
		a.api.SetAuth(prev)
		return "❌ Этот authData не принят, оставляю старый:\n<code>" + bot.Esc(err.Error()) + "</code>"
	}
	if err := a.st.SetKV(ctx, "tonnel.auth", authData); err != nil {
		return "Сессия рабочая, но сохранить не вышло: " + bot.Esc(err.Error())
	}
	return fmt.Sprintf("✅ Сессия Tonnel обновлена (юзер %d).", a.api.UserID())
}

// buySignal executes a confirmed purchase and returns the message to show plus
// the gift id, so the caller can attach a link to the lot.
//
// A confirmed tap is an explicit override, so the auto-buy gates and the armed
// switch do not apply — but the listing is re-read first, because the card may
// have been sitting in the chat for a while.
func (a *App) buySignal(ctx context.Context, signalID int64) (string, int64) {
	permit := a.rm.LockPurchase()
	defer permit.Release()

	sig, err := a.st.GetSignal(ctx, signalID)
	if err != nil {
		return "Не смог прочитать сигнал: " + bot.Esc(err.Error()), 0
	}
	if sig == nil {
		return "Этого сигнала уже нет.", 0
	}

	g, err := a.api.GiftData(ctx, sig.GiftID)
	if err != nil {
		return "Не смог перечитать лот: " + bot.Esc(err.Error()), sig.GiftID
	}
	if g.GiftID.Int() == 0 {
		g.GiftID = tonnel.FlexInt(sig.GiftID)
	}
	if g.Buyer != nil {
		return "🏃 Поздно — лот уже забрали.", sig.GiftID
	}

	price := g.Price.Float()
	if price <= 0 {
		return "Лот больше не продаётся.", sig.GiftID
	}
	if price > sig.Price {
		return fmt.Sprintf(
			"⚠️ Цена выросла с %s до %s после алерта, поэтому ничего не купил.\nЕсли всё ещё нужен — <code>/val %d</code>.",
			num(sig.Price), num(price), sig.GiftID), sig.GiftID
	}

	now := time.Now()
	v, err := a.priceGift(ctx, *g, now)
	if err != nil {
		return "Не смог оценить лот перед покупкой: " + bot.Esc(err.Error()), sig.GiftID
	}

	out, buyErr := a.ex.Buy(ctx, v, *g, "manual", now)
	return a.renderPurchase(ctx, signalID, out, buyErr, false), sig.GiftID
}

// BookForSignal shows the ladder behind a card.
func (a *App) bookForSignalText(ctx context.Context, signalID int64) string {
	sig, err := a.st.GetSignal(ctx, signalID)
	if err != nil || sig == nil {
		return "Этого сигнала уже нет."
	}
	a.books.Invalidate(sig.Key)
	return a.bookText(ctx, sig.Key.Name, sig.Key.Model)
}

// MuteSignal silences the model behind a card.
func (a *App) muteSignalText(ctx context.Context, signalID int64, d time.Duration) string {
	sig, err := a.st.GetSignal(ctx, signalID)
	if err != nil || sig == nil {
		return "Этого сигнала уже нет."
	}
	if err := a.st.SetMute(ctx, sig.Key.ID(), time.Now().Add(d)); err != nil {
		return "Не смог поставить мьют: " + bot.Esc(err.Error())
	}
	return fmt.Sprintf("🔕 Замьютил <b>%s</b> на %s", bot.Esc(sig.Key.String()), dur(d))
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
		fmt.Sprintf("В %s нет модели по запросу %q. Открой <code>/floor %s</code> — там весь список.",
			bot.Esc(name), bot.Esc(model), bot.Esc(name))
}

func (a *App) unknownCollection(ctx context.Context, input string) string {
	matches, err := a.st.SearchCollections(ctx, input, 8)
	if err != nil || len(matches) == 0 {
		return fmt.Sprintf("Не знаю коллекцию %q. Возможно, снапшот рынка ещё не загрузился — глянь <code>/status</code>.", bot.Esc(input))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%q подходит под несколько коллекций:\n", bot.Esc(input))
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
