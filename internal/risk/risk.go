// Package risk owns every constraint that stands between a signal and spending
// real money.
//
// The limits live here rather than in config because they are editable at
// runtime (/limits set) and persisted: a restart must never hand back a daily
// budget that has already been spent.
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"floorline/internal/store"
	"floorline/internal/tonnel"
)

const (
	kvLimits = "risk.limits"
	kvArmed  = "risk.armed"
)

// Limits are the hard money constraints.
type Limits struct {
	MaxTicket                float64       `json:"max_ticket"`          // biggest single purchase
	DailyBudget              float64       `json:"daily_budget"`        // total spend per UTC day
	MaxOpenPositions         int           `json:"max_positions"`       // inventory ceiling
	MaxBuysPerHour           int           `json:"max_buys_per_hour"`   // cascade brake
	ModelCooldown            time.Duration `json:"model_cooldown"`      // no repeat buys of one model
	MinBalanceReserve        float64       `json:"min_balance_reserve"` // never spend below this
	MinMarkup                float64       `json:"min_markup"`          // refuse to relist below entry+this
	MaxExitDays              float64       `json:"max_exit_days"`       // must be sellable this fast
	MaxModelExposurePct      float64       `json:"max_model_exposure_pct"`
	MaxCollectionExposurePct float64       `json:"max_collection_exposure_pct"`
	MaxPositionsPerModel     int           `json:"max_positions_per_model"`
}

// DefaultLimits deliberately leaves MaxTicket and DailyBudget at zero. Arming
// is refused until the operator sets them, so there is no default budget for
// the bot to spend.
func DefaultLimits() Limits {
	return Limits{
		MaxTicket:                0,
		DailyBudget:              0,
		MaxOpenPositions:         8,
		MaxBuysPerHour:           3,
		ModelCooldown:            15 * time.Minute,
		MinBalanceReserve:        0,
		MinMarkup:                0.03,
		MaxExitDays:              3,
		MaxModelExposurePct:      .15,
		MaxCollectionExposurePct: .30,
		MaxPositionsPerModel:     2,
	}
}

// Manager enforces the limits and holds the armed/disarmed state.
type Manager struct {
	st *store.Store

	mu            sync.RWMutex
	limits        Limits
	armed         bool
	disabledUntil time.Time
	lastReason    string
	failStreak    int
	balance       float64
	balanceKnown  bool

	// OnDisarm is called whenever the bot disarms itself, so the operator finds
	// out immediately rather than by noticing nothing is happening.
	OnDisarm func(reason string)
}

// New builds a Manager and restores persisted state.
func New(ctx context.Context, st *store.Store) (*Manager, error) {
	m := &Manager{st: st, limits: DefaultLimits()}

	raw, err := st.GetKV(ctx, kvLimits)
	if err != nil {
		return nil, err
	}
	if raw != "" {
		var l Limits
		if err := json.Unmarshal([]byte(raw), &l); err == nil {
			m.limits = l
			if m.limits.MaxModelExposurePct <= 0 {
				m.limits.MaxModelExposurePct = .15
			}
			if m.limits.MaxCollectionExposurePct <= 0 {
				m.limits.MaxCollectionExposurePct = .30
			}
			if m.limits.MaxPositionsPerModel <= 0 {
				m.limits.MaxPositionsPerModel = 2
			}
		}
	}
	armed, err := st.GetKV(ctx, kvArmed)
	if err != nil {
		return nil, err
	}
	m.armed = armed == "1"
	return m, nil
}

// Limits returns a copy of the current limits.
func (m *Manager) Limits() Limits {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.limits
}

// Armed reports whether unattended buying is enabled right now.
func (m *Manager) Armed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.armed && time.Now().After(m.disabledUntil)
}

// LastReason returns why the bot last refused or disarmed.
func (m *Manager) LastReason() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastReason
}

// Arm enables unattended buying. It refuses while the two limits that bound
// total loss are unset.
func (m *Manager) Arm(ctx context.Context) error {
	m.mu.Lock()
	if m.limits.MaxTicket <= 0 || m.limits.DailyBudget <= 0 {
		m.mu.Unlock()
		return fmt.Errorf("set max_ticket and daily_budget first: /limits set max_ticket 50")
	}
	m.armed = true
	m.disabledUntil = time.Time{}
	m.failStreak = 0
	m.lastReason = ""
	m.mu.Unlock()
	return m.st.SetKV(ctx, kvArmed, "1")
}

// Disarm turns unattended buying off and records why.
func (m *Manager) Disarm(ctx context.Context, reason string) error {
	m.mu.Lock()
	wasArmed := m.armed
	m.armed = false
	m.lastReason = reason
	cb := m.OnDisarm
	m.mu.Unlock()

	if wasArmed && cb != nil && reason != "" {
		cb(reason)
	}
	return m.st.SetKV(ctx, kvArmed, "0")
}

// SetBalance records the last known account balance.
func (m *Manager) SetBalance(v float64) {
	m.mu.Lock()
	m.balance, m.balanceKnown = v, true
	m.mu.Unlock()
}

// Balance returns the last known balance and whether it has ever been read.
func (m *Manager) Balance() (float64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.balance, m.balanceKnown
}

// RecordFailure counts a failed purchase and disarms after a run of them,
// on the assumption that something systemic is wrong rather than unlucky.
func (m *Manager) RecordFailure(ctx context.Context) {
	m.mu.Lock()
	m.failStreak++
	n := m.failStreak
	m.mu.Unlock()

	if n >= 3 {
		_ = m.Disarm(ctx, fmt.Sprintf("%d purchases failed in a row", n))
	}
}

// RecordSuccess clears the failure streak.
func (m *Manager) RecordSuccess() {
	m.mu.Lock()
	m.failStreak = 0
	m.mu.Unlock()
}

// Pause suspends buying for a while without clearing the armed flag. Used for
// transient trouble such as an anti-bot block.
func (m *Manager) Pause(d time.Duration, reason string) {
	m.mu.Lock()
	until := time.Now().Add(d)
	if until.After(m.disabledUntil) {
		m.disabledUntil = until
	}
	m.lastReason = reason
	m.mu.Unlock()
}

// DisabledUntil returns when a pause expires.
func (m *Manager) DisabledUntil() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.disabledUntil
}

// Allow decides whether a purchase of `price` for `key` may proceed right now.
// The returned string explains any refusal and is surfaced in /status.
func (m *Manager) Allow(ctx context.Context, key tonnel.ModelKey, price float64, now time.Time) (bool, string) {
	m.mu.RLock()
	l := m.limits
	armed := m.armed
	disabledUntil := m.disabledUntil
	balance, balanceKnown := m.balance, m.balanceKnown
	m.mu.RUnlock()

	if !armed {
		return false, "auto-buy disarmed"
	}
	if now.Before(disabledUntil) {
		return false, fmt.Sprintf("paused for another %s", disabledUntil.Sub(now).Round(time.Second))
	}
	if l.MaxTicket <= 0 || l.DailyBudget <= 0 {
		return false, "limits not configured"
	}
	if price > l.MaxTicket {
		return false, fmt.Sprintf("price %.2f above max_ticket %.2f", price, l.MaxTicket)
	}

	day := now.UTC().Format("2006-01-02")
	spend, err := m.st.SpendToday(ctx, day)
	if err != nil {
		return false, "ledger unavailable: " + err.Error()
	}
	if spend.Spent+price > l.DailyBudget {
		return false, fmt.Sprintf("daily budget exhausted (%.2f of %.2f spent)", spend.Spent, l.DailyBudget)
	}

	if l.MaxOpenPositions > 0 {
		open, err := m.st.CountOpenPositions(ctx)
		if err != nil {
			return false, "position count unavailable: " + err.Error()
		}
		if open >= l.MaxOpenPositions {
			return false, fmt.Sprintf("%d open positions, limit %d", open, l.MaxOpenPositions)
		}
	}

	if l.MaxBuysPerHour > 0 {
		n, err := m.st.BuysSince(ctx, now.Add(-time.Hour))
		if err != nil {
			return false, "buy count unavailable: " + err.Error()
		}
		if n >= l.MaxBuysPerHour {
			return false, fmt.Sprintf("%d buys in the last hour, limit %d", n, l.MaxBuysPerHour)
		}
	}

	if l.ModelCooldown > 0 {
		last, err := m.st.LastBuyForModel(ctx, key)
		if err != nil {
			return false, "cooldown check unavailable: " + err.Error()
		}
		if !last.IsZero() && now.Sub(last) < l.ModelCooldown {
			return false, fmt.Sprintf("bought this model %s ago, cooldown %s",
				now.Sub(last).Round(time.Second), l.ModelCooldown)
		}
	}
	fit, why, err := m.portfolioFit(ctx, key, price, l)
	if err != nil {
		return false, "portfolio exposure unavailable: " + err.Error()
	}
	if fit <= 0 {
		return false, why
	}

	if balanceKnown && l.MinBalanceReserve > 0 && balance-price < l.MinBalanceReserve {
		return false, fmt.Sprintf("balance %.2f would drop below reserve %.2f", balance, l.MinBalanceReserve)
	}
	if balanceKnown && balance < price {
		return false, fmt.Sprintf("balance %.2f is below the price %.2f", balance, price)
	}

	return true, ""
}

// PortfolioFit is a soft ranking multiplier which reaches zero at a hard
// concentration limit. Allow uses the same calculation as a veto.
func (m *Manager) PortfolioFit(ctx context.Context, key tonnel.ModelKey, price float64) (float64, string, error) {
	return m.portfolioFit(ctx, key, price, m.Limits())
}

func (m *Manager) portfolioFit(ctx context.Context, key tonnel.ModelKey, price float64, l Limits) (float64, string, error) {
	mv, cv, count, err := m.st.PositionExposure(ctx, key)
	if err != nil {
		return 0, "", err
	}
	if l.MaxPositionsPerModel > 0 && count >= l.MaxPositionsPerModel {
		return 0, fmt.Sprintf("%d positions in this model, limit %d", count, l.MaxPositionsPerModel), nil
	}
	positions, err := m.st.OpenPositions(ctx)
	if err != nil {
		return 0, "", err
	}
	// Percentage limits need an actual portfolio denominator. During bootstrap
	// the per-model count limit is the meaningful guard; otherwise the first
	// purchase would necessarily be 100% concentrated and no portfolio could
	// ever be built.
	if len(positions) < 3 {
		return 1, "", nil
	}
	nav := 0.0
	for _, p := range positions {
		if p.BuyPrice > 0 {
			nav += p.BuyPrice
		} else if p.ListPrice > 0 {
			nav += p.ListPrice
		}
	}
	m.mu.RLock()
	if m.balanceKnown {
		nav += m.balance
	}
	m.mu.RUnlock()
	if nav+price <= 0 {
		return 1, "", nil
	}
	nav += price
	mPct := (mv + price) / nav
	cPct := (cv + price) / nav
	if l.MaxModelExposurePct > 0 && mPct > l.MaxModelExposurePct {
		return 0, fmt.Sprintf("model exposure %.0f%% above %.0f%%", mPct*100, l.MaxModelExposurePct*100), nil
	}
	if l.MaxCollectionExposurePct > 0 && cPct > l.MaxCollectionExposurePct {
		return 0, fmt.Sprintf("collection exposure %.0f%% above %.0f%%", cPct*100, l.MaxCollectionExposurePct*100), nil
	}
	fit := 1.0
	if l.MaxModelExposurePct > 0 {
		fit = math.Min(fit, 1-mPct/l.MaxModelExposurePct*.5)
	}
	if l.MaxCollectionExposurePct > 0 {
		fit = math.Min(fit, 1-cPct/l.MaxCollectionExposurePct*.5)
	}
	return math.Max(fit, .1), "", nil
}

// Commit books a completed purchase against the daily budget.
func (m *Manager) Commit(ctx context.Context, price float64, now time.Time) error {
	day := now.UTC().Format("2006-01-02")
	m.mu.Lock()
	if m.balanceKnown {
		m.balance -= price
	}
	m.mu.Unlock()
	return m.st.AddSpend(ctx, day, price)
}

// SetLimit updates one limit by name and persists the whole set.
func (m *Manager) SetLimit(ctx context.Context, key, value string) error {
	m.mu.Lock()
	l := m.limits
	if err := applyLimit(&l, key, value); err != nil {
		m.mu.Unlock()
		return err
	}
	m.limits = l
	m.mu.Unlock()

	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return m.st.SetKV(ctx, kvLimits, string(raw))
}

func applyLimit(l *Limits, key, value string) error {
	num := func() (float64, error) { return strconv.ParseFloat(strings.TrimSpace(value), 64) }

	switch strings.ToLower(strings.TrimSpace(key)) {
	case "max_ticket":
		v, err := num()
		if err != nil || v < 0 {
			return fmt.Errorf("max_ticket must be a non-negative number")
		}
		l.MaxTicket = v
	case "daily_budget":
		v, err := num()
		if err != nil || v < 0 {
			return fmt.Errorf("daily_budget must be a non-negative number")
		}
		l.DailyBudget = v
	case "max_positions":
		v, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || v < 0 {
			return fmt.Errorf("max_positions must be a non-negative integer")
		}
		l.MaxOpenPositions = v
	case "max_buys_per_hour":
		v, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || v < 0 {
			return fmt.Errorf("max_buys_per_hour must be a non-negative integer")
		}
		l.MaxBuysPerHour = v
	case "model_cooldown_min":
		v, err := num()
		if err != nil || v < 0 {
			return fmt.Errorf("model_cooldown_min must be a non-negative number of minutes")
		}
		l.ModelCooldown = time.Duration(v * float64(time.Minute))
	case "min_balance_reserve":
		v, err := num()
		if err != nil || v < 0 {
			return fmt.Errorf("min_balance_reserve must be a non-negative number")
		}
		l.MinBalanceReserve = v
	case "min_markup":
		v, err := num()
		if err != nil || v < 0 || v > 1 {
			return fmt.Errorf("min_markup must be a fraction between 0 and 1 (0.03 = 3%%)")
		}
		l.MinMarkup = v
	case "max_exit_days":
		v, err := num()
		if err != nil || v <= 0 {
			return fmt.Errorf("max_exit_days must be a positive number of days")
		}
		l.MaxExitDays = v
	case "max_model_exposure_pct":
		v, err := num()
		if err != nil || v <= 0 || v > 1 {
			return fmt.Errorf("max_model_exposure_pct must be in (0,1]")
		}
		l.MaxModelExposurePct = v
	case "max_collection_exposure_pct":
		v, err := num()
		if err != nil || v <= 0 || v > 1 {
			return fmt.Errorf("max_collection_exposure_pct must be in (0,1]")
		}
		l.MaxCollectionExposurePct = v
	case "max_positions_per_model":
		v, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || v <= 0 {
			return fmt.Errorf("max_positions_per_model must be positive")
		}
		l.MaxPositionsPerModel = v
	default:
		return fmt.Errorf("unknown limit %q; known: %s", key, strings.Join(LimitKeys(), ", "))
	}
	return nil
}

// LimitKeys lists the settable limit names.
func LimitKeys() []string {
	keys := []string{
		"max_ticket", "daily_budget", "max_positions", "max_buys_per_hour",
		"model_cooldown_min", "min_balance_reserve", "min_markup", "max_exit_days",
		"max_model_exposure_pct", "max_collection_exposure_pct", "max_positions_per_model",
	}
	sort.Strings(keys)
	return keys
}

// Describe renders the limits and today's usage for /limits.
func (m *Manager) Describe(ctx context.Context, now time.Time) string {
	l := m.Limits()
	day := now.UTC().Format("2006-01-02")
	spend, _ := m.st.SpendToday(ctx, day)
	open, _ := m.st.CountOpenPositions(ctx)

	var b strings.Builder
	fmt.Fprintf(&b, "max_ticket          %s\n", money(l.MaxTicket))
	fmt.Fprintf(&b, "daily_budget        %s  (spent today %.2f in %d buys)\n", money(l.DailyBudget), spend.Spent, spend.Buys)
	fmt.Fprintf(&b, "max_positions       %d  (open %d)\n", l.MaxOpenPositions, open)
	fmt.Fprintf(&b, "max_buys_per_hour   %d\n", l.MaxBuysPerHour)
	fmt.Fprintf(&b, "model_cooldown_min  %.0f\n", l.ModelCooldown.Minutes())
	fmt.Fprintf(&b, "min_balance_reserve %s\n", money(l.MinBalanceReserve))
	fmt.Fprintf(&b, "min_markup          %.1f%%\n", l.MinMarkup*100)
	fmt.Fprintf(&b, "max_exit_days       %.1f\n", l.MaxExitDays)
	fmt.Fprintf(&b, "max_model_exposure  %.0f%%\n", l.MaxModelExposurePct*100)
	fmt.Fprintf(&b, "max_collection_exp  %.0f%%\n", l.MaxCollectionExposurePct*100)
	fmt.Fprintf(&b, "max_pos_per_model   %d\n", l.MaxPositionsPerModel)
	return b.String()
}

func money(v float64) string {
	if v <= 0 {
		return "not set"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
