// Package logx configures the process-wide structured logger.
package logx

import (
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Setup installs a human-readable console logger at the given level.
// Accepted levels: trace, debug, info, warn, error.
func Setup(level string) {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil || lvl == zerolog.NoLevel {
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)
	log.Logger = zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.RFC3339,
	}).With().Timestamp().Logger()
}

// L returns the global logger.
func L() *zerolog.Logger { return &log.Logger }
