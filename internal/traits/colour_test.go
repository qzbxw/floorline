package traits

import "testing"

// The word list could not see any of these: none of the names contains a colour
// word, and every one of them is unmistakably a colour.
func TestFamilyReadsTheColourNotTheName(t *testing.T) {
	for _, tc := range []struct {
		name, hex, want string
	}{
		{"Cyberpunk", "#858ff3", FamilyBlue},    // violet-blue
		{"Old Gold", "#c8a951", FamilyWarm},     //
		{"Onyx Black", "#0e0e10", FamilyDark},   //
		{"Ivory White", "#f6f2e9", FamilyLight}, //
		{"Desert Sand", "#6b4a2f", FamilyEarth}, // dark muted orange is brown
		{"Pine", "#1f7a3d", FamilyGreen},        //
		{"Cherry", "#c31432", FamilyRed},        //
		{"Orchid", "#c46be0", FamilyPurple},     //
		{"Steel", "#8a8f94", FamilyLight},       // a grey is not a hue
	} {
		if got := Family(tc.hex); got != tc.want {
			t.Errorf("%s (%s) = %q, want %q", tc.name, tc.hex, got, tc.want)
		}
	}
}

func TestFamilyRejectsNonsense(t *testing.T) {
	for _, s := range []string{"", "#fff", "not a colour", "#gggggg", "858ff3zz"} {
		if got := Family(s); got != "" {
			t.Errorf("Family(%q) = %q, want no answer", s, got)
		}
	}
	// The leading hash is optional, because half the world omits it.
	if Family("858ff3") != FamilyBlue {
		t.Error("a bare hex triplet was rejected")
	}
}
