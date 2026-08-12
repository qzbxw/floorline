package pricing

import (
	"strconv"
	"strings"
)

// Appearance is what a gift looks like, judged from its attribute names and its
// number. It exists because the floor is blind to it: a model floor is set by
// whichever specimen is ugliest, so an Onyx Black one with a matching symbol
// sits in the same bucket as the plainest example of the same model and looks,
// to a floor-following bot, like the same asset at the same price.
//
// Two rules keep this from becoming astrology.
//
// First, none of it ever adds to a price. The only thing allowed to move a
// number is a measured premium from real sales — ComputeAttributeValue, which
// needs MinAttributeSamples matching prints before it returns anything at all.
// Appearance decides which comparables are admissible and what the card says;
// it never decides what a gift is worth.
//
// Second, the vocabulary is deliberately small and concrete. Colour families
// and a handful of backdrop classes, matched on whole words. Guessing at
// aesthetics with a long tail of clever rules produces confident nonsense, and
// confident nonsense is what the desk is being repaired for.
type Appearance struct {
	// Premium reports that this gift has a reason to trade above the plain
	// examples of its model. It is a claim about comparability, not about size:
	// it says "do not price this off the ordinary ones", never "add 20%".
	Premium bool
	// BackdropClass names the backdrop's tier when it has one.
	BackdropClass string
	// Mono reports that the backdrop and symbol sit in the same colour family,
	// which is the look the market pays for and the one the floor cannot see.
	Mono bool
	// NumberClass names why the gift number is collectible, when it is.
	NumberClass string
	// Reasons is the human-readable form, for the card.
	Reasons []string
}

// darkBackdrops are the tier the desk keeps missing. Their model floors are
// unremarkable because the floor is whatever the cheapest specimen asks, while
// the specimens themselves are the ones collectors chase.
var darkBackdrops = map[string]string{
	"black": "чёрный", "onyx": "оникс", "obsidian": "обсидиан",
	"midnight": "полночный", "noir": "нуар", "raven": "воронёный",
	"charcoal": "угольный", "ink": "чернильный", "jet": "агатовый",
	"shadow": "теневой",
}

// gemBackdrops are the precious-material names. They carry a weaker claim than
// the dark tier — plenty of them are ordinary — so they only ever mark a gift
// as worth comparing carefully, never as worth more.
var gemBackdrops = map[string]string{
	"platinum": "платина", "gold": "золото", "golden": "золото",
	"diamond": "бриллиант", "emerald": "изумруд", "ruby": "рубин",
	"sapphire": "сапфир", "amethyst": "аметист", "malachite": "малахит",
	"moonstone": "лунный камень", "pearl": "жемчуг", "silver": "серебро",
}

// colourFamilies maps a colour word to the family it belongs to, so a backdrop
// and a symbol can be told to match without pretending to read pixels.
var colourFamilies = map[string]string{
	"black": "dark", "onyx": "dark", "obsidian": "dark", "charcoal": "dark",
	"midnight": "dark", "noir": "dark", "ink": "dark", "jet": "dark", "raven": "dark",

	"white": "light", "ivory": "light", "snow": "light", "pearl": "light",
	"platinum": "light", "silver": "light", "moonstone": "light",

	"green": "green", "emerald": "green", "malachite": "green", "mint": "green",
	"olive": "green", "shamrock": "green", "pistachio": "green", "camo": "green",
	"pine": "green", "forest": "green", "lime": "green",

	"blue": "blue", "azure": "blue", "sapphire": "blue", "navy": "blue",
	"cobalt": "blue", "aquamarine": "blue", "turquoise": "blue", "teal": "blue",
	"cyan": "blue", "denim": "blue",

	"red": "red", "ruby": "red", "crimson": "red", "scarlet": "red",
	"burgundy": "red", "coral": "red", "cherry": "red", "rosewood": "red",
	"tomato": "red",

	"purple": "purple", "violet": "purple", "amethyst": "purple",
	"lavender": "purple", "lilac": "purple", "orchid": "purple", "plum": "purple",

	"orange": "warm", "amber": "warm", "tangerine": "warm", "apricot": "warm",
	"mustard": "warm", "gold": "warm", "golden": "warm", "honey": "warm",
	"copper": "warm", "bronze": "warm", "caramel": "warm",

	"pink": "pink", "rose": "pink", "magenta": "pink", "fuchsia": "pink",
	"blush": "pink", "salmon": "pink",

	"brown": "earth", "chocolate": "earth", "chestnut": "earth", "coffee": "earth",
	"walnut": "earth", "sand": "earth", "beige": "earth", "clay": "earth",
}

// Appraise reads a gift's appearance from its attribute names and number.
func Appraise(backdrop, symbol string, giftNum int64) Appearance {
	var a Appearance

	bd := words(backdrop)
	for _, w := range bd {
		if name, ok := darkBackdrops[w]; ok {
			a.BackdropClass, a.Premium = name, true
			a.Reasons = append(a.Reasons, "тёмный фон ("+name+") — флор модели его не отражает")
			break
		}
	}
	if a.BackdropClass == "" {
		for _, w := range bd {
			if name, ok := gemBackdrops[w]; ok {
				a.BackdropClass = name
				a.Reasons = append(a.Reasons, "фон из премиальной группы ("+name+")")
				break
			}
		}
	}

	if fam := family(bd); fam != "" && fam == family(words(symbol)) {
		a.Mono, a.Premium = true, true
		a.Reasons = append(a.Reasons, "моно: фон и символ в одной гамме")
	}

	if class := numberClass(giftNum); class != "" {
		a.NumberClass = class
		a.Premium = true
		a.Reasons = append(a.Reasons, "номер: "+class)
	}
	return a
}

// family returns the colour family of the first word that has one.
func family(ws []string) string {
	for _, w := range ws {
		if f, ok := colourFamilies[w]; ok {
			return f
		}
	}
	return ""
}

// words splits an attribute name into lowercase word tokens.
func words(s string) []string {
	return strings.Fields(strings.ToLower(strings.ReplaceAll(s, "-", " ")))
}

// numberClass names why a gift number is collectible, or returns empty.
//
// Only shapes that are unambiguous and that people actually ask for. "Looks
// nice" is not a category.
func numberClass(n int64) string {
	if n <= 0 {
		return ""
	}
	s := strconv.FormatInt(n, 10)
	switch {
	case n < 100:
		return "#" + s + " — двузначный"
	case n < 1000:
		return "#" + s + " — трёхзначный"
	case repdigit(s):
		return "повтор " + s
	case n%100_000 == 0, n%10_000 == 0:
		return "круглый " + s
	case palindrome(s) && len(s) >= 4:
		return "палиндром " + s
	}
	return ""
}

func repdigit(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return len(s) >= 3
}

func palindrome(s string) bool {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		if s[i] != s[j] {
			return false
		}
	}
	return true
}
