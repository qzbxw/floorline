// Package bot is the Telegram front end.
//
// It deliberately contains no market logic: every handler forwards to a Backend
// and prints what comes back. That keeps the trading rules in one place and
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
// nothing themselves — each method returns a ready, HTML-safe message.
type Backend interface {
	Status(ctx context.Context) string
	Floor(ctx context.Context, collection, model string) string
	BookText(ctx context.Context, collection, model string) string
	Hist(ctx context.Context, collection, model string) string
	Val(ctx context.Context, giftID int64) string

	Positions(ctx context.Context) string
	PnL(ctx context.Context) string
	BalanceText(ctx context.Context) string
	Relist(ctx context.Context, giftID int64) string

	Arm(ctx context.Context) string
	Disarm(ctx context.Context) string
	LimitsText(ctx context.Context) string
	SetLimit(ctx context.Context, key, value string) string

	Watch(ctx context.Context, collection, model string, maxPrice float64) string
	Unwatch(ctx context.Context, collection, model string) string
	Watchlist(ctx context.Context) string
	Mute(ctx context.Context, collection, model string, d time.Duration) string
	Unmute(ctx context.Context, collection, model string) string

	SetAuth(ctx context.Context, authData string) string

	BuySignal(ctx context.Context, signalID int64) string
	BookForSignal(ctx context.Context, signalID int64) string
	MuteSignal(ctx context.Context, signalID int64, d time.Duration) string
}

// Callback identifiers. Payloads carry a signal id only, because Telegram caps
// callback data at 64 bytes and collection/model names do not reliably fit.
const (
	cbBuy  = "fl_buy"
	cbBook = "fl_book"
	cbMute = "fl_mute"
	cbDrop = "fl_drop"
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

// New creates the bot and registers all handlers.
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
func (b *Bot) Notify(text string) {
	b.send(text, nil)
}

// NotifySignal pushes an actionable card with its inline keyboard.
func (b *Bot) NotifySignal(text string, signalID int64, price float64) {
	id := strconv.FormatInt(signalID, 10)
	m := &tele.ReplyMarkup{}
	buy := m.Data(fmt.Sprintf("⚡️ Buy %s", trimNum(price)), cbBuy, id)
	book := m.Data("📊 Book", cbBook, id)
	mute := m.Data("🔕 Mute 1h", cbMute, id)
	drop := m.Data("🗑", cbDrop, id)
	m.Inline(m.Row(buy), m.Row(book, mute, drop))
	b.send(text, m)
}

func (b *Bot) send(text string, markup *tele.ReplyMarkup) {
	for i, chunk := range splitMessage(text) {
		opts := &tele.SendOptions{ParseMode: tele.ModeHTML, DisableWebPagePreview: true}
		if markup != nil && i == 0 {
			opts.ReplyMarkup = markup
		}
		if _, err := b.tb.Send(b.owner, chunk, opts); err != nil {
			log.Error().Err(err).Msg("telegram send failed")
			return
		}
	}
}

func (b *Bot) register() {
	tb := b.tb

	tb.Handle("/start", b.reply(func(ctx context.Context, c tele.Context) string { return helpText }))
	tb.Handle("/help", b.reply(func(ctx context.Context, c tele.Context) string { return helpText }))

	tb.Handle("/status", b.reply(func(ctx context.Context, c tele.Context) string {
		return b.back.Status(ctx)
	}))

	tb.Handle("/floor", b.reply(func(ctx context.Context, c tele.Context) string {
		col, model := splitTarget(c.Message().Payload)
		if col == "" {
			return "Usage: <code>/floor Plush Pepe</code> or <code>/floor Plush Pepe / Pink Diamond</code>"
		}
		return b.back.Floor(ctx, col, model)
	}))

	tb.Handle("/book", b.reply(func(ctx context.Context, c tele.Context) string {
		col, model := splitTarget(c.Message().Payload)
		if col == "" || model == "" {
			return "Usage: <code>/book Plush Pepe / Pink Diamond</code>"
		}
		return b.back.BookText(ctx, col, model)
	}))

	tb.Handle("/hist", b.reply(func(ctx context.Context, c tele.Context) string {
		col, model := splitTarget(c.Message().Payload)
		if col == "" || model == "" {
			return "Usage: <code>/hist Plush Pepe / Pink Diamond</code>"
		}
		return b.back.Hist(ctx, col, model)
	}))

	tb.Handle("/val", b.reply(func(ctx context.Context, c tele.Context) string {
		id, err := strconv.ParseInt(strings.TrimSpace(c.Message().Payload), 10, 64)
		if err != nil {
			return "Usage: <code>/val 123456</code> (Tonnel gift id)"
		}
		return b.back.Val(ctx, id)
	}))

	tb.Handle("/pos", b.reply(func(ctx context.Context, c tele.Context) string {
		return b.back.Positions(ctx)
	}))
	tb.Handle("/pnl", b.reply(func(ctx context.Context, c tele.Context) string {
		return b.back.PnL(ctx)
	}))
	tb.Handle("/balance", b.reply(func(ctx context.Context, c tele.Context) string {
		return b.back.BalanceText(ctx)
	}))

	tb.Handle("/relist", b.reply(func(ctx context.Context, c tele.Context) string {
		id, err := strconv.ParseInt(strings.TrimSpace(c.Message().Payload), 10, 64)
		if err != nil {
			return "Usage: <code>/relist 123456</code> (Tonnel gift id from /pos)"
		}
		return b.back.Relist(ctx, id)
	}))

	tb.Handle("/arm", b.reply(func(ctx context.Context, c tele.Context) string {
		return b.back.Arm(ctx)
	}))
	tb.Handle("/disarm", b.reply(func(ctx context.Context, c tele.Context) string {
		return b.back.Disarm(ctx)
	}))

	tb.Handle("/limits", b.reply(func(ctx context.Context, c tele.Context) string {
		f := strings.Fields(c.Message().Payload)
		if len(f) == 0 {
			return b.back.LimitsText(ctx)
		}
		if len(f) == 3 && strings.EqualFold(f[0], "set") {
			return b.back.SetLimit(ctx, f[1], f[2])
		}
		return "Usage: <code>/limits</code> or <code>/limits set max_ticket 50</code>"
	}))

	tb.Handle("/watch", b.reply(func(ctx context.Context, c tele.Context) string {
		target, maxPrice := splitTrailingNumber(c.Message().Payload)
		col, model := splitTarget(target)
		if col == "" || model == "" {
			return "Usage: <code>/watch Plush Pepe / Pink Diamond [max price]</code>"
		}
		return b.back.Watch(ctx, col, model, maxPrice)
	}))

	tb.Handle("/unwatch", b.reply(func(ctx context.Context, c tele.Context) string {
		col, model := splitTarget(c.Message().Payload)
		if col == "" || model == "" {
			return "Usage: <code>/unwatch Plush Pepe / Pink Diamond</code>"
		}
		return b.back.Unwatch(ctx, col, model)
	}))

	tb.Handle("/watchlist", b.reply(func(ctx context.Context, c tele.Context) string {
		return b.back.Watchlist(ctx)
	}))

	tb.Handle("/mute", b.reply(func(ctx context.Context, c tele.Context) string {
		target, hours := splitTrailingNumber(c.Message().Payload)
		col, model := splitTarget(target)
		if col == "" {
			return "Usage: <code>/mute Plush Pepe [hours]</code> or <code>/mute Plush Pepe / Pink Diamond 4</code>"
		}
		if hours <= 0 {
			hours = 1
		}
		return b.back.Mute(ctx, col, model, time.Duration(hours*float64(time.Hour)))
	}))

	tb.Handle("/unmute", b.reply(func(ctx context.Context, c tele.Context) string {
		col, model := splitTarget(c.Message().Payload)
		if col == "" {
			return "Usage: <code>/unmute Plush Pepe</code>"
		}
		return b.back.Unmute(ctx, col, model)
	}))

	tb.Handle("/auth", b.reply(func(ctx context.Context, c tele.Context) string {
		raw := strings.TrimSpace(c.Message().Payload)
		if raw == "" {
			return "Usage: <code>/auth &lt;authData&gt;</code> — the initData string from LocalStorage of market.tonnel.network"
		}
		return b.back.SetAuth(ctx, raw)
	}))

	tb.Handle(&tele.Btn{Unique: cbBuy}, b.callback(func(ctx context.Context, id int64, c tele.Context) string {
		return b.back.BuySignal(ctx, id)
	}))
	tb.Handle(&tele.Btn{Unique: cbBook}, b.callback(func(ctx context.Context, id int64, c tele.Context) string {
		return b.back.BookForSignal(ctx, id)
	}))
	tb.Handle(&tele.Btn{Unique: cbMute}, b.callback(func(ctx context.Context, id int64, c tele.Context) string {
		return b.back.MuteSignal(ctx, id, time.Hour)
	}))
	tb.Handle(&tele.Btn{Unique: cbDrop}, func(c tele.Context) error {
		_ = c.Respond(&tele.CallbackResponse{Text: "skipped"})
		return c.Delete()
	})
}

// reply adapts a text-producing function into a telebot handler.
func (b *Bot) reply(fn func(context.Context, tele.Context) string) tele.HandlerFunc {
	return func(c tele.Context) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		text := fn(ctx, c)
		if text == "" {
			return nil
		}
		for _, chunk := range splitMessage(text) {
			if err := c.Send(chunk, &tele.SendOptions{ParseMode: tele.ModeHTML, DisableWebPagePreview: true}); err != nil {
				return err
			}
		}
		return nil
	}
}

// callback adapts a signal-id handler, acknowledging the tap immediately so the
// button stops spinning while a purchase is in flight.
func (b *Bot) callback(fn func(context.Context, int64, tele.Context) string) tele.HandlerFunc {
	return func(c tele.Context) error {
		id, err := strconv.ParseInt(strings.TrimSpace(c.Data()), 10, 64)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "malformed button", ShowAlert: true})
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "working…"})

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		text := fn(ctx, id, c)
		if text == "" {
			return nil
		}
		for _, chunk := range splitMessage(text) {
			if err := c.Send(chunk, &tele.SendOptions{ParseMode: tele.ModeHTML, DisableWebPagePreview: true}); err != nil {
				return err
			}
		}
		return nil
	}
}

// splitTarget parses "Collection / Model" into its two halves. A slash is
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

func trimNum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

const helpText = `<b>Floorline</b> — Tonnel gift trading desk.

<b>Market</b>
/floor <i>Collection [/ Model]</i> — floor, supply, rarity, cheapest asks
/book <i>Collection / Model</i> — the ask ladder
/hist <i>Collection / Model</i> — real trades, median, velocity
/val <i>gift_id</i> — full valuation of one listing

<b>Book</b>
/pos — open positions
/pnl — realised and unrealised, net of fees
/balance — account balance
/relist <i>gift_id</i> — reprice an owned gift against the current book

<b>Alerts</b>
/watch <i>Collection / Model [max price]</i>
/unwatch <i>Collection / Model</i>
/watchlist
/mute <i>Collection [/ Model] [hours]</i>
/unmute <i>Collection [/ Model]</i>

<b>Auto-buy</b>
/arm — enable unattended buying (needs limits first)
/disarm — stop it
/limits — show limits and today's usage
/limits set <i>key value</i> — e.g. <code>/limits set max_ticket 50</code>

<b>Ops</b>
/status — pollers, data freshness, warm-up
/auth <i>authData</i> — replace an expired Tonnel session`
