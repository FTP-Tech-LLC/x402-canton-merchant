// Command echo is a paid echo server: one paid route behind the Canton gate.
//
//	X402_PAY_TO='merchant::1220<64 hex>' go run ./examples/echo
//
// Environment: X402_PAY_TO (required), X402_PRICE_CC (default 0.01),
// X402_FACILITATOR_URL (default FTP's), LISTEN_ADDR (default :8080), REDIS_URL
// (optional: the shared Redis store, for several processes).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/redis/go-redis/v9"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/goredis"
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

	store, closeRedis, err := storeFromEnv()
	if err != nil {
		return err
	}
	defer closeRedis()

	cfg := canton.Config{
		FacilitatorURL: os.Getenv("X402_FACILITATOR_URL"),
		Routes: map[string]canton.Route{
			"GET /v1/data": {PayTo: os.Getenv("X402_PAY_TO"), Price: canton.CC(envOr("X402_PRICE_CC", "0.01"))},
		},
		Logger: logger,
		Store:  store, // nil: in memory
	}
	paid, err := canton.NewGate(ctx, cfg) // ctx ends the startup wait; the reconciler lives until Close
	if err != nil {
		return err
	}

	e := echolib.New()
	e.HideBanner, e.HidePort = true, true
	e.Use(middleware.Recover())                     // outside the gate: a panic is never settled
	e.Use(cantonecho.Middleware(paid.Middleware())) // e.Use, not e.Pre; keyed by the literal path
	e.GET("/v1/data", func(c echolib.Context) error {
		payer, _ := canton.PayerFrom(c.Request().Context())
		// A returned error is rendered by echo's error handler and is never settled.
		return c.JSON(http.StatusOK, map[string]any{"data": 42, "payer": payer})
	})

	// Registration self-check: every paid key must answer an unpaid request with 402.
	if err := cantonecho.Check(e, cfg.Routes); err != nil {
		return err
	}

	budget := canton.RequestBudget(cfg)
	server := &http.Server{Addr: envOr("LISTEN_ADDR", ":8080"), Handler: e, ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10}
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
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
