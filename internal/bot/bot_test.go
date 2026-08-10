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
