// Package fx reads the public GRAM/USDT market. GRAM is the currency of the
// TON blockchain; Tonnel's private API still uses its legacy "TON" wire label.
package fx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"floorline/internal/store"
)

const DefaultBaseURL = "https://api.gateio.ws/api/v4"

type Client struct {
	base string
	http *http.Client
}

func New(base string, timeout time.Duration) *Client {
	if strings.TrimSpace(base) == "" {
		base = DefaultBaseURL
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: timeout}}
}

func (c *Client) Current(ctx context.Context) (store.GramQuote, error) {
	var rows []struct {
		CurrencyPair string `json:"currency_pair"`
		LastRaw      string `json:"last"`
		AskRaw       string `json:"lowest_ask"`
		BidRaw       string `json:"highest_bid"`
		ChangeRaw    string `json:"change_percentage"`
	}
	if err := c.get(ctx, "/spot/tickers?currency_pair=GRAM_USDT", &rows); err != nil {
		return store.GramQuote{}, err
	}
	if len(rows) == 0 {
		return store.GramQuote{}, fmt.Errorf("GRAM_USDT ticker is empty")
	}
	r := rows[0]
	q := store.GramQuote{TS: time.Now().UTC(), USD: number(r.LastRaw), Ask: number(r.AskRaw), Bid: number(r.BidRaw), Change24: number(r.ChangeRaw) / 100}
	if q.USD <= 0 {
		return q, fmt.Errorf("GRAM_USDT ticker has no price")
	}
	return q, nil
}

// HourlyHistory returns up to 30 days of closed hourly candles. Gate's candle
// payload is [timestamp, quoteVolume, close, high, low, open, baseVolume,closed].
func (c *Client) HourlyHistory(ctx context.Context, limit int) ([]store.GramQuote, error) {
	if limit <= 0 || limit > 1000 {
		limit = 720
	}
	var raw [][]string
	path := "/spot/candlesticks?currency_pair=GRAM_USDT&interval=1h&limit=" + strconv.Itoa(limit)
	if err := c.get(ctx, path, &raw); err != nil {
		return nil, err
	}
	out := make([]store.GramQuote, 0, len(raw))
	for _, v := range raw {
		if len(v) < 6 {
			continue
		}
		ts, err := strconv.ParseInt(v[0], 10, 64)
		if err != nil {
			continue
		}
		p := number(v[2])
		if p <= 0 {
			continue
		}
		out = append(out, store.GramQuote{TS: time.Unix(ts, 0).UTC(), USD: p})
	}
	return out, nil
}

// MinuteHistory provides enough resolution to make a 15-minute volatility
// gate useful immediately after a restart, before the live poller accumulates.
func (c *Client) MinuteHistory(ctx context.Context, limit int) ([]store.GramQuote, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	return c.candles(ctx, "1m", limit)
}

func (c *Client) candles(ctx context.Context, interval string, limit int) ([]store.GramQuote, error) {
	var raw [][]string
	path := "/spot/candlesticks?currency_pair=GRAM_USDT&interval=" + url.QueryEscape(interval) + "&limit=" + strconv.Itoa(limit)
	if err := c.get(ctx, path, &raw); err != nil {
		return nil, err
	}
	out := make([]store.GramQuote, 0, len(raw))
	for _, v := range raw {
		if len(v) < 6 {
			continue
		}
		ts, err := strconv.ParseInt(v[0], 10, 64)
		if err != nil {
			continue
		}
		p := number(v[2])
		if p > 0 {
			out = append(out, store.GramQuote{TS: time.Unix(ts, 0).UTC(), USD: p})
		}
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	u, err := url.Parse(c.base + path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "floorline/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GRAM quote http %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode GRAM quote: %w", err)
	}
	return nil
}
func number(s string) float64 { v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return v }
