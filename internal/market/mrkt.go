package market

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"golang.org/x/time/rate"
)

const (
	mrktAPI    = "https://api.tgmrkt.io/api/v1"
	mrktOrigin = "https://cdn.tgmrkt.io"
)

// MRKT reads model floors from the MRKT marketplace.
//
// MRKT has no "all floors for a collection" endpoint, so a floor is the
// cheapest listing from a filtered search. That is one request per model, hence
// the per-model cache.
type MRKT struct {
	http tls_client.HttpClient
	fee  float64
	lim  *rate.Limiter

	// initData is a Telegram WebApp payload that can be exchanged for a token
	// whenever the current one expires; token is a ready bearer value. Either
	// is enough, and initData is preferable because it self-renews.
	initData string

	// session, when present, mints a fresh initData on demand by opening the
	// mini app as the real account. It supersedes the pasted value: a scraped
	// payload expires and cannot be renewed without the operator, which is the
	// state that reads to the marketplace as an API being driven directly.
	session InitDataSource

	tokenMu sync.Mutex
	token   string
	static  bool // token came from config and cannot be refreshed

	floors *cache[float64]
	books  *cache[[]float64]
}

// NewMRKT builds the MRKT reader. Supply a Telegram session, a bearer token or
// the WebApp initData; with none of them, the source is disabled.
func NewMRKT(session InitDataSource, initData, token string, fee float64, ttl time.Duration) (*MRKT, error) {
	hc, err := newHTTPClient(15)
	if err != nil {
		return nil, fmt.Errorf("mrkt: %w", err)
	}
	token = strings.TrimSpace(token)
	return &MRKT{
		http:     hc,
		fee:      fee,
		lim:      humanPace(),
		session:  session,
		initData: strings.TrimSpace(initData),
		token:    token,
		// A session can always mint a new payload, so the token is never final.
		static: session == nil && token != "" && strings.TrimSpace(initData) == "",
		floors: newCache[float64](ttl),
		books:  newCache[[]float64](ttl),
	}, nil
}

// Venue implements Source.
func (m *MRKT) Venue() string { return "MRKT" }

// Enabled implements Source.
//
// A session object that has never been logged in cannot mint anything, so it
// does not make the venue configured. Counting it as configured turned MRKT
// into a permanently unreachable venue the moment app credentials were put in
// the environment — and an unreachable venue blocks unattended buying.
func (m *MRKT) Enabled() bool {
	if m == nil {
		return false
	}
	if m.initData != "" || m.token != "" {
		return true
	}
	return m.session != nil && m.session.Ready()
}

// Fee implements Source.
func (m *MRKT) Fee() float64 { return m.fee }

// ModelFloor implements Source.
func (m *MRKT) ModelFloor(ctx context.Context, collection, model string) (float64, error) {
	return m.ModelFloorForAttributes(ctx, collection, model, "", "")
}

func (m *MRKT) ModelFloorForAttributes(ctx context.Context, collection, model, backdrop, symbol string) (float64, error) {
	if !m.Enabled() {
		return 0, errNoCredentials
	}
	key := matchKey(collection) + "|" + matchKey(model) + "|" + matchKey(backdrop) + "|" + matchKey(symbol)
	return m.floors.get(key, func() (float64, error) {
		return m.cheapestAsk(ctx, collection, model, backdrop, symbol)
	})
}

func (m *MRKT) ModelAsks(ctx context.Context, collection, model, backdrop, symbol string, limit int) ([]float64, error) {
	if !m.Enabled() {
		return nil, errNoCredentials
	}
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	key := matchKey(collection) + "|" + matchKey(model) + "|" + matchKey(backdrop) + "|" + matchKey(symbol)
	return m.books.get(key, func() ([]float64, error) {
		return m.askDepth(ctx, collection, model, backdrop, symbol, limit)
	})
}

// cheapestAsk asks for a single listing sorted by price ascending, which is the
// model floor by definition.
func (m *MRKT) cheapestAsk(ctx context.Context, collection, model, backdrop, symbol string) (float64, error) {
	asks, err := m.askDepth(ctx, collection, model, backdrop, symbol, 1)
	if err != nil || len(asks) == 0 {
		return 0, err
	}
	return asks[0], nil
}

func (m *MRKT) askDepth(ctx context.Context, collection, model, backdrop, symbol string, limit int) ([]float64, error) {
	backdrops := []string{}
	if backdrop != "" {
		backdrops = []string{backdrop}
	}
	symbols := []string{}
	if symbol != "" {
		symbols = []string{symbol}
	}
	body := map[string]any{
		"collectionNames": []string{collection},
		"modelNames":      []string{model},
		"backdropNames":   backdrops,
		"symbolNames":     symbols,
		"ordering":        "Price",
		"lowToHigh":       true,
		"maxPrice":        nil,
		"minPrice":        nil,
		"mintable":        nil,
		"number":          nil,
		"count":           limit,
		"cursor":          "",
		"query":           nil,
		"promotedFirst":   false,
	}

	var payload struct {
		Gifts []struct {
			SalePrice int64 `json:"salePrice"`
			// MRKT computes this itself; it is a tighter comparable than the
			// collection floor when it is present.
			FloorByBackdropModel *int64 `json:"floorPriceNanoTONsByBackdropModel"`
		} `json:"gifts"`
	}
	if err := m.post(ctx, "/gifts/saling", body, &payload, true); err != nil {
		return nil, err
	}
	asks := make([]float64, 0, len(payload.Gifts))
	for _, gift := range payload.Gifts {
		if p := nanoToGRAM(gift.SalePrice); p > 0 {
			asks = append(asks, p)
		}
	}
	sort.Float64s(asks)
	return asks, nil
}

// post sends an authenticated JSON request, refreshing the token once on 401.
func (m *MRKT) post(ctx context.Context, path string, body, out any, allowRetry bool) error {
	token, err := m.ensureToken(ctx)
	if err != nil {
		return err
	}
	status, raw, err := m.do(ctx, path, body, token)
	if err != nil {
		return err
	}

	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if !allowRetry || m.static || m.initData == "" {
			return fmt.Errorf("mrkt %s: http %d (session rejected)", path, status)
		}
		m.invalidate(token)
		return m.post(ctx, path, body, out, false)
	}
	if status != http.StatusOK {
		return fmt.Errorf("mrkt %s: http %d", path, status)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mrkt %s: decode: %w", path, err)
	}
	return nil
}

func (m *MRKT) do(ctx context.Context, path string, body any, token string) (int, []byte, error) {
	if err := m.lim.Wait(ctx); err != nil {
		return 0, nil, err
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mrktAPI+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header = http.Header{
		"accept":       {"application/json, text/plain, */*"},
		"content-type": {"application/json"},
		"origin":       {mrktOrigin},
		"referer":      {mrktOrigin + "/"},
		"user-agent":   {userAgent},
	}
	if token != "" {
		req.Header.Set("authorization", token)
		req.Header.Set("cookie", "access_token="+token)
	}

	resp, err := m.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// ensureToken returns a usable bearer token, exchanging initData for one the
// first time it is needed.
func (m *MRKT) ensureToken(ctx context.Context) (string, error) {
	m.tokenMu.Lock()
	if m.token != "" {
		t := m.token
		m.tokenMu.Unlock()
		return t, nil
	}
	m.tokenMu.Unlock()

	initData, err := m.freshInitData(ctx)
	if err != nil {
		return "", err
	}

	status, raw, err := m.do(ctx, "/auth", map[string]any{"data": initData}, "")
	if err != nil {
		return "", fmt.Errorf("mrkt auth: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("mrkt auth: http %d", status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("mrkt auth: decode: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("mrkt auth: response carried no token")
	}

	m.tokenMu.Lock()
	m.token = out.Token
	m.tokenMu.Unlock()
	return out.Token, nil
}

// freshInitData prefers a payload minted right now by the real account, and
// falls back to whatever was pasted into the environment.
func (m *MRKT) freshInitData(ctx context.Context) (string, error) {
	if m.session != nil {
		if data, err := m.session.InitData(ctx, "MRKT"); err == nil && data != "" {
			return data, nil
		} else if err != nil && m.initData == "" {
			return "", fmt.Errorf("mrkt: could not open the mini app: %w", err)
		}
	}
	if m.initData == "" {
		return "", fmt.Errorf("mrkt: no token, no initData and no Telegram session")
	}
	return m.initData, nil
}

// invalidate drops a token that the server rejected, but only if it is still
// the current one — a concurrent request may already have replaced it.
//
// The cached mini-app payload goes with it: if the token it produced is no
// longer accepted, the payload behind it is stale too.
func (m *MRKT) invalidate(stale string) {
	m.tokenMu.Lock()
	if m.token == stale {
		m.token = ""
	}
	m.tokenMu.Unlock()
	if m.session != nil {
		m.session.Invalidate("MRKT")
	}
}
