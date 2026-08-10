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
	"os"
	"os/signal"
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
	case "run", "smoke", "backfill", "dump":
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
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
			days = cfg.LookbackDays
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
  dump <x>   print one endpoint's raw JSON (feed, sales, sales-all, balance, mygifts)

Flags:
  -env path   env file to load (default ".env")
  -days n     backfill: days of history (default: LOOKBACK_DAYS)
`)
}
