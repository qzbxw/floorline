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
	case "run", "smoke", "backfill":
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

	case "backfill":
		if days <= 0 {
			days = cfg.LookbackDays
		}
		fmt.Printf("Downloading %d days of trade history…\n", days)
		start := time.Now()
		err := a.Backfill(ctx, days, func(p app.BackfillProgress) {
			if p.Done {
				fmt.Printf("done: %d new trades, oldest %s, took %s\n",
					p.Inserted, p.Oldest.Format(time.RFC3339), time.Since(start).Round(time.Second))
				return
			}
			fmt.Printf("  page %d · %d new · back to %s\n",
				p.Pages, p.Inserted, p.Oldest.Format("2006-01-02 15:04"))
		})
		return err

	default:
		if err := cfg.RequireBot(); err != nil {
			return err
		}
		return a.Run(ctx)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `floorline — Tonnel gift trading desk

Usage:
  floorline [flags] [command]

Commands:
  run        start the pollers and the Telegram bot (default)
  smoke      probe every read endpoint and exit
  backfill   download trade history and exit

Flags:
  -env path   env file to load (default ".env")
  -days n     backfill: days of history (default: LOOKBACK_DAYS)
`)
}
