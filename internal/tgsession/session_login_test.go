package tgsession

import (
	"os"
	"path/filepath"
	"testing"
)

// Telegram writes the session file as soon as it has an auth key with the data
// centre, which is before the user is authorised — so an abandoned login leaves
// a perfectly valid-looking file behind. Treating that as "logged in" is how a
// venue went from unconfigured to configured-and-permanently-failing, and a
// venue that fails slowly starves the ones that would have answered.
func TestSessionFileAloneIsNotALogin(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{AppID: 1, AppHash: "h", SessionFile: filepath.Join(dir, "s.json")})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if c.LoggedIn() {
		t.Fatal("a fresh config must not report a login")
	}

	// What an abandoned login leaves behind.
	if err := os.WriteFile(c.cfg.SessionFile, []byte(`{"Version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c.LoggedIn() {
		t.Error("a session file without an observed authorisation is not a login")
	}

	if err := c.markAuthorized(); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if !c.LoggedIn() {
		t.Error("an observed authorisation must report a login")
	}
}
