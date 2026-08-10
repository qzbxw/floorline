package tonnel

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSplitAttr(t *testing.T) {
	cases := []struct {
		in     string
		name   string
		rarity float64
	}{
		{"Dark Soul (0.4%)", "Dark Soul", 0.4},
		{"Ranger Green (1.5%)", "Ranger Green", 1.5},
		{"Plain", "Plain", 0},
		{"  Spaced Out (12%)  ", "Spaced Out", 12},
		{"Weird (Name) (2.5%)", "Weird (Name)", 2.5},
		{"", "", 0},
	}
	for _, c := range cases {
		name, rarity := SplitAttr(c.in)
		if name != c.name || rarity != c.rarity {
			t.Errorf("SplitAttr(%q) = (%q, %v), want (%q, %v)", c.in, name, rarity, c.name, c.rarity)
		}
	}
}

func TestTitleCase(t *testing.T) {
	cases := map[string]string{
		"plush pepe":       "Plush Pepe",
		"PLUSH PEPE":       "PLUSH PEPE", // only the first letter is forced
		"lunar snake":      "Lunar Snake",
		"jack-in-the-box":  "Jack-in-the-Box",
		"Jack-In-The-Box":  "Jack-in-the-Box",
		"  toy   bear   ":  "Toy   Bear",
		"b-day candle":     "B-day Candle",
		"astral shard":     "Astral Shard",
		"desk calendar":    "Desk Calendar",
		"eternal rose":     "Eternal Rose",
		"heroic helmet   ": "Heroic Helmet",
	}
	for in, want := range cases {
		if got := TitleCase(in); got != want {
			t.Errorf("TitleCase(%q) = %q, want %q", in, got, want)
		}
	}
}

// The endpoints are private and have been observed returning the same field as
// a number in one response and a string in another, so decoding must not be
// brittle about it: one odd row should not drop a whole page of listings.
func TestFlexDecoding(t *testing.T) {
	const raw = `{
		"gift_id": "12345",
		"gift_num": 678,
		"name": "Plush Pepe",
		"model": "Pink Diamond (0.4%)",
		"price": "1240.5",
		"rarity": null,
		"modelRarity": 0.4,
		"seller": 6083232778,
		"premarket": "false",
		"telegramMarketplace": true,
		"export_at": "2026-03-01T10:00:00.000Z",
		"message_post_time": 1712345678
	}`

	var g Gift
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if g.GiftID.Int() != 12345 {
		t.Errorf("gift_id = %d, want 12345", g.GiftID.Int())
	}
	if g.GiftNum.Int() != 678 {
		t.Errorf("gift_num = %d, want 678", g.GiftNum.Int())
	}
	if g.Price.Float() != 1240.5 {
		t.Errorf("price = %v, want 1240.5", g.Price.Float())
	}
	if g.Rarity.Float() != 0 {
		t.Errorf("null rarity should decode to 0, got %v", g.Rarity.Float())
	}
	if g.Premarket.Bool() {
		t.Error(`premarket "false" should decode to false`)
	}
	if !g.TelegramMarketplace.Bool() {
		t.Error("telegramMarketplace true should decode to true")
	}
	if g.ExportAt.Year() != 2026 || g.ExportAt.Month() != time.March {
		t.Errorf("export_at = %v, want March 2026", g.ExportAt.Time)
	}
	if g.MessagePostTime.Unix() != 1712345678 {
		t.Errorf("message_post_time = %v, want unix 1712345678", g.MessagePostTime.Unix())
	}
	if key := g.Key(); key.Name != "Plush Pepe" || key.Model != "Pink Diamond" {
		t.Errorf("Key() = %+v, want {Plush Pepe Pink Diamond}", key)
	}
}

// Bundles price a whole pack in one row. Letting one into the book would make
// the model floor look dramatically lower than it is.
func TestIsBundle(t *testing.T) {
	if !(&Gift{GiftID: -5}).IsBundle() {
		t.Error("a negative gift_id must be treated as a bundle")
	}
	if (&Gift{GiftID: 12}).IsBundle() {
		t.Error("a plain listing must not be treated as a bundle")
	}
	withData := &Gift{GiftID: 12, BundleData: json.RawMessage(`{"count":3}`)}
	if !withData.IsBundle() {
		t.Error("a row carrying bundleData must be treated as a bundle")
	}
}

func TestSaleWhenPrefersTimestamp(t *testing.T) {
	s := Sale{}
	if !s.When().IsZero() {
		t.Error("a sale with no timestamps should report a zero time")
	}
	s.UpdatedAt = FlexTime{time.Unix(100, 0).UTC()}
	if s.When().Unix() != 100 {
		t.Errorf("fallback to updatedAt failed: %v", s.When())
	}
	s.Timestamp = FlexTime{time.Unix(200, 0).UTC()}
	if s.When().Unix() != 200 {
		t.Errorf("timestamp should win: %v", s.When())
	}
}

func TestAPIErrorClassification(t *testing.T) {
	if !(&APIError{Status: 403}).IsBlocked() {
		t.Error("403 must be classified as an anti-bot block")
	}
	if !(&APIError{Status: 429}).IsBlocked() {
		t.Error("429 must be classified as an anti-bot block")
	}
	if (&APIError{Status: 400}).IsBlocked() {
		t.Error("400 is a business rejection, not a block")
	}
	if !(&APIError{Status: 200, Message: "Invalid authData"}).IsAuth() {
		t.Error("an auth message must be classified as an auth failure")
	}
}
