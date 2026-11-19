package echo_test

// Audit probes for the echo adapter (lens: echo, v0.2). Each test asserts a
// money rule; a FAILING test is a finding. Run:
//
//	GOTMPDIR=... go test ./echo/ -run Probe -v

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func quietEcho() *echolib.Echo {
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.HideBanner, e.HidePort = true, true
	return e
}

func paidRequest(sig string, extra map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/paid", nil)
	r.Header.Set("Accept", "application/json")
	r.Header.Set("PAYMENT-SIGNATURE", sig)
	for k, v := range extra {
		r.Header.Set(k, v)
	}
	return r
}

// F1: gzip inside the gate. The handler writes a 2xx, then returns an error.
// echo's Gzip leaves res.Writer = its (now closed, pooled) gzip writer; the
// adapter resets Committed but not Writer, so c.Error renders into the dead
// gzip writer, the buffer stays empty, and the gate settles an empty 200.
func TestProbeGzipInsideGateErrorAfterWriteIsNotSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.Use(middleware.Gzip())
	e.GET("/paid", func(c echolib.Context) error {
		_ = c.String(200, "partial paid content")
		return errors.New("the handler failed after writing")
	})
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, paidRequest(sig, map[string]string{"Accept-Encoding": "gzip"}))
	t.Logf("status=%d content-encoding=%q len=%d payment-response=%v", w.Code, w.Header().Get("Content-Encoding"), w.Body.Len(), w.Header().Get("PAYMENT-RESPONSE") != "")
	assert.Zero(t, f.SettleCalls.Load(), "a returned error is never settled")
	assert.GreaterOrEqual(t, w.Code, 400)
}

// Echo's Logger registered before the gate (outside it, as documented): an error
// after a write is rendered and not settled. Logger or Recover INSIDE the gate
// answer the error themselves below the adapter and are unsupported (README).
func TestLoggerOutsideTheGateErrorAfterWriteIsNotSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{HandleError: true, LogStatus: true,
		LogValuesFunc: func(echolib.Context, middleware.RequestLoggerValues) error { return nil }}))
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error {
		_ = c.String(200, "partial paid content")
		return errors.New("the handler failed after writing")
	})
	w := testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
	assert.Equal(t, 500, w.Code)
	assert.Zero(t, f.SettleCalls.Load(), "a returned error is never settled")
}

// Recover outside the gate (as documented): a panic after a 2xx write is never settled.
func TestRecoverOutsideTheGatePanicAfterWriteIsNotSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	var calls atomic.Int32
	e.GET("/paid", func(c echolib.Context) error {
		if calls.Add(1) > 1 {
			return c.String(200, "ok")
		}
		_ = c.String(200, "partial paid content")
		panic("bug after writing")
	})
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := testkit.Do(e, "GET", "/paid", sig)
	assert.Equal(t, 500, w.Code)
	assert.NotContains(t, w.Body.String(), "partial paid content")
	assert.Zero(t, f.SettleCalls.Load(), "a panic is never settled")
	assert.Equal(t, 200, testkit.Do(e, "GET", "/paid", sig).Code, "and its lock was released")
}

// F3a: a custom HTTPErrorHandler that writes nothing for some error.
func TestProbeErrorHandlerThatWritesNothingIsNotSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.HTTPErrorHandler = func(err error, c echolib.Context) {
		if errors.Is(err, context.Canceled) {
			return // common: "the client went away, nothing to answer"
		}
		e.DefaultHTTPErrorHandler(err, c)
	}
	e.GET("/paid", func(c echolib.Context) error {
		// an internal sub-context was cancelled (errgroup sibling failed); the payer is still there
		ctx, cancel := context.WithCancel(c.Request().Context())
		cancel()
		return ctx.Err()
	})
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("status=%d body=%q payment-response=%v", w.Code, w.Body.String(), w.Header().Get("PAYMENT-RESPONSE") != "")
	assert.Zero(t, f.SettleCalls.Load(), "a returned error is never settled")
}

// F3b: an HTTPError without a Code (zero value). Plain echo panics on
// WriteHeader(0); through the adapter it is a settled 200.
func TestProbeHTTPErrorWithZeroCodeIsNotSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error {
		return &echolib.HTTPError{Message: "bad input"}
	})
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("status=%d body=%q", w.Code, w.Body.String())
	assert.Zero(t, f.SettleCalls.Load(), "a returned error is never settled")
}

// F3c: a custom HTTPErrorHandler that renders errors as 200 (JSON-RPC style).
func TestProbeErrorRenderedAs2xxIsNotSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.HTTPErrorHandler = func(err error, c echolib.Context) {
		_ = c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
	}
	e.GET("/paid", func(c echolib.Context) error { return errors.New("upstream failed") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("status=%d body=%q", w.Code, w.Body.String())
	assert.Zero(t, f.SettleCalls.Load(), "README: a returned error ... is never charged")
}

// F4: an outer ResponseWriter wrapper without Flush/Unwrap (a status
// recorder; http.TimeoutHandler is the same). The gate hands *echo.Response to
// its flush; echo.Response.Flush panics when the writer under it cannot flush.
type plainRecorder struct {
	http.ResponseWriter
	status int
}

func (p *plainRecorder) WriteHeader(code int) { p.status = code; p.ResponseWriter.WriteHeader(code) }

func TestProbeOuterWriterWithoutFlushDoesNotPanicAfterSettle(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc { // a status recorder in front of the gate
		return func(c echolib.Context) error {
			c.Response().Writer = &plainRecorder{ResponseWriter: c.Response().Writer}
			return next(c)
		}
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	var hits atomic.Int32
	e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	first := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("first: status=%d body=%q settles=%d", first.Code, first.Body.String(), f.SettleCalls.Load())
	replay := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("replay: status=%d body=%q", replay.Code, replay.Body.String())
	assert.Equal(t, 200, first.Code)
	assert.Equal(t, 402, replay.Code, "a delivered payment's replay is 402 payment_already_redeemed")
}

func TestProbeOuterWriterWithoutFlushNoRecoverDeliversAfterCharge(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc {
		return func(c echolib.Context) error {
			c.Response().Writer = &plainRecorder{ResponseWriter: c.Response().Writer}
			return next(c)
		}
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	assert.NotPanics(t, func() { testkit.Do(e, "GET", "/paid", sig) }, "the gate must not panic after the payer was charged")
	t.Logf("settles=%d", f.SettleCalls.Load())
}

// F4 via http.TimeoutHandler around the echo server (a common pattern), real server.
func TestProbeTimeoutHandlerAroundEchoServesPaidResponse(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid") })
	h := http.TimeoutHandler(e, 10*time.Minute, "timeout")
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	first := testkit.Do(h, "GET", "/paid", sig)
	replay := testkit.Do(h, "GET", "/paid", sig)
	t.Logf("first=%d %q replay=%d %q settles=%d", first.Code, first.Body.String(), replay.Code, replay.Body.String(), f.SettleCalls.Load())
	assert.Equal(t, 200, first.Code)
	assert.Equal(t, 402, replay.Code)
}

// F4 without Recover, on a real net/http server: the payer is charged and gets nothing.
func TestProbeTimeoutHandlerNoRecoverRealServer(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid") })
	srv := httptest.NewUnstartedServer(http.TimeoutHandler(e, 10*time.Minute, "timeout"))
	srv.Config.ErrorLog = nil
	srv.Start()
	defer srv.Close()
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	req, _ := http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PAYMENT-SIGNATURE", sig)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("first: status=%d body=%q", resp.StatusCode, b)
	} else {
		t.Logf("first: transport error %v (settles=%d)", err, f.SettleCalls.Load())
	}
	assert.NoError(t, err, "a charged payer must get the response")
	req2, _ := http.NewRequest("GET", srv.URL+"/paid", nil)
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("PAYMENT-SIGNATURE", sig)
	resp2, err2 := http.DefaultClient.Do(req2)
	if err2 == nil {
		b, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Logf("retry of the same payment: status=%d body=%q", resp2.StatusCode, b)
	} else {
		t.Logf("retry: transport error %v", err2)
	}
}

// F5: a Response.Before hook registered inside the gate (handler or inner
// middleware) runs again when the gate writes its own refusal, so a handler
// header reaches the 402 of a refused settle.
func TestProbeBeforeHookHeaderDoesNotReachGateRefusal(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	var runs atomic.Int32
	e.GET("/paid", func(c echolib.Context) error {
		c.Response().Before(func() { // e.g. a session saved just before the response is written
			runs.Add(1)
			c.Response().Header().Add("Set-Cookie", "access=paid")
		})
		return c.String(200, "paid-body")
	})
	f.OnSettle(func(_ int32, w http.ResponseWriter) {
		testkit.SettleRefused(w, "invalid_exact_canton_insufficient_balance")
	})
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("status=%d set-cookie=%q hook runs=%d body=%q", w.Code, w.Header().Values("Set-Cookie"), runs.Load(), w.Body.String())
	assert.Equal(t, 402, w.Code)
	assert.Empty(t, w.Header().Get("Set-Cookie"), "a refusal answer carries none of the handler's headers")
}

func TestProbeBeforeHookRunsOnceOnSuccess(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	var runs atomic.Int32
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc { // outer middleware with a hook
		return func(c echolib.Context) error {
			c.Response().Before(func() { runs.Add(1); c.Response().Header().Add("Vary", "Origin") })
			return next(c)
		}
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid-body") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	runs.Store(0)
	w := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("status=%d vary=%q hook runs=%d", w.Code, w.Header().Values("Vary"), runs.Load())
	assert.Equal(t, int32(1), runs.Load(), "echo runs Before hooks once per response")
	assert.Len(t, w.Header().Values("Vary"), 1)
}

// F7: headers of a 2xx the handler committed survive the reset: the error
// answer "replaces" status and body only.
func TestProbeErrorAfterCommitted2xxDropsThe2xxHeaders(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error {
		c.SetCookie(&http.Cookie{Name: "access", Value: "paid-entitlement"})
		c.Response().Header().Set("Content-Disposition", `attachment; filename="report.bin"`)
		_ = c.Blob(200, "application/octet-stream", []byte("paid bytes"))
		return errors.New("post-write step failed")
	})
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("status=%d ct=%q cd=%q cookie=%q body=%q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"), w.Header().Values("Set-Cookie"), w.Body.String())
	assert.Equal(t, 500, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
	assert.Empty(t, w.Header().Values("Set-Cookie"), "the replaced 2xx's entitlement cookie is not served free")
	assert.True(t, strings.HasPrefix(w.Header().Get("Content-Type"), "application/json"))
}

// e.Any on a paid path serves the other methods free (documented); Check reports it.
func TestAnyOnAPaidPathIsReportedByCheck(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(cantonecho.Middleware(g.Middleware()))
	var hits atomic.Int32
	e.Any("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid content") })
	assert.Equal(t, 200, testkit.Do(e, "POST", "/paid", "").Code, "POST /paid is not a paid key: served free")
	err := cantonecho.Check(e, testkit.Routes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a paid key")
}

func TestCheckPassesForACorrectSetup(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid content") })
	assert.NoError(t, cantonecho.Check(e, testkit.Routes()))
}

// A paid route added to a group before Group.Use is not gated (echo binds group
// middleware at route-add time); Check reports it.
func TestGroupUseAfterRouteIsReportedByCheck(t *testing.T) {
	f := testkit.StartFacilitator(t)
	routes := map[string]canton.Route{"GET /api/v1/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
	e := quietEcho()
	v1 := e.Group("/api/v1")
	v1.GET("/data", func(c echolib.Context) error { return c.String(200, "paid") })
	v1.Use(cantonecho.Middleware(g.Middleware()))
	assert.Equal(t, 200, testkit.Do(e, "GET", "/api/v1/data", "").Code)
	require.Error(t, cantonecho.Check(e, routes))
}

// Documented: the adapter renders a paid handler's error itself, so an outer
// middleware sees nil (the status carries the failure), unlike on a free route.
func TestOuterMiddlewareSeesNilForAPaidHandlersError(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	var seen, seenFree error
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc {
		return func(c echolib.Context) error {
			err := next(c)
			if c.Path() == "/free" {
				seenFree = err
			} else {
				seen = err
			}
			return err
		}
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return errors.New("db down") })
	e.GET("/free", func(c echolib.Context) error { return errors.New("db down") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	assert.Equal(t, 500, testkit.Do(e, "GET", "/paid", sig).Code)
	testkit.Do(e, "GET", "/free", "")
	assert.NoError(t, seen)
	assert.Error(t, seenFree)
}

// Memory: total bytes allocated for one paid 8 MiB answer, echo vs net/http core.
func TestProbeMemoryPerPaidResponse(t *testing.T) {
	body := strings.Repeat("x", 8<<20)
	measure := func(h http.Handler, sig string) uint64 {
		var before, after runtimeMem
		before.read()
		w := testkit.Do(h, "GET", "/paid", sig)
		after.read()
		require.Equal(t, 200, w.Code)
		return after.total - before.total
	}
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, body) })
	sigE := testkit.SignatureFor(t, e, testkit.PreparedTx)
	echoAlloc := measure(e, sigE)

	f2 := testkit.StartFacilitator(t)
	g2 := testkit.NewGate(t, f2)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /paid", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
	h := g2.Middleware()(mux)
	sigN := testkit.SignatureFor(t, h, "bbcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	netAlloc := measure(h, sigN)
	t.Logf("body=%d MiB; allocated: echo=%.1f MiB (%.1fx), net/http=%.1f MiB (%.1fx)",
		len(body)>>20, float64(echoAlloc)/(1<<20), float64(echoAlloc)/float64(len(body)),
		float64(netAlloc)/(1<<20), float64(netAlloc)/float64(len(body)))
}
