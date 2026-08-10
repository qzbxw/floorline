package app

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"floorline/internal/tonnel"
)

// BackfillProgress is reported as the history download walks backwards.
type BackfillProgress struct {
	Pages    int
	Inserted int
	Oldest   time.Time
	Done     bool
}

// Backfill downloads trade history until it reaches `days` back.
//
// Velocity is the single most important input to every decision, and it cannot
// be computed from a database that only knows about the last five minutes. The
// bot stays in warm-up — refusing to buy anything unattended — until this has
// run once.
func (a *App) Backfill(ctx context.Context, days int, onProgress func(BackfillProgress)) error {
	const pageSize = 50
	const maxPages = 4000

	cutoff := time.Now().AddDate(0, 0, -days)
	var inserted int

	for page := 1; page <= maxPages; page++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		sales, err := a.api.SaleHistory(ctx, tonnel.SaleHistoryQuery{
			Page: page, Limit: pageSize, Type: "SALE",
		})
		if err != nil {
			return fmt.Errorf("backfill page %d: %w", page, err)
		}
		if len(sales) == 0 {
			break
		}

		n, err := a.st.InsertSales(ctx, sales)
		if err != nil {
			return fmt.Errorf("backfill store page %d: %w", page, err)
		}
		inserted += n

		// Pages come newest-first, so the last row is the oldest on the page.
		oldest := sales[len(sales)-1].When()
		if onProgress != nil && page%10 == 0 {
			onProgress(BackfillProgress{Pages: page, Inserted: inserted, Oldest: oldest})
		}
		if !oldest.IsZero() && oldest.Before(cutoff) {
			break
		}
		if len(sales) < pageSize {
			break // reached the end of what the marketplace will serve
		}
	}

	if err := a.st.SetKV(ctx, "backfill.done", "1"); err != nil {
		return err
	}
	if err := a.refreshCoverage(ctx); err != nil {
		return err
	}
	if onProgress != nil {
		oldest, _ := a.st.OldestSaleTime(ctx)
		onProgress(BackfillProgress{Inserted: inserted, Oldest: oldest, Done: true})
	}
	return nil
}

// backfillIfNeeded runs the history download on first start, in the background,
// and tells the operator when the bot becomes ready to act on its own.
func (a *App) backfillIfNeeded(ctx context.Context) {
	done, err := a.st.GetKV(ctx, "backfill.done")
	if err != nil {
		log.Warn().Err(err).Msg("could not read backfill state")
		return
	}
	if done == "1" {
		return
	}

	a.notify(fmt.Sprintf(
		"⏳ Downloading %d days of trade history. Signals stay conservative and auto-buy is held back until this finishes.",
		a.cfg.LookbackDays))

	start := time.Now()
	err = a.Backfill(ctx, a.cfg.LookbackDays, func(p BackfillProgress) {
		if p.Done {
			return
		}
		log.Info().Int("pages", p.Pages).Int("new", p.Inserted).
			Time("oldest", p.Oldest).Msg("backfilling trade history")
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error().Err(err).Msg("backfill failed")
		a.notify("⚠️ History download failed: " + err.Error() +
			"\nAuto-buy stays held back. Retry with <code>floorline backfill</code>.")
		return
	}

	count, _ := a.st.CountSales(ctx)
	a.notify(fmt.Sprintf("✅ History ready — %d trades covering %s, downloaded in %s.\nAuto-buy gates are now live.",
		count, dur(a.Coverage()), dur(time.Since(start))))
}
