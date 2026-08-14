package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"floorline/internal/bot"
	"floorline/internal/exec"
	"floorline/internal/tonnel"
)

// App implements bot.Backend.
//
// The methods here are thin: each one takes the text produced in backend.go and
// attaches the buttons that make the view actionable. Keeping the two apart
// means the trading logic never has to know about keyboards.
var _ bot.Backend = (*App)(nil)

// Callback routes, mirrored from the bot package.
const (
	cbRelist  = "fl_relist"
	cbModel   = "fl_model"
	cbRefresh = "fl_refresh"
)

// navRefs maps short keyboard handles to models. Telegram caps callback data at
// 64 bytes, which a collection and model name together do not reliably fit
// into, so list views mint a handle instead of embedding the names.
type navRefs struct {
	mu   sync.Mutex
	seq  int64
	keys map[string]tonnel.ModelKey
	// order is a FIFO of handles, trimmed so an always-on process cannot grow
	// this map without bound.
	order []string
}

const navRefLimit = 400

func newNavRefs() *navRefs { return &navRefs{keys: make(map[string]tonnel.ModelKey)} }

func (n *navRefs) put(key tonnel.ModelKey) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seq++
	ref := strconv.FormatInt(n.seq, 36)
	n.keys[ref] = key
	n.order = append(n.order, ref)
	for len(n.order) > navRefLimit {
		delete(n.keys, n.order[0])
		n.order = n.order[1:]
	}
	return ref
}

func (n *navRefs) get(ref string) (tonnel.ModelKey, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	k, ok := n.keys[ref]
	return k, ok
}

// Home is the dashboard.
//
// It replaces a static greeting that said the same eight words every time. A
// trading desk's home screen should answer the questions asked on opening it —
// is it armed, is it really trading or only recording, how much can it spend,
// what is open, is a session running — and none of those were reachable without
// at least one more tap.
func (a *App) Home(ctx context.Context) bot.Reply {
	var b strings.Builder
	b.WriteString("⚡️ <b>Floorline</b>\n\n")

	// The switches, in the order they can bite: armed but shadowed is the state
	// that looks like trading and is not.
	switch {
	case !a.rm.Armed():
		b.WriteString("🔴 <b>автобай выключен</b>")
	case a.rm.ShadowMode():
		b.WriteString("🌑 <b>shadow</b> — считает, но не покупает")
	default:
		b.WriteString("🟢 <b>торгует сам</b>")
	}
	if a.rm.ResellEnabled() {
		b.WriteString(" · ♻️ ресейл")
	}
	b.WriteString("\n")

	if bal, ok := a.rm.Balance(); ok {
		fmt.Fprintf(&b, "💰 %s GRAM", num(bal))
	} else {
		b.WriteString("💰 баланс неизвестен")
	}
	if n, err := a.st.CountOpenPositions(ctx); err == nil && n > 0 {
		fmt.Fprintf(&b, " · 📍 %s", plural(n, "лот", "лота", "лотов"))
	}
	b.WriteString("\n")

	if s := a.loadSession(ctx); s.Active() {
		fmt.Fprintf(&b, "⚔️ сессия %s · %s\n", dur(time.Since(s.StartedAt)), plural(len(s.Pairs), "пара", "пары", "пар"))
	}
	if !a.Warm() {
		fmt.Fprintf(&b, "⏳ история прогревается — %s из %dд\n", dur(a.Coverage()), a.cfg.LookbackDays)
	}

	r := bot.Text(b.String())
	for _, row := range bot.HomeRows() {
		r = r.WithRow(row...)
	}
	return r
}

// ---- market views -------------------------------------------------------

// Status reports the health of every moving part.
func (a *App) Status(ctx context.Context) bot.Reply {
	return bot.Text(a.statusText(ctx)).
		WithRow(bot.Callback("🔄 Обновить", cbRefresh, "status"))
}

// Gram shows the external GRAM/USDT rate and floors that have not caught up.
func (a *App) Gram(ctx context.Context) bot.Reply {
	return bot.Text(a.gramText(ctx)).WithRow(bot.Callback("🔄 Обновить", cbRefresh, "gram"))
}

// Collections renders every tracked collection as a button, so browsing never
// requires typing a name. The handle carries the target view, which the model
// picker then inherits.
func (a *App) Collections(ctx context.Context, view string) bot.Reply {
	names, err := a.st.CollectionNames(ctx)
	if err != nil {
		return bot.Text("Не смог прочитать список коллекций: " + bot.Esc(err.Error()))
	}
	if len(names) == 0 {
		return bot.Text("Коллекций пока нет — снапшот рынка ещё не загрузился. Глянь <code>/status</code>.")
	}

	r := bot.Text(viewTitle(view) + " — выбери коллекцию:")
	var line []bot.Button
	for i, n := range names {
		if i >= 24 {
			break // a longer keyboard is unusable on a phone
		}
		ref := a.nav.put(tonnel.ModelKey{Name: n})
		line = append(line, bot.Callback(truncate(n, 18), cbModel, ref+"|"+view))
		if len(line) == 2 {
			r = r.WithRow(line...)
			line = nil
		}
	}
	return r.WithRow(line...)
}

// viewTitle names a drill-down target for the picker headings.
func viewTitle(view string) string {
	switch view {
	case "book":
		return "📖 Стакан"
	case "hist":
		return "🕒 Сделки"
	default:
		return "📈 Флор"
	}
}

// modelPicker lists a collection's models as buttons, all pointing at one view.
func (a *App) modelPicker(ctx context.Context, collection, view string) bot.Reply {
	rows, err := a.st.ModelsForCollection(ctx, collection)
	if err != nil || len(rows) == 0 {
		return bot.Text("По " + bot.Esc(collection) + " моделей пока нет.")
	}

	r := bot.Text(viewTitle(view) + " · <b>" + bot.Esc(collection) + "</b> — выбери модель:")
	var line []bot.Button
	shown := 0
	for _, row := range rows {
		if row.Floor <= 0 || row.Supply == 0 {
			continue // nothing to look at
		}
		ref := a.nav.put(row.Key)
		line = append(line, bot.Callback(truncate(row.Key.Model, 18), cbModel, ref+"|"+view))
		if len(line) == 2 {
			r = r.WithRow(line...)
			line = nil
		}
		if shown++; shown >= 16 {
			break
		}
	}
	return r.WithRow(line...)
}

// Floor shows a collection's models, or one model in detail.
func (a *App) Floor(ctx context.Context, collection, model string) bot.Reply {
	r := bot.Text(a.floorText(ctx, collection, model))

	if model == "" {
		// A bare collection listing is a browsing step, so offer the models
		// themselves as buttons rather than making the operator retype a name
		// with a slash in the middle of it on a phone.
		name := a.resolveCollection(ctx, collection)
		if name == "" {
			return r
		}
		rows, err := a.st.ModelsForCollection(ctx, name)
		if err != nil {
			return r
		}
		var line []bot.Button
		shown := 0
		for _, row := range rows {
			if row.Floor <= 0 || row.Supply == 0 {
				continue // nothing to look at
			}
			ref := a.nav.put(row.Key)
			line = append(line, bot.Callback(truncate(row.Key.Model, 18), cbModel, ref+"|floor"))
			if len(line) == 2 {
				r = r.WithRow(line...)
				line = nil
			}
			shown++
			if shown >= 8 {
				break
			}
		}
		return r.WithRow(line...)
	}

	key, ok := a.resolveModel(ctx, a.resolveCollection(ctx, collection), model)
	if !ok {
		return r
	}
	return a.withModelNav(r, key, "floor")
}

// BookText prints the ask ladder with cumulative depth.
func (a *App) BookText(ctx context.Context, collection, model string) bot.Reply {
	r := bot.Text(a.bookText(ctx, collection, model))
	if key, ok := a.resolveModel(ctx, a.resolveCollection(ctx, collection), model); ok {
		return a.withModelNav(r, key, "book")
	}
	return r
}

// Hist prints the real trade history for a model.
func (a *App) Hist(ctx context.Context, collection, model string) bot.Reply {
	r := bot.Text(a.histText(ctx, collection, model))
	if key, ok := a.resolveModel(ctx, a.resolveCollection(ctx, collection), model); ok {
		return a.withModelNav(r, key, "hist")
	}
	return r
}

// withModelNav adds the two views the operator does not currently have, so
// floor → book → history is one tap apart in either direction.
func (a *App) withModelNav(r bot.Reply, key tonnel.ModelKey, current string) bot.Reply {
	ref := a.nav.put(key)
	var row []bot.Button
	for _, v := range []struct{ id, label string }{
		{"floor", "📉 Флор"},
		{"book", "📊 Стакан"},
		{"hist", "🕒 Сделки"},
	} {
		if v.id == current {
			continue
		}
		row = append(row, bot.Callback(v.label, cbModel, ref+"|"+v.id))
	}
	return r.WithRow(row...)
}

// ModelByRef resolves a keyboard handle minted by a list view.
//
// A handle with no model is a collection the operator picked, so the answer is
// the model picker for the same view rather than a dead end.
func (a *App) ModelByRef(ctx context.Context, ref, view string) bot.Reply {
	key, ok := a.nav.get(ref)
	if !ok {
		return bot.Text("Кнопка протухла — открой раздел заново.")
	}
	if key.Model == "" {
		if view == "floor" {
			return a.Floor(ctx, key.Name, "")
		}
		return a.modelPicker(ctx, key.Name, view)
	}
	switch view {
	case "book":
		return a.BookText(ctx, key.Name, key.Model)
	case "hist":
		return a.Hist(ctx, key.Name, key.Model)
	default:
		return a.Floor(ctx, key.Name, key.Model)
	}
}

// Val prices one listing in full, including the gates it fails.
//
// The preview is on for the same reason it is on the card: this is usually
// reached by pasting a share link, and the first thing worth knowing about a
// gift is what it looks like.
func (a *App) Val(ctx context.Context, giftID int64) bot.Reply {
	return bot.Text(a.valText(ctx, giftID)).
		WithRow(bot.Link("🔗 Открыть в Tonnel", bot.TonnelGiftURL(giftID))).
		WithPreview()
}

// ---- book ---------------------------------------------------------------

// Positions lists open inventory with a reprice button for each.
//
// The buttons used to be one position per row plus a link button beside it,
// which for eight positions is nine rows of keyboard under the text — more
// screen than the positions themselves. The link now lives in the text, where
// the model name can carry it, and the buttons pack two to a row.
func (a *App) Positions(ctx context.Context) bot.Reply {
	r := bot.Text(a.portfolioText(ctx))

	positions, err := a.st.OpenPositions(ctx)
	if err != nil || len(positions) == 0 {
		return r
	}
	var line []bot.Button
	for i, p := range positions {
		if i >= 8 {
			break // a keyboard longer than this is unusable on a phone
		}
		line = append(line, bot.Callback("♻️ "+truncate(p.Key.Model, 12), cbRelist, p.GiftID))
		if len(line) == 2 {
			r = r.WithRow(line...)
			line = nil
		}
	}
	r = r.WithRow(line...)
	return r.WithRow(
		bot.Callback("🔄 Обновить", cbRefresh, "pos"),
		bot.Callback("📊 Обзор", cbRefresh, "portfolio"),
	)
}

func (a *App) Portfolio(ctx context.Context) bot.Reply {
	return bot.Text(a.portfolioText(ctx)).WithRow(bot.Callback("🔄 Обновить", cbRefresh, "portfolio"))
}
func (a *App) Advice(ctx context.Context, giftID int64) bot.Reply {
	return bot.Text(a.adviceText(ctx, giftID)).WithRow(bot.Callback("♻️ Переставить", cbRelist, giftID), bot.Link("🔗", bot.TonnelGiftURL(giftID)))
}
func (a *App) PositionHistory(ctx context.Context, giftID int64) bot.Reply {
	return bot.Text(a.positionHistoryText(ctx, giftID)).WithRow(bot.Link("🔗 Открыть в Tonnel", bot.TonnelGiftURL(giftID)))
}
func (a *App) SetCost(ctx context.Context, giftID int64, price float64) bot.Reply {
	return bot.Text(a.setCostText(ctx, giftID, price))
}
func (a *App) ExitAt(ctx context.Context, giftID int64, price float64, confirm string) bot.Reply {
	return bot.Text(a.exitAtText(ctx, giftID, price, confirm))
}

// PnL reports realised and unrealised profit, net of fees.
func (a *App) PnL(ctx context.Context) bot.Reply {
	return bot.Text(a.pnlText(ctx)).
		WithRow(bot.Callback("🔄 Обновить", cbRefresh, "pnl"))
}

// BalanceText reports the account balance.
func (a *App) BalanceText(ctx context.Context) bot.Reply {
	return bot.Text(a.balanceText(ctx))
}

// Relist reprices an owned gift against the current book.
func (a *App) Relist(ctx context.Context, giftID int64) bot.Reply {
	return bot.Text(a.relistText(ctx, giftID))
}

// ---- auto-buy and settings ---------------------------------------------

// AutoPanel is the one screen that owns unattended trading: what is on, what is
// blocking it, and a switch for each.
//
// Every button is a toggle labelled with the state it will produce, next to a
// line saying what the state is now. The old menu offered four fixed buttons —
// "Автобай вкл", "Автобай выкл", "Ресейл вкл", "Ресейл выкл" — none of which
// showed which was in effect, so there was no way to tell a switch that had not
// worked from one that had.
func (a *App) AutoPanel(ctx context.Context) bot.Reply {
	return a.autoPanel(ctx, a.autoPanelText(ctx))
}

func (a *App) autoPanel(ctx context.Context, text string) bot.Reply {
	// Each toggle is labelled with the state it will produce, and the checklist
	// above says what the state is now. Two to a row: these are four switches,
	// not four sections.
	buy := bot.Callback("▶️ Покупка", cbRefresh, "arm")
	if a.rm.Armed() {
		buy = bot.Callback("⏹ Покупка", cbRefresh, "disarm")
	}
	resell := bot.Callback("▶️ Ресейл", cbRefresh, "resell_on")
	if a.rm.ResellEnabled() {
		resell = bot.Callback("⏹ Ресейл", cbRefresh, "resell_off")
	}
	// The two that used to need a .env edit and a restart.
	shadow := bot.Callback("🌑 Вкл shadow", cbRefresh, "shadow_on")
	if a.rm.ShadowMode() {
		shadow = bot.Callback("🌗 Выйти из shadow", cbRefresh, "shadow_off")
	}

	r := bot.Text(text).WithRow(buy, resell).WithRow(shadow)
	if !a.calibrated(ctx) {
		if a.rm.CalibrationWaived() {
			r = r.WithRow(bot.Callback("🔒 Вернуть калибровку", cbRefresh, "calib_require"))
		} else {
			r = r.WithRow(bot.Callback("⚠️ Снять калибровку", cbRefresh, "calib_waive"))
		}
	}
	return r.WithRow(
		bot.Callback("💰 Лимиты", cbRefresh, "limits"),
		bot.Callback("🔄 Обновить", cbRefresh, "autobuy"),
	)
}

// Arm enables unattended buying. It answers with the whole panel, so the switch
// and its effect are visible in one message instead of a claim of success
// followed by silence.
func (a *App) Arm(ctx context.Context) bot.Reply {
	return a.autoPanel(ctx, a.armText(ctx))
}

// Disarm stops unattended buying.
func (a *App) Disarm(ctx context.Context) bot.Reply {
	return a.autoPanel(ctx, a.disarmText(ctx))
}

// SetShadow switches recording-only mode.
func (a *App) SetShadow(ctx context.Context, on bool) bot.Reply {
	return a.autoPanel(ctx, a.setShadowText(ctx, on))
}

// WaiveCalibration accepts, or reinstates, the scoring sample requirement.
func (a *App) WaiveCalibration(ctx context.Context, on bool) bot.Reply {
	return a.autoPanel(ctx, a.waiveCalibrationText(ctx, on))
}

// Scan sweeps the standing book for mispriced lots.
//
// The argument is a collection, a price band, a result limit, or any
// combination: "/scan 3-5", "/scan 25", "/scan Plush Pepe 3-5 15".
func (a *App) Scan(ctx context.Context, arg string) bot.Reply {
	r := bot.Text(a.scanText(ctx, arg))
	// Always a way back to the band picker: the first thing anyone wants after
	// reading a sweep is the same sweep one band over.
	return r.WithRow(bot.Callback("🔭 Другой диапазон", cbRefresh, "scan"))
}

// ScanMenu asks which price band to sweep before spending any requests on one.
//
// The sweep used to start the moment the button was pressed, bounded by the
// free balance, which answers "what can I buy right now" — and that is not the
// question most of the time. Asking first costs one tap and makes the band a
// decision rather than a default nobody chose.
func (a *App) ScanMenu(ctx context.Context) bot.Reply {
	var b strings.Builder
	b.WriteString("🔭 <b>Скан</b> — в каком диапазоне искать?\n\n")
	room, known := a.spendable()
	if known {
		fmt.Fprintf(&b, "Свободно под покупку сейчас <b>%s</b>", num(room))
		if l := a.rm.Limits(); l.MinBalanceReserve > 0 {
			if bal, ok := a.rm.Balance(); ok {
				fmt.Fprintf(&b, " (баланс %s − резерв %s)", num(bal), num(l.MinBalanceReserve))
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("Диапазон — про рынок, а не про кошелёк: можно смотреть и выше того, что сейчас лежит.\n\n")
	b.WriteString("Текстом тоже работает: <code>/scan 3-5</code> · <code>/scan -5</code> · <code>/scan Plush Pepe 3-5 25</code>")

	r := bot.Text(b.String())
	// Bands, not a keypad. Four taps that cover the whole market beat a free
	// text field on a phone, and the exact number is one message away for the
	// rare case it matters.
	r = r.WithRow(
		bot.Callback("до 3", cbRefresh, "scan:-3"),
		bot.Callback("3 – 5", cbRefresh, "scan:3-5"),
		bot.Callback("5 – 10", cbRefresh, "scan:5-10"),
	)
	r = r.WithRow(
		bot.Callback("10 – 20", cbRefresh, "scan:10-20"),
		bot.Callback("20 – 50", cbRefresh, "scan:20-50"),
		bot.Callback("50+", cbRefresh, "scan:50-"),
	)
	if known {
		r = r.WithRow(bot.Callback(fmt.Sprintf("💰 Под баланс (до %s)", num(room)), cbRefresh, "scan:balance"))
	}
	return r
}

// Trade opens, refreshes or closes a trading session.
//
// The board is one message edited in place rather than a stream, which is the
// whole point: sitting down to trade means watching a handful of pairs change,
// not scrolling past the same pair five times.
func (a *App) Trade(ctx context.Context, arg string) bot.Reply {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "off", "стоп", "выкл":
		return bot.Text(a.closeTradeSession(ctx)).WithRow(bot.Callback("🏠 Меню", "m_menu", ""))
	case "":
		if a.loadSession(ctx).Active() {
			return a.tradeBoard(ctx)
		}
		return a.tradeBoard(ctx, a.openTradeSession(ctx))
	default:
		return bot.Text("Формат: <code>/trade</code> — начать или обновить, <code>/trade off</code> — выйти.")
	}
}

// tradeBoard attaches the session controls to a board rendered elsewhere.
func (a *App) tradeBoard(ctx context.Context, text ...string) bot.Reply {
	body := ""
	if len(text) > 0 {
		body = text[0]
	} else {
		body = a.sessionBoard(ctx)
	}
	return bot.Text(body).WithRow(
		bot.Callback("🔄 Обновить", cbRefresh, "trade"),
		bot.Callback("♻️ Пересобрать пары", cbRefresh, "trade_reset"),
	).WithRow(
		bot.Callback("⏹ Выйти из сессии", cbRefresh, "trade_off"),
	)
}

// ResetSession picks the pairs again against the current market.
func (a *App) ResetSession(ctx context.Context) bot.Reply {
	return a.tradeBoard(ctx, a.openTradeSession(ctx))
}

// Resell shows or switches automatic selling.
func (a *App) Resell(ctx context.Context, arg string) bot.Reply {
	return a.autoPanel(ctx, a.resellText(ctx, arg))
}

// LimitsText shows the limits and today's usage.
func (a *App) LimitsText(ctx context.Context) bot.Reply { return bot.Text(a.limitsText(ctx)) }

// SetLimit updates one limit.
func (a *App) SetLimit(ctx context.Context, key, value string) bot.Reply {
	return bot.Text(a.setLimitText(ctx, key, value))
}

// Watch subscribes to a model.
func (a *App) Watch(ctx context.Context, collection, model string, maxPrice float64) bot.Reply {
	return bot.Text(a.watchText(ctx, collection, model, maxPrice))
}

// Unwatch removes a subscription.
func (a *App) Unwatch(ctx context.Context, collection, model string) bot.Reply {
	return bot.Text(a.unwatchText(ctx, collection, model))
}

// Watchlist lists subscriptions with their current floors.
func (a *App) Watchlist(ctx context.Context) bot.Reply {
	r := bot.Text(a.watchlistText(ctx))
	watches, err := a.st.Watches(ctx)
	if err != nil {
		return r
	}
	var line []bot.Button
	for i, w := range watches {
		if i >= 8 {
			break
		}
		ref := a.nav.put(w.Key)
		line = append(line, bot.Callback(truncate(w.Key.Model, 18), cbModel, ref+"|floor"))
		if len(line) == 2 {
			r = r.WithRow(line...)
			line = nil
		}
	}
	return r.WithRow(line...)
}

// Mute silences alerts for a collection or a single model.
func (a *App) Mute(ctx context.Context, collection, model string, d time.Duration) bot.Reply {
	return bot.Text(a.muteText(ctx, collection, model, d))
}

// Unmute clears a mute.
func (a *App) Unmute(ctx context.Context, collection, model string) bot.Reply {
	return bot.Text(a.unmuteText(ctx, collection, model))
}

// SetAuth replaces the Tonnel session.
func (a *App) SetAuth(ctx context.Context, authData string) bot.Reply {
	return bot.Text(a.setAuthText(ctx, authData))
}

// ---- card callbacks -----------------------------------------------------

// BuySignal executes a purchase the operator confirmed on a card. The reply
// replaces the card, so no stale Buy button is left behind.
func (a *App) BuySignal(ctx context.Context, signalID int64) bot.Reply {
	text, giftID := a.buySignal(ctx, signalID)
	r := bot.Text(text)
	if giftID > 0 {
		// The two things wanted immediately after a purchase: look at the lot,
		// or price the exit. The card used to end with "Управление — /pos" as
		// text, which is a command to retype on a phone.
		r = r.WithRow(
			bot.Link("🔗 Tonnel", bot.TonnelGiftURL(giftID)),
			bot.Callback("♻️ Переставить", cbRelist, giftID),
		)
	}
	return r.WithRow(bot.Callback("📍 Лоты", cbRefresh, "pos"))
}

// BookForSignal shows the ladder behind a card.
func (a *App) BookForSignal(ctx context.Context, signalID int64) bot.Reply {
	sig, err := a.st.GetSignal(ctx, signalID)
	if err != nil || sig == nil {
		return bot.Text("Этого сигнала уже нет.")
	}
	a.books.Invalidate(sig.Key)
	return a.BookText(ctx, sig.Key.Name, sig.Key.Model)
}

// MuteSignal silences the model behind a card.
func (a *App) MuteSignal(ctx context.Context, signalID int64, d time.Duration) bot.Reply {
	return bot.Text(a.muteSignalText(ctx, signalID, d))
}

// ---- purchase reporting -------------------------------------------------

// renderPurchase turns an execution outcome into the message the operator sees.
// It is shared by the Buy button, which replaces the card with it, and by
// auto-buy, which has no card and pushes it as a notification.
func (a *App) renderPurchase(ctx context.Context, sigID int64, out *exec.Outcome, err error, auto bool) string {
	kind := "Ручная покупка"
	if auto {
		kind = "Автобай"
	}

	if err != nil || out == nil || !out.Bought {
		msg := fmt.Sprintf("❌ <b>%s не прошла</b>", kind)
		if out != nil {
			msg += "\n" + bot.Esc(out.Key.String())
			if out.Note != "" {
				msg += "\n<i>" + bot.Esc(out.Note) + "</i>"
			}
		}
		if err != nil {
			msg += "\n<code>" + bot.Esc(err.Error()) + "</code>"
		}
		if sigID > 0 {
			_ = a.st.SetSignalAction(ctx, sigID, "failed")
		}
		return msg
	}

	var b strings.Builder
	fmt.Fprintf(&b, "✅ <b>%s</b> · %s\n", kind, bot.Esc(out.Key.String()))
	// The ask and the debit differ by the referral, and the gap is the one
	// number worth checking on every purchase.
	fmt.Fprintf(&b, "Аск %s → списалось <b>%s</b>\n", num(out.AskPrice), num(out.BuyPrice))
	if out.Listed {
		gain := out.ListPrice - out.BuyPrice
		fmt.Fprintf(&b, "Выставил %s · заберут — <b>%s</b> (%+.1f%%)\n",
			num(out.ListPrice), num(gain), gain/out.BuyPrice*100)
	} else {
		b.WriteString("⚠️ <b>Не выставил</b>\n")
	}
	if out.Note != "" {
		fmt.Fprintf(&b, "<i>%s</i>\n", bot.Esc(out.Note))
	}

	if sigID > 0 {
		_ = a.st.SetSignalAction(ctx, sigID, "bought")
	}
	return b.String()
}
