// Package config loads Floorline settings from the environment (and an optional .env file).
//
// Everything that is a *policy* threshold lives here. Everything that is a *money*
// limit lives in package risk, because those are mutable at runtime via /limits set
// and are persisted so a restart cannot silently reset a spent daily budget.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// SignalGates decides what gets pushed to Telegram as an actionable card.
type SignalGates struct {
	MinEdge     float64 // risk-adjusted net edge after fees and uncertainty buffer
	MinVelocity float64 // sales per day over the lookback window
	MinSales    int     // absolute number of sales over the lookback window
	MaxMADRatio float64 // median absolute deviation / median — price stability
	MinTrend    float64 // median(7d) / median(14d) — reject falling knives
	MinPrice    float64 // ignore dust
	MaxPrice    float64 // 0 = unlimited
	// MinNet is the absolute profit a trade has to be worth in GRAM. A
	// percentage on its own cannot tell +0.07 on a 3-GRAM lot apart from a
	// trade, and the chat filled with the former.
	MinNet float64
	// MaxExitDays rejects the warehouse. Several cards in the 12 Aug log
	// projected "быстро за ~8.8д" and one at ~10д; that is not a flip, and
	// holding a thin model for a fortnight is how a small bank stops working.
	MaxExitDays float64
}

// AutoGates is layered strictly on top of SignalGates for unattended buying.
type AutoGates struct {
	MinEdge     float64
	MinVelocity float64
	MinSales    int
	// MinTurnover is distinct gifts divided by trades. The endpoint exposes no
	// counterparties, so this is the wash-trading guard: a tape made of one
	// gift changing hands repeatedly must not qualify for unattended buying.
	MinTurnover    float64
	MaxMADRatio    float64
	MinTrend       float64
	MaxDataAge     time.Duration // refuse to act on stale market data
	MaxGramMove15m float64       // pause unattended buys during sharp GRAM moves
}

// Config is immutable for the lifetime of the process.
type Config struct {
	BotToken string
	OwnerID  int64

	AuthData string
	// TonnelOrigin is the front-end origin sent with every Tonnel request.
	// Empty means the client's default.
	TonnelOrigin string

	// Cross-market comparison credentials. Each venue is independent: with
	// none of them set, cards simply omit the comparison line.
	PortalsAuth string
	MrktInit    string // Telegram WebApp initData, exchanged for a token that self-renews
	MrktToken   string // a ready bearer token, if you would rather paste that

	// A real Telegram account, used to open the marketplaces' mini apps and
	// mint a fresh initData whenever one is needed. This is what makes the
	// HTTPS calls look like a person using the app rather than an API being
	// driven directly — the distinction MRKT bans accounts over.
	TelegramAppID   int
	TelegramAppHash string
	TelegramPhone   string
	SessionPath     string

	DBPath   string
	LogLevel string

	TonnelFee  float64 // 0.005 purchase referral added on top of the displayed ask
	PortalsFee float64 // cross-market comparison only
	MrktFee    float64 // cross-market comparison only
	Undercut   float64 // how far below the best competing ask we model our exit

	Sig  SignalGates
	Auto AutoGates

	LookbackDays          int
	AttributeLookbackDays int

	FeedInterval time.Duration
	// ScanInterval is how often the standing book is swept. The feed only ever
	// shows new listings; most of the market is a gift that has been sitting at
	// the same price and became interesting because everything moved around it.
	ScanInterval      time.Duration
	StatsInterval     time.Duration
	SalesInterval     time.Duration
	InventoryInterval time.Duration
	BookCacheTTL      time.Duration

	// SalesBatch is how many collections the trade poller refreshes per tick.
	// Trade history is only readable per collection, so the whole market is
	// covered by rotation rather than by one global query.
	SalesBatch int
	// SalesWindow is how far back the incremental trade poller looks. It only
	// has to comfortably exceed the time for one full rotation of the
	// collection list; the deep history comes from the backfill.
	SalesWindow time.Duration

	ReadRPS   float64
	ReadBurst int

	HTTPTimeout           time.Duration
	CrossMarkTTL          time.Duration
	ShadowMode            bool
	CalibrationMinSignals int
	CalibrationMinDays    int
	GramQuoteURL          string
	GramQuoteInterval     time.Duration
}

// LoadDotEnv reads KEY=VALUE lines from path into the environment.
// Existing environment variables always win, so `FOO=1 ./floorline` overrides the file.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// Load builds a Config from the environment, applying the documented defaults.
func Load() (*Config, error) {
	c := &Config{
		BotToken:     os.Getenv("TELEGRAM_BOT_TOKEN"),
		OwnerID:      envInt64("TELEGRAM_OWNER_ID", 0),
		AuthData:     os.Getenv("TONNEL_AUTH_DATA"),
		TonnelOrigin: os.Getenv("TONNEL_ORIGIN"),
		PortalsAuth:  os.Getenv("PORTALS_AUTH_DATA"),
		MrktInit:     os.Getenv("MRKT_INIT_DATA"),
		MrktToken:    os.Getenv("MRKT_TOKEN"),

		TelegramAppID:   envInt("TELEGRAM_APP_ID", 0),
		TelegramAppHash: os.Getenv("TELEGRAM_APP_HASH"),
		TelegramPhone:   os.Getenv("TELEGRAM_PHONE"),
		SessionPath:     envStr("TELEGRAM_SESSION", "./tgsession.json"),

		DBPath:   envStr("DB_PATH", "./floorline.db"),
		LogLevel: envStr("LOG_LEVEL", "info"),

		TonnelFee:  envFloat("TONNEL_FEE", 0.005),
		PortalsFee: envFloat("PORTALS_FEE", 0.02),
		MrktFee:    envFloat("MRKT_FEE", 0.02),
		Undercut:   envFloat("UNDERCUT", 0.01),

		Sig: SignalGates{
			// 1% was set when the exit was overstated by roughly a factor of
			// two, so it was really admitting anything above break-even. With
			// the exit honest, most of what it let through is now negative and
			// disappears on its own; this is the bar for what is left.
			MinEdge:     envFloat("SIG_MIN_EDGE", 0.045),
			MinVelocity: envFloat("SIG_MIN_VELOCITY", 0.5),
			MinSales:    envInt("SIG_MIN_SALES", 6),
			MaxMADRatio: envFloat("SIG_MAX_MAD_RATIO", 0.35),
			MinTrend:    envFloat("SIG_MIN_TREND", 0.90),
			MinPrice:    envFloat("SIG_MIN_PRICE", 1),
			MaxPrice:    envFloat("SIG_MAX_PRICE", 0),
			MinNet:      envFloat("SIG_MIN_NET", 0.25),
			MaxExitDays: envFloat("SIG_MAX_EXIT_DAYS", 4),
		},
		Auto: AutoGates{
			MinEdge:        envFloat("AUTOBUY_MIN_EDGE", 0.06),
			MinVelocity:    envFloat("AUTOBUY_MIN_VELOCITY", 1.0),
			MinSales:       envInt("AUTOBUY_MIN_SALES", 10),
			MinTurnover:    envFloat("AUTOBUY_MIN_TURNOVER", 0.6),
			MaxMADRatio:    envFloat("AUTOBUY_MAX_MAD_RATIO", 0.25),
			MinTrend:       envFloat("AUTOBUY_MIN_TREND", 0.95),
			MaxDataAge:     envDur("AUTOBUY_MAX_DATA_AGE", 5*time.Minute),
			MaxGramMove15m: envFloat("AUTOBUY_MAX_GRAM_MOVE_15M", .03),
		},

		LookbackDays:          envInt("LOOKBACK_DAYS", 14),
		AttributeLookbackDays: envInt("ATTRIBUTE_LOOKBACK_DAYS", 60),

		FeedInterval:      envDur("POLL_FEED", 2*time.Second),
		ScanInterval:      envDur("POLL_SCAN", 12*time.Minute),
		StatsInterval:     envDur("POLL_STATS", 60*time.Second),
		SalesInterval:     envDur("POLL_SALES", 25*time.Second),
		InventoryInterval: envDur("POLL_INVENTORY", 60*time.Second),
		BookCacheTTL:      envDur("BOOK_CACHE_TTL", 15*time.Second),

		SalesBatch:  envInt("SALES_BATCH", 4),
		SalesWindow: envDur("SALES_WINDOW", 45*time.Minute),

		ReadRPS:   envFloat("READ_RPS", 2),
		ReadBurst: envInt("READ_BURST", 5),

		HTTPTimeout:           envDur("HTTP_TIMEOUT", 20*time.Second),
		CrossMarkTTL:          envDur("CROSS_MARKET_TTL", 5*time.Minute),
		ShadowMode:            envBool("SHADOW_MODE", true),
		CalibrationMinSignals: envInt("CALIBRATION_MIN_SIGNALS", 200),
		CalibrationMinDays:    envInt("CALIBRATION_MIN_DAYS", 14),
		GramQuoteURL:          envStr("GRAM_QUOTE_URL", "https://api.gateio.ws/api/v4"),
		GramQuoteInterval:     envDur("POLL_GRAM", 30*time.Second),
	}

	if c.Undercut < 0 || c.Undercut >= 0.5 {
		return nil, fmt.Errorf("UNDERCUT must be in [0, 0.5), got %v", c.Undercut)
	}
	if c.LookbackDays < 1 {
		return nil, fmt.Errorf("LOOKBACK_DAYS must be >= 1")
	}
	if c.AttributeLookbackDays < c.LookbackDays {
		return nil, fmt.Errorf("ATTRIBUTE_LOOKBACK_DAYS must be >= LOOKBACK_DAYS")
	}
	if c.ReadRPS <= 0 {
		return nil, fmt.Errorf("READ_RPS must be > 0")
	}
	return c, nil
}

func envBool(k string, def bool) bool {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// RequireBot reports whether the settings needed to actually run the bot are present.
func (c *Config) RequireBot() error {
	var missing []string
	if c.BotToken == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if c.OwnerID == 0 {
		missing = append(missing, "TELEGRAM_OWNER_ID")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}
	return nil
}

// RequireAuth reports whether a Tonnel authData is available.
func (c *Config) RequireAuth() error {
	if strings.TrimSpace(c.AuthData) == "" {
		return errors.New("TONNEL_AUTH_DATA is empty. It is the Telegram WebApp initData string, " +
			"which the current Tonnel front end keeps in memory rather than Local Storage: open the " +
			"mini app with DevTools, run `copy(Telegram.WebApp.initData)` in the console, or copy the " +
			"user_auth field out of any request to gifts2.tonnel.network in the Network tab")
	}
	return nil
}

func envStr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func envInt64(k string, def int64) int64 {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i
		}
	}
	return def
}

// envDur accepts either a Go duration ("2s", "1m30s") or a bare number of seconds.
func envDur(k string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(f * float64(time.Second))
	}
	return def
}
