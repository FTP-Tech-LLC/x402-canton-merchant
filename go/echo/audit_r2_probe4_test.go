package echo_test

// Round-2 probes: a key with no exact echo route, and error handlers that
// panic or redirect.

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// A typo between the key and the route: Check passes, the paid key answers 404
// after /verify, and the real route is served free.
func TestR2CheckPassesAKeyWithNoRoute(t *testing.T) {
	f := testkit.StartFacilitator(t)
	routes := map[string]canton.Route{"GET /v1/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
	e := r2Echo()
	e.Use(cantonecho.Middleware(g.Middleware()))
	var hits atomic.Int32
	e.GET("/v1/date", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid content") })
	err := cantonecho.Check(e, routes)
	free := testkit.Do(e, "GET", "/v1/date", "")
	t.Logf("Check=%v; GET /v1/date unpaid=%d %q hits=%d", err, free.Code, free.Body.String(), hits.Load())
	assert.Error(t, err, "a paid key no echo route serves is a registration bug Check can see")
}

// A custom HTTPErrorHandler that panics on a paid route, Recover outside: not
// settled, the lock is released (the retry runs again).
func TestR2PanickingErrorHandlerIsNotSettledAndReleasesTheLock(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	var panicky atomic.Bool
	panicky.Store(true)
	e.HTTPErrorHandler = func(err error, c echolib.Context) {
		if panicky.Load() {
			panicky.Store(false)
			panic("error handler bug")
		}
		_ = c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	var hits atomic.Int32
	e.GET("/paid", func(c echolib.Context) error {
		if hits.Add(1) == 1 {
			_ = c.String(200, "partial")
			return errors.New("failed after writing")
		}
		return c.String(200, "paid")
	})
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	first := testkit.Do(e, "GET", "/paid", sig)
	retry := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("first=%d %q settles=%d; retry=%d %q", first.Code, first.Body.String(), f.SettleCalls.Load(), retry.Code, retry.Body.String())
	assert.NotEqual(t, 200, first.Code)
	assert.Equal(t, 200, retry.Code)
	assert.Equal(t, int32(1), f.SettleCalls.Load())
}

// An error handler that answers an auth error with a redirect to the login
// page: on a paid route the payer gets a 500 instead (not charged).
func TestR2ErrorRenderedAsRedirectBecomes500(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	e.HTTPErrorHandler = func(err error, c echolib.Context) {
		if errors.Is(err, echolib.ErrUnauthorized) {
			_ = c.Redirect(http.StatusFound, "/login")
			return
		}
		e.DefaultHTTPErrorHandler(err, c)
	}
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return echolib.ErrUnauthorized })
	w := testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
	t.Logf("status=%d location=%q body=%q settles=%d", w.Code, w.Header().Get("Location"), w.Body.String(), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load())
}
