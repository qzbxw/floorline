package venue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"floorline/internal/tonnel"
)

const (
	mrktAPI    = "https://api.tgmrkt.io/api/v1"
	mrktOrigin = "https://cdn.tgmrkt.io"
)

// MRKT trades on tgmrkt.io.
//
// This is the venue that banned the account for two weeks, for reading its API
// without going through the mini app. Every request here therefore carries a
// session-minted payload and moves at a human pace.
type MRKT struct {
	http    tls_client.HttpClient
	session InitDataSource
	fee     float64
	pace    Pacer

	tokenMu sync.Mutex
	token   string
}

func NewMRKT(hc tls_client.HttpClient, session InitDataSource, fee float64, pace Pacer) *MRKT {
	return &MRKT{http: hc, session: session, fee: fee, pace: pace}
}

func (m *MRKT) Name() string    { return "MRKT" }
func (m *MRKT) Enabled() bool   { return m != nil && m.http != nil && m.session != nil }
func (m *MRKT) BuyFee() float64 { return m.fee }

// Book reads the cheap end of a model's sell queue.
func (m *MRKT) Book(ctx context.Context, key tonnel.ModelKey, backdrop, symbol string, limit int) ([]Listing, error) {
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	backdrops, symbols := []string{}, []string{}
	if backdrop != "" {
		backdrops = []string{backdrop}
	}
	if symbol != "" {
		symbols = []string{symbol}
	}
	body := map[string]any{
		"collectionNames": []string{key.Name},
		"modelNames":      []string{key.Model},
		"backdropNames":   backdrops,
		"symbolNames":     symbols,
		"ordering":        "Price",
		"lowToHigh":       true,
		"count":           limit,
		"cursor":          "",
		"promotedFirst":   false,
	}

	var payload struct {
		Gifts []struct {
			ID        string `json:"id"`
			Number    int64  `json:"number"`
			SalePrice int64  `json:"salePrice"`
			Backdrop  string `json:"backdropName"`
			Symbol    string `json:"symbolName"`
		} `json:"gifts"`
	}
	if err := m.post(ctx, "/gifts/saling", body, &payload, true); err != nil {
		return nil, err
	}

	out := make([]Listing, 0, len(payload.Gifts))
	for _, g := range payload.Gifts {
		price := float64(g.SalePrice) / 1e9
		if price <= 0 {
			continue
		}
		out = append(out, Listing{
			Venue: m.Name(), ID: g.ID, GiftNum: g.Number, Key: key,
			Backdrop: g.Backdrop, Symbol: g.Symbol, Price: price,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Price < out[j].Price })
	return out, nil
}

// Buy purchases one listing.
func (m *MRKT) Buy(ctx context.Context, l Listing, price float64) (*Receipt, error) {
	if l.ID == "" && price == 0 {
		return nil, ErrBuyNotWired // capability probe, see Registry.Buyable
	}
	if err := validate(l, price); err != nil {
		return nil, err
	}
	return m.buyRequest(ctx, l, price)
}

// buyRequest is the only place an MRKT purchase is constructed.
//
// Empty on purpose, for the same reason as the Portals one: the read shapes in
// this file came from real observed responses, the purchase shape has not been
// observed, and a plausible guess at a money-moving call is worse than no call
// at all. It is also the venue with the least patience for traffic that does
// not look like the mini app, so the request has to match what the app really
// sends — including the headers.
//
// To wire it: open MRKT in Telegram Web with DevTools, buy the cheapest lot,
// and copy the POST from the Network tab verbatim.
func (m *MRKT) buyRequest(ctx context.Context, l Listing, price float64) (*Receipt, error) {
	return nil, fmt.Errorf("%w: сними запрос покупки из мини-аппа MRKT и вставь его сюда (internal/venue/mrkt.go)", ErrBuyNotWired)
}

// Owned lists the gifts this account holds on MRKT.
func (m *MRKT) Owned(ctx context.Context) ([]Listing, error) {
	body := map[string]any{"count": 100, "cursor": ""}
	var payload struct {
		Gifts []struct {
			ID        string `json:"id"`
			Number    int64  `json:"number"`
			Name      string `json:"collectionName"`
			Model     string `json:"modelName"`
			SalePrice int64  `json:"salePrice"`
		} `json:"gifts"`
	}
	if err := m.post(ctx, "/gifts/my", body, &payload, true); err != nil {
		return nil, err
	}
	out := make([]Listing, 0, len(payload.Gifts))
	for _, g := range payload.Gifts {
		out = append(out, Listing{
			Venue: m.Name(), ID: g.ID, GiftNum: g.Number,
			Key:   tonnel.ModelKey{Name: g.Name, Model: g.Model},
			Price: float64(g.SalePrice) / 1e9,
		})
	}
	return out, nil
}

// Balance reads the spendable GRAM on this venue.
func (m *MRKT) Balance(ctx context.Context) (float64, error) {
	var payload struct {
		Balance int64 `json:"balance"`
	}
	if err := m.post(ctx, "/me/balance", map[string]any{}, &payload, true); err != nil {
		return 0, err
	}
	return float64(payload.Balance) / 1e9, nil
}

// post sends an authenticated request, minting a new session token once if the
// current one is rejected.
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
		if !allowRetry {
			return fmt.Errorf("mrkt %s: http %d (сессия отклонена)", path, status)
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
	if err := m.pace.Wait(ctx); err != nil {
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
		"user-agent":   {UserAgent},
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

// ensureToken exchanges a freshly minted mini-app payload for a bearer token.
func (m *MRKT) ensureToken(ctx context.Context) (string, error) {
	m.tokenMu.Lock()
	if m.token != "" {
		t := m.token
		m.tokenMu.Unlock()
		return t, nil
	}
	m.tokenMu.Unlock()

	if m.session == nil {
		return "", fmt.Errorf("mrkt: нет Telegram-сессии — выполни floorline login")
	}
	data, err := m.session.InitData(ctx, m.Name())
	if err != nil {
		return "", fmt.Errorf("mrkt: не открыл мини-апп: %w", err)
	}

	status, raw, err := m.do(ctx, "/auth", map[string]any{"data": data}, "")
	if err != nil {
		return "", fmt.Errorf("mrkt auth: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("mrkt auth: http %d", status)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("mrkt auth: decode: %w", err)
	}
	if payload.Token == "" {
		return "", fmt.Errorf("mrkt auth: в ответе нет токена")
	}

	m.tokenMu.Lock()
	m.token = payload.Token
	m.tokenMu.Unlock()
	return payload.Token, nil
}

// invalidate drops a rejected token and the payload that produced it.
func (m *MRKT) invalidate(stale string) {
	m.tokenMu.Lock()
	if m.token == stale {
		m.token = ""
	}
	m.tokenMu.Unlock()
	if m.session != nil {
		m.session.Invalidate(m.Name())
	}
}
