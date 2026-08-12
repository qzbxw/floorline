// Package bot is the Telegram front end.
//
// It deliberately contains no market logic: every handler forwards to a Backend
// and renders what comes back. That keeps the trading rules in one place and
// makes the bot layer trivial to reason about.
package bot

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	tele "gopkg.in/telebot.v3"
)

// Backend is everything the bot can ask the application to do. Handlers render
// nothing themselves — each method returns ready, HTML-safe output.
type Backend interface {
	Status(ctx context.Context) Reply
	Gram(ctx context.Context) Reply
	Floor(ctx context.Context, collection, model string) Reply
	BookText(ctx context.Context, collection, model string) Reply
	Hist(ctx context.Context, collection, model string) Reply
	Val(ctx context.Context, giftID int64) Reply

	Positions(ctx context.Context) Reply
	Portfolio(ctx context.Context) Reply
	Advice(ctx context.Context, giftID int64) Reply
	PositionHistory(ctx context.Context, giftID int64) Reply
	SetCost(ctx context.Context, giftID int64, price float64) Reply
	ExitAt(ctx context.Context, giftID int64, price float64, confirm string) Reply
	PnL(ctx context.Context) Reply
	BalanceText(ctx context.Context) Reply
	Relist(ctx context.Context, giftID int64) Reply

	Arm(ctx context.Context) Reply
	Disarm(ctx context.Context) Reply
	// Resell shows automatic selling with an empty argument, and switches it
	// with "on" or "off".
	Resell(ctx context.Context, arg string) Reply
	LimitsText(ctx context.Context) Reply
	SetLimit(ctx context.Context, key, value string) Reply

	Watch(ctx context.Context, collection, model string, maxPrice float64) Reply
	Unwatch(ctx context.Context, collection, model string) Reply
	Watchlist(ctx context.Context) Reply
	Mute(ctx context.Context, collection, model string, d time.Duration) Reply
	Unmute(ctx context.Context, collection, model string) Reply

	SetAuth(ctx context.Context, authData string) Reply

	// BuySignal executes a purchase the operator confirmed on a card.
	BuySignal(ctx context.Context, signalID int64) Reply
	BookForSignal(ctx context.Context, signalID int64) Reply
	MuteSignal(ctx context.Context, signalID int64, d time.Duration) Reply
	// ModelByRef resolves a keyboard reference produced by a list view.
	ModelByRef(ctx context.Context, ref string, view string) Reply
	// Collections renders the collection picker for a drill-down view.
	Collections(ctx context.Context, view string) Reply
}

// Callback routes. Payloads stay short because Telegram caps callback data at
// 64 bytes, which real collection and model names do not reliably fit into.
const (
	cbBuy     = "fl_buy"     // ask for confirmation
	cbConfirm = "fl_ok"      // actually spend money
	cbCancel  = "fl_no"      // back out
	cbBook    = "fl_book"    // ladder behind a card
	cbMute    = "fl_mute"    // silence the model
	cbDrop    = "fl_drop"    // dismiss the card
	cbRelist  = "fl_relist"  // reprice an owned gift
	cbModel   = "fl_model"   // drill into a model from a list view
	cbRefresh = "fl_refresh" // re-run a read-only view
)

// telegramMessageLimit is the hard cap on a single message body.
const telegramMessageLimit = 3800

// Bot wraps the Telegram client.
type Bot struct {
	tb    *tele.Bot
	owner tele.Recipient
	back  Backend
}

type recipient struct{ id int64 }

func (r recipient) Recipient() string { return strconv.FormatInt(r.id, 10) }

// New creates the bot, registers all handlers and publishes the command menu.
func New(token string, ownerID int64, back Backend) (*Bot, error) {
	tb, err := tele.NewBot(tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
		OnError: func(err error, c tele.Context) {
			log.Error().Err(err).Msg("telegram handler failed")
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	b := &Bot{tb: tb, owner: recipient{id: ownerID}, back: back}

	// Single-tenant by design: this bot moves the owner's money, so anything
	// from another account is dropped silently rather than answered.
	tb.Use(func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			if c.Sender() == nil || c.Sender().ID != ownerID {
				return nil
			}
			return next(c)
		}
	})

	b.register()
	if err := tb.SetCommands(commandMenu); err != nil {
		// A missing menu is cosmetic; it must not stop the desk from running.
		log.Warn().Err(err).Msg("could not publish the command menu")
	}
	return b, nil
}

// Start runs the update loop until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) {
	go func() {
		<-ctx.Done()
		b.tb.Stop()
	}()
	b.tb.Start()
}

// Username returns the bot's own @name.
func (b *Bot) Username() string {
	if b.tb.Me == nil {
		return ""
	}
	return b.tb.Me.Username
}

// Notify pushes an informational message to the owner.
func (b *Bot) Notify(text string) { b.send(Text(text)) }

// NotifySignal pushes an actionable card.
//
// The Buy button only opens a confirmation — a stray tap on a phone must not be
// able to spend money on its own.
func (b *Bot) NotifySignal(text string, signalID, giftID int64, price float64) {
	b.send(Reply{Text: text}.
		WithRow(Callback(fmt.Sprintf("⚡️ Купить за %s", trimNum(price)), cbBuy, signalID)).
		WithRow(
			Link("🔗 Открыть в Tonnel", TonnelGiftURL(giftID)),
			Callback("📊 Стакан", cbBook, signalID),
		).
		WithRow(
			Callback("🔕 Мьют 1ч", cbMute, signalID),
			Callback("🗑", cbDrop, signalID),
		))
}

func (b *Bot) send(r Reply) {
	if r.Empty() {
		return
	}
	chunks := splitMessage(r.Text)
	for i, chunk := range chunks {
		opts := &tele.SendOptions{ParseMode: tele.ModeHTML, DisableWebPagePreview: true}
		// The keyboard belongs on the last chunk, next to where the eye ends up.
		if i == len(chunks)-1 {
			opts.ReplyMarkup = markup(r.Rows)
		}
		if _, err := b.tb.Send(b.owner, chunk, opts); err != nil {
			log.Error().Err(err).Msg("telegram send failed")
			return
		}
	}
}

// markup converts rows of Buttons into a telebot keyboard.
func markup(rows [][]Button) *tele.ReplyMarkup {
	if len(rows) == 0 {
		return nil
	}
	m := &tele.ReplyMarkup{}
	out := make([]tele.Row, 0, len(rows))
	for _, row := range rows {
		btns := make([]tele.Btn, 0, len(row))
		for _, bt := range row {
			if bt.URL != "" {
				btns = append(btns, m.URL(bt.Label, bt.URL))
				continue
			}
			btns = append(btns, m.Data(bt.Label, bt.Unique, bt.Data))
		}
		if len(btns) > 0 {
			out = append(out, m.Row(btns...))
		}
	}
	m.Inline(out...)
	return m
}

func (b *Bot) register() {
	tb := b.tb

	tb.Handle("/start", b.reply(func(ctx context.Context, c tele.Context) Reply { return mainMenu() }))
	tb.Handle("/help", b.reply(func(ctx context.Context, c tele.Context) Reply { return helpMenuReply() }))

	tb.Handle("/status", b.reply(func(ctx context.Context, c tele.Context) Reply {
		return b.back.Status(ctx)
	}))
	tb.Handle("/gram", b.reply(func(ctx context.Context, c tele.Context) Reply {
		return b.back.Gram(ctx)
	}))

	tb.Handle("/floor", b.reply(func(ctx context.Context, c tele.Context) Reply {
		col, model := splitTarget(c.Message().Payload)
		if col == "" {
			return Text("Формат: <code>/floor Plush Pepe</code> или <code>/floor Plush Pepe / Pink Diamond</code>\n\nПроще — через кнопки: /start → 📊 Рынок → 📈 Флор")
		}
		return b.back.Floor(ctx, col, model)
	}))

	tb.Handle("/book", b.reply(func(ctx context.Context, c tele.Context) Reply {
		col, model := splitTarget(c.Message().Payload)
		if col == "" || model == "" {
			return Text("Формат: <code>/book Plush Pepe / Pink Diamond</code>\n\nПроще — через кнопки: /start → 📊 Рынок → 📖 Стакан")
		}
		return b.back.BookText(ctx, col, model)
	}))

	tb.Handle("/hist", b.reply(func(ctx context.Context, c tele.Context) Reply {
		col, model := splitTarget(c.Message().Payload)
		if col == "" || model == "" {
			return Text("Формат: <code>/hist Plush Pepe / Pink Diamond</code>\n\nПроще — через кнопки: /start → 📊 Рынок → 🕒 Сделки")
		}
		return b.back.Hist(ctx, col, model)
	}))

	tb.Handle("/val", b.reply(func(ctx context.Context, c tele.Context) Reply {
		id, ok := ParseGiftRef(c.Message().Payload)
		if !ok {
			return Text("Формат: <code>/val 123456</code> — ID лота на Tonnel.\nИли просто кинь сюда ссылку из мини-аппа: «Share» → отправить боту.")
		}
		return b.back.Val(ctx, id)
	}))

	tb.Handle("/pos", b.reply(func(ctx context.Context, c tele.Context) Reply {
		return b.back.Positions(ctx)
	}))
	tb.Handle("/portfolio", b.reply(func(ctx context.Context, c tele.Context) Reply { return b.back.Portfolio(ctx) }))
	tb.Handle("/advice", b.reply(func(ctx context.Context, c tele.Context) Reply {
		id, err := strconv.ParseInt(strings.TrimSpace(c.Message().Payload), 10, 64)
		if err != nil {
			return Text("Формат: <code>/advice 123456</code> — ID гифта из <code>/pos</code>")
		}
		return b.back.Advice(ctx, id)
	}))
	tb.Handle("/history", b.reply(func(ctx context.Context, c tele.Context) Reply {
		id, err := strconv.ParseInt(strings.TrimSpace(c.Message().Payload), 10, 64)
		if err != nil {
			return Text("Формат: <code>/history 123456</code> — ID гифта из <code>/pos</code>")
		}
		return b.back.PositionHistory(ctx, id)
	}))
	tb.Handle("/cost", b.reply(func(ctx context.Context, c tele.Context) Reply {
		f := strings.Fields(c.Message().Payload)
		if len(f) != 2 {
			return Text("Формат: <code>/cost 123456 4.25</code> — ID гифта и цена входа")
		}
		id, e1 := strconv.ParseInt(f[0], 10, 64)
		price, e2 := strconv.ParseFloat(f[1], 64)
		if e1 != nil || e2 != nil {
			return Text("Формат: <code>/cost 123456 4.25</code> — ID гифта и цена входа")
		}
		return b.back.SetCost(ctx, id, price)
	}))
	tb.Handle("/exit", b.reply(func(ctx context.Context, c tele.Context) Reply {
		f := strings.Fields(c.Message().Payload)
		if len(f) < 2 {
			return Text("Формат: <code>/exit 123456 3.25</code>, потом подтвердить")
		}
		id, e1 := strconv.ParseInt(f[0], 10, 64)
		price, e2 := strconv.ParseFloat(f[1], 64)
		if e1 != nil || e2 != nil {
			return Text("Формат: <code>/exit 123456 3.25</code>")
		}
		confirm := ""
		if len(f) == 3 {
			confirm = f[2]
		}
		return b.back.ExitAt(ctx, id, price, confirm)
	}))
	tb.Handle("/pnl", b.reply(func(ctx context.Context, c tele.Context) Reply {
		return b.back.PnL(ctx)
	}))
	tb.Handle("/balance", b.reply(func(ctx context.Context, c tele.Context) Reply {
		return b.back.BalanceText(ctx)
	}))

	tb.Handle("/relist", b.reply(func(ctx context.Context, c tele.Context) Reply {
		id, err := strconv.ParseInt(strings.TrimSpace(c.Message().Payload), 10, 64)
		if err != nil {
			return Text("Формат: <code>/relist 123456</code> — или просто жми кнопку под <code>/pos</code>")
		}
		return b.back.Relist(ctx, id)
	}))

	tb.Handle("/arm", b.reply(func(ctx context.Context, c tele.Context) Reply {
		return b.back.Arm(ctx)
	}))
	tb.Handle("/disarm", b.reply(func(ctx context.Context, c tele.Context) Reply {
		return b.back.Disarm(ctx)
	}))
	tb.Handle("/resell", b.reply(func(ctx context.Context, c tele.Context) Reply {
		return b.back.Resell(ctx, c.Message().Payload)
	}))

	tb.Handle("/limits", b.reply(func(ctx context.Context, c tele.Context) Reply {
		f := strings.Fields(c.Message().Payload)
		switch {
		case len(f) == 0:
			return b.back.LimitsText(ctx)
		case len(f) == 3 && strings.EqualFold(f[0], "set"):
			return b.back.SetLimit(ctx, f[1], f[2])
		case len(f) == 2:
			// "/limits max_ticket 50" is the obvious typo; accept it.
			return b.back.SetLimit(ctx, f[0], f[1])
		default:
			return Text("Формат: <code>/limits</code> или <code>/limits set max_ticket 50</code>")
		}
	}))

	tb.Handle("/watch", b.reply(func(ctx context.Context, c tele.Context) Reply {
		target, maxPrice := splitTrailingNumber(c.Message().Payload)
		col, model := splitTarget(target)
		if col == "" || model == "" {
			return Text("Формат: <code>/watch Plush Pepe / Pink Diamond [макс. цена]</code>")
		}
		return b.back.Watch(ctx, col, model, maxPrice)
	}))

	tb.Handle("/unwatch", b.reply(func(ctx context.Context, c tele.Context) Reply {
		col, model := splitTarget(c.Message().Payload)
		if col == "" || model == "" {
			return Text("Формат: <code>/unwatch Plush Pepe / Pink Diamond</code>")
		}
		return b.back.Unwatch(ctx, col, model)
	}))

	tb.Handle("/watchlist", b.reply(func(ctx context.Context, c tele.Context) Reply {
		return b.back.Watchlist(ctx)
	}))

	tb.Handle("/mute", b.reply(func(ctx context.Context, c tele.Context) Reply {
		target, hours := splitTrailingNumber(c.Message().Payload)
		col, model := splitTarget(target)
		if col == "" {
			return Text("Формат: <code>/mute Plush Pepe [часы]</code> или <code>/mute Plush Pepe / Pink Diamond 4</code>")
		}
		if hours <= 0 {
			hours = 1
		}
		return b.back.Mute(ctx, col, model, time.Duration(hours*float64(time.Hour)))
	}))

	tb.Handle("/unmute", b.reply(func(ctx context.Context, c tele.Context) Reply {
		col, model := splitTarget(c.Message().Payload)
		if col == "" {
			return Text("Формат: <code>/unmute Plush Pepe</code>")
		}
		return b.back.Unmute(ctx, col, model)
	}))

	tb.Handle("/auth", b.reply(func(ctx context.Context, c tele.Context) Reply {
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return Text("Формат: <code>/auth &lt;authData&gt;</code>\n\nЭто строка initData из мини-аппа Tonnel. В LocalStorage её нет — открой мини-апп с DevTools и в консоли выполни <code>copy(Telegram.WebApp.initData)</code>.")
		}
		// The credential must not stay visible in the chat history.
		if err := c.Delete(); err != nil {
			log.Warn().Err(err).Msg("could not delete the message carrying authData")
		}
		return b.back.SetAuth(ctx, raw)
	}))

	// Anything that is not a command: the fast path from the Tonnel mini app.
	// Sharing a gift there produces a link plus a caption, and the numeric id
	// is nowhere on screen, so pasting that share into the chat has to be
	// enough to get a valuation.
	tb.Handle(tele.OnText, b.reply(func(ctx context.Context, c tele.Context) Reply {
		text := strings.TrimSpace(c.Message().Text)
		if strings.HasPrefix(text, "/") {
			return Reply{}
		}
		if id, ok := ParseGiftRef(text); ok {
			return b.back.Val(ctx, id)
		}
		return Text("Не нашёл тут лота. Кинь ссылку на гифт из Tonnel (Share) или ID числом — оценю.\n\nВсё остальное — через /start.")
	}))

	b.registerCallbacks()
}

func (b *Bot) registerCallbacks() {
	tb := b.tb

	tb.Handle(&tele.Btn{Unique: cbBuy}, func(c tele.Context) error {
		id := strings.TrimSpace(c.Data())
		_ = c.Respond(&tele.CallbackResponse{Text: "подтверди"})
		return c.Edit(&tele.ReplyMarkup{InlineKeyboard: markup([][]Button{
			{Callback("✅ Подтвердить", cbConfirm, id)},
			{Callback("✖️ Отмена", cbCancel, id)},
		}).InlineKeyboard})
	})

	tb.Handle(&tele.Btn{Unique: cbCancel}, func(c tele.Context) error {
		sigID, err := strconv.ParseInt(strings.TrimSpace(c.Data()), 10, 64)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "ошибка", ShowAlert: true})
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "отменено"})
		return c.Edit(&tele.ReplyMarkup{InlineKeyboard: markup([][]Button{
			{Callback("⚡️ Купить", cbBuy, sigID)},
			{Callback("📊 Лесенка", cbBook, sigID), Callback("🔕 Тишина 1ч", cbMute, sigID), Callback("🗑", cbDrop, sigID)},
		}).InlineKeyboard})
	})

	// The purchase result replaces the card. Leaving a live Buy button on a
	// listing we already acted on is how double-taps happen.
	tb.Handle(&tele.Btn{Unique: cbConfirm}, b.editing(func(ctx context.Context, id int64, c tele.Context) Reply {
		return b.back.BuySignal(ctx, id)
	}))

	tb.Handle(&tele.Btn{Unique: cbBook}, b.sending(func(ctx context.Context, id int64, c tele.Context) Reply {
		return b.back.BookForSignal(ctx, id)
	}))

	tb.Handle(&tele.Btn{Unique: cbMute}, b.editing(func(ctx context.Context, id int64, c tele.Context) Reply {
		return b.back.MuteSignal(ctx, id, time.Hour)
	}))

	tb.Handle(&tele.Btn{Unique: cbDrop}, func(c tele.Context) error {
		_ = c.Respond(&tele.CallbackResponse{Text: "skipped"})
		return c.Delete()
	})

	tb.Handle(&tele.Btn{Unique: cbRelist}, b.sending(func(ctx context.Context, id int64, c tele.Context) Reply {
		return b.back.Relist(ctx, id)
	}))

	// Drill-down from a list view. The payload is "<ref>|<view>", where ref is
	// an opaque handle the backend minted when it rendered the list.
	tb.Handle(&tele.Btn{Unique: cbModel}, func(c tele.Context) error {
		parts := strings.SplitN(c.Data(), "|", 2)
		if len(parts) != 2 {
			return c.Respond(&tele.CallbackResponse{Text: "ошибка кнопки", ShowAlert: true})
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "загружаю…"})

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// Editing in place keeps the drill-down on one message instead of
		// leaving a trail of dead pickers up the chat.
		r := b.back.ModelByRef(ctx, parts[0], parts[1])
		return b.editWith(c, backWith(r, cbPick, parts[1], "🔙 Коллекции"))
	})

	tb.Handle(&tele.Btn{Unique: cbRefresh}, func(c tele.Context) error {
		_ = c.Respond(&tele.CallbackResponse{Text: "загружаю…"})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Every branch returns the backend's own view, which carries action
		// buttons but no navigation — so each one gets a way back to the menu
		// it was opened from.
		var r Reply
		switch strings.TrimSpace(c.Data()) {
		case "pos":
			r = back(b.back.Positions(ctx), cbPortfolio, "🔙 Портфель")
		case "status":
			r = back(b.back.Status(ctx), cbPortfolio, "🔙 Портфель")
		case "pnl":
			r = back(b.back.PnL(ctx), cbPortfolio, "🔙 Портфель")
		case "portfolio":
			r = back(b.back.Portfolio(ctx), cbPortfolio, "🔙 Портфель")
		case "balance":
			r = back(b.back.BalanceText(ctx), cbPortfolio, "🔙 Портфель")
		case "gram":
			r = back(b.back.Gram(ctx), cbMarket, "🔙 Рынок")
		case "watchlist":
			r = back(b.back.Watchlist(ctx), cbAlerts, "🔙 Алерты")
		case "limits":
			r = back(b.back.LimitsText(ctx), cbSettings, "🔙 Настройки")
		case "arm":
			r = back(b.back.Arm(ctx), cbAuto, "🔙 Автобай")
		case "disarm":
			r = back(b.back.Disarm(ctx), cbAuto, "🔙 Автобай")
		case "resell_on":
			r = back(b.back.Resell(ctx, "on"), cbAuto, "🔙 Автобай")
		case "resell_off":
			r = back(b.back.Resell(ctx, "off"), cbAuto, "🔙 Автобай")
		default:
			return nil
		}
		return b.editWith(c, r)
	})

	// The collection picker: the entry point for floor, book and history, so a
	// model can be reached by tapping instead of typing a name with a slash.
	tb.Handle(&tele.Btn{Unique: cbPick}, func(c tele.Context) error {
		view := strings.TrimSpace(c.Data())
		_ = c.Respond(&tele.CallbackResponse{Text: "загружаю…"})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return b.editWith(c, back(b.back.Collections(ctx, view), cbMarket, "🔙 Рынок"))
	})

	// Menu navigation
	tb.Handle(&tele.Btn{Unique: cbMenu}, func(c tele.Context) error {
		_ = c.Respond(&tele.CallbackResponse{Text: "main menu"})
		return b.editWith(c, mainMenu())
	})

	tb.Handle(&tele.Btn{Unique: cbMarket}, func(c tele.Context) error {
		action := strings.TrimSpace(c.Data())
		if action == "" {
			_ = c.Respond(&tele.CallbackResponse{Text: "рынок"})
			return b.editWith(c, marketMenu())
		}
		// Submenu for market actions (floor, book, hist, val)
		_ = c.Respond(&tele.CallbackResponse{Text: "загружаю…"})
		return b.editWith(c, marketActionMenu(action))
	})

	tb.Handle(&tele.Btn{Unique: cbPortfolio}, func(c tele.Context) error {
		_ = c.Respond(&tele.CallbackResponse{Text: "портфель"})
		return b.editWith(c, portfolioMenu())
	})

	tb.Handle(&tele.Btn{Unique: cbAlerts}, func(c tele.Context) error {
		action := strings.TrimSpace(c.Data())
		if action == "" {
			_ = c.Respond(&tele.CallbackResponse{Text: "алерты"})
			return b.editWith(c, alertsMenu())
		}
		// Submenu for alert actions (watch, mute)
		_ = c.Respond(&tele.CallbackResponse{Text: "загружаю…"})
		return b.editWith(c, alertsActionMenu(action))
	})

	tb.Handle(&tele.Btn{Unique: cbSettings}, func(c tele.Context) error {
		action := strings.TrimSpace(c.Data())
		if action == "" {
			_ = c.Respond(&tele.CallbackResponse{Text: "настройки"})
			return b.editWith(c, settingsMenu())
		}
		// Submenu for settings actions (auth)
		_ = c.Respond(&tele.CallbackResponse{Text: "загружаю…"})
		return b.editWith(c, settingsActionMenu(action))
	})

	tb.Handle(&tele.Btn{Unique: cbAuto}, func(c tele.Context) error {
		_ = c.Respond(&tele.CallbackResponse{Text: "автобай"})
		return b.editWith(c, autoMenu())
	})

	tb.Handle(&tele.Btn{Unique: cbHelp}, func(c tele.Context) error {
		_ = c.Respond(&tele.CallbackResponse{Text: "справка"})
		return b.editWith(c, helpMenuReply())
	})
}

// reply adapts a Reply-producing function into a telebot message handler.
func (b *Bot) reply(fn func(context.Context, tele.Context) Reply) tele.HandlerFunc {
	return func(c tele.Context) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return b.deliver(c, fn(ctx, c))
	}
}

// sending answers a callback with a new message, for views that supplement the
// card rather than replace it.
func (b *Bot) sending(fn func(context.Context, int64, tele.Context) Reply) tele.HandlerFunc {
	return b.callback(fn, false)
}

// editing answers a callback by replacing the card it came from.
func (b *Bot) editing(fn func(context.Context, int64, tele.Context) Reply) tele.HandlerFunc {
	return b.callback(fn, true)
}

func (b *Bot) callback(fn func(context.Context, int64, tele.Context) Reply, edit bool) tele.HandlerFunc {
	return func(c tele.Context) error {
		id, err := strconv.ParseInt(strings.TrimSpace(c.Data()), 10, 64)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "ошибка кнопки", ShowAlert: true})
		}
		// Acknowledge immediately so the button stops spinning while a purchase
		// is in flight.
		_ = c.Respond(&tele.CallbackResponse{Text: "working…"})

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		r := fn(ctx, id, c)
		if edit {
			return b.editWith(c, r)
		}
		return b.deliver(c, r)
	}
}

// editWith replaces the message a callback came from. Telegram rejects an edit
// that changes nothing, and a result too long to fit is sent as a new message
// rather than truncated.
func (b *Bot) editWith(c tele.Context, r Reply) error {
	if r.Empty() {
		return nil
	}
	if len(r.Text) > telegramMessageLimit {
		return b.deliver(c, r)
	}
	opts := &tele.SendOptions{ParseMode: tele.ModeHTML, DisableWebPagePreview: true}
	if m := markup(r.Rows); m != nil {
		opts.ReplyMarkup = m
	}
	if err := c.Edit(r.Text, opts); err != nil {
		if strings.Contains(err.Error(), "message is not modified") {
			return nil
		}
		// The card may be too old to edit; do not lose the result over that.
		log.Warn().Err(err).Msg("editing the card failed, sending a new message")
		return b.deliver(c, r)
	}
	return nil
}

// deliver sends a Reply as one or more new messages.
func (b *Bot) deliver(c tele.Context, r Reply) error {
	if r.Empty() {
		return nil
	}
	chunks := splitMessage(r.Text)
	for i, chunk := range chunks {
		opts := &tele.SendOptions{ParseMode: tele.ModeHTML, DisableWebPagePreview: true}
		if i == len(chunks)-1 {
			if m := markup(r.Rows); m != nil {
				opts.ReplyMarkup = m
			}
		}
		if err := c.Send(chunk, opts); err != nil {
			return err
		}
	}
	return nil
}

// splitTarget parses "Collection / Model" into its two halves. A separator is
// required because both parts contain spaces and are otherwise ambiguous.
func splitTarget(payload string) (collection, model string) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return "", ""
	}
	for _, sep := range []string{"/", "|", " - "} {
		if a, b, ok := strings.Cut(payload, sep); ok {
			return strings.TrimSpace(a), strings.TrimSpace(b)
		}
	}
	return payload, ""
}

// splitTrailingNumber peels an optional numeric argument off the end, so
// "/watch Plush Pepe / Pink Diamond 1200" works without extra syntax.
func splitTrailingNumber(payload string) (rest string, num float64) {
	payload = strings.TrimSpace(payload)
	fields := strings.Fields(payload)
	if len(fields) == 0 {
		return "", 0
	}
	last := fields[len(fields)-1]
	if v, err := strconv.ParseFloat(last, 64); err == nil {
		return strings.TrimSpace(strings.TrimSuffix(payload, last)), v
	}
	return payload, 0
}

// splitMessage chunks long output on line boundaries so Telegram accepts it.
func splitMessage(text string) []string {
	if len(text) <= telegramMessageLimit {
		return []string{text}
	}
	var out []string
	var b strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if b.Len()+len(line) > telegramMessageLimit && b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
		b.WriteString(line)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// Esc escapes text for HTML parse mode. Attribute names come straight from the
// marketplace, so they are never trusted as markup.
func Esc(s string) string { return html.EscapeString(s) }

func trimNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// commandMenu is what Telegram shows when the operator types "/".
//
// It is deliberately two entries long. Every view is reachable by tapping, so
// advertising two dozen commands would only ask the operator to memorise what
// the keyboard already offers. The typed commands still work — they are the
// fast path for anyone who knows them, and the way to pass an id or a price —
// and /help documents all of them.
var commandMenu = []tele.Command{
	{Text: "start", Description: "главное меню"},
	{Text: "help", Description: "все команды"},
}
