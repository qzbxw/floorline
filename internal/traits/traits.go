// Package traits reads the gift catalogue from api.changes.tg: which models,
// backdrops and symbols a collection has, how rare each one is, and what colour
// a backdrop actually is.
//
// Thanks to @GiftChanges (api.changes.tg) for this API.
//
// It matters because every other number the desk has comes from a marketplace,
// and a marketplace tells you what people are asking, never what the thing is.
// Rarity here is the mint itself: the percentages Telegram assigned when the
// collection was issued. Tonnel prints some of them as a suffix — "Dark Soul
// (0.4%)" — but leaves them off often enough that the desk regularly prices a
// specimen with no idea how scarce it is, and it has no source at all for how
// scarce a *combination* is.
//
// The colours are the other half. Whether a gift is "mono" — backdrop and
// symbol in one palette, the look that trades above the plain examples — was
// decided by matching colour words in the attribute names, which cannot see
// that "Cyberpunk" is violet or that "Old Gold" and "Midas Bunny" are the same
// gold. This endpoint publishes the backdrop's actual centre and edge colours,
// so that judgement can be made from the colour rather than from its name.
//
// The data is a property of the collection, not of the market: it changes when
// Telegram issues something new and never otherwise. So it is fetched once per
// collection and cached for a day, on the machine's own address — this is not
// Tonnel, it does not share the anti-bot layer, and it costs none of the
// metered proxy plan.
package traits

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultHost serves the catalogue.
const DefaultHost = "api.changes.tg"

// Attribution is the credit the API asks every consumer to show its users. It
// is surfaced in the bot's help text; see the package comment for the source.
const Attribution = "Редкость и цвета трейтов — @GiftChanges (api.changes.tg)"

// cacheTTL is how long a collection's catalogue is reused. The mint does not
// change; only a new collection does, and a day is well inside the window in
// which the desk would notice one through the market anyway.
const cacheTTL = 24 * time.Hour

// Rarity is the share of a collection that carries one attribute value, in
// percent — 0.4 means four in a thousand.
type Rarity float64

// Catalogue is one collection's mint: every attribute value and its rarity,
// plus the colours of the backdrops.
type Catalogue struct {
	Collection string
	Models     map[string]Rarity
	Backdrops  map[string]Rarity
	Symbols    map[string]Rarity
	// BackdropColour is the centre colour of each backdrop, as "#rrggbb".
	BackdropColour map[string]string
	FetchedAt      time.Time
}

// Client fetches catalogues and remembers them.
type Client struct {
	host string
	http *http.Client

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	mu   sync.Mutex
	cat  *Catalogue
	err  error
	at   time.Time
	once bool
}

// New builds a client. A nil http.Client means a sensible default.
func New(host string, timeout time.Duration) *Client {
	if host == "" {
		host = DefaultHost
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{
		host:    host,
		http:    &http.Client{Timeout: timeout},
		entries: make(map[string]*entry),
	}
}

// Get returns a collection's catalogue, fetching it at most once a day.
//
// A failure is cached too, for the same reason a book fetch caches one: a
// collection this API has never heard of must not be re-asked on every
// valuation. Callers treat a nil catalogue as "no independent data", which is
// always a tolerable answer here — nothing in the desk depends on this being
// available.
func (c *Client) Get(ctx context.Context, collection string) (*Catalogue, error) {
	key := normalise(collection)
	if key == "" {
		return nil, fmt.Errorf("empty collection")
	}

	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok {
		e = &entry{}
		c.entries[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.once && time.Since(e.at) < cacheTTL {
		return e.cat, e.err
	}

	cat, err := c.fetch(ctx, collection, key)
	e.cat, e.err, e.at, e.once = cat, err, time.Now(), true
	return cat, err
}

func (c *Client) fetch(ctx context.Context, collection, key string) (*Catalogue, error) {
	var payload struct {
		Gift struct {
			Name string `json:"name"`
		} `json:"gift"`
		Models    []namedRarity `json:"models"`
		Backdrops []namedRarity `json:"backdrops"`
		Symbols   []namedRarity `json:"symbols"`
	}
	if err := c.getJSON(ctx, "/gift/"+key, &payload); err != nil {
		return nil, err
	}

	cat := &Catalogue{
		Collection:     collection,
		Models:         index(payload.Models),
		Backdrops:      index(payload.Backdrops),
		Symbols:        index(payload.Symbols),
		BackdropColour: map[string]string{},
		FetchedAt:      time.Now(),
	}
	if payload.Gift.Name != "" {
		cat.Collection = payload.Gift.Name
	}

	// Colours come from a second endpoint. They are the smaller half of the
	// value here, so a failure is not one: the catalogue is still worth having
	// without them.
	var backdrops []struct {
		Name string `json:"name"`
		Hex  struct {
			CenterColor string `json:"centerColor"`
		} `json:"hex"`
	}
	if err := c.getJSON(ctx, "/backdrops/"+key, &backdrops); err == nil {
		for _, b := range backdrops {
			if b.Name != "" && b.Hex.CenterColor != "" {
				cat.BackdropColour[fold(b.Name)] = b.Hex.CenterColor
			}
		}
	}
	return cat, nil
}

type namedRarity struct {
	Name   string  `json:"name"`
	Rarity float64 `json:"rarity"`
}

func index(in []namedRarity) map[string]Rarity {
	out := make(map[string]Rarity, len(in))
	for _, v := range in {
		if v.Name != "" {
			out[fold(v.Name)] = Rarity(v.Rarity)
		}
	}
	return out
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+c.host+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "floorline/1.0 (+gift desk)")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("traits %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("traits %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("traits %s: http %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("traits %s: decode: %w", path, err)
	}
	return nil
}

// Model, Backdrop and Symbol return a value's rarity, and whether it is known.
func (c *Catalogue) Model(name string) (Rarity, bool) { return lookup(c.Models, name) }

func (c *Catalogue) Backdrop(name string) (Rarity, bool) { return lookup(c.Backdrops, name) }

func (c *Catalogue) Symbol(name string) (Rarity, bool) { return lookup(c.Symbols, name) }

// Colour returns a backdrop's centre colour as "#rrggbb".
func (c *Catalogue) Colour(backdrop string) (string, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.BackdropColour[fold(backdrop)]
	return v, ok
}

func lookup(m map[string]Rarity, name string) (Rarity, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[fold(name)]
	return v, ok
}

// Combined is how rare this exact specimen is, as a share of the collection.
//
// The three draws are independent at mint, so their shares multiply. This is
// the number no marketplace can give: a model floor is set by whatever ordinary
// specimen of that model is cheapest, and says nothing about one that also drew
// a 1% backdrop and a 0.4% symbol. Values that are unknown are left out rather
// than assumed, so the result is always "at least this rare".
func (c *Catalogue) Combined(model, backdrop, symbol string) (share Rarity, known int) {
	share = 1
	for _, v := range []struct {
		m    map[string]Rarity
		name string
	}{{c.Models, model}, {c.Backdrops, backdrop}, {c.Symbols, symbol}} {
		if r, ok := lookup(v.m, v.name); ok && r > 0 {
			share *= r / 100
			known++
		}
	}
	if known == 0 {
		return 0, 0
	}
	return share * 100, known
}

// OneIn is Combined stated the way anyone actually reads it: one specimen in
// how many. Zero when nothing about the specimen is known.
func (c *Catalogue) OneIn(model, backdrop, symbol string) float64 {
	share, known := c.Combined(model, backdrop, symbol)
	if known == 0 || share <= 0 {
		return 0
	}
	return 100 / float64(share)
}

// normalise turns a collection name into the API's dashed form. The API accepts
// several spellings; this picks the one that is unambiguous in a URL.
func normalise(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.Join(strings.Fields(s), "-")
	return s
}

// fold is the key form for attribute lookups: case and spacing folded, so
// "Old Gold" and "old gold" are one key.
func fold(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}
