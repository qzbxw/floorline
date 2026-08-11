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
