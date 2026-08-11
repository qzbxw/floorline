package fx

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCurrentAndHistory(t *testing.T) {
	c := New("https://quotes.test", 0)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch r.URL.Path {
		case "/spot/tickers":
			body = `[{"currency_pair":"GRAM_USDT","last":"1.3335","lowest_ask":"1.3342","highest_bid":"1.3339","change_percentage":"0.67"}]`
		case "/spot/candlesticks":
			body = `[["1786477500","61","1.3329","1.34","1.32","1.33","45","true"]]`
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	q, err := c.Current(context.Background())
	if err != nil || q.USD != 1.3335 || q.Change24 != .0067 {
		t.Fatalf("quote=%+v err=%v", q, err)
	}
	h, err := c.HourlyHistory(context.Background(), 10)
	if err != nil || len(h) != 1 || h[0].USD != 1.3329 {
		t.Fatalf("history=%+v err=%v", h, err)
	}
	m, err := c.MinuteHistory(context.Background(), 10)
	if err != nil || len(m) != 1 || m[0].USD != 1.3329 {
		t.Fatalf("minute history=%+v err=%v", m, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
