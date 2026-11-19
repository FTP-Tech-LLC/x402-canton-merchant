package echo_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// Error handlers that render something below 400, or a 4xx/5xx: never settled.
func TestR3ErrorRenderingGuard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		render func(err error, c echolib.Context)
		want   int
	}{
		{"1xx only", func(_ error, c echolib.Context) { _ = c.NoContent(http.StatusEarlyHints) }, 500},
		{"code 0", func(_ error, c echolib.Context) { c.Response().WriteHeader(0) }, 500},
		{"200 JSON-RPC style", func(err error, c echolib.Context) { _ = c.JSON(200, map[string]string{"error": err.Error()}) }, 500},
		{"304", func(_ error, c echolib.Context) { _ = c.NoContent(http.StatusNotModified) }, 500},
		{"308 redirect", func(_ error, c echolib.Context) { _ = c.Redirect(http.StatusPermanentRedirect, "/login") }, 500},
		{"raw Writer 200", func(_ error, c echolib.Context) { _, _ = c.Response().Writer.Write([]byte("ok")) }, 500},
		{"403", func(_ error, c echolib.Context) { _ = c.String(403, "no") }, 403},
		{"1000", func(_ error, c echolib.Context) { c.Response().WriteHeader(1000) }, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			e := r3Echo()
			e.HTTPErrorHandler = tc.render
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.GET("/paid", func(c echolib.Context) error {
				c.Response().Header().Set("Set-Cookie", "paid=1")
				_ = c.String(200, "PAID CONTENT")
				return errors.New("failed after writing")
			})
			w := testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
			t.Logf("%s: %d %q cookie=%q location=%q settles=%d", tc.name, w.Code, w.Body.String(), w.Header().Get("Set-Cookie"), w.Header().Get("Location"), f.SettleCalls.Load())
			assert.Equal(t, tc.want, w.Code)
			assert.NotContains(t, w.Body.String(), "PAID CONTENT")
			assert.Empty(t, w.Header().Get("Set-Cookie"))
			assert.Zero(t, f.SettleCalls.Load())
		})
	}
}

// Gzip and BodyDump INSIDE the gate (README: not on a paid route) on the success
// path: what is delivered and charged.
func TestR3WrappersInsideTheGateSuccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inner echolib.MiddlewareFunc
	}{
		{"Gzip inside", middleware.Gzip()},
		{"BodyDump inside", middleware.BodyDump(func(echolib.Context, []byte, []byte) {})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			e := r3Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.Use(tc.inner)
			e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid content paid content") })
			sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
			do := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("GET", "/paid", nil)
				r.Header.Set("PAYMENT-SIGNATURE", sig)
				r.Header.Set("Accept-Encoding", "gzip")
				w := httptest.NewRecorder()
				e.ServeHTTP(w, r)
				return w
			}
			w := do()
			body := w.Body.Bytes()
			if w.Header().Get("Content-Encoding") == "gzip" {
				zr, err := gzip.NewReader(bytes.NewReader(body))
				if assert.NoError(t, err) {
					body, err = io.ReadAll(zr)
					assert.NoError(t, err)
				}
			}
			replay := do()
			t.Logf("%s: %d ce=%q cl=%q plain=%q settles=%d replay=%d", tc.name, w.Code, w.Header().Get("Content-Encoding"), w.Header().Get("Content-Length"), body, f.SettleCalls.Load(), replay.Code)
			assert.Equal(t, 200, w.Code)
			assert.Equal(t, "paid content paid content", string(body))
			assert.Equal(t, int32(1), f.SettleCalls.Load())
			assert.Equal(t, 402, replay.Code)
		})
	}
}
