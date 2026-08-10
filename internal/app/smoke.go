package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"floorline/internal/tonnel"
)

// Smoke exercises every read endpoint and prints what happened.
//
// This is the first thing to run against live credentials. Plain Go HTTP is
// rejected by the anti-bot layer on these hosts, so if the impersonating
// transport is not working, everything else is moot — and this says so in a few
// seconds rather than as a wall of poller errors later.
func (a *App) Smoke(ctx context.Context, w io.Writer) error {
	type check struct {
		name string
		run  func(context.Context) (string, error)
	}

	checks := []check{
		{"signature", func(context.Context) (string, error) {
			ts, wtf, err := tonnel.Sign(time.Now())
			if err != nil {
				return "", err
			}
			back, err := tonnel.VerifySignature(wtf)
			if err != nil {
				return "", err
			}
			if back != ts {
				return "", fmt.Errorf("round trip mismatch: signed %s, decoded %s", ts, back)
			}
			return "wtf signature round-trips", nil
		}},

		{"feed (pageGifts)", func(ctx context.Context) (string, error) {
			gifts, err := a.api.Feed(ctx, 5)
			if err != nil {
				return "", err
			}
			if len(gifts) == 0 {
				return "", fmt.Errorf("no listings returned")
			}
			g := gifts[0]
			return fmt.Sprintf("%d listings; newest %s / %s at %s",
				len(gifts), g.Name, g.Model, num(g.Price.Float())), nil
		}},

		{"market snapshot (filterStats)", func(ctx context.Context) (string, error) {
			stats, err := a.api.FilterStats(ctx)
			if err != nil {
				return "", err
			}
			if len(stats) == 0 {
				return "", fmt.Errorf("no model stats returned")
			}
			names := map[string]struct{}{}
			for _, s := range stats {
				names[s.Key.Name] = struct{}{}
			}
			return fmt.Sprintf("%d models across %d collections", len(stats), len(names)), nil
		}},

		{"trade history (saleHistory)", func(ctx context.Context) (string, error) {
			sales, err := a.api.SaleHistory(ctx, tonnel.SaleHistoryQuery{Limit: 10, Type: "SALE"})
			if err != nil {
				return "", err
			}
			if len(sales) == 0 {
				return "", fmt.Errorf("no trades returned")
			}
			s := sales[0]
			return fmt.Sprintf("%d trades; newest %s at %s (%s)",
				len(sales), s.Name, num(s.Price.Float()), s.When().Format(time.RFC3339)), nil
		}},

		{"balance", func(ctx context.Context) (string, error) {
			b, err := a.api.Balance(ctx)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("TON %s (user %d)", num(b.TON), a.api.UserID()), nil
		}},

		{"inventory (myGifts)", func(ctx context.Context) (string, error) {
			listed, err := a.api.MyGifts(ctx, true, 1, 10)
			if err != nil {
				return "", err
			}
			idle, err := a.api.MyGifts(ctx, false, 1, 10)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%d listed, %d idle", len(listed), len(idle)), nil
		}},

		{"model book", func(ctx context.Context) (string, error) {
			gifts, err := a.api.Feed(ctx, 1)
			if err != nil || len(gifts) == 0 {
				return "", fmt.Errorf("no listing to probe with")
			}
			key := gifts[0].Key()
			book, err := a.api.ModelBook(ctx, key, 5)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s → %d asks", key, len(book)), nil
		}},
	}

	var failed, blocked, authFailed int
	for _, c := range checks {
		start := time.Now()
		msg, err := c.run(ctx)
		took := time.Since(start).Round(time.Millisecond)
		if err != nil {
			failed++
			var ae *tonnel.APIError
			if errors.As(err, &ae) {
				switch {
				case ae.IsBlocked():
					blocked++
				case ae.IsAuth():
					authFailed++
				}
			}
			fmt.Fprintf(w, "  FAIL  %-30s %8s  %v\n", c.name, took, err)
			continue
		}
		fmt.Fprintf(w, "  ok    %-30s %8s  %s\n", c.name, took, msg)
	}

	if a.pt.Enabled() {
		fmt.Fprintln(w, "\nCross-market reference:")
		gifts, err := a.api.Feed(ctx, 1)
		if err == nil && len(gifts) > 0 {
			key := gifts[0].Key()
			if f, ok := a.pt.ModelFloor(ctx, key.Name, key.Model); ok {
				fmt.Fprintf(w, "  ok    portals floor for %s: %s\n", key, num(f))
			} else {
				fmt.Fprintf(w, "  warn  portals returned no floor for %s\n", key)
			}
		}
	} else {
		fmt.Fprintln(w, "\nPortals comparison disabled (PORTALS_AUTH_DATA is unset).")
	}

	if failed == 0 {
		return nil
	}

	// Separating these two is the whole point of the command. A block means the
	// transport is wrong and nothing will ever work; an auth failure means the
	// transport is fine and only the session needs replacing.
	fmt.Fprintln(w)
	switch {
	case blocked > 0:
		fmt.Fprintln(w, "Anti-bot layer is refusing requests (403/429). The TLS transport or the")
		fmt.Fprintln(w, "request rate is the problem — lower READ_RPS, or try again from another IP.")
	case authFailed > 0:
		fmt.Fprintln(w, "The transport works — requests reached Tonnel and were answered. Only the")
		fmt.Fprintln(w, "session was rejected. Grab a fresh authData from LocalStorage of")
		fmt.Fprintln(w, "market.tonnel.network and put it in TONNEL_AUTH_DATA (or send /auth to the bot).")
	default:
		fmt.Fprintln(w, "Endpoints answered but the payloads did not decode as expected — the private")
		fmt.Fprintln(w, "API has probably changed shape. Everything to fix lives in internal/tonnel.")
	}
	return fmt.Errorf("%d of %d checks failed", failed, len(checks))
}
