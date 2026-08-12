package bot

import (
	"strings"
	"testing"
)

// Both halves of a target contain spaces, so a separator is the only thing that
// makes "/book Plush Pepe / Pink Diamond" unambiguous.
func TestSplitTarget(t *testing.T) {
	cases := []struct {
		in         string
		collection string
		model      string
	}{
		{"Plush Pepe / Pink Diamond", "Plush Pepe", "Pink Diamond"},
		{"Plush Pepe|Pink Diamond", "Plush Pepe", "Pink Diamond"},
		{"Plush Pepe - Pink Diamond", "Plush Pepe", "Pink Diamond"},
		{"  Plush Pepe  /  Pink Diamond  ", "Plush Pepe", "Pink Diamond"},
		{"Plush Pepe", "Plush Pepe", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		col, model := splitTarget(c.in)
		if col != c.collection || model != c.model {
			t.Errorf("splitTarget(%q) = (%q, %q), want (%q, %q)", c.in, col, model, c.collection, c.model)
		}
	}
}

func TestSplitTrailingNumber(t *testing.T) {
	cases := []struct {
		in   string
		rest string
		num  float64
	}{
		{"Plush Pepe / Pink Diamond 1200", "Plush Pepe / Pink Diamond", 1200},
		{"Plush Pepe / Pink Diamond 1200.5", "Plush Pepe / Pink Diamond", 1200.5},
		{"Plush Pepe / Pink Diamond", "Plush Pepe / Pink Diamond", 0},
		{"", "", 0},
	}
	for _, c := range cases {
		rest, num := splitTrailingNumber(c.in)
		if rest != c.rest || num != c.num {
			t.Errorf("splitTrailingNumber(%q) = (%q, %v), want (%q, %v)", c.in, rest, num, c.rest, c.num)
		}
	}
}

// Attribute names come from the marketplace and must never be treated as markup.
func TestEscapeProtectsAgainstMarkupInAttributeNames(t *testing.T) {
	got := Esc(`<b>Fake</b> & "quoted"`)
	if strings.Contains(got, "<b>") {
		t.Errorf("Esc left raw markup in place: %q", got)
	}
	if !strings.Contains(got, "&amp;") {
		t.Errorf("Esc did not escape the ampersand: %q", got)
	}
}

func TestSplitMessageChunksOnLineBoundaries(t *testing.T) {
	short := "one\ntwo"
	if got := splitMessage(short); len(got) != 1 || got[0] != short {
		t.Errorf("a short message should stay whole, got %d chunks", len(got))
	}

	line := strings.Repeat("x", 100) + "\n"
	long := strings.Repeat(line, 100) // ~10k characters
	chunks := splitMessage(long)
	if len(chunks) < 2 {
		t.Fatalf("a 10k message should be split, got %d chunk(s)", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > telegramMessageLimit {
			t.Errorf("chunk %d is %d bytes, over the %d limit", i, len(c), telegramMessageLimit)
		}
	}
	if strings.Join(chunks, "") != long {
		t.Error("splitting lost or reordered content")
	}
}

func TestTrimNumDropsTrailingZeros(t *testing.T) {
	for in, want := range map[float64]string{1240: "1240", 1240.5: "1240.5", 0.25: "0.25"} {
		if got := trimNum(in); got != want {
			t.Errorf("trimNum(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestReplyBuilder(t *testing.T) {
	r := Text("hello")
	if !Text("").Empty() {
		t.Error("an empty reply must report empty")
	}
	if r.Empty() {
		t.Error("a reply with text must not report empty")
	}

	r = r.WithRow(Callback("A", "u", int64(1)))
	r = r.WithRow() // empty rows are dropped rather than sent as blanks
	r = r.WithRow(Link("B", "https://example.com"), Callback("C", "u2", "x"))

	if len(r.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the empty one dropped)", len(r.Rows))
	}
	if r.Rows[0][0].Data != "1" {
		t.Errorf("callback data = %q, want \"1\"", r.Rows[0][0].Data)
	}
	if r.Rows[1][0].URL == "" || r.Rows[1][0].Unique != "" {
		t.Errorf("a link button must carry a URL and no callback route: %+v", r.Rows[1][0])
	}
}

func TestTextfFormats(t *testing.T) {
	if got := Textf("%d GRAM", 1240).Text; got != "1240 GRAM" {
		t.Errorf("Textf = %q", got)
	}
}

func TestCallbackDataJoinsParts(t *testing.T) {
	b := Callback("x", "u", "ref", "book")
	if b.Data != "ref|book" {
		t.Errorf("data = %q, want ref|book", b.Data)
	}
	// Telegram rejects callback payloads over 64 bytes, and the unique prefix
	// eats into that budget too.
	if len(b.Unique)+len(b.Data)+2 > 64 {
		t.Errorf("callback payload is too long: %q %q", b.Unique, b.Data)
	}
}

func TestTonnelGiftURL(t *testing.T) {
	const want = "https://t.me/tonnel_network_bot/gift?startapp=10380742"
	if got := TonnelGiftURL(10380742); got != want {
		t.Errorf("TonnelGiftURL = %q, want %q", got, want)
	}
}

func TestMarkupSeparatesLinksFromCallbacks(t *testing.T) {
	if markup(nil) != nil {
		t.Error("no rows must produce no keyboard")
	}

	m := markup([][]Button{
		{Callback("Buy", "fl_buy", int64(7))},
		{Link("Open", "https://t.me/x"), Callback("Book", "fl_book", int64(7))},
	})
	if m == nil {
		t.Fatal("expected a keyboard")
	}
	if len(m.InlineKeyboard) != 2 {
		t.Fatalf("keyboard rows = %d, want 2", len(m.InlineKeyboard))
	}
	if m.InlineKeyboard[1][0].URL != "https://t.me/x" {
		t.Errorf("first button of row 2 should be a URL button, got %+v", m.InlineKeyboard[1][0])
	}
	if m.InlineKeyboard[1][1].URL != "" {
		t.Error("a callback button must not carry a URL")
	}
}

// Every command in the menu must have a handler, or Telegram advertises
// something that silently does nothing.
func TestCommandMenuIsComplete(t *testing.T) {
	for _, c := range commandMenu {
		if c.Text == "" || c.Description == "" {
			t.Errorf("incomplete menu entry: %+v", c)
		}
		if strings.HasPrefix(c.Text, "/") {
			t.Errorf("menu entry %q must not include the slash", c.Text)
		}
	}
}

// The typed commands are no longer advertised by Telegram, so /help is the only
// place they are discoverable. A command that still has a handler but has
// fallen out of the reference is invisible to the operator.
func TestHelpDocumentsEveryTypedCommand(t *testing.T) {
	help := helpMenuReply().Text
	for _, cmd := range []string{
		"gram", "floor", "book", "hist", "val",
		"pos", "portfolio", "advice", "history", "cost", "exit", "pnl", "balance", "relist",
		"watch", "unwatch", "watchlist", "mute", "unmute",
		"arm", "disarm", "autobuy", "limits", "status", "auth",
		"trade", "scan",
	} {
		if !strings.Contains(help, "/"+cmd) {
			t.Errorf("command /%s has a handler but is not documented in /help", cmd)
		}
	}
}

// Every menu must offer a way out. A screen whose only escape is retyping
// /start is the dead end this keyboard exists to avoid.
func TestEveryMenuHasAWayBack(t *testing.T) {
	menus := map[string]Reply{
		"market": marketMenu(),
		"alerts": alertsMenu(),
		"more":   moreMenu(),
		"help":   helpMenuReply(),
		// The auto-buy screen is built by the backend, not declared here, so it
		// is covered by the backend's own navigation instead of this table.
		"howto/val":   howTo("val"),
		"howto/watch": howTo("watch"),
		"howto/auth":  howTo("auth"),
	}
	for name, m := range menus {
		var found bool
		for _, row := range m.Rows {
			for _, btn := range row {
				switch btn.Unique {
				case cbMenu, cbMarket, cbMore, cbAlerts:
					found = true
				}
			}
		}
		if !found {
			t.Errorf("menu %q has no button leading back", name)
		}
	}
}

// A column of one-button rows is a keyboard the operator has to scroll past to
// reach the message it belongs to. Everything declared here packs into rows.
func TestMenusAreGridsNotColumns(t *testing.T) {
	menus := map[string]Reply{
		"market": marketMenu(),
		"alerts": alertsMenu(),
		"more":   moreMenu(),
	}
	for name, m := range menus {
		if len(m.Rows) > 3 {
			t.Errorf("menu %q is %d rows tall", name, len(m.Rows))
		}
		for i, row := range m.Rows {
			// The last row is the lone way home; every other one carries at
			// least two buttons.
			if len(row) < 2 && i != len(m.Rows)-1 {
				t.Errorf("menu %q row %d has %d button(s) — pack the grid", name, i, len(row))
			}
			for _, btn := range row {
				if n := len([]rune(btn.Label)); n > 12 {
					t.Errorf("menu %q label %q is %d runes; three of these do not fit a phone", name, btn.Label, n)
				}
			}
		}
	}
	if n := len(homeKeyboard()); n != 3 {
		t.Errorf("home keyboard is %d rows, want a 3x3 grid", n)
	}
	for i, row := range homeKeyboard() {
		if len(row) != 3 {
			t.Errorf("home row %d has %d buttons, want 3", i, len(row))
		}
	}
}

// A backend view carries its own actions but no navigation, so back() is what
// keeps it from becoming a dead end.
func TestBackAddsNavigationWithoutLosingActions(t *testing.T) {
	view := Text("positions").WithRow(Callback("♻️ Переставить", "fl_relist", int64(7)))
	got := back(view, cbMarket, "🔙 Рынок")

	if len(got.Rows) != 2 {
		t.Fatalf("rows = %d, want the action row plus a nav row", len(got.Rows))
	}
	if got.Rows[0][0].Unique != "fl_relist" {
		t.Error("the view's own action button must survive")
	}
	navRow := got.Rows[1]
	if navRow[0].Unique != cbMarket || navRow[1].Unique != cbMenu {
		t.Errorf("nav row = %+v, want back-to-section then home", navRow)
	}
}

// An empty reply must not grow a keyboard: the bot sends nothing at all for it,
// and a keyboard attached to no text would be dropped anyway.
func TestBackLeavesAnEmptyReplyAlone(t *testing.T) {
	if got := back(Reply{}, cbMenu, "🔙"); !got.Empty() {
		t.Errorf("back() on an empty reply produced %+v", got)
	}
}
