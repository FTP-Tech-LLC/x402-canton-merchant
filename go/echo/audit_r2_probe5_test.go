package echo_test

// Round-2 probes of things that should hold (all expected to pass).

import (
	"net/http"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR2HooksAndLoggerAroundThePrivateResponse(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	var outerAfter, innerAfter, innerBefore atomic.Int32
	var logged []int
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{LogStatus: true, HandleError: true,
		LogValuesFunc: func(_ echolib.Context, v middleware.RequestLoggerValues) error {
			logged = append(logged, v.Status)
			return nil
		}}))
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc {
		return func(c echolib.Context) error { c.Response().After(func() { outerAfter.Add(1) }); return next(c) }
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	var fail atomic.Bool
	e.GET("/paid", func(c echolib.Context) error {
		c.Response().Before(func() { innerBefore.Add(1) })
		c.Response().After(func() { innerAfter.Add(1) })
		if fail.Load() {
			return echolib.NewHTTPError(http.StatusNotFound).WithInternal(echolib.NewHTTPError(http.StatusOK, "inner 2xx"))
		}
		return c.String(200, "paid")
	})
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx) // logged 402
	outerAfter.Store(0)                                   // count the paid request only
	w := testkit.Do(e, "GET", "/paid", sig)
	assert.Equal(t, 200, w.Code)
	t.Logf("success: outerAfter=%d innerBefore=%d innerAfter=%d logged=%v", outerAfter.Load(), innerBefore.Load(), innerAfter.Load(), logged)
	assert.Equal(t, int32(1), outerAfter.Load(), "outer After runs once, at the gate's write")
	assert.Equal(t, int32(1), innerBefore.Load())
	assert.Equal(t, int32(1), innerAfter.Load())

	fail.Store(true)
	w = testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, "1111111111111111111111111111111111111111111111111111111111111111"))
	t.Logf("HTTPError(404, Internal HTTPError(200)): %d %q settles=%d logged=%v", w.Code, w.Body.String(), f.SettleCalls.Load(), logged)
	assert.Equal(t, 500, w.Code, "the inner 200 is replaced by the 500 guard")
	assert.Equal(t, int32(1), f.SettleCalls.Load())
	assert.Equal(t, []int{402, 200, 402, 500}, logged)
}

func TestR2ErrorHandlerThatWritesTwice(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	e.HTTPErrorHandler = func(err error, c echolib.Context) {
		_ = c.JSON(http.StatusBadGateway, map[string]string{"a": "1"})
		_ = c.String(http.StatusOK, "second write")
	}
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { _ = c.String(200, "partial"); return echolib.ErrInternalServerError })
	w := testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
	t.Logf("%d %q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), "partial")
	assert.Zero(t, f.SettleCalls.Load())
}
