package canton

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	// DefaultFacilitatorURL is FTP's public MainNet facilitator.
	DefaultFacilitatorURL = "https://facilitator.ftptech.xyz"

	defaultSettleTimeout = 65 * time.Second // one /settle can take about 57 s: 45 s execute + 20 x 600 ms poll
	defaultSettleCalls   = 4
	defaultSupportedWait = 60 * time.Second
	defaultExecuteBefore = 120
	minExecuteBefore     = 1
	maxExecuteBefore     = 600 // the facilitator's CANTON_X402_TF_MAX_EXECUTE_BEFORE_S default
	maxMemoBytes         = 256
)

// supportedAttemptTimeout bounds one startup attempt: GET /supported and the
// SDK's own initialization. A variable so tests can run fast.
var supportedAttemptTimeout = 10 * time.Second

// Config configures NewMiddleware. Only Routes is required.
type Config struct {
	// FacilitatorURL is the facilitator base URL. Default DefaultFacilitatorURL.
	FacilitatorURL string
	// Routes maps "METHOD /literal/path" to the route's payment terms.
	Routes map[string]Route
	// Store keeps the one-delivery state. Default NewMemoryStore(): one process only.
	Store Store
	// Timeout is the deadline the SDK puts on /verify. It does not bound your
	// handler (the SDK hands the handler the plain request) and does not cut
	// /settle, which runs on SettleTimeout. Default: the largest route
	// ExecuteBeforeSeconds.
	Timeout time.Duration
	// SettleTimeout bounds one /settle call. Default 65s.
	SettleTimeout time.Duration
	// SettleCalls is the most /settle calls for one paid request, the SDK's own
	// retry included. Default 4.
	SettleCalls int
	// SettleOnRedirect lets a 3xx from a paid handler be settled. Default false:
	// a 3xx becomes a 502 and is not settled.
	SettleOnRedirect bool
	// SupportedWait is how long NewMiddleware retries GET /supported. Default 60s.
	SupportedWait time.Duration
	// HTTPClient is used for facilitator calls. Default: a plain http.Client.
	// The plugin uses a copy with Timeout 0: every call is bounded by its own
	// context, so a Timeout here cannot silently cut a /settle short.
	HTTPClient *http.Client
	// Logger receives structured logs. Default slog.Default().
	Logger *slog.Logger
}

// Route is the payment terms of one paid route.
type Route struct {
	// PayTo is the merchant party, "<hint>::1220<64 hex>".
	PayTo string
	// Price is what one call costs: CC, Token or Atomic.
	Price Price
	// ExecuteBeforeSeconds is how long the signed payment stays valid. Default 120, allowed 1..600.
	ExecuteBeforeSeconds int
	// Memo is optional: at most 256 UTF-8 bytes, no leading or trailing whitespace.
	Memo string
	// Description goes into the 402 resource info.
	Description string
	// MimeType goes into the 402 resource info.
	MimeType string
}

// resolved is Config with defaults applied and every field checked.
type resolved struct {
	Config
	routes []checkedRoute
}

func (c Config) resolve() (resolved, error) {
	var problems []error
	out := resolved{Config: c}
	if out.FacilitatorURL == "" {
		out.FacilitatorURL = DefaultFacilitatorURL
	}
	if parsed, err := url.Parse(out.FacilitatorURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		problems = append(problems, fmt.Errorf("FacilitatorURL %q must be an http(s) URL", out.FacilitatorURL))
	}
	routes, routeProblems := checkRoutes(c.Routes)
	out.routes = routes
	problems = append(problems, routeProblems...)

	problems = append(problems, out.applyDefaults()...)
	return out, errors.Join(problems...)
}

func (c *resolved) applyDefaults() []error {
	var problems []error
	if c.Store == nil {
		c.Store = NewMemoryStore()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
	c.Timeout = durationOrDefault(c.Timeout, largestExecuteBefore(c.routes), "Timeout", &problems)
	c.SettleTimeout = durationOrDefault(c.SettleTimeout, defaultSettleTimeout, "SettleTimeout", &problems)
	c.SupportedWait = durationOrDefault(c.SupportedWait, defaultSupportedWait, "SupportedWait", &problems)
	switch {
	case c.SettleCalls == 0:
		c.SettleCalls = defaultSettleCalls
	case c.SettleCalls < 0:
		problems = append(problems, fmt.Errorf("SettleCalls %d must be positive", c.SettleCalls))
	}
	return problems
}

func durationOrDefault(value, fallback time.Duration, name string, problems *[]error) time.Duration {
	switch {
	case value == 0:
		return fallback
	case value < 0:
		*problems = append(*problems, fmt.Errorf("%s %s must be positive", name, value))
		return fallback
	default:
		return value
	}
}

func largestExecuteBefore(routes []checkedRoute) time.Duration {
	largest := 0
	for _, route := range routes {
		largest = max(largest, route.executeBeforeSeconds)
	}
	if largest == 0 {
		largest = defaultExecuteBefore
	}
	return time.Duration(largest) * time.Second
}

// RequestBudget is the longest one paid request can take with this config: the
// larger of Timeout and the longest route lifetime, every /settle call, the
// pauses between them, and the Store calls after settle. Use it as the minimum
// for http.Server.WriteTimeout, a proxy's read timeout and the shutdown grace.
// The gate lifts the write deadline itself after /verify passes when the
// server's ResponseWriter supports it; the other two are yours to set.
func RequestBudget(cfg Config) time.Duration {
	conf, _ := cfg.resolve()
	lifetime := largestExecuteBefore(conf.routes)
	calls := conf.SettleCalls
	if calls <= 0 {
		calls = defaultSettleCalls // an invalid config is refused by NewMiddleware; this stays positive
	}
	return max(conf.Timeout, lifetime) + time.Duration(calls)*conf.SettleTimeout +
		time.Duration(calls-1)*productionRetryAfterCap + (postVerifyStoreCalls+preLockStoreCalls)*storeCallTimeout
}
