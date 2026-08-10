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

// ---- market views -------------------------------------------------------

// Status reports the health of every moving part.
func (a *App) Status(ctx context.Context) bot.Reply {
	return bot.Text(a.statusText(ctx)).
		WithRow(bot.Callback("🔄 Refresh", cbRefresh, "status"))
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
		{"floor", "📉 Floor"},
		{"book", "📊 Book"},
		{"hist", "🕒 Trades"},
	} {
		if v.id == current {
			continue
		}
		row = append(row, bot.Callback(v.label, cbModel, ref+"|"+v.id))
	}
	return r.WithRow(row...)
}

// ModelByRef resolves a keyboard handle minted by a list view.
func (a *App) ModelByRef(ctx context.Context, ref, view string) bot.Reply {
	key, ok := a.nav.get(ref)
	if !ok {
		return bot.Text("That button has expired — run the command again.")
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
func (a *App) Val(ctx context.Context, giftID int64) bot.Reply {
	return bot.Text(a.valText(ctx, giftID)).
		WithRow(bot.Link("🔗 Open on Tonnel", bot.TonnelGiftURL(giftID)))
}

// ---- book ---------------------------------------------------------------

// Positions lists open inventory, each row carrying its own Relist button.
func (a *App) Positions(ctx context.Context) bot.Reply {
	r := bot.Text(a.positionsText(ctx))

	positions, err := a.st.OpenPositions(ctx)
	if err != nil || len(positions) == 0 {
		return r
	}
	for i, p := range positions {
		if i >= 8 {
			break // a keyboard longer than this is unusable on a phone
		}
		label := fmt.Sprintf("♻️ Relist %s", truncate(p.Key.Model, 14))
		r = r.WithRow(
			bot.Callback(label, cbRelist, p.GiftID),
			bot.Link("🔗", bot.TonnelGiftURL(p.GiftID)),
		)
	}
	return r.WithRow(bot.Callback("🔄 Refresh", cbRefresh, "pos"))
}

// PnL reports realised and unrealised profit, net of fees.
func (a *App) PnL(ctx context.Context) bot.Reply {
	return bot.Text(a.pnlText(ctx)).
		WithRow(bot.Callback("🔄 Refresh", cbRefresh, "pnl"))
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

// Arm enables unattended buying.
func (a *App) Arm(ctx context.Context) bot.Reply { return bot.Text(a.armText(ctx)) }

// Disarm stops unattended buying.
func (a *App) Disarm(ctx context.Context) bot.Reply { return bot.Text(a.disarmText(ctx)) }

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
		r = r.WithRow(bot.Link("🔗 Open on Tonnel", bot.TonnelGiftURL(giftID)))
	}
	return r
}

// BookForSignal shows the ladder behind a card.
func (a *App) BookForSignal(ctx context.Context, signalID int64) bot.Reply {
	sig, err := a.st.GetSignal(ctx, signalID)
	if err != nil || sig == nil {
		return bot.Text("That signal is gone.")
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
	kind := "Manual buy"
	if auto {
		kind = "Auto-buy"
	}

	if err != nil || out == nil || !out.Bought {
		msg := fmt.Sprintf("❌ <b>%s failed</b>", kind)
		if out != nil {
			msg += "\n" + bot.Esc(out.Key.String())
			if out.Note != "" {
				msg += "\n" + bot.Esc(out.Note)
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
	fmt.Fprintf(&b, "✅ <b>%s</b> — %s\nBought at %s", kind, bot.Esc(out.Key.String()), num(out.BuyPrice))
	if out.Listed {
		gain := out.ListPrice*(1-a.cfg.TonnelFee) - out.BuyPrice
		fmt.Fprintf(&b, "\nRelisted at %s → net %s if it fills (%+.1f%%)",
			num(out.ListPrice), num(gain), gain/out.BuyPrice*100)
	} else {
		b.WriteString("\n⚠️ <b>Not relisted.</b>")
	}
	if out.Note != "" {
		fmt.Fprintf(&b, "\n<i>%s</i>", bot.Esc(out.Note))
	}
	fmt.Fprintf(&b, "\n\nManage it with <code>/pos</code>.")

	if sigID > 0 {
		_ = a.st.SetSignalAction(ctx, sigID, "bought")
	}
	return b.String()
}
