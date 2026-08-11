package app

import (
	"context"
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

func (a *App) pollGram(ctx context.Context) error {
	q, err := a.fx.Current(ctx)
	if err != nil {
		return err
	}
	if err := a.st.InsertGramQuotes(ctx, []store.GramQuote{q}); err != nil {
		return err
	}
	done, _ := a.st.GetKV(ctx, "gram.backfill.done")
	if done != "2" {
		hourly, err := a.fx.HourlyHistory(ctx, 720)
		if err != nil {
			return fmt.Errorf("backfill hourly GRAM/USD: %w", err)
		}
		minute, err := a.fx.MinuteHistory(ctx, 1000)
		if err != nil {
			return fmt.Errorf("backfill minute GRAM/USD: %w", err)
		}
		if err := a.st.InsertGramQuotes(ctx, append(hourly, minute...)); err != nil {
			return err
		}
		_ = a.st.SetKV(ctx, "gram.backfill.done", "2")
	}
	a.detectGramVolatility(ctx, q.TS)
	return nil
}

func ratePoint(q store.GramQuote) pricing.RatePoint { return pricing.RatePoint{TS: q.TS, USD: q.USD} }

func (a *App) fxForModel(ctx context.Context, key tonnel.ModelKey, currentFloor float64, now time.Time) pricing.FXContext {
	cur, ok, _ := a.st.LatestGramQuote(ctx)
	if !ok {
		return pricing.FXContext{}
	}
	q15, _, _ := a.st.GramQuoteAt(ctx, now.Add(-15*time.Minute))
	q1, _, _ := a.st.GramQuoteAt(ctx, now.Add(-time.Hour))
	floor1, _, _ := a.st.FloorAt(ctx, key, now.Add(-time.Hour))
	return pricing.ComputeFXContext(now, currentFloor, ratePoint(cur), ratePoint(q15), ratePoint(q1), floor1)
}

func (a *App) normalizeGramSales(ctx context.Context, sales []store.SaleRow, since time.Time) ([]store.SaleRow, float64, float64) {
	cur, ok, _ := a.st.LatestGramQuote(ctx)
	if !ok || time.Since(cur.TS) > 5*time.Minute {
		return sales, 0, 0
	}
	quotes, err := a.st.GramQuotesSince(ctx, since.Add(-2*time.Hour))
	if err != nil {
		return sales, 0, cur.USD
	}
	rates := make([]pricing.RatePoint, 0, len(quotes))
	for _, q := range quotes {
		rates = append(rates, ratePoint(q))
	}
	normalized, coverage := pricing.NormalizeSalesForRate(sales, rates, cur.USD)
	return normalized, coverage, cur.USD
}

type floorLagRow struct {
	Key                  tonnel.ModelKey
	Floor, Expected, Lag float64
}

func (a *App) floorLags(ctx context.Context, now time.Time) ([]floorLagRow, error) {
	keys, err := a.trackedModels(ctx)
	if err != nil {
		return nil, err
	}
	var out []floorLagRow
	for _, key := range keys {
		st, err := a.st.ModelStat(ctx, key)
		if err != nil || st == nil || st.Floor <= 0 {
			continue
		}
		fx := a.fxForModel(ctx, key, st.Floor, now)
		if fx.ExpectedFloor > 0 {
			out = append(out, floorLagRow{key, st.Floor, fx.ExpectedFloor, fx.FloorLag})
		}
	}
	sort.Slice(out, func(i, j int) bool { return math.Abs(out[i].Lag) > math.Abs(out[j].Lag) })
	return out, nil
}

func (a *App) gramText(ctx context.Context) string {
	now := time.Now()
	q, ok, err := a.st.LatestGramQuote(ctx)
	if err != nil {
		return "Could not read GRAM rate: " + bot.Esc(err.Error())
	}
	if !ok {
		return "GRAM rate has not been loaded yet."
	}
	q15, _, _ := a.st.GramQuoteAt(ctx, now.Add(-15*time.Minute))
	q1, _, _ := a.st.GramQuoteAt(ctx, now.Add(-time.Hour))
	move := func(old store.GramQuote, target time.Time, tolerance time.Duration) float64 {
		if old.USD <= 0 || absDuration(old.TS.Sub(target)) > tolerance {
			return 0
		}
		return q.USD/old.USD - 1
	}
	spread := 0.0
	if q.Bid > 0 && q.Ask > 0 {
		spread = (q.Ask/q.Bid - 1) * 100
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>GRAM/USDT %s</b>\n15m %+.2f%% · 1h %+.2f%% · 24h %+.2f%% · spread %.2f%%\nQuote %s\n", num(q.USD), move(q15, now.Add(-15*time.Minute), 10*time.Minute)*100, move(q1, now.Add(-time.Hour), 15*time.Minute)*100, q.Change24*100, spread, ago(q.TS))
	if now.Sub(q.TS) > 5*time.Minute {
		b.WriteString("⚠️ Quote is stale; auto-buy is blocked.\n")
	}
	lags, _ := a.floorLags(ctx, now)
	if len(lags) > 0 {
		b.WriteString("\n<b>Tracked floor lag</b>\n")
		for i, r := range lags {
			if i >= 10 {
				break
			}
			side := "stale expensive"
			if r.Lag < 0 {
				side = "stale cheap"
			}
			fmt.Fprintf(&b, "%s · floor %s vs FX %s · %+.1f%% (%s)\n", bot.Esc(r.Key.String()), num(r.Floor), num(r.Expected), r.Lag*100, side)
		}
	}
	return b.String()
}

func (a *App) detectGramVolatility(ctx context.Context, now time.Time) {
	q, ok, _ := a.st.LatestGramQuote(ctx)
	if !ok {
		return
	}
	q15, ok15, _ := a.st.GramQuoteAt(ctx, now.Add(-15*time.Minute))
	q1, ok1, _ := a.st.GramQuoteAt(ctx, now.Add(-time.Hour))
	m15, m1 := 0.0, 0.0
	if ok15 && q15.USD > 0 && absDuration(q15.TS.Sub(now.Add(-15*time.Minute))) <= 10*time.Minute {
		m15 = q.USD/q15.USD - 1
	}
	if ok1 && q1.USD > 0 && absDuration(q1.TS.Sub(now.Add(-time.Hour))) <= 15*time.Minute {
		m1 = q.USD/q1.USD - 1
	}
	if math.Abs(m15) < .02 && math.Abs(m1) < .04 {
		return
	}
	if !a.throttle("gram-volatility", 30*time.Minute) {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🌪 <b>GRAM volatility</b>\nGRAM/USDT %s · 15m %+.1f%% · 1h %+.1f%%\n", num(q.USD), m15*100, m1*100)
	lags, _ := a.floorLags(ctx, now)
	shown := 0
	for _, row := range lags {
		if shown == 3 {
			break
		}
		if math.Abs(row.Lag) < .05 {
			continue
		}
		fmt.Fprintf(&b, "%s floor lag %+.1f%%\n", bot.Esc(row.Key.String()), row.Lag*100)
		shown++
	}
	if shown == 0 {
		b.WriteString("Tracked floors have no confirmed 5%+ lag yet.\n")
	}
	b.WriteString("Open /gram before trading; auto-buy is blocked during sharp 15m moves.")
	a.notify(b.String())
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
