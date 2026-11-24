package audible

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/timeutil"
)

// Endpoints are the three hosts the client talks to, as base URLs with a scheme
// and no trailing slash. EndpointsFor gives a marketplace's real ones; a test, or
// a build with the audiblefake tag, points all three at a fake.
type Endpoints struct {
	// Amazon serves the sign-in page and the maplanding redirect
	// (https://www.amazon.<tld>).
	Amazon string
	// AmazonAPI is the identity API: register, token refresh, deregister
	// (https://api.amazon.<tld>).
	AmazonAPI string
	// API is the Audible API: library, license, annotations
	// (https://api.audible.<tld>).
	API string
}

// EndpointsFor returns the marketplace's endpoints. In a build with the
// audiblefake tag and AUDIOSILO_BRIDGE_AUDIBLE_FAKE_URL set, every endpoint is
// that URL instead (release builds never carry the tag, so the variable does
// nothing there).
func EndpointsFor(mp Marketplace) Endpoints {
	if u := FakeURL(); u != "" {
		return Endpoints{Amazon: u, AmazonAPI: u, API: u}
	}
	return Endpoints{
		Amazon:    "https://" + mp.AmazonDomain(),
		AmazonAPI: "https://" + mp.AmazonAPIDomain(),
		API:       "https://" + mp.APIDomain(),
	}
}

// Option adjusts a Client, or one call of Register, BuildAuthorizeURL,
// RefreshAccessToken or Deregister.
type Option func(*settings)

// WithEndpoints points the calls at e instead of the marketplace's hosts.
func WithEndpoints(e Endpoints) Option {
	return func(s *settings) {
		e.Amazon = strings.TrimRight(e.Amazon, "/")
		e.AmazonAPI = strings.TrimRight(e.AmazonAPI, "/")
		e.API = strings.TrimRight(e.API, "/")
		s.endpoints = &e
	}
}

// WithHTTPClient uses h for the API calls (its Timeout is kept). A download
// uses h's Transport with no overall timeout.
func WithHTTPClient(h *http.Client) Option {
	return func(s *settings) { s.http = h }
}

// WithLogger logs what a call notices about Audible's answers (a library
// title's implausible release date) to l; never a token or a request.
func WithLogger(l *slog.Logger) Option {
	return func(s *settings) { s.log = l }
}

// withSleep replaces the backoff's wait (tests).
func withSleep(f func(context.Context, time.Duration) error) Option {
	return func(s *settings) { s.sleep = f }
}

// withOddDateLog gives the client its own memory of the odd dates it logged
// (tests: the process's would carry over from one test to the next).
func withOddDateLog(o *oddDateLog) Option {
	return func(s *settings) { s.oddDates = o }
}

// withStall replaces how long a download waits for its next byte (tests).
func withStall(d time.Duration) Option {
	return func(s *settings) { s.stall = d }
}

// settings is what the options set, resolved against a marketplace.
type settings struct {
	endpoints *Endpoints
	http      *http.Client
	sleep     func(context.Context, time.Duration) error
	stall     time.Duration // 0: downloadStall
	log       *slog.Logger  // nil: no log
	oddDates  *oddDateLog   // nil: oddDatesLogged, the process's
}

// endpointsOf is the endpoints opts set, else mp's.
func endpointsOf(mp Marketplace, opts []Option) Endpoints {
	return *applyOpts(mp, opts).endpoints
}

// applyOpts applies opts, with mp's endpoints when they set none.
func applyOpts(mp Marketplace, opts []Option) settings {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if s.endpoints == nil {
		e := EndpointsFor(mp)
		s.endpoints = &e
	}
	return s
}

// resolve applies opts over the defaults for mp.
func resolve(mp Marketplace, timeout time.Duration, opts []Option) settings {
	s := applyOpts(mp, opts)
	if s.http == nil {
		s.http = &http.Client{Timeout: timeout, Transport: wrapTransport(http.DefaultTransport)}
	}
	if s.sleep == nil {
		s.sleep = timeutil.SleepCtx
	}
	if s.stall <= 0 {
		s.stall = downloadStall
	}
	if s.oddDates == nil {
		s.oddDates = oddDatesLogged
	}
	return s
}
