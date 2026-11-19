// Command nethttp is a paid net/http server: one paid route, an IP limiter in
// front of the gate and a per-payer limiter inside the handler.
//
//	X402_PAY_TO='merchant::1220<64 hex>' go run ./examples/nethttp
//
// Environment: X402_PAY_TO (required), X402_PRICE_CC (default 0.01),
// X402_FACILITATOR_URL (default FTP's), LISTEN_ADDR (default :8080). It runs one
// process with the in-memory store, or the shared Redis store when REDIS_URL is
// set (for example redis://localhost:6379/1).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/goredis"
)

const (
	ipLimit      = 600 // requests per IP per window, unpaid probes included
	payerLimit   = 10  // paid calls per payer per window, the facilitator's per-payer settle cap
	limitWindow  = time.Minute
	writeTimeout = 30 * time.Second // ordinary: the gate lifts it after /verify
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	payTo := os.Getenv("X402_PAY_TO")
	price := envOr("X402_PRICE_CC", "0.01")
	store, closeRedis, err := storeFromEnv()
	if err != nil {
		return err
	}
	defer closeRedis()

	cfg := canton.Config{
		FacilitatorURL: os.Getenv("X402_FACILITATOR_URL"), // empty means the default
		Routes: map[string]canton.Route{
			"GET /v1/data": {PayTo: payTo, Price: canton.CC(price), Description: "Example data"},
		},
		Logger: logger,
		Store:  store, // nil: in memory
	}
	// NewGate gives the gate its own lifetime: its background reconciler runs until Close, which
	// comes after server.Shutdown has returned, so requests that drain during the grace period
	// are reconciled too. SupportedWait already bounds startup.
	paid, err := canton.NewGate(ctx, cfg) // ctx ends the startup wait only
	if err != nil {
		return err
	}
	gate := paid.Middleware()

	payerLimiter := newLimiter(payerLimit)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/data", func(w http.ResponseWriter, r *http.Request) {
		payer, _ := canton.PayerFrom(r.Context())
		// A 4xx from a paid handler is not settled, so the payer is not charged, except on
		// the retry of a payment that already moved: let a retry through, and the gate
		// answers any refusal there with 503 "do NOT pay again" anyway.
		if !canton.IsRetry(r.Context()) && !payerLimiter.allow(payer) {
			http.Error(w, "too many paid calls, slow down", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":42}`))
	})

	// The IP limiter sits in front of the gate. The hosted MCP calls from one IP,
	// so keep this cap high and let the per-payer limiter do the fine work.
	ipLimiter := newLimiter(ipLimit)
	// Mount the gate outermost around the mux. Any wrapper of the ResponseWriter in
	// front of it must implement Unwrap() http.ResponseWriter.
	handler := limitByIP(ipLimiter, gate(mux))

	// One number for every wait that must cover a whole paid request: the
	// shutdown grace below and a proxy's read timeout. The gate lifts the write
	// deadline itself after /verify passes, so WriteTimeout stays ordinary.
	budget := canton.RequestBudget(cfg)
	server := &http.Server{
		Addr:              envOr("LISTEN_ADDR", ":8080"),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      writeTimeout,
		MaxHeaderBytes:    64 << 10, // a PAYMENT-SIGNATURE is 8 to 16 KiB; proxies must accept 32 KiB
	}
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	// In-flight paid requests may be mid-settle: give them the full budget.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	// One last bounded pass for what the drain left behind (claims, locks, records held only in memory).
	closeCtx, closeCancel := context.WithTimeout(context.Background(), budget) // at least RequestBudget
	defer closeCancel()
	if err := paid.Close(closeCtx); err != nil {
		logger.Error("gate close", slog.String("error", err.Error()))
	}
	if shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
		return shutdownErr
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func limitByIP(l *limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = strings.TrimSpace(r.RemoteAddr)
		}
		// Nothing in front of the gate may answer 4xx to a request that carries a payment
		// (a retry of a charged payment would lose its "do NOT pay again"): the gate's
		// lock and the facilitator's quotas bound those. Count only unpaid probes.
		if r.Header.Get("PAYMENT-SIGNATURE") == "" && !l.allow(host) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limiter is a fixed-window counter per key. Enough for an example; use a real
// limiter (and a shared one for several instances) in production.
type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Time
	counts map[string]int
}

func newLimiter(limit int) *limiter {
	return &limiter{limit: limit, counts: map[string]int{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := time.Now(); now.Sub(l.window) >= limitWindow {
		l.window, l.counts = now, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.limit
}

// storeFromEnv returns the shared Redis store when REDIS_URL is set (the setup for
// several processes: use a dedicated database with maxmemory-policy noeviction),
// and a nil store, which means the in-memory one, otherwise.
func storeFromEnv() (store canton.Store, closeRedis func(), err error) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		return nil, func() {}, nil
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		return nil, nil, err
	}
	options.ContextTimeoutEnabled = true // go-redis then honours the gate's per-call deadlines
	rdb := redis.NewClient(options)
	return goredis.NewStore(rdb, "x402:example:"), func() { _ = rdb.Close() }, nil
}
