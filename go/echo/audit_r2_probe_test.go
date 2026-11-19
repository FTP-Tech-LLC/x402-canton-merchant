package echo_test

// Round-2 probes for the echo adapter. Each test asserts the money rule; a
// failing test is a finding.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func r2Echo() *echolib.Echo {
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.HideBanner, e.HidePort = true, true
	return e
}

// serveWith sends one request through h with a chosen writer and headers.
func serveWith(h http.Handler, w http.ResponseWriter, target, signature string, header map[string]string) {
	r := httptest.NewRequestWithContext(context.Background(), "GET", target, nil)
	r.Header.Set("Accept", "application/json")
	for k, v := range header {
		r.Header.Set(k, v)
	}
	if signature != "" {
		r.Header.Set("PAYMENT-SIGNATURE", signature)
	}
	h.ServeHTTP(w, r)
}

// R2-1: echo's Gzip registered OUTSIDE the gate (README: "outside is fine").
// The final flush breaks like a dropped connection. gzipResponseWriter.Flush
// drops the error, so the gate marks the claim delivered: the retry of the
// charged payment gets 402 payment_already_redeemed instead of being served once.
func TestR2GzipOutsideTheGateFailedFlushGivesTheClaimBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		mw   echolib.MiddlewareFunc
	}{
		{"no wrapper (control)", nil},
		{"Gzip outside", middleware.Gzip()},
		{"Gzip MinLength 1024 outside", middleware.GzipWithConfig(middleware.GzipConfig{MinLength: 1024})},
		{"BodyDump outside", middleware.BodyDump(func(echolib.Context, []byte, []byte) {})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			f.OnSettle(func(n int32, w http.ResponseWriter) {
				if n == 1 {
					testkit.SettleOK(w)
					return
				}
				testkit.SettleAlreadySettled(w)
			})
			g := testkit.NewGate(t, f)
			e := r2Echo()
			e.Use(middleware.Recover())
			if tc.mw != nil {
				e.Use(tc.mw)
			}
			e.Use(cantonecho.Middleware(g.Middleware()))
			var hits atomic.Int32
			e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid") })
			sig := testkit.SignatureFor(t, e, testkit.PreparedTx)

			serveWith(e, testkit.BrokenFlushWriter{ResponseRecorder: httptest.NewRecorder()}, "/paid", sig,
				map[string]string{"Accept-Encoding": "gzip"})
			retry := testkit.Do(e, "GET", "/paid", sig)
			t.Logf("%s: retry after a failed final flush = %d %q (settles=%d, hits=%d)",
				tc.name, retry.Code, retry.Body.String(), f.SettleCalls.Load(), hits.Load())
			assert.Equal(t, 200, retry.Code, "the delivery failed on the connection: the retry is served once")
		})
	}
}

// R2-2: echo's BodyDump registered OUTSIDE the gate, with a writer under echo
// that cannot flush (a plain status recorder in front of echo, or
// http.TimeoutHandler). bodyDumpResponseWriter.Flush panics on ErrNotSupported,
// so the gate's final flush panics after settle and claim: the claim is never
// marked, and the replay gets 503 "do NOT pay again" instead of the final 402.
func TestR2BodyDumpOutsideWithAPlainWriterDoesNotPanicAfterSettle(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	var panics atomic.Int32
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc { // count what Recover below swallows
		return func(c echolib.Context) error {
			defer func() {
				if v := recover(); v != nil {
					panics.Add(1)
					t.Logf("panic: %v", v)
				}
			}()
			return next(c)
		}
	})
	e.Use(middleware.BodyDump(func(echolib.Context, []byte, []byte) {}))
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx) // the recorder flushes: no panic here

	first := httptest.NewRecorder()
	serveWith(e, testkit.PlainWriter{ResponseWriter: first}, "/paid", sig, nil)
	replay := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("first=%d %q panics=%d settles=%d; replay=%d %q", first.Code, first.Body.String(), panics.Load(),
		f.SettleCalls.Load(), replay.Code, replay.Body.String())
	assert.Zero(t, panics.Load(), "the gate's final flush must not panic")
	assert.Equal(t, 402, replay.Code, "a delivered payment's replay is the final 402, not 503")
}

// R2-2b: the same, the 402 offer itself (no payment): every request to a paid
// route panics.
func TestR2BodyDumpOutsideWithAPlainWriterOfferDoesNotPanic(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	e.Use(middleware.BodyDump(func(echolib.Context, []byte, []byte) {}))
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid") })
	assert.NotPanics(t, func() {
		serveWith(e, testkit.PlainWriter{ResponseWriter: httptest.NewRecorder()}, "/paid", "", nil)
	})
}
