package venue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"floorline/internal/tonnel"
)

const portalsAPI = "https://portals.tg/api"

// Portals trades on portals.tg.
//
// Reads work today. The purchase call does not, and the reason is written out
// in buyRequest below.
type Portals struct {
	http    tls_client.HttpClient
	session InitDataSource
	fee     float64
	pace    Pacer
}

func NewPortals(hc tls_client.HttpClient, session InitDataSource, fee float64, pace Pacer) *Portals {
	return &Portals{http: hc, session: session, fee: fee, pace: pace}
}

func (p *Portals) Name() string    { return "Portals" }
func (p *Portals) Enabled() bool   { return p != nil && p.http != nil }
func (p *Portals) BuyFee() float64 { return p.fee }

// Book reads the cheap end of a model's sell queue.
func (p *Portals) Book(ctx context.Context, key tonnel.ModelKey, backdrop, symbol string, limit int) ([]Listing, error) {
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	if err := p.pace.Wait(ctx); err != nil {
		return nil, err
	}
	v := url.Values{
		"offset":                {"0"},
		"limit":                 {strconv.Itoa(limit)},
		"sort_by":               {"price asc"},
		"filter_by_collections": {key.Name},
		"filter_by_models":      {key.Model},
		"status":                {"listed"},
	}
	if backdrop != "" {
		v.Set("filter_by_backdrops", backdrop)
	}
	if symbol != "" {
		v.Set("filter_by_symbols", symbol)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, portalsAPI+"/nfts/search?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	p.headers(ctx, req)

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("portals listings: http %d", resp.StatusCode)
	}

	var payload struct {
		Results []struct {
			ID     string `json:"id"`
			Price  string `json:"price"`
			Number int64  `json:"external_collection_number"`
			Name   string `json:"name"`
			Attrs  []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"attributes"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("portals listings: decode: %w", err)
	}

	out := make([]Listing, 0, len(payload.Results))
	for _, row := range payload.Results {
		price, err := strconv.ParseFloat(row.Price, 64)
		if err != nil || price <= 0 {
			continue
		}
		l := Listing{
			Venue: p.Name(), ID: row.ID, GiftNum: row.Number,
			Key: key, Price: price,
		}
		for _, a := range row.Attrs {
			switch a.Type {
			case "backdrop":
				l.Backdrop = a.Value
			case "symbol":
				l.Symbol = a.Value
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Price < out[j].Price })
	return out, nil
}

// Buy purchases one listing.
func (p *Portals) Buy(ctx context.Context, l Listing, price float64) (*Receipt, error) {
	if l.ID == "" && price == 0 {
		return nil, ErrBuyNotWired // capability probe, see Registry.Buyable
	}
	if err := validate(l, price); err != nil {
		return nil, err
	}
	return p.buyRequest(ctx, l, price)
}

// buyRequest is the only place a Portals purchase is constructed.
//
// It is empty on purpose. The read endpoints above were established by
// observing real responses; the purchase endpoint has not been observed, and
// its path, body and signing are not documented anywhere trustworthy. Writing a
// plausible-looking request here would produce code that compiles, passes
// review, and does something unknown with real money the first time it runs —
// possibly buying at a price the desk did not agree to.
//
// To wire it: open the Portals mini app with DevTools, buy the cheapest lot on
// the market, and copy the request from the Network tab — method, path, body
// and every header that is not obviously a browser default. Everything else in
// this file is ready for it.
func (p *Portals) buyRequest(ctx context.Context, l Listing, price float64) (*Receipt, error) {
	return nil, fmt.Errorf("%w: сними запрос покупки из мини-аппа Portals и вставь его сюда (internal/venue/portals.go)", ErrBuyNotWired)
}

// Owned lists the gifts this account holds on Portals.
func (p *Portals) Owned(ctx context.Context) ([]Listing, error) {
	if err := p.pace.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		portalsAPI+"/nfts/owned?offset=0&limit=100", nil)
	if err != nil {
		return nil, err
	}
	p.headers(ctx, req)

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("portals owned: http %d", resp.StatusCode)
	}

	var payload struct {
		Results []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Number int64  `json:"external_collection_number"`
			Price  string `json:"price"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("portals owned: decode: %w", err)
	}
	out := make([]Listing, 0, len(payload.Results))
	for _, row := range payload.Results {
		price, _ := strconv.ParseFloat(row.Price, 64)
		out = append(out, Listing{
			Venue: p.Name(), ID: row.ID, GiftNum: row.Number,
			Key: tonnel.ModelKey{Name: row.Name}, Price: price,
		})
	}
	return out, nil
}

// Balance reads the spendable GRAM on this venue.
func (p *Portals) Balance(ctx context.Context) (float64, error) {
	if err := p.pace.Wait(ctx); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, portalsAPI+"/users/balance", nil)
	if err != nil {
		return 0, err
	}
	p.headers(ctx, req)

	resp, err := p.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("portals balance: http %d", resp.StatusCode)
	}
	var payload struct {
		Balance string `json:"balance"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return 0, fmt.Errorf("portals balance: decode: %w", err)
	}
	return strconv.ParseFloat(strings.TrimSpace(payload.Balance), 64)
}

func (p *Portals) headers(ctx context.Context, req *http.Request) {
	req.Header = http.Header{
		"accept":     {"application/json"},
		"origin":     {"https://portals.tg"},
		"referer":    {"https://portals.tg/"},
		"user-agent": {UserAgent},
	}
	if p.session != nil {
		if data, err := p.session.InitData(ctx, p.Name()); err == nil && data != "" {
			req.Header.Set("authorization", "tma "+data)
		}
	}
}
