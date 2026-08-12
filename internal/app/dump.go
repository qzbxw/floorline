package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"floorline/internal/tonnel"
)

// Dump prints the raw JSON of one endpoint.
//
// These endpoints are private and undocumented, and their field names drift.
// When a response stops decoding into the shape we expect, guessing is slow and
// wrong; looking at the bytes is fast and right.
func (a *App) Dump(ctx context.Context, w io.Writer, what string) error {
	var raw any

	switch what {
	case "feed":
		err := a.api.Raw(ctx, tonnel.HostRead, "/api/pageGifts", map[string]any{
			"filter":      tonnel.BaseFilter().JSON(),
			"limit":       2,
			"page":        1,
			"sort":        tonnel.SortLatest,
			"price_range": nil,
			"ref":         0,
			"user_auth":   a.api.Auth(),
		}, &raw)
		if err != nil {
			return err
		}

	case "sales":
		var sortObj map[string]any
		_ = json.Unmarshal([]byte(`{"timestamp":-1,"gift_id":-1}`), &sortObj)
		err := a.api.Raw(ctx, tonnel.HostRead, "/api/saleHistory", map[string]any{
			"authData": a.api.Auth(),
			"page":     1,
			"limit":    3,
			"type":     "SALE",
			"filter":   map[string]any{},
			"sort":     sortObj,
		}, &raw)
		if err != nil {
			return err
		}

	case "bids":
		var sortObj map[string]any
		_ = json.Unmarshal([]byte(`{"timestamp":-1,"gift_id":-1}`), &sortObj)
		err := a.api.Raw(ctx, tonnel.HostRead, "/api/saleHistory", map[string]any{
			"authData": a.api.Auth(),
			"page":     1,
			"limit":    3,
			"type":     "BID",
			"filter":   map[string]any{},
			"sort":     sortObj,
		}, &raw)
		if err != nil {
			return err
		}

	case "balance":
		err := a.api.Raw(ctx, tonnel.HostRead, "/api/balance/info",
			map[string]any{"authData": a.api.Auth()}, &raw)
		if err != nil {
			return err
		}

	case "mygifts":
		gifts, err := a.api.MyGifts(ctx, true, 1, 2)
		if err != nil {
			return err
		}
		raw = gifts

	case "fresh":
		return a.probeFreshness(ctx, w)

	case "types":
		return a.probeTypes(ctx, w)

	default:
		// "gift 123" prints one listing exactly as the marketplace returns it.
		// The purchase path re-reads a gift immediately before spending money
		// and refuses on anything unexpected, so when a buy is rejected this is
		// the only way to see which field it tripped over.
		if id, ok := strings.CutPrefix(what, "gift "); ok {
			err := a.api.Raw(ctx, tonnel.HostRead, "/api/giftData/"+strings.TrimSpace(id),
				map[string]any{"authData": a.api.Auth()}, &raw)
			if err != nil {
				return err
			}
			break
		}
		return fmt.Errorf("unknown dump target %q (feed, sales, bids, fresh, types, balance, mygifts, gift <id>)", what)
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(bytes.TrimSpace(out), '\n'))
	return err
}

func short(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// probeFreshness reports how recently each of a few collections traded, which
// is the quickest proof that the data is live rather than cached.
func (a *App) probeFreshness(ctx context.Context, w io.Writer) error {
	cols := []string{"Lunar Snake", "B-Day Candle", "Toy Bear", "Snake Box", "Plush Pepe", "Durov's Cap"}
	now := time.Now().UTC()

	fmt.Fprintf(w, "now %s UTC\n\n", now.Format("2006-01-02 15:04:05"))
	for _, c := range cols {
		// Every settled type, not just SALE: on most collections the direct
		// sales are a rounding error next to the internal ones, and looking at
		// SALE alone makes a busy market look abandoned.
		sales, err := a.api.TradeHistory(ctx, c, now.Add(-24*time.Hour), 2)
		if err != nil {
			fmt.Fprintf(w, "%-16s ERROR %v\n", c, err)
			continue
		}
		if len(sales) == 0 {
			fmt.Fprintf(w, "%-16s nothing traded in the last 24h\n", c)
			continue
		}
		sort.Slice(sales, func(i, j int) bool { return sales[i].When().After(sales[j].When()) })
		fmt.Fprintf(w, "%-16s %d trades in 24h, newest first:\n", c, len(sales))
		for i := range sales {
			if i >= 3 {
				break
			}
			t := sales[i].When()
			fmt.Fprintf(w, "    %s UTC  (%7s ago)  %9.2f GRAM  #%-7d %s\n",
				t.Format("15:04:05"), dur(now.Sub(t)), sales[i].Price.Float(),
				sales[i].GiftNum.Int(), sales[i].Type)
		}
	}
	return nil
}

// probeTypes measures how much volume each trade type carries. Counting only
// direct sales would understate the market if internal transfers are where the
// business happens.
func (a *App) probeTypes(ctx context.Context, w io.Writer) error {
	since := time.Now().Add(-14 * 24 * time.Hour)
	cols := []string{"Liberty Figure", "Durov's Cap", "Scared Cat", "Lunar Snake", "Plush Pepe", "Toy Bear"}

	for _, kind := range []string{"SALE", "INTERNAL_SALE", "BID", "ALL"} {
		total := 0
		parts := []string{}
		for _, c := range cols {
			n := 0
			for page := 1; page <= 20; page++ {
				sales, err := a.api.SaleHistory(ctx, tonnel.SaleHistoryQuery{
					Page: page, Limit: 50, Type: kind, Name: c, Since: since,
				})
				if err != nil {
					fmt.Fprintf(w, "%-14s %s ERROR %v\n", kind, c, err)
					break
				}
				n += len(sales)
				if len(sales) < 50 {
					break
				}
			}
			total += n
			parts = append(parts, fmt.Sprintf("%s=%d", c, n))
		}
		fmt.Fprintf(w, "%-14s total %4d over 14d   %s\n", kind, total, strings.Join(parts, " "))
	}
	return nil
}
