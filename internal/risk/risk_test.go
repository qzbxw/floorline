package risk

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"floorline/internal/store"
	"floorline/internal/tonnel"
)

var key = tonnel.ModelKey{Name: "Plush Pepe", Model: "Pink Diamond"}

func newManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "risk.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	m, err := New(context.Background(), st)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return m, st
}

// armed returns a manager with workable limits already configured.
func armed(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	m, st := newManager(t)
	ctx := context.Background()
	mustSet(t, m, "max_ticket", "100")
	mustSet(t, m, "daily_budget", "500")
	if err := m.Arm(ctx); err != nil {
		t.Fatalf("arm: %v", err)
	}
	return m, st
}

func mustSet(t *testing.T, m *Manager, k, v string) {
	t.Helper()
	if err := m.SetLimit(context.Background(), k, v); err != nil {
		t.Fatalf("set %s=%s: %v", k, v, err)
	}
}

// Arming without a ticket size and a daily budget would leave the bot with no
// bound on how much it can lose, so it must be refused.
func TestArmRefusedWithoutMoneyLimits(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()

	err := m.Arm(ctx)
	if err == nil {
		t.Fatal("arming with no limits must be refused")
	}
	if !strings.Contains(err.Error(), "max_ticket") {
		t.Errorf("error should name the missing limit, got %q", err)
	}
	if m.Armed() {
		t.Error("the manager reports armed after a refused arm")
	}

	mustSet(t, m, "max_ticket", "50")
	if err := m.Arm(ctx); err == nil {
		t.Error("a ticket size alone is not enough; the daily budget is still unset")
	}

	mustSet(t, m, "daily_budget", "200")
	if err := m.Arm(ctx); err != nil {
		t.Fatalf("arm with both limits: %v", err)
	}
	if !m.Armed() {
		t.Error("the manager should be armed now")
	}
}

func TestAllowRefusesWhenDisarmed(t *testing.T) {
	m, _ := newManager(t)
	ok, why := m.Allow(context.Background(), key, 10, time.Now())
	if ok {
		t.Error("a disarmed manager must not allow a purchase")
	}
	if !strings.Contains(why, "disarmed") {
		t.Errorf("reason = %q, want it to mention being disarmed", why)
	}
}

func TestAllowEnforcesTicketSize(t *testing.T) {
	m, _ := armed(t)
	now := time.Now()

	if ok, why := m.Allow(context.Background(), key, 100, now); !ok {
		t.Errorf("a purchase exactly at the limit should be allowed, got %q", why)
	}
	ok, why := m.Allow(context.Background(), key, 100.01, now)
	if ok {
		t.Error("a purchase above max_ticket must be refused")
	}
	if !strings.Contains(why, "max_ticket") {
		t.Errorf("reason = %q, want it to mention max_ticket", why)
	}
}

// The budget lives in the database precisely so a restart cannot hand it back.
func TestDailyBudgetIsEnforcedAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "risk.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	m, err := New(ctx, st)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	mustSet(t, m, "max_ticket", "100")
	mustSet(t, m, "daily_budget", "250")
	mustSet(t, m, "max_buys_per_hour", "0")
	mustSet(t, m, "model_cooldown_min", "0")
	if err := m.Arm(ctx); err != nil {
		t.Fatalf("arm: %v", err)
	}

	now := time.Now()
	for i := 0; i < 2; i++ {
		if ok, why := m.Allow(ctx, key, 100, now); !ok {
			t.Fatalf("purchase %d refused: %s", i, why)
		}
		if err := m.Commit(ctx, 100, now); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	// 200 of 250 is spent, so a third 100 would overshoot. The budget is a
	// ceiling on committed spend, not a threshold you may cross once.
	if ok, why := m.Allow(ctx, key, 100, now); ok {
		t.Error("the daily budget was not enforced")
	} else if !strings.Contains(why, "budget") {
		t.Errorf("reason = %q, want it to mention the budget", why)
	}
	if ok, why := m.Allow(ctx, key, 50, now); !ok {
		t.Errorf("a purchase that fits in the remaining 50 should be allowed: %s", why)
	}
	st.Close()

	// Restart against the same database.
	st2, err := store.Open(filepath.Join(dir, "risk.db"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	m2, err := New(ctx, st2)
	if err != nil {
		t.Fatalf("new after restart: %v", err)
	}
	if !m2.Armed() {
		t.Error("the armed flag should persist across a restart")
	}
	if ok, _ := m2.Allow(ctx, key, 100, now); ok {
		t.Error("a restart handed back an already-spent daily budget")
	}
}

func TestAllowEnforcesOpenPositionCeiling(t *testing.T) {
	ctx := context.Background()
	m, st := armed(t)
	mustSet(t, m, "max_positions", "1")

	if err := st.UpsertPosition(ctx, store.Position{
		GiftID: 1, Key: key, BuyPrice: 10, BoughtAt: time.Now(),
		Status: store.StatusOpen, Source: "test",
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	ok, why := m.Allow(ctx, key, 10, time.Now())
	if ok {
		t.Error("the open-position ceiling was not enforced")
	}
	if !strings.Contains(why, "open positions") {
		t.Errorf("reason = %q, want it to mention open positions", why)
	}
}

func TestAllowEnforcesHourlyBuyLimit(t *testing.T) {
	ctx := context.Background()
	m, st := armed(t)
	mustSet(t, m, "max_buys_per_hour", "1")
	mustSet(t, m, "model_cooldown_min", "0")

	now := time.Now()
	if _, err := st.ClaimBuy(ctx, 1, 10, now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.FinishBuy(ctx, 1, "bought", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}

	ok, why := m.Allow(ctx, key, 10, now)
	if ok {
		t.Error("the hourly cascade brake was not enforced")
	}
	if !strings.Contains(why, "last hour") {
		t.Errorf("reason = %q, want it to mention the hourly limit", why)
	}
}

func TestAllowEnforcesModelCooldown(t *testing.T) {
	ctx := context.Background()
	m, st := armed(t)
	mustSet(t, m, "model_cooldown_min", "15")

	now := time.Now()
	if err := st.UpsertPosition(ctx, store.Position{
		GiftID: 1, Key: key, BuyPrice: 10, BoughtAt: now.Add(-5 * time.Minute),
		Status: store.StatusSold, Source: "test",
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	ok, why := m.Allow(ctx, key, 10, now)
	if ok {
		t.Error("the per-model cooldown was not enforced")
	}
	if !strings.Contains(why, "cooldown") {
		t.Errorf("reason = %q, want it to mention the cooldown", why)
	}

	other := tonnel.ModelKey{Name: "Plush Pepe", Model: "Blue Steel"}
	if ok, why := m.Allow(ctx, other, 10, now); !ok {
		t.Errorf("the cooldown must be per model, but another model was refused: %s", why)
	}
}

func TestAllowRespectsBalanceReserve(t *testing.T) {
	ctx := context.Background()
	m, _ := armed(t)
	mustSet(t, m, "min_balance_reserve", "50")
	mustSet(t, m, "model_cooldown_min", "0")

	m.SetBalance(120)
	if ok, why := m.Allow(ctx, key, 60, time.Now()); !ok {
		t.Errorf("120 − 60 leaves 60, above the 50 reserve, but was refused: %s", why)
	}
	if ok, _ := m.Allow(ctx, key, 90, time.Now()); ok {
		t.Error("a purchase that would breach the reserve must be refused")
	}

	m.SetBalance(5)
	if ok, why := m.Allow(ctx, key, 10, time.Now()); ok {
		t.Error("a purchase larger than the balance must be refused")
	} else if !strings.Contains(why, "balance") {
		t.Errorf("reason = %q, want it to mention the balance", why)
	}
}

func TestConcentrationBlocksAfterPortfolioBootstrap(t *testing.T) {
	ctx := context.Background()
	m, st := armed(t)
	mustSet(t, m, "max_model_exposure_pct", "0.35")
	mustSet(t, m, "max_collection_exposure_pct", "0.80")
	mustSet(t, m, "max_positions_per_model", "5")
	mustSet(t, m, "model_cooldown_min", "0")
	m.SetBalance(100)
	for i, k := range []tonnel.ModelKey{{Name: "A", Model: "1"}, {Name: "B", Model: "1"}, {Name: "C", Model: "1"}} {
		if err := st.UpsertPosition(ctx, store.Position{GiftID: int64(i + 1), Key: k, BuyPrice: 100, BoughtAt: time.Now(), Status: store.StatusOpen, Source: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	ok, why := m.Allow(ctx, tonnel.ModelKey{Name: "A", Model: "1"}, 100, time.Now())
	if ok || !strings.Contains(why, "model exposure") {
		t.Fatalf("concentration allowed: %v %q", ok, why)
	}
}

// A run of failures means something systemic is wrong, not that we are unlucky.
func TestThreeFailuresDisarm(t *testing.T) {
	ctx := context.Background()
	m, _ := armed(t)

	var reason string
	m.OnDisarm = func(r string) { reason = r }

	m.RecordFailure(ctx)
	m.RecordFailure(ctx)
	if !m.Armed() {
		t.Fatal("two failures should not disarm")
	}
	m.RecordFailure(ctx)
	if m.Armed() {
		t.Error("three consecutive failures must disarm")
	}
	if !strings.Contains(reason, "row") {
		t.Errorf("disarm reason = %q, want it to explain the failure run", reason)
	}
}

func TestSuccessClearsTheFailureStreak(t *testing.T) {
	ctx := context.Background()
	m, _ := armed(t)

	m.RecordFailure(ctx)
	m.RecordFailure(ctx)
	m.RecordSuccess()
	m.RecordFailure(ctx)
	m.RecordFailure(ctx)

	if !m.Armed() {
		t.Error("a success in between must reset the streak")
	}
}

func TestPauseBlocksTemporarily(t *testing.T) {
	ctx := context.Background()
	m, _ := armed(t)

	m.Pause(time.Hour, "anti-bot block")
	if m.Armed() {
		t.Error("a paused manager must not report armed")
	}
	ok, why := m.Allow(ctx, key, 10, time.Now())
	if ok {
		t.Error("a paused manager must refuse purchases")
	}
	if !strings.Contains(why, "paused") {
		t.Errorf("reason = %q, want it to mention the pause", why)
	}

	// Re-arming clears the pause explicitly.
	if err := m.Arm(ctx); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	if !m.Armed() {
		t.Error("re-arming should clear the pause")
	}
}

func TestSetLimitValidatesInput(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)

	bad := []struct{ k, v string }{
		{"max_ticket", "-1"},
		{"max_ticket", "abc"},
		{"daily_budget", "-5"},
		{"max_positions", "1.5"},
		{"min_markup", "2"},
		{"max_exit_days", "0"},
		{"nonsense", "1"},
	}
	for _, c := range bad {
		if err := m.SetLimit(ctx, c.k, c.v); err == nil {
			t.Errorf("SetLimit(%q, %q) was accepted", c.k, c.v)
		}
	}

	mustSet(t, m, "model_cooldown_min", "30")
	if got := m.Limits().ModelCooldown; got != 30*time.Minute {
		t.Errorf("cooldown = %v, want 30m", got)
	}
	mustSet(t, m, "min_markup", "0.05")
	if got := m.Limits().MinMarkup; got != 0.05 {
		t.Errorf("min markup = %v, want 0.05", got)
	}
}

func TestLimitsPersist(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "risk.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	m, _ := New(ctx, st)
	mustSet(t, m, "max_ticket", "77")
	st.Close()

	st2, err := store.Open(filepath.Join(dir, "risk.db"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	m2, err := New(ctx, st2)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := m2.Limits().MaxTicket; got != 77 {
		t.Errorf("max_ticket after restart = %v, want 77", got)
	}
}

func TestDescribeMentionsUnsetLimits(t *testing.T) {
	m, _ := newManager(t)
	out := m.Describe(context.Background(), time.Now())
	if !strings.Contains(out, "not set") {
		t.Errorf("Describe should flag unset money limits:\n%s", out)
	}
}
