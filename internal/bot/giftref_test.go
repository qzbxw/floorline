package bot

import "testing"

func TestParseGiftRefReadsWhateverTheMiniAppShares(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
		ok   bool
	}{
		{"share with caption", "https://t.me/tonnel_network_bot/gift?startapp=10023047\nCheck out this gift!", 10023047, true},
		{"bare link", "https://t.me/tonnel_network_bot/gift?startapp=10380168", 10380168, true},
		{"prefixed payload", "https://t.me/tonnel_network_bot/gift?startapp=gift_10380168", 10380168, true},
		{"bare id", "10382739", 10382739, true},
		{"id with spaces", "  10382739  ", 10382739, true},
		{"caption first", "Check out this gift! https://t.me/tonnel_network_bot/gift?startapp=99887766", 99887766, true},
		{"our own url helper", TonnelGiftURL(4242424), 4242424, true},
		{"not a gift", "Plush Pepe / Pink Diamond", 0, false},
		{"empty", "   ", 0, false},
		{"short number is a price, not an id", "3.28", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseGiftRef(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("ParseGiftRef(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
