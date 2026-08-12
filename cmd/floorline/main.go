// Command floorline is a Telegram trading desk for Tonnel gift listings.
//
//	floorline run       start the pollers and the bot
//	floorline smoke     probe every read endpoint and exit
//	floorline backfill  download trade history and exit
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"floorline/internal/app"
	"floorline/internal/config"
	"floorline/internal/logx"
)

func main() {
	envPath := flag.String("env", ".env", "path to the env file")
	days := flag.Int("days", 0, "backfill: days of history to download (default: LOOKBACK_DAYS)")
	flag.Usage = usage
	flag.Parse()

	cmd := "run"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}

	if err := config.LoadDotEnv(*envPath); err != nil {
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", *envPath, err)
		os.Exit(1)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration: %v\n", err)
		os.Exit(1)
	}
	logx.Setup(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cmd, cfg, *days); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Info().Msg("shut down")
			return
		}
		log.Fatal().Err(err).Msg("floorline failed")
	}
}

func run(ctx context.Context, cmd string, cfg *config.Config, days int) error {
	switch cmd {
	case "run", "smoke", "backfill", "dump", "portfolio", "gram", "history", "val", "login", "scan":
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}

	// Signing in needs neither a Tonnel session nor a database — it is the step
	// that happens before any of that works.
	if cmd == "login" {
		return app.Login(ctx, cfg, os.Stdin, os.Stdout)
	}

	if err := cfg.RequireAuth(); err != nil {
		return err
	}

	a, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.Close()

	switch cmd {
	case "scan":
		// The same sweep the poller runs, on demand. The feed only shows new
		// listings; this walks the standing book.
		collection := ""
		if flag.NArg() > 1 {
			collection = strings.Join(flag.Args()[1:], " ")
		}
		fmt.Println(stripHTML(a.Scan(ctx, collection).Text))
		return nil

	case "val":
		// The same valuation the /val card shows, without needing Telegram. This
		// is how a pricing change gets checked against real listings before it
		// is trusted with money.
		if flag.NArg() < 2 {
			return fmt.Errorf("val requires at least one gift id")
		}
		if err := a.SyncGram(ctx); err != nil {
			log.Warn().Err(err).Msg("GRAM quote unavailable; FX lines will be blank")
		}
		for _, arg := range flag.Args()[1:] {
			id, err := strconv.ParseInt(arg, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid gift id %q: %w", arg, err)
			}
			fmt.Printf("═══ gift %d ═══\n", id)
			fmt.Println(stripHTML(a.Val(ctx, id).Text))
			fmt.Println()
		}
		return nil

	case "history":
		if flag.NArg() < 2 {
			return fmt.Errorf("history requires a gift id")
		}
		id, err := strconv.ParseInt(flag.Arg(1), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid gift id: %w", err)
		}
		if err := a.SyncInventory(ctx); err != nil {
			return err
		}
		fmt.Println(a.PositionHistory(ctx, id).Text)
		return nil
	case "gram":
		if err := a.SyncGram(ctx); err != nil {
			return err
		}
		fmt.Println(a.Gram(ctx).Text)
		return nil
	case "portfolio":
		if err := a.SyncGram(ctx); err != nil {
			log.Warn().Err(err).Msg("GRAM quote unavailable; portfolio will use native-price history")
		}
		if err := a.SyncInventory(ctx); err != nil {
			return err
		}
		fmt.Println(a.PortfolioReport(ctx))
		return nil
	case "smoke":
		fmt.Println("Probing Tonnel endpoints…")
		fmt.Println()
		if err := a.Smoke(ctx, os.Stdout); err != nil {
			return err
		}
		fmt.Println()
		fmt.Println("All endpoints reachable.")
		return nil

	case "dump":
		target := "sales"
		if flag.NArg() > 1 {
			target = flag.Arg(1)
		}
		return a.Dump(ctx, os.Stdout, target)

	case "backfill":
		if days <= 0 {
			days = cfg.AttributeLookbackDays
		}
		fmt.Printf("Downloading %d days of trade history…\n", days)
		start := time.Now()
		err := a.Backfill(ctx, days, func(p app.BackfillProgress) {
			if p.Finished {
				fmt.Printf("\ndone: %d trades stored, oldest %s, %d requests in %s\n",
					p.Inserted, p.Oldest.Format("2006-01-02"), p.Requests,
					time.Since(start).Round(time.Second))
				return
			}
			fmt.Printf("\r  %3d/%d collections · %6d trades · %-28s",
				p.Done, p.Total, p.Inserted, truncateName(p.Collection))
		})
		return err

	default:
		if err := cfg.RequireBot(); err != nil {
			return err
		}
		return a.Run(ctx)
	}
}

// stripHTML renders the bot's Telegram markup as plain terminal text. The
// backend returns one HTML string for every surface, so the CLI unwraps rather
// than the views branching on where they are going.
func stripHTML(s string) string {
	r := strings.NewReplacer(
		"<b>", "", "</b>", "", "<i>", "", "</i>", "",
		"<pre>", "", "</pre>", "", "<code>", "", "</code>", "",
	)
	return html.UnescapeString(r.Replace(s))
}

// truncateName keeps the progress line from wrapping in a narrow terminal.
func truncateName(s string) string {
	if len(s) <= 28 {
		return s
	}
	return s[:27] + "…"
}

func usage() {
	fmt.Fprint(os.Stderr, `floorline — Tonnel gift trading desk

Usage:
  floorline [flags] [command]

Commands:
  run        start the pollers and the Telegram bot (default)
  smoke      probe every read endpoint and exit
  backfill   download trade history and exit
  portfolio  sync all inventory pages and print recommendations
  gram       refresh and print GRAM/USDT plus tracked floor lag
  history ID sync inventory and print the full position lifecycle
  val ID...  price one or more listings and print why they pass or fail
  scan [коллекция]  sweep the standing book for mispriced lots
  login      sign in to Telegram once, so the other marketplaces can be
             reached through their mini apps instead of as a bare API
  dump <x>   print one endpoint's raw JSON (feed, sales, sales-all, balance, mygifts)

Flags:
  -env path   env file to load (default ".env")
  -days n     backfill: days of history (default: LOOKBACK_DAYS)
`)
}
