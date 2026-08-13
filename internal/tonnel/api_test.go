package tonnel

import (
	"encoding/json"
	"strings"
	"testing"
)

// Attribute values carry a rarity suffix, so filters must match on a prefix
// regex. Getting this wrong returns an empty page rather than an error, which
// would silently look like "this model has no listings".
func TestFilterAttrUsesPrefixRegex(t *testing.T) {
	f := BaseFilter().WithCollection("plush pepe")
	f.WithAttr("model", "pink diamond")

	raw := f.json()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("filter is not valid JSON: %v (%s)", err, raw)
	}

	if decoded["gift_name"] != "Plush Pepe" {
		t.Errorf("gift_name = %v, want Plush Pepe", decoded["gift_name"])
	}

	model, ok := decoded["model"].(map[string]any)
	if !ok {
		t.Fatalf("model filter is not an object: %v", decoded["model"])
	}
	if got, want := model["$regex"], `^Pink Diamond \(`; got != want {
		t.Errorf("model regex = %v, want %v", got, want)
	}
}

func TestFilterAttrPassesThroughExactValues(t *testing.T) {
	f := Filter{}
	f.WithAttr("model", "Pink Diamond (0.4%)")
	if got := f["model"]; got != "Pink Diamond (0.4%)" {
		t.Errorf("a value that already carries a rarity suffix must be used verbatim, got %v", got)
	}
}

func TestFilterAttrEscapesRegexMetacharacters(t *testing.T) {
	f := Filter{}
	f.WithAttr("model", "B-day (Special)")
	m := f["model"].(Filter)
	got := m["$regex"].(string)
	if strings.Contains(got, `(Special)`) {
		t.Errorf("unescaped parentheses would change the match: %q", got)
	}
	if !strings.Contains(got, `\(Special\)`) {
		t.Errorf("regex = %q, want escaped parentheses", got)
	}
}

func TestBaseFilterExcludesSoldAndRefunded(t *testing.T) {
	raw := BaseFilter().json()
	for _, want := range []string{`"buyer"`, `"refunded"`, `"export_at"`, `"asset":"TON"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("base filter is missing %s: %s", want, raw)
		}
	}
}

// decodeInto has to cope with both shapes the API uses: a bare array, and an
// envelope with the payload under "data".
func TestDecodeIntoHandlesBothShapes(t *testing.T) {
	var bare []Gift
	if err := decodeInto("t", 200, []byte(`[{"gift_id":1,"name":"A"}]`), &bare); err != nil {
		t.Fatalf("bare array: %v", err)
	}
	if len(bare) != 1 || bare[0].GiftID.Int() != 1 {
		t.Errorf("bare array decoded to %+v", bare)
	}

	var wrapped []Gift
	if err := decodeInto("t", 200, []byte(`{"status":"success","data":[{"gift_id":2,"name":"B"}]}`), &wrapped); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if len(wrapped) != 1 || wrapped[0].GiftID.Int() != 2 {
		t.Errorf("envelope decoded to %+v", wrapped)
	}
}

func TestDecodeIntoSurfacesErrorEnvelope(t *testing.T) {
	var out []Gift
	err := decodeInto("t", 200, []byte(`{"status":"error","message":"gift not found"}`), &out)
	if err == nil {
		t.Fatal("an error envelope with HTTP 200 must still be an error")
	}
	var ae *APIError
	if !asAPIError(err, &ae) || ae.Message != "gift not found" {
		t.Errorf("error = %v, want an APIError carrying the message", err)
	}
}

func TestWriteResultRequiresExplicitSuccess(t *testing.T) {
	for name, r := range map[string]*Result{
		"nil":          nil,
		"empty":        {},
		"message only": {Message: "done"},
		"error":        {Status: "error"},
	} {
		if r.OK() {
			t.Errorf("%s result was accepted as a successful money write", name)
		}
	}
	for name, r := range map[string]*Result{
		"status":   {Status: "success"},
		"raw ok":   {Raw: map[string]any{"ok": true}},
		"raw flag": {Raw: map[string]any{"success": true}},
	} {
		if !r.OK() {
			t.Errorf("%s result was not accepted", name)
		}
	}
}

func TestWriteResultDistinguishesRejectionFromAmbiguity(t *testing.T) {
	if !(&Result{Status: "error"}).Rejected() {
		t.Fatal("explicit error was not classified as rejection")
	}
	if (&Result{}).Rejected() || (*Result)(nil).Rejected() {
		t.Fatal("empty response was classified as a definite rejection")
	}
}

func TestUserIDFromAuth(t *testing.T) {
	auth := `query_id=AAA&user=%7B%22id%22%3A6083232778%2C%22first_name%22%3A%22A%22%7D&auth_date=1700000000&hash=abc`
	id, err := userIDFromAuth(auth)
	if err != nil {
		t.Fatalf("userIDFromAuth: %v", err)
	}
	if id != 6083232778 {
		t.Errorf("id = %d, want 6083232778", id)
	}

	if _, err := userIDFromAuth(""); err == nil {
		t.Error("empty authData must be rejected")
	}
	if _, err := userIDFromAuth("query_id=AAA&hash=abc"); err == nil {
		t.Error("authData with no user field must be rejected")
	}
}

func asAPIError(err error, target **APIError) bool {
	ae, ok := err.(*APIError)
	if ok {
		*target = ae
	}
	return ok
}

// Tonnel throttles with an HTML page carrying a meta-refresh, and 200 bytes of
// that markup was being quoted into Telegram — inside position cards, inside
// portfolio advice, inside every block notification. The one fact worth keeping
// is where it wants us to go.
func TestHTMLThrottlePageIsSummarisedNotQuoted(t *testing.T) {
	page := []byte(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta http-equiv="Refresh" content="3; url='https://rs-market.tonnel.network'" />
    <title>Redirecting...</title>
</head>
<body>go away</body></html>`)

	got := extractMessage(page)
	if strings.Contains(got, "<") {
		t.Errorf("markup leaked into the message: %q", got)
	}
	if !strings.Contains(got, "rs-market.tonnel.network") {
		t.Errorf("message = %q, want the redirect target kept", got)
	}
	if len(got) > 80 {
		t.Errorf("message is %d chars; it is meant to fit a chat line: %q", len(got), got)
	}
}

// A refusal from one front end is not a refusal from Tonnel. The alternate host
// sat in the constants unused while every read failed for hours.
func TestReadHostRotatesAndWrapsAround(t *testing.T) {
	c, err := New(Options{AuthData: "x", ReadHosts: []string{"a.example", "b.example"}})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if got := c.ReadHost(); got != "a.example" {
		t.Errorf("first host = %q", got)
	}
	if got := c.rotateReadHost(); got != "b.example" {
		t.Errorf("after one rotation = %q", got)
	}
	if got := c.rotateReadHost(); got != "a.example" {
		t.Errorf("rotation must wrap, got %q", got)
	}
	// Only the label is redirected; a host named outright is left alone.
	if got := c.resolveHost(HostWrite); got != HostWrite {
		t.Errorf("write host was rewritten to %q", got)
	}
	if got := c.resolveHost(HostRead); got != c.ReadHost() {
		t.Errorf("read label resolved to %q, want the current candidate", got)
	}
}

// A single candidate must not divide by zero or spin.
func TestSingleReadHostIsStable(t *testing.T) {
	c, err := New(Options{AuthData: "x", ReadHosts: []string{"only.example"}})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if got := c.rotateReadHost(); got != "only.example" {
		t.Errorf("rotation with one host = %q", got)
	}
}

// The marketplace web front end answers 405 to the API paths, so it is not a
// drop-in replacement and must not be in the rotation by default.
func TestDefaultReadHostsExcludeTheWebFrontEnd(t *testing.T) {
	for _, h := range DefaultReadHosts() {
		if h == HostReadRS {
			t.Errorf("%s answers 405 to POST /api/pageGifts; rotating into it fails requests for no reason", h)
		}
	}
	if len(DefaultReadHosts()) < 2 {
		t.Error("there has to be somewhere to rotate to")
	}
}

// A balance of zero is not a harmless misread. spendable() takes it against the
// ticket limit, so every signal above nothing is rejected as unaffordable and
// the desk goes quiet with nothing in the log to explain why.
//
// The old descent looked for a fixed "amount"/"value"/"balance" inside a nested
// object, which meant the single most likely shape this endpoint returns —
// {"balance": {"gram": 25.4}} — matched the outer key, descended, found none of
// those three names, and answered zero.
func TestBalanceIsFoundWhicheverShapeItArrivesIn(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want float64
	}{
		{"flat", map[string]any{"balance": 25.4}, 25.4},
		{"nested under balance", map[string]any{"balance": map[string]any{"gram": 25.4}}, 25.4},
		{"nested under data", map[string]any{"data": map[string]any{"gram": 25.4}}, 25.4},
		{"amount inside the currency", map[string]any{"gram": map[string]any{"amount": 25.4}}, 25.4},
		{"alternate spelling", map[string]any{"gramBalance": 25.4}, 25.4},
		{"absent", map[string]any{"usdt": 3}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pickFloat(c.raw, "balance", "gram", "GRAM", "gramBalance", "ton", "TON", "tonBalance")
			if got != c.want {
				t.Errorf("balance = %v, want %v", got, c.want)
			}
		})
	}
}

// An outer key that is already a number must win over anything nested, or a
// payload carrying both a total and a breakdown reports the wrong one.
func TestBalancePrefersTheDirectValue(t *testing.T) {
	raw := map[string]any{
		"balance": 25.4,
		"data":    map[string]any{"balance": 999.0},
	}
	if got := pickFloat(raw, "balance", "gram"); got != 25.4 {
		t.Errorf("balance = %v, want the direct 25.4", got)
	}
}
