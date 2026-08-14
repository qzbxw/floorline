package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"floorline/internal/tonnel"
)

// probeCollection and probeModel are a widely listed pair used only to check
// that the other venues answer at all, when there is no live Tonnel listing to
// probe with. A "nothing listed" result here is informative, not a failure —
// what matters is whether the venue answered.
const (
	probeCollection = "Plush Pepe"
	probeModel      = "Aqua Plush"
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

		// First, because it is now how the desk sees the market. If this works
		// and everything below is challenged, the desk still trades — degraded,
		// but seeing every new ask and every settled sale.
		{"event stream (marketplace/ws)", func(ctx context.Context) (string, error) {
			return a.probeStream(ctx, 20*time.Second)
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

		// Deliberately filtered by collection. Unfiltered, this endpoint ignores
		// its sort argument and returns an arbitrary slice of year-old rows —
		// checking it that way would report success while proving nothing about
		// the data the pricing engine actually runs on.
		{"trade history (saleHistory)", func(ctx context.Context) (string, error) {
			sales, err := a.api.SaleHistory(ctx, tonnel.SaleHistoryQuery{
				Limit: 10, Type: "SALE", Name: probeCollection,
			})
			if err != nil {
				return "", err
			}
			if len(sales) == 0 {
				return "", fmt.Errorf("no trades returned for %s", probeCollection)
			}
			newest := sales[0].When()
			for i := range sales {
				if t := sales[i].When(); t.After(newest) {
					newest = t
				}
			}
			if sales[0].Name() == "" {
				return "", fmt.Errorf("trades decoded without a collection name — the payload shape changed")
			}
			return fmt.Sprintf("%d %s trades; newest %s ago at %s",
				len(sales), sales[0].Name(), dur(time.Since(newest)), num(sales[0].Price.Float())), nil
		}},

		{"balance", func(ctx context.Context) (string, error) {
			b, err := a.api.Balance(ctx)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("GRAM %s (user %d)", num(b.GRAM), a.api.UserID()), nil
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

	fmt.Fprintf(w, "  origin  %s\n", a.api.Origin())
	// Which routes exist, before anything is tried through them. When the reads
	// below fail, the first question is always whether it is Tonnel or this
	// address, and a rotation makes that answerable rather than a guess.
	for _, r := range a.api.Routes() {
		kind := "free"
		if r.Metered {
			kind = "metered — used only while the free route is refused"
		}
		fmt.Fprintf(w, "  route   %-24s %s\n", r.Name, kind)
	}
	fmt.Fprintln(w)

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

	fmt.Fprintln(w)
	if !a.cross.Enabled() {
		fmt.Fprintln(w, "Cross-market comparison disabled (set PORTALS_AUTH_DATA and/or MRKT_INIT_DATA).")
	} else {
		fmt.Fprintf(w, "Cross-market venues: %s\n", strings.Join(a.cross.Venues(), ", "))

		// Probe with a live Tonnel listing when we have one, but fall back to a
		// known-liquid model otherwise: the other venues' credentials must be
		// verifiable even when the Tonnel session is the thing that is broken.
		probe := tonnel.ModelKey{Name: probeCollection, Model: probeModel}
		if gifts, err := a.api.Feed(ctx, 1); err == nil && len(gifts) > 0 {
			probe = gifts[0].Key()
		} else {
			fmt.Fprintf(w, "  note  no Tonnel listing to probe with; falling back to %s\n", probe)
		}

		for _, r := range a.cross.Probe(ctx, probe.Name, probe.Model) {
			switch {
			case r.Err != nil:
				fmt.Fprintf(w, "  FAIL  %-8s %v\n", r.Venue, r.Err)
			case r.Floor == 0:
				fmt.Fprintf(w, "  warn  %-8s reachable, but nothing listed for %s\n", r.Venue, probe)
			default:
				fmt.Fprintf(w, "  ok    %-8s %d asks for %s: floor %s · depth ref %s\n", r.Venue, len(r.Asks), probe, num(r.Floor), num(r.Reference))
			}
		}
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
		for _, r := range a.api.Routes() {
			state := "available"
			if r.Cooling > 0 {
				state = fmt.Sprintf("refused, resting %s", r.Cooling.Round(time.Second))
			}
			fmt.Fprintf(w, "  route %-24s %s\n", r.Name, state)
		}
		fmt.Fprintln(w, "A route listed as refused is that address being challenged, not Tonnel being")
		fmt.Fprintln(w, "down. Add another with TONNEL_PROXIES if every one of them is refused.")
	case authFailed > 0:
		fmt.Fprintln(w, "The transport works — requests reached Tonnel and were answered. Only the")
		fmt.Fprintln(w, "session was rejected. Copy Telegram.WebApp.initData from the Tonnel mini app")
		fmt.Fprintln(w, "or user_auth from a gifts2 request, then send it with /auth.")
	default:
		fmt.Fprintln(w, "Endpoints answered but the payloads did not decode as expected — the private")
		fmt.Fprintln(w, "API has probably changed shape. Everything to fix lives in internal/tonnel.")
	}
	return fmt.Errorf("%d of %d checks failed", failed, len(checks))
}
