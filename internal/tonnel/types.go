package tonnel

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// These endpoints are private and undocumented, so every numeric field is decoded
// leniently: the backend has been observed to return the same value as a JSON
// number in one response and as a string in another. Flex* types absorb that
// instead of failing the whole page.

// Flex64 decodes from a JSON number, a numeric string, or null.
type Flex64 float64

func (f *Flex64) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil // tolerate garbage rather than dropping the record
	}
	*f = Flex64(v)
	return nil
}

// Float returns the value as a plain float64.
func (f Flex64) Float() float64 { return float64(f) }

// FlexInt decodes from a JSON number, a numeric string, or null.
type FlexInt int64

func (f *FlexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = FlexInt(v)
		return nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		*f = FlexInt(int64(v))
	}
	return nil
}

// Int returns the value as a plain int64.
func (f FlexInt) Int() int64 { return int64(f) }

// FlexBool decodes from a JSON bool, "true"/"false", or 0/1.
type FlexBool bool

func (f *FlexBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	switch s {
	case "true", "1":
		*f = true
	default:
		*f = false
	}
	return nil
}

// Bool returns the value as a plain bool.
func (f FlexBool) Bool() bool { return bool(f) }

// FlexTime decodes an ISO-8601 string, a unix seconds number, or a unix
// milliseconds number into a time.Time.
type FlexTime struct{ time.Time }

func (t *FlexTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" || s == `""` {
		return nil
	}
	if s[0] == '"' {
		raw := strings.Trim(s, `"`)
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
			if v, err := time.Parse(layout, raw); err == nil {
				t.Time = v.UTC()
				return nil
			}
		}
		// Some fields arrive as a numeric string.
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			t.Time = unixGuess(n)
		}
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		t.Time = unixGuess(n)
	}
	return nil
}

// unixGuess distinguishes seconds from milliseconds by magnitude.
func unixGuess(n int64) time.Time {
	if n > 1e12 {
		return time.UnixMilli(n).UTC()
	}
	return time.Unix(n, 0).UTC()
}

// Gift is one row of the Tonnel order book.
type Gift struct {
	GiftID   FlexInt `json:"gift_id"`
	GiftNum  FlexInt `json:"gift_num"`
	Name     string  `json:"name"`
	Model    string  `json:"model"`
	Backdrop string  `json:"backdrop"`
	Symbol   string  `json:"symbol"`

	Price Flex64 `json:"price"`
	Asset string `json:"asset"`

	Rarity         Flex64 `json:"rarity"`
	ModelRarity    Flex64 `json:"modelRarity"`
	BackdropRarity Flex64 `json:"backdropRarity"`
	SymbolRarity   Flex64 `json:"symbolRarity"`

	Seller FlexInt `json:"seller"`
	Buyer  *int64  `json:"buyer"`
	Status string  `json:"status"`

	Refunded            FlexBool `json:"refunded"`
	Premarket           FlexBool `json:"premarket"`
	TelegramMarketplace FlexBool `json:"telegramMarketplace"`

	ExportAt        FlexTime        `json:"export_at"`
	MessagePostTime FlexTime        `json:"message_post_time"`
	AuctionID       *string         `json:"auction_id"`
	BundleData      json.RawMessage `json:"bundleData"`
}

// IsBundle reports whether the row is a multi-gift bundle. Tonnel encodes those
// with a negative gift_id, and their price covers the whole pack — including them
// in floor or valuation math produces nonsense.
func (g *Gift) IsBundle() bool {
	return g.GiftID.Int() < 0 || len(g.BundleData) > 4
}

// Key returns the canonical (collection, model) identifier used everywhere else.
func (g *Gift) Key() ModelKey {
	return ModelKey{Name: g.Name, Model: BaseAttr(g.Model)}
}

// Sale is one row of the real trade history.
type Sale struct {
	GiftID   FlexInt `json:"gift_id"`
	GiftNum  FlexInt `json:"gift_num"`
	Name     string  `json:"name"`
	Model    string  `json:"model"`
	Backdrop string  `json:"backdrop"`
	Symbol   string  `json:"symbol"`
	Price    Flex64  `json:"price"`
	Asset    string  `json:"asset"`
	Seller   FlexInt `json:"seller"`
	Buyer    FlexInt `json:"buyer"`
	Type     string  `json:"type"`

	Timestamp FlexTime `json:"timestamp"`
	SoldAt    FlexTime `json:"sold_at"`
	UpdatedAt FlexTime `json:"updatedAt"`
}

// When returns the best available timestamp for the trade.
func (s *Sale) When() time.Time {
	for _, t := range []FlexTime{s.Timestamp, s.SoldAt, s.UpdatedAt} {
		if !t.IsZero() {
			return t.Time
		}
	}
	return time.Time{}
}

// Key returns the canonical (collection, model) identifier.
func (s *Sale) Key() ModelKey {
	return ModelKey{Name: s.Name, Model: BaseAttr(s.Model)}
}

// ModelKey identifies the actual tradable unit. Collection floor is noise —
// a Plush Pepe with a 0.4% model and one with a 15% model are different assets.
type ModelKey struct {
	Name  string
	Model string
}

func (k ModelKey) String() string { return k.Name + " · " + k.Model }

// ID is the stable string form used as a database key and mute scope.
func (k ModelKey) ID() string { return k.Name + "|" + k.Model }

// ModelStat is one entry of the filterStats response: floor, supply and rarity
// for a single (collection, model) pair. One call returns the whole market.
type ModelStat struct {
	Key    ModelKey
	Floor  float64
	Supply int
	Rarity float64
}

// Balance is the account state from /api/balance/info.
type Balance struct {
	TON    float64
	USDT   float64
	Tonnel float64
	Raw    map[string]any
}

// attrRe strips the rarity suffix Tonnel appends to every attribute value,
// e.g. "Dark Soul (0.4%)" -> "Dark Soul" + 0.4.
var attrRe = regexp.MustCompile(`^(.*?)\s*\(([0-9.]+)%\)\s*$`)

// SplitAttr separates an attribute into its display name and rarity percentage.
func SplitAttr(s string) (name string, rarity float64) {
	s = strings.TrimSpace(s)
	m := attrRe.FindStringSubmatch(s)
	if m == nil {
		return s, 0
	}
	r, _ := strconv.ParseFloat(m[2], 64)
	return strings.TrimSpace(m[1]), r
}

// BaseAttr returns the attribute name without its rarity suffix.
func BaseAttr(s string) string {
	n, _ := SplitAttr(s)
	return n
}

// TitleCase matches Tonnel's capitalisation rules for filter values: every word
// gets an uppercase first letter, the rest is left alone. Queries silently return
// nothing if the case is wrong.
func TitleCase(s string) string {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "jack-in-the-box") {
		return "Jack-in-the-Box" // the one name that does not follow the rule
	}
	var b strings.Builder
	upNext := true
	for _, r := range s {
		if upNext && unicode.IsLetter(r) {
			b.WriteRune(unicode.ToUpper(r))
			upNext = false
			continue
		}
		if unicode.IsSpace(r) {
			upNext = true
		}
		b.WriteRune(r)
	}
	return b.String()
}

// APIError is returned when the endpoint answers with a non-2xx status or an
// error envelope. Status is preserved so callers can distinguish a Cloudflare
// block (403/429) from a genuine rejection.
type APIError struct {
	Op      string
	Status  int
	Message string
	Body    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("tonnel %s: http %d: %s", e.Op, e.Status, e.Message)
	}
	return fmt.Sprintf("tonnel %s: http %d: %s", e.Op, e.Status, truncate(e.Body, 300))
}

// IsBlocked reports whether the failure looks like an anti-bot block rather than
// a business-logic rejection. Callers back off hard on this.
func (e *APIError) IsBlocked() bool { return e.Status == 403 || e.Status == 429 || e.Status == 503 }

// IsAuth reports whether the failure looks like an expired or invalid authData.
func (e *APIError) IsAuth() bool {
	if e.Status == 401 {
		return true
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "auth") || strings.Contains(m, "unauthorized")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
