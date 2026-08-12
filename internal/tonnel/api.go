package tonnel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Sort orders accepted by pageGifts. These are literal JSON strings, not objects.
const (
	SortPriceAsc  = `{"price":1,"gift_id":-1}`
	SortPriceDesc = `{"price":-1,"gift_id":-1}`
	SortLatest    = `{"message_post_time":-1,"gift_id":-1}` // the live listing feed
	SortRarity    = `{"rarity":-1,"gift_id":-1}`
	SortGiftNumUp = `{"gift_num":1,"gift_id":-1}`

	sortSalesLatest = `{"timestamp":-1,"gift_id":-1}`
)

// AssetGRAM is the native Gram currency. Tonnel has not migrated its private
// wire value yet, so requests must still send the legacy literal "TON".
const AssetGRAM = "TON"

// AssetTON is kept for source compatibility; new code should use AssetGRAM.
const AssetTON = AssetGRAM

// maxPageLimit is enforced server-side; larger values return an error.
const maxPageLimit = 30

// Filter is a MongoDB-style query object. Tonnel passes it straight to its
// database, which is why attribute matching needs regexes and exact casing.
type Filter map[string]any

// BaseFilter is the mandatory predicate for "things that are actually for sale".
// Without it the endpoint also returns sold, refunded and un-exported rows.
func BaseFilter() Filter {
	return Filter{
		"price":     Filter{"$exists": true},
		"refunded":  Filter{"$ne": true},
		"buyer":     Filter{"$exists": false},
		"export_at": Filter{"$exists": true},
		"asset":     AssetGRAM,
	}
}

// WithCollection pins the filter to one collection. Casing must match Tonnel's.
func (f Filter) WithCollection(name string) Filter {
	f["gift_name"] = TitleCase(name)
	return f
}

// WithAttr pins an attribute (model/backdrop/symbol). Values carry a rarity
// suffix like "Dark Soul (0.4%)", so a prefix regex is the only way to match on
// the human-readable part alone.
func (f Filter) WithAttr(field, value string) Filter {
	value = strings.TrimSpace(value)
	if value == "" {
		return f
	}
	// A value that already ends in a rarity suffix is a complete stored value
	// and matches exactly. Testing for a bare "(" would misfire on model names
	// that legitimately contain parentheses.
	if attrRe.MatchString(value) {
		f[field] = TitleCase(value)
		return f
	}
	f[field] = Filter{"$regex": "^" + regexEscape(TitleCase(value)) + ` \(`}
	return f
}

// regexEscape quotes the characters that appear in real attribute names and
// would otherwise be interpreted by the server-side regex engine.
func regexEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '^', '$', '|':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// JSON renders the filter as the string the endpoint expects.
func (f Filter) JSON() string { return f.json() }

func (f Filter) json() string {
	b, err := json.Marshal(f)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Raw performs a bare POST and decodes into out. It exists for the dump
// command, which needs to see fields our typed structs do not know about.
func (c *Client) Raw(ctx context.Context, host, path string, body, out any) error {
	return c.call(ctx, callOpts{host: host, path: path, body: body, out: out, retries: 1})
}

// PageQuery describes one order-book page request.
type PageQuery struct {
	Filter Filter
	Sort   string
	Page   int
	Limit  int
}

// PageGifts fetches one page of the order book.
func (c *Client) PageGifts(ctx context.Context, q PageQuery) ([]Gift, error) {
	if q.Limit <= 0 || q.Limit > maxPageLimit {
		q.Limit = maxPageLimit
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.Sort == "" {
		q.Sort = SortLatest
	}
	if q.Filter == nil {
		q.Filter = BaseFilter()
	}

	body := map[string]any{
		"filter":      q.Filter.json(),
		"limit":       q.Limit,
		"page":        q.Page,
		"sort":        q.Sort,
		"price_range": nil,
		"ref":         0,
		"user_auth":   c.Auth(),
	}

	var out []Gift
	err := c.call(ctx, callOpts{host: HostRead, path: "/api/pageGifts", body: body, out: &out, retries: 2})
	return out, err
}

// Feed returns the newest listings, most recent first. This is the primary
// signal source: it surfaces every new ask across the whole marketplace.
func (c *Client) Feed(ctx context.Context, limit int) ([]Gift, error) {
	return c.PageGifts(ctx, PageQuery{Filter: BaseFilter(), Sort: SortLatest, Limit: limit})
}

// ModelBook returns the cheapest asks for one (collection, model) pair, which is
// what the valuation engine needs to know the real exit price.
func (c *Client) ModelBook(ctx context.Context, key ModelKey, limit int) ([]Gift, error) {
	f := BaseFilter().WithCollection(key.Name)
	f.WithAttr("model", key.Model)
	if limit <= 0 || limit > maxPageLimit {
		limit = maxPageLimit
	}

	// Tonnel rejects a seller $ne predicate on pageGifts, so exclusion has to
	// happen client-side. Keep paging until we have `limit` external asks; this
	// prevents our inventory from occupying the cheap-page budget.
	uid := c.UserID()
	out := make([]Gift, 0, limit)
	for page := 1; page <= 4 && len(out) < limit; page++ {
		rows, err := c.PageGifts(ctx, PageQuery{Filter: f, Sort: SortPriceAsc, Page: page, Limit: maxPageLimit})
		if err != nil {
			return nil, err
		}
		for i := range rows {
			if uid != 0 && rows[i].Seller.Int() == uid {
				continue
			}
			out = append(out, rows[i])
			if len(out) == limit {
				break
			}
		}
		if len(rows) < maxPageLimit {
			break
		}
	}
	return out, nil
}

// CollectionBook returns the cheapest asks across a whole collection.
func (c *Client) CollectionBook(ctx context.Context, name string, limit int) ([]Gift, error) {
	return c.PageGifts(ctx, PageQuery{
		Filter: BaseFilter().WithCollection(name),
		Sort:   SortPriceAsc,
		Limit:  limit,
	})
}

// FilterStats returns floor, supply and rarity for every model of every
// collection in a single call. It is the cheapest full-market snapshot Tonnel
// offers and the backbone of the pricing engine.
func (c *Client) FilterStats(ctx context.Context) ([]ModelStat, error) {
	body := map[string]any{"authData": c.Auth()}

	var raw map[string]struct {
		FloorPrice *float64 `json:"floorPrice"`
		HowMany    int      `json:"howMany"`
	}
	err := c.call(ctx, callOpts{host: HostRead, path: "/api/filterStats", body: body, out: &raw, retries: 2})
	if err != nil {
		return nil, err
	}

	out := make([]ModelStat, 0, len(raw))
	for key, v := range raw {
		// Keys look like "Toy Bear_Wizard (1.5%)".
		name, modelPart, ok := strings.Cut(key, "_")
		if !ok {
			continue
		}
		model, rarity := SplitAttr(modelPart)
		if name == "" || model == "" {
			continue
		}
		st := ModelStat{
			Key:    ModelKey{Name: strings.TrimSpace(name), Model: model},
			Supply: v.HowMany,
			Rarity: rarity,
		}
		if v.FloorPrice != nil {
			st.Floor = *v.FloorPrice
		}
		out = append(out, st)
	}
	return out, nil
}

// SaleHistoryQuery describes a trade-history request.
type SaleHistoryQuery struct {
	Page  int
	Limit int
	Type  string // ALL | SALE | INTERNAL_SALE | BID
	Name  string
	Model string
	// Since restricts the result to trades at or after this time. This is the
	// only reliable way to get *recent* history: the endpoint's ordering cannot
	// be depended on, but the filter is passed straight to its database, so a
	// range predicate is exact. An empty result then genuinely means "nothing
	// traded in this window" rather than "the sort put it on another page".
	Since time.Time
}

// SaleHistory returns real executed trades. Unlike the order book, this is
// evidence of what people actually pay, which is what liquidity is measured on.
//
// Note the payload shape differs from pageGifts: here `filter` and `sort` are
// real JSON objects, not strings.
func (c *Client) SaleHistory(ctx context.Context, q SaleHistoryQuery) ([]Sale, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.Type == "" {
		q.Type = "SALE"
	}

	filter := Filter{}
	if q.Name != "" {
		filter["gift_name"] = TitleCase(q.Name)
	}
	if q.Model != "" {
		filter.WithAttr("model", q.Model)
	}
	if !q.Since.IsZero() {
		filter["timestamp"] = Filter{"$gte": q.Since.UTC().Format("2006-01-02T15:04:05.000Z")}
	}

	var sortObj map[string]any
	_ = json.Unmarshal([]byte(sortSalesLatest), &sortObj)

	body := map[string]any{
		"authData": c.Auth(),
		"page":     q.Page,
		"limit":    q.Limit,
		"type":     q.Type,
		"filter":   filter,
		"sort":     sortObj,
	}

	var out []Sale
	err := c.call(ctx, callOpts{host: HostRead, path: "/api/saleHistory", body: body, out: &out, retries: 2})
	return out, err
}

// SettledTypes are the trade types that represent a completed purchase at a
// real price.
//
// BID is deliberately absent: those rows are auction bids, and a single auction
// produces a whole escalating ladder of them for one gift. Counting them would
// inflate velocity several-fold on exactly the collections that run auctions.
// INTERNAL_SALE, on the other hand, is a genuine purchase that simply settles
// inside Tonnel — on many collections it is nearly all of the volume, and
// ignoring it makes a busy market look dead.
var SettledTypes = []string{"SALE", "INTERNAL_SALE"}

// TradeHistory fetches every settled trade for a collection in a time window,
// paging through each type until the window is exhausted.
//
// maxPages bounds the work per type; a collection busier than that is already
// far past any liquidity threshold, so the exact count stops mattering.
func (c *Client) TradeHistory(ctx context.Context, name string, since time.Time, maxPages int) ([]Sale, error) {
	if maxPages <= 0 {
		maxPages = 20
	}
	const pageSize = 50

	var out []Sale
	for _, kind := range SettledTypes {
		for page := 1; page <= maxPages; page++ {
			batch, err := c.SaleHistory(ctx, SaleHistoryQuery{
				Page: page, Limit: pageSize, Type: kind, Name: name, Since: since,
			})
			if err != nil {
				return out, err
			}
			out = append(out, batch...)
			if len(batch) < pageSize {
				break
			}
		}
	}
	return out, nil
}

// GiftData fetches a single listing by its Tonnel gift id.
func (c *Client) GiftData(ctx context.Context, giftID int64) (*Gift, error) {
	body := map[string]any{"authData": c.Auth()}
	var g Gift
	err := c.call(ctx, callOpts{
		host: HostRead, path: fmt.Sprintf("/api/giftData/%d", giftID),
		body: body, out: &g, retries: 1,
	})
	if err != nil {
		return nil, err
	}
	// This endpoint answers without a gift_id field: the id is in the URL, so
	// the response is about that gift by construction. Callers cannot know that,
	// and a zero id silently broke them — the book stopped excluding the very
	// listing being valued, and the executor rejected every purchase because the
	// id "did not match". Stamp it here so a Gift is always fully identified,
	// whichever endpoint produced it.
	if g.GiftID.Int() == 0 {
		g.GiftID = FlexInt(giftID)
	}
	return &g, nil
}

// MyGifts returns the caller's own gifts, either currently listed or idle.
func (c *Client) MyGifts(ctx context.Context, listed bool, page, limit int) ([]Gift, error) {
	uid := c.UserID()
	if uid == 0 {
		return nil, fmt.Errorf("cannot read inventory: authData has no usable user id")
	}
	if limit <= 0 || limit > maxPageLimit {
		limit = maxPageLimit
	}
	if page <= 0 {
		page = 1
	}

	var f Filter
	sort := SortGiftNumUp
	if listed {
		sort = SortLatest
		f = Filter{
			"seller": uid,
			"buyer":  Filter{"$exists": false},
			"$or": []any{
				Filter{"price": Filter{"$exists": true}},
				Filter{"auction_id": Filter{"$exists": true}},
			},
		}
	} else {
		f = Filter{
			"seller":   uid,
			"buyer":    Filter{"$exists": false},
			"refunded": Filter{"$ne": true},
			"price":    Filter{"$exists": false},
		}
	}

	return c.PageGifts(ctx, PageQuery{Filter: f, Sort: sort, Page: page, Limit: limit})
}

// Balance reads the account balances.
func (c *Client) Balance(ctx context.Context) (*Balance, error) {
	body := map[string]any{"authData": c.Auth()}
	var raw map[string]any
	if err := c.call(ctx, callOpts{host: HostRead, path: "/api/balance/info", body: body, out: &raw, retries: 1}); err != nil {
		return nil, err
	}
	b := &Balance{Raw: raw}
	b.GRAM = pickFloat(raw, "balance", "gram", "GRAM", "gramBalance", "ton", "TON", "tonBalance")
	b.TON = b.GRAM
	b.USDT = pickFloat(raw, "usdt", "USDT", "usdtBalance")
	b.Tonnel = pickFloat(raw, "tonnel", "TONNEL", "tonnelBalance")
	return b, nil
}

// pickFloat digs a numeric value out of an untyped map, trying several key
// spellings and descending one level into nested objects.
func pickFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		if f, ok := toFloat(v); ok {
			return f
		}
		if nested, ok := v.(map[string]any); ok {
			if f := pickFloat(nested, "amount", "value", "balance"); f != 0 {
				return f
			}
		}
	}
	return 0
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Result is the response of a write operation.
type Result struct {
	Status  string         `json:"status"`
	Message string         `json:"message"`
	Raw     map[string]any `json:"-"`
}

// OK reports whether the operation succeeded.
func (r *Result) OK() bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Status)) {
	case "success", "ok":
		return true
	}
	if ok, exists := r.Raw["ok"].(bool); exists {
		return ok
	}
	if ok, exists := r.Raw["success"].(bool); exists {
		return ok
	}
	return false // an ambiguous write response is never proof of ownership
}

// Rejected reports an explicit business rejection. Anything else that is not
// OK is ambiguous and must be reconciled against inventory after a money write.
func (r *Result) Rejected() bool {
	if r == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Status), "error") || strings.EqualFold(strings.TrimSpace(r.Status), "failed") {
		return true
	}
	if ok, exists := r.Raw["ok"].(bool); exists {
		return !ok
	}
	if ok, exists := r.Raw["success"].(bool); exists {
		return !ok
	}
	return false
}

// BuyGift purchases a listing at an exact price.
//
// The price is validated server-side: if the listing moved or was already taken,
// the call is rejected rather than filled at a different price. That check is why
// there is no client-side re-read before buying — an extra round trip would only
// lose the race without adding safety.
//
// There is deliberately no retry. On an ambiguous failure the caller must
// reconcile against MyGifts instead of firing a second purchase.
func (c *Client) BuyGift(ctx context.Context, giftID int64, price float64) (*Result, error) {
	ts, wtf, err := Sign(time.Now())
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"authData":  c.Auth(),
		"asset":     AssetGRAM,
		"price":     price,
		"timestamp": ts,
		"wtf":       wtf,
	}
	var raw map[string]any
	err = c.call(ctx, callOpts{
		host: HostWrite, path: fmt.Sprintf("/api/buyGift/%d", giftID),
		body: body, out: &raw, write: true, retries: 0,
	})
	return resultFrom(raw), err
}

// ListForSale puts an owned gift on the market.
func (c *Client) ListForSale(ctx context.Context, giftID int64, price float64) (*Result, error) {
	ts, wtf, err := Sign(time.Now())
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"authData":  c.Auth(),
		"gift_id":   giftID,
		"price":     price,
		"asset":     AssetGRAM,
		"timestamp": ts,
		"wtf":       wtf,
	}
	var raw map[string]any
	err = c.call(ctx, callOpts{
		host: HostWrite, path: "/api/listForSale",
		body: body, out: &raw, write: true, retries: 1,
	})
	return resultFrom(raw), err
}

// CancelSale withdraws one of the caller's listings.
func (c *Client) CancelSale(ctx context.Context, giftID int64) (*Result, error) {
	ts, wtf, err := Sign(time.Now())
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"authData":  c.Auth(),
		"gift_id":   giftID,
		"timestamp": ts,
		"wtf":       wtf,
	}
	var raw map[string]any
	err = c.call(ctx, callOpts{
		host: HostWrite, path: "/api/cancelSale",
		body: body, out: &raw, write: true, retries: 1,
	})
	return resultFrom(raw), err
}

func resultFrom(raw map[string]any) *Result {
	if raw == nil {
		return nil
	}
	r := &Result{Raw: raw}
	if s, ok := raw["status"].(string); ok {
		r.Status = s
	}
	if m, ok := raw["message"].(string); ok {
		r.Message = m
	}
	return r
}
