package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"golang.org/x/time/rate"
)

// portalsAPI is the live host. The older portals-market.com used by the
// archived Python wrappers no longer resolves — Portals moved to portals.tg and
// changed the response shape at the same time.
const portalsAPI = "https://portals.tg/api"

// Portals reads model floors from the Portals marketplace.
//
// One request returns every model floor for a collection, so the cache is keyed
// by collection rather than by model — a burst of cards for different models of
// the same collection costs a single call.
//
// Unlike MRKT, these read endpoints answer without credentials, so the venue is
// available with no configuration at all. An authorization token is still sent
// when one is supplied, in case that changes.
type Portals struct {
	http tls_client.HttpClient
	auth string
	fee  float64
	lim  *rate.Limiter

	floors *cache[map[string]float64]
}

// NewPortals builds the Portals reader. authData is optional.
func NewPortals(authData string, fee float64, ttl time.Duration) (*Portals, error) {
	hc, err := newHTTPClient(15)
	if err != nil {
		return nil, fmt.Errorf("portals: %w", err)
	}
	return &Portals{
		http:   hc,
		auth:   strings.TrimSpace(authData),
		fee:    fee,
		lim:    rate.NewLimiter(rate.Limit(1), 2),
		floors: newCache[map[string]float64](ttl),
	}, nil
}

// Venue implements Source.
func (p *Portals) Venue() string { return "Portals" }

// Enabled implements Source. Portals needs no credentials to be read.
func (p *Portals) Enabled() bool { return p != nil }

// Fee implements Source.
func (p *Portals) Fee() float64 { return p.fee }

// ModelFloor implements Source.
func (p *Portals) ModelFloor(ctx context.Context, collection, model string) (float64, error) {
	if !p.Enabled() {
		return 0, errNoCredentials
	}
	short := PortalsShortName(collection)
	floors, err := p.floors.get(short, func() (map[string]float64, error) {
		return p.fetch(ctx, short)
	})
	if err != nil {
		return 0, err
	}
	return floors[matchKey(model)], nil
}

// portalsFilters is the response of /collections/filters. Prices arrive as
// strings, and models with nothing listed simply omit the field.
type portalsFilters struct {
	Collections map[string]struct {
		Models []struct {
			Name       string `json:"name"`
			FloorPrice string `json:"floor_price"`
			Supply     int    `json:"supply"`
		} `json:"models"`
	} `json:"collections"`
}

func (p *Portals) fetch(ctx context.Context, short string) (map[string]float64, error) {
	if err := p.lim.Wait(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		portalsAPI+"/collections/filters?short_names="+short, nil)
	if err != nil {
		return nil, err
	}
	req.Header = http.Header{
		"accept":     {"application/json"},
		"origin":     {"https://portals.tg"},
		"referer":    {"https://portals.tg/"},
		"user-agent": {userAgent},
	}
	if p.auth != "" {
		req.Header.Set("authorization", p.auth)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("portals filters: http %d", resp.StatusCode)
	}

	var payload portalsFilters
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("portals filters: decode: %w", err)
	}

	col, ok := payload.Collections[short]
	if !ok {
		return nil, nil // unknown collection on this venue, not an error
	}
	out := make(map[string]float64, len(col.Models))
	for _, m := range col.Models {
		if m.FloorPrice == "" {
			continue // nothing listed for this model
		}
		f, err := strconv.ParseFloat(m.FloorPrice, 64)
		if err != nil || f <= 0 {
			continue
		}
		out[matchKey(m.Name)] = f
	}
	return out, nil
}

// PortalsShortName converts a display collection name to the slug Portals uses
// in its API ("Plush Pepe" -> "plushpepe").
func PortalsShortName(name string) string {
	r := strings.NewReplacer(" ", "", "'", "", "’", "", "-", "")
	return strings.ToLower(r.Replace(strings.TrimSpace(name)))
}
