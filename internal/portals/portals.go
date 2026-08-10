// Package portals reads model floor prices from the Portals marketplace.
//
// This is a price *reference* only. Floorline never trades here: moving a gift
// out of Tonnel's off-chain engine to sell it elsewhere takes minutes to hours
// plus gas, by which time any spread has gone. What the comparison is good for
// is sanity — if Tonnel's floor for a model is far off what a second venue
// shows, one of the two numbers is wrong and the trade deserves a second look.
package portals

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/time/rate"
)

const apiBase = "https://portals-market.com/api"

// Client reads Portals collection filters.
type Client struct {
	http tls_client.HttpClient
	auth string
	lim  *rate.Limiter

	mu    sync.Mutex
	cache map[string]*entry
	ttl   time.Duration
}

type entry struct {
	floors map[string]float64
	at     time.Time
	err    error
}

// New builds a Portals reader. An empty authData disables the client, and every
// lookup then reports "unavailable" instead of failing.
func New(authData string, ttl time.Duration) (*Client, error) {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeout(15),
		tls_client.WithClientProfile(profiles.Chrome_131),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithCatchPanics(),
	)
	if err != nil {
		return nil, fmt.Errorf("build portals client: %w", err)
	}
	return &Client{
		http:  hc,
		auth:  strings.TrimSpace(authData),
		lim:   rate.NewLimiter(rate.Limit(1), 2),
		cache: make(map[string]*entry),
		ttl:   ttl,
	}, nil
}

// Enabled reports whether credentials are configured.
func (c *Client) Enabled() bool { return c != nil && c.auth != "" }

// ModelFloor returns the Portals floor for one model of a collection.
func (c *Client) ModelFloor(ctx context.Context, collection, model string) (float64, bool) {
	if !c.Enabled() {
		return 0, false
	}
	floors, err := c.modelFloors(ctx, collection)
	if err != nil || floors == nil {
		return 0, false
	}
	// Portals keys models by their plain name; match case-insensitively because
	// the two marketplaces do not agree on capitalisation.
	want := strings.ToLower(strings.TrimSpace(model))
	for k, v := range floors {
		if strings.ToLower(strings.TrimSpace(k)) == want {
			return v, v > 0
		}
	}
	return 0, false
}

func (c *Client) modelFloors(ctx context.Context, collection string) (map[string]float64, error) {
	short := ShortName(collection)

	c.mu.Lock()
	e, ok := c.cache[short]
	if ok && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		return e.floors, e.err
	}
	c.mu.Unlock()

	floors, err := c.fetch(ctx, short)

	c.mu.Lock()
	c.cache[short] = &entry{floors: floors, err: err, at: time.Now()}
	c.mu.Unlock()
	return floors, err
}

func (c *Client) fetch(ctx context.Context, short string) (map[string]float64, error) {
	if err := c.lim.Wait(ctx); err != nil {
		return nil, err
	}

	url := apiBase + "/collections/filters?short_names=" + short
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = http.Header{
		"accept":        {"application/json"},
		"authorization": {c.auth},
		"origin":        {"https://portals-market.com"},
		"referer":       {"https://portals-market.com/"},
		"user-agent":    {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("portals filters: http %d", resp.StatusCode)
	}

	var payload struct {
		FloorPrices map[string]struct {
			Models map[string]json.Number `json:"models"`
		} `json:"floor_prices"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("portals filters: decode: %w", err)
	}

	col, ok := payload.FloorPrices[short]
	if !ok {
		return nil, nil
	}
	out := make(map[string]float64, len(col.Models))
	for k, v := range col.Models {
		if f, err := v.Float64(); err == nil {
			out[k] = f
		}
	}
	return out, nil
}

// ShortName converts a display collection name to the slug Portals uses.
func ShortName(name string) string {
	r := strings.NewReplacer(" ", "", "'", "", "’", "", "-", "")
	return strings.ToLower(r.Replace(strings.TrimSpace(name)))
}
