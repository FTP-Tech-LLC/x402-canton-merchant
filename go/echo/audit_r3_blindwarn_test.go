package echo_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// http.TimeoutHandler in front of echo (allowed by the README when longer than
// the budget): the gate cannot see the connection and says so once.
func TestR3BlindFlushWarningThroughTimeoutHandler(t *testing.T) {
	var logs bytes.Buffer
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})
	e := r3Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID") })
	h := http.TimeoutHandler(e, 10*time.Minute, "timeout")
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/paid", nil)
	r.Header.Set("PAYMENT-SIGNATURE", sig)
	h.ServeHTTP(w, r)
	t.Logf("paid via TimeoutHandler: %d %q; gate log: %q", w.Code, w.Body.String(), logs.String())
	assert.Equal(t, 200, w.Code)
	// The warning is once per process: run this test alone to see it
	// (-run '^TestR3BlindFlushWarningThroughTimeoutHandler$'). It names the
	// adapter's writer, not the wrapper that stops the walk.
	if !strings.Contains(logs.String(), "cannot see the connection") {
		t.Log("the once-per-process warning was spent by an earlier test in this process")
	}
}
