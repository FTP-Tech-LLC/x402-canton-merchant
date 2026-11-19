package echo_test

import (
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

// A panic after a 2xx write, with Recover outermost and Gzip or BodyDump between
// it and the gate: never settled, the lock is released, the retry is served.
func TestR3PanicAfterWriteWithOuterWrappers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		outer echolib.MiddlewareFunc
	}{
		{"Gzip outside", middleware.Gzip()},
		{"BodyDump outside", middleware.BodyDump(func(echolib.Context, []byte, []byte) {})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			e := r3Echo()
			e.Use(middleware.Recover())
			e.Use(tc.outer)
			e.Use(cantonecho.Middleware(g.Middleware()))
			var calls atomic.Int32
			e.GET("/paid", func(c echolib.Context) error {
				if calls.Add(1) == 1 {
					_ = c.String(200, "partial paid content")
					panic("bug")
				}
				return c.String(200, "paid")
			})
			sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
			do := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("GET", "/paid", nil)
				r.Header.Set("PAYMENT-SIGNATURE", sig)
				r.Header.Set("Accept-Encoding", "gzip")
				w := httptest.NewRecorder()
				e.ServeHTTP(w, r)
				return w
			}
			first := do()
			second := do()
			t.Logf("first=%d ce=%q len=%d settles=%d; retry=%d ce=%q", first.Code, first.Header().Get("Content-Encoding"), first.Body.Len(), f.SettleCalls.Load(), second.Code, second.Header().Get("Content-Encoding"))
			assert.Equal(t, http.StatusInternalServerError, first.Code)
			assert.NotContains(t, first.Body.String(), "partial paid content")
			assert.Equal(t, 200, second.Code)
			assert.Equal(t, int32(1), f.SettleCalls.Load())
		})
	}
}
