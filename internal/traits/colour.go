package traits

import (
	"math"
	"strconv"
	"strings"
)

// Colour families, named to match the ones the pricing engine already uses for
// attribute words so the two can be compared directly.
const (
	FamilyDark   = "dark"
	FamilyLight  = "light"
	FamilyGreen  = "green"
	FamilyBlue   = "blue"
	FamilyRed    = "red"
	FamilyPurple = "purple"
	FamilyWarm   = "warm"
	FamilyPink   = "pink"
	FamilyEarth  = "earth"
)

// Family classifies a "#rrggbb" colour into the same vocabulary the attribute
// names are matched against.
//
// This is what the word list could never do. "Cyberpunk" is violet, "Old Gold"
// is gold and "Desert Sand" is earth, and none of those names contain a colour
// word — so a backdrop whose whole value is that it matches the model read as
// having no colour at all. Hue, saturation and lightness answer it directly and
// for every backdrop, including the ones named after a mood.
//
// The thresholds are deliberately coarse. The question being asked is "are
// these two in the same palette", which is a six-way split at most; a finer
// classification would be inventing precision the question does not have.
func Family(hex string) string {
	r, g, b, ok := parseHex(hex)
	if !ok {
		return ""
	}
	h, s, l := hsl(r, g, b)

	// Lightness first: a near-black or near-white is that, whatever its hue.
	switch {
	case l <= 0.18:
		return FamilyDark
	case l >= 0.86:
		return FamilyLight
	}
	// A colour with almost no saturation is a grey, which reads as dark or
	// light rather than as a hue.
	if s <= 0.12 {
		if l < 0.5 {
			return FamilyDark
		}
		return FamilyLight
	}
	// Brown is the one family that is not a hue band: it is a dark, muted
	// orange, and calling it "warm" would put chocolate and gold together.
	if h >= 15 && h < 45 && l < 0.42 {
		return FamilyEarth
	}
	switch {
	case h < 15 || h >= 345:
		return FamilyRed
	case h < 45:
		return FamilyWarm
	case h < 70:
		return FamilyWarm // yellow and gold sit with orange
	case h < 170:
		return FamilyGreen
	case h < 260:
		return FamilyBlue
	case h < 300:
		return FamilyPurple
	default:
		return FamilyPink
	}
}

func parseHex(s string) (r, g, b float64, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return float64((v >> 16) & 0xff), float64((v >> 8) & 0xff), float64(v & 0xff), true
}

// hsl returns hue in degrees, saturation and lightness in 0..1.
func hsl(r, g, b float64) (h, s, l float64) {
	r, g, b = r/255, g/255, b/255
	maxc := math.Max(r, math.Max(g, b))
	minc := math.Min(r, math.Min(g, b))
	l = (maxc + minc) / 2
	if maxc == minc {
		return 0, 0, l
	}
	d := maxc - minc
	if l > 0.5 {
		s = d / (2 - maxc - minc)
	} else {
		s = d / (maxc + minc)
	}
	switch maxc {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h * 60, s, l
}
