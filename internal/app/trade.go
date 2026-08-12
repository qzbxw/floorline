package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"floorline/internal/bot"
	"floorline/internal/pricing"
	"floorline/internal/tonnel"
)

// A trading session is the desk in the mode the operator actually works in.
//
// The ordinary flow is a firehose: every model on the market, judged one lot at
// a time, arriving whenever it arrives. That is right for catching a misprice
// at four in the morning and wrong for sitting down to trade, when the useful
// question is not "is this one lot good" but "which handful of pairs are worth
// my attention for the next hour, and what is happening in them right now".
//
// So a session picks that handful once, by liquidity, and then narrows
// everything to it: only those pairs produce cards, and they arrive as one
// board that is edited in place rather than as a scrolling feed. Leaving the
// session restores the firehose.
const (
	kvTradeSession = "trade.session"
	// sessionPairs is how many pairs a session watches. Enough that something is
	// always moving, few enough to hold in your head.
	sessionPairs = 10
	// sessionBoardLots is how many live candidates the board shows.
	sessionBoardLots = 6
)

// TradeSession is the active session, persisted so a restart does not silently
// drop the operator back into the firehose.
type TradeSession struct {
	StartedAt time.Time         `json:"started_at"`
	Pairs     []tonnel.ModelKey `json:"pairs"`
}

// Active reports whether a session is running.
func (s *TradeSession) Active() bool { return s != nil && len(s.Pairs) > 0 }

// Covers reports whether a model is on the session's list.
func (s *TradeSession) Covers(key tonnel.ModelKey) bool {
	if !s.Active() {
		return false
	}
	for _, p := range s.Pairs {
		if p == key {
			return true
		}
	}
	return false
}

// sessionState caches the session so the hot signal path does not hit the
// database for every listing the feed produces.
type sessionState struct {
	mu      sync.RWMutex
	loaded  bool
	session *TradeSession
}

// loadSession restores the session from storage on first use.
func (a *App) loadSession(ctx context.Context) *TradeSession {
	a.trade.mu.Lock()
	defer a.trade.mu.Unlock()
	if a.trade.loaded {
		return a.trade.session
	}
	a.trade.loaded = true
	raw, err := a.st.GetKV(ctx, kvTradeSession)
	if err != nil || raw == "" {
		return nil
	}
	var s TradeSession
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil
	}
	a.trade.session = &s
	return a.trade.session
}

func (a *App) saveSession(ctx context.Context, s *TradeSession) error {
	a.trade.mu.Lock()
	a.trade.session, a.trade.loaded = s, true
	a.trade.mu.Unlock()
	if s == nil {
		return a.st.SetKV(ctx, kvTradeSession, "")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return a.st.SetKV(ctx, kvTradeSession, string(raw))
}

// liquidPairs ranks models by how tradeable they are, not merely how often they
// print.
//
// The scan poller orders by velocity alone, which is the right question for
// "where might a misprice be" and the wrong one for "where do I want to be
// standing". A model that trades twice a day across two physical gifts is one
// item being passed around; the same rate across twenty is a market. And the
// ticket size matters at both ends — a 3-GRAM model cannot pay for the
// attention a session costs however briskly it moves.
func (a *App) liquidPairs(ctx context.Context, limit int) []tonnel.ModelKey {
	stats, err := a.st.ModelStats(ctx)
	if err != nil || len(stats) == 0 {
		return nil
	}
	type ranked struct {
		key   tonnel.ModelKey
		score float64
	}
	now, window := time.Now(), a.window()
	out := make([]ranked, 0, len(stats))
	for _, s := range stats {
		if s.Floor <= 0 || s.Supply <= 0 {
			continue
		}
		sales, err := a.st.SalesSince(ctx, s.Key, now.Add(-window))
		if err != nil || len(sales) < a.cfg.Sig.MinSales {
			continue
		}
		liq := pricing.ComputeLiquidity(sales, now, window, a.Coverage())
		if liq.Velocity < a.cfg.Sig.MinVelocity || liq.Median <= 0 {
			continue
		}
		// Turnover keeps a washed tape out; the median is the ticket the pair
		// actually trades at, so the ranking prefers where the money is.
		out = append(out, ranked{
			key:   s.Key,
			score: liq.Velocity * float64(liq.DistinctGifts) * liq.Median * clamp01(liq.Turnover),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > limit {
		out = out[:limit]
	}
	keys := make([]tonnel.ModelKey, 0, len(out))
	for _, r := range out {
		keys = append(keys, r.key)
	}
	return keys
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// openTradeSession picks the pairs and opens the board.
func (a *App) openTradeSession(ctx context.Context) string {
	pairs := a.liquidPairs(ctx, sessionPairs)
	if len(pairs) == 0 {
		return "Не из чего собрать сессию — рынок ещё не прогрузился. Глянь <code>/status</code>."
	}
	if err := a.saveSession(ctx, &TradeSession{StartedAt: time.Now(), Pairs: pairs}); err != nil {
		return "Не смог сохранить сессию: " + bot.Esc(err.Error())
	}
	return a.sessionBoard(ctx)
}

// closeTradeSession closes it and restores the ordinary feed.
func (a *App) closeTradeSession(ctx context.Context) string {
	if !a.loadSession(ctx).Active() {
		return "Сессия и так не запущена."
	}
	if err := a.saveSession(ctx, nil); err != nil {
		return "Не смог закрыть сессию: " + bot.Esc(err.Error())
	}
	return "⏹ <b>Сессия закрыта.</b> Сигналы снова идут по всему рынку."
}

// sessionBoard renders the live view: the pairs being watched and the best
// candidates standing in them right now.
func (a *App) sessionBoard(ctx context.Context) string {
	s := a.loadSession(ctx)
	if !s.Active() {
		return "Сессия не запущена. Включить — <code>/trade</code>."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "⚔️ <b>Сессия</b> · %s", dur(time.Since(s.StartedAt)))
	if room, ok := a.spendable(); ok {
		fmt.Fprintf(&b, " · банк %s", num(room))
	}
	b.WriteString("\n\n")

	names := make([]string, 0, len(s.Pairs))
	for _, p := range s.Pairs {
		names = append(names, p.Model)
	}
	fmt.Fprintf(&b, "<i>%s</i>\n\n", bot.Esc(strings.Join(names, " · ")))

	found := a.scanPass(ctx, s.Pairs, time.Now())
	if len(found) == 0 {
		b.WriteString("Сейчас в этих парах ничего с плюсовым эджем. Это нормально — жми обновить.\n")
	}
	for i, c := range found {
		if i >= sessionBoardLots {
			break
		}
		light := "🟡"
		switch {
		case len(c.Fails) == 0 && c.Score >= 10:
			light = "🟢"
		case len(c.Fails) > 0:
			light = "⚪️"
		}
		fmt.Fprintf(&b, "%s <b>%s</b>\n", light, bot.Esc(c.Val.Key.String()))
		fmt.Fprintf(&b, "   %s → %s · <b>%s</b> · %s · скор %.0f\n",
			num(c.Val.Cost), num(c.Val.FastExit), pct(c.Val.Edge), days(c.Val.FastExpectedDays), c.Score)
		if len(c.Fails) > 0 {
			fmt.Fprintf(&b, "   <i>%s</i>\n", bot.Esc(c.Fails[0]))
		}
		fmt.Fprintf(&b, "   <code>/val %d</code>\n", c.Gift.GiftID.Int())
	}

	fmt.Fprintf(&b, "\n<i>обновлено %s</i>", dt(time.Now()))
	return b.String()
}
