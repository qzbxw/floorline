package venue

import (
	"context"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/time/rate"
)

// UserAgent is fixed per process and matched to the TLS profile below. A stable
// fingerprint paired with a churning User-Agent is something no real browser
// produces, and it is exactly what an anti-bot layer looks for.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// InitDataSource mints a Telegram mini-app payload for a venue.
type InitDataSource interface {
	InitData(ctx context.Context, venue string) (string, error)
	Invalidate(venue string)
}

// Pacer throttles requests to a venue.
type Pacer interface {
	Wait(ctx context.Context) error
}

// HumanPace is the request budget for a marketplace.
//
// MRKT banned this account for two weeks over traffic that did not look like
// the mini app, and rate is half of that tell: a person tapping through
// listings does not sustain a request a second, and never does so evenly for
// hours. One every two seconds with a small burst is a fast human and still far
// more throughput than the desk needs.
func HumanPace() Pacer { return rate.NewLimiter(rate.Every(2*time.Second), 3) }

// NewHTTPClient builds a browser-grade transport. Every venue sits behind the
// same kind of anti-bot layer.
func NewHTTPClient(timeoutSeconds int) (tls_client.HttpClient, error) {
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeout(timeoutSeconds),
		tls_client.WithClientProfile(profiles.Chrome_131),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithCatchPanics(),
	)
}
