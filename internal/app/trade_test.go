package app

import (
	"testing"
	"time"

	"floorline/internal/tonnel"
)

// A session is a deliberate narrowing, so the two questions it has to answer
// are "is this pair mine" and "is it still mine after a restart".
func TestSessionCoversOnlyItsOwnPairs(t *testing.T) {
	mine := tonnel.ModelKey{Name: "Lol Pop", Model: "Mirage"}
	other := tonnel.ModelKey{Name: "Lol Pop", Model: "Lavender Ice"}

	s := &TradeSession{StartedAt: time.Now(), Pairs: []tonnel.ModelKey{mine}}
	if !s.Covers(mine) {
		t.Error("a pair on the list must be covered")
	}
	// Same collection, different model: the session is about pairs, and the
	// whole point of picking ten is that the other few thousand stay quiet.
	if s.Covers(other) {
		t.Error("a different model of the same collection is not on the list")
	}

	// No session at all must never read as "covers nothing", or the ordinary
	// feed would go silent the moment the session ended.
	var none *TradeSession
	if none.Active() || none.Covers(mine) {
		t.Error("a nil session must be inactive rather than an empty allowlist")
	}
	if (&TradeSession{}).Active() {
		t.Error("a session with no pairs is not running")
	}
}
