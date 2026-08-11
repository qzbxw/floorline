package app

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"floorline/internal/tonnel"
)

// BackfillProgress is reported as the history download walks the market.
type BackfillProgress struct {
	Collection string
	Done       int // collections completed
	Total      int // collections to do
	Inserted   int // new trades stored so far
	Requests   int
	Oldest     time.Time
	Finished   bool
}

// Backfill downloads trade history until it reaches `days` back.
//
// It walks collection by collection and asks for a time window explicitly,
// because neither of the alternatives survives contact with the live API: the
// unfiltered endpoint returns an arbitrary slice of the archive, and the sort
// argument cannot be relied on even when filtered. A `timestamp >= cutoff`
// predicate is exact — verified against collections that trade hourly and ones
// that have not traded in a month.
//
// Velocity is the single most important input to every decision, and it cannot
// be computed from a database that only knows about the last five minutes. The
// bot stays in warm-up — refusing to buy anything unattended — until this has
// run once.
func (a *App) Backfill(ctx context.Context, days int, onProgress func(BackfillProgress)) error {
	const maxPagesPerType = 40

	// The collection list comes from the full-market snapshot, so make sure we
	// have one before deciding there is nothing to do.
	if names, err := a.st.CollectionNames(ctx); err != nil || len(names) == 0 {
		if err := a.pollStats(ctx); err != nil {
			return fmt.Errorf("load the market snapshot first: %w", err)
		}
	}
	names, err := a.st.CollectionNames(ctx)
	if err != nil {
		return fmt.Errorf("list collections: %w", err)
	}
	if len(names) == 0 {
		return fmt.Errorf("no collections known; the market snapshot is empty")
	}

	cutoff := time.Now().AddDate(0, 0, -days)
	var inserted, requests int

	for i, name := range names {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		sales, err := a.api.TradeHistory(ctx, name, cutoff, maxPagesPerType)
		requests += len(tonnel.SettledTypes)
		if err != nil {
			return fmt.Errorf("backfill %s: %w", name, err)
		}
		n, err := a.st.InsertSales(ctx, sales)
		if err != nil {
			return fmt.Errorf("backfill store %s: %w", name, err)
		}
		inserted += n

		if onProgress != nil {
			onProgress(BackfillProgress{
				Collection: name,
				Done:       i + 1,
				Total:      len(names),
				Inserted:   inserted,
				Requests:   requests,
			})
		}
	}

	if err := a.st.SetKV(ctx, "backfill.done", "1"); err != nil {
		return err
	}
	if days >= a.cfg.AttributeLookbackDays {
		_ = a.st.SetKV(ctx, "backfill.attributes.done", "1")
	}
	if err := a.refreshCoverage(ctx); err != nil {
		return err
	}
	if onProgress != nil {
		oldest, _ := a.st.OldestSaleTime(ctx)
		onProgress(BackfillProgress{
			Done: len(names), Total: len(names),
			Inserted: inserted, Requests: requests,
			Oldest: oldest, Finished: true,
		})
	}
	return nil
}

// backfillIfNeeded runs the history download on first start, in the background,
// and tells the operator when the bot becomes ready to act on its own.
func (a *App) backfillIfNeeded(ctx context.Context) {
	done, err := a.st.GetKV(ctx, "backfill.attributes.done")
	if err != nil {
		log.Warn().Err(err).Msg("could not read backfill state")
		return
	}
	if done == "1" {
		return
	}

	a.notify(fmt.Sprintf(
		"⏳ Качаю историю сделок за %d дней — нужна для ликвы и премий за атрибуты. Пока не докачаю, сигналы держу консервативными, а автобай заблокирован.",
		a.cfg.AttributeLookbackDays))

	start := time.Now()
	last := time.Now()
	err = a.Backfill(ctx, a.cfg.AttributeLookbackDays, func(p BackfillProgress) {
		if p.Finished || time.Since(last) < 30*time.Second {
			return
		}
		last = time.Now()
		log.Info().Int("collections", p.Done).Int("of", p.Total).
			Int("trades", p.Inserted).Str("current", p.Collection).
			Msg("backfilling trade history")
		a.notify(fmt.Sprintf("⏳ История: %d/%d коллекций · %d сделок пока", p.Done, p.Total, p.Inserted))
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error().Err(err).Msg("backfill failed")
		a.notify("⚠️ Не смог скачать историю: " + err.Error() +
			"\nАвтобай остаётся заблокированным. Повтори через <code>floorline backfill</code>.")
		return
	}

	count, _ := a.st.CountSales(ctx)
	a.notify(fmt.Sprintf("✅ История готова — %d сделок за %s, скачал за %s.\nФильтры автобая включены.",
		count, dur(a.Coverage()), dur(time.Since(start))))
}
