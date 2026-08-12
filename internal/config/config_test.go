package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDotEnvDoesNotOverrideTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# a comment

export TELEGRAM_BOT_TOKEN=from-file
TONNEL_AUTH_DATA="quoted value"
SIG_MIN_EDGE=0.07
EMPTY=
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	t.Setenv("TELEGRAM_BOT_TOKEN", "from-environment")
	// The remaining keys are unset in this test process, so the file supplies them.
	os.Unsetenv("TONNEL_AUTH_DATA")
	os.Unsetenv("SIG_MIN_EDGE")

	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	t.Cleanup(func() {
		os.Unsetenv("TONNEL_AUTH_DATA")
		os.Unsetenv("SIG_MIN_EDGE")
		os.Unsetenv("EMPTY")
	})

	if got := os.Getenv("TELEGRAM_BOT_TOKEN"); got != "from-environment" {
		t.Errorf("token = %q; an explicit environment variable must win over the file", got)
	}
	if got := os.Getenv("TONNEL_AUTH_DATA"); got != "quoted value" {
		t.Errorf("authData = %q, want the unquoted value", got)
	}
	if got := os.Getenv("SIG_MIN_EDGE"); got != "0.07" {
		t.Errorf("SIG_MIN_EDGE = %q, want 0.07", got)
	}
}

func TestLoadDotEnvIgnoresAMissingFile(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "nope.env")); err != nil {
		t.Errorf("a missing env file should be fine, got %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	clearConfigEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Tonnel adds the referral charge to the buyer's displayed ask.
	if c.TonnelFee != 0.005 {
		t.Errorf("TonnelFee = %v, want 0.005", c.TonnelFee)
	}
	// The edge bars are deliberately well clear of break-even. They were set
	// while the exit was overstated by roughly a factor of two, so 1% was
	// admitting anything that was not an outright loss.
	if c.Sig.MinEdge != 0.045 || c.Sig.MinVelocity != .5 || c.Sig.MinSales != 6 {
		t.Errorf("signal gates = %+v, want the honest-edge profile", c.Sig)
	}
	// A percentage alone cannot tell a trade from a rounding error, and a fast
	// exit measured in weeks is a warehouse.
	if c.Sig.MinNet != 0.25 || c.Sig.MaxExitDays != 4 {
		t.Errorf("signal gates = %+v, want an absolute floor and a holding limit", c.Sig)
	}
	if c.Auto.MinEdge != 0.06 || c.Auto.MinTurnover != 0.6 {
		t.Errorf("auto gates = %+v, want the strict profile", c.Auto)
	}
	if c.FeedInterval != 2*time.Second {
		t.Errorf("feed interval = %v, want 2s", c.FeedInterval)
	}
	if c.LookbackDays != 14 {
		t.Errorf("lookback = %d, want 14", c.LookbackDays)
	}
}

func TestDurationsAcceptSecondsOrGoSyntax(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("POLL_FEED", "1500ms")
	t.Setenv("POLL_STATS", "90")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.FeedInterval != 1500*time.Millisecond {
		t.Errorf("feed interval = %v, want 1.5s", c.FeedInterval)
	}
	if c.StatsInterval != 90*time.Second {
		t.Errorf("stats interval = %v, want 90s from a bare number", c.StatsInterval)
	}
}

func TestLoadRejectsNonsense(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("UNDERCUT", "0.9")
	if _, err := Load(); err == nil {
		t.Error("a 90% undercut must be rejected")
	}

	clearConfigEnv(t)
	t.Setenv("LOOKBACK_DAYS", "0")
	if _, err := Load(); err == nil {
		t.Error("a zero lookback window must be rejected")
	}
}

func TestRequireBotAndAuth(t *testing.T) {
	clearConfigEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.RequireBot(); err == nil {
		t.Error("RequireBot must fail without a token and an owner id")
	}
	if err := c.RequireAuth(); err == nil {
		t.Error("RequireAuth must fail without authData")
	}

	c.BotToken, c.OwnerID, c.AuthData = "t", 1, "x"
	if err := c.RequireBot(); err != nil {
		t.Errorf("RequireBot: %v", err)
	}
	if err := c.RequireAuth(); err != nil {
		t.Errorf("RequireAuth: %v", err)
	}
}

// clearConfigEnv removes every setting Load reads, so each test starts clean.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"TELEGRAM_BOT_TOKEN", "TELEGRAM_OWNER_ID", "TONNEL_AUTH_DATA", "PORTALS_AUTH_DATA",
		"DB_PATH", "LOG_LEVEL", "TONNEL_FEE", "PORTALS_FEE", "MRKT_FEE", "UNDERCUT",
		"SIG_MIN_EDGE", "SIG_MIN_VELOCITY", "SIG_MIN_SALES", "SIG_MAX_MAD_RATIO",
		"SIG_MIN_TREND", "SIG_MIN_PRICE", "SIG_MAX_PRICE",
		"AUTOBUY_MIN_EDGE", "AUTOBUY_MIN_VELOCITY", "AUTOBUY_MIN_SALES", "AUTOBUY_MIN_TURNOVER",
		"AUTOBUY_MAX_MAD_RATIO", "AUTOBUY_MIN_TREND", "AUTOBUY_MAX_DATA_AGE",
		"AUTOBUY_MAX_GRAM_MOVE_15M",
		"LOOKBACK_DAYS", "ATTRIBUTE_LOOKBACK_DAYS", "SHADOW_MODE", "CALIBRATION_MIN_SIGNALS", "CALIBRATION_MIN_DAYS", "POLL_FEED", "POLL_STATS", "POLL_SALES", "POLL_INVENTORY",
		"GRAM_QUOTE_URL", "POLL_GRAM",
		"BOOK_CACHE_TTL", "READ_RPS", "READ_BURST", "HTTP_TIMEOUT",
	}
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}
