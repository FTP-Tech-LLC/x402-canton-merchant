package echo_test

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// flushFails is a connection whose final flush fails (the payer is gone): Write
// lands in the server's buffer, the flush to the socket errors. net/http's own
// *response reports this through FlushError.
type flushFails struct {
	http.ResponseWriter
	fail *atomic.Bool
}

func (f flushFails) FlushError() error {
	if f.fail.Load() {
		return errors.New("write tcp: broken pipe")
	}
	return http.NewResponseController(f.ResponseWriter).Flush()
}

func TestProbeFlushErrorAfterClaimIsSeenThroughEcho(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	var hits atomic.Int32
	e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid") })
	var fail atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { e.ServeHTTP(flushFails{w, &fail}, r) })
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	fail.Store(true)
	testkit.Do(h, "GET", "/paid", sig) // charged; the flush to the payer fails
	fail.Store(false)
	retry := testkit.Do(h, "GET", "/paid", sig)
	t.Logf("echo: retry after a failed flush = %d %q (settles=%d, hits=%d)", retry.Code, retry.Body.String(), f.SettleCalls.Load(), hits.Load())
	assert.Equal(t, 200, retry.Code, "an undelivered answer is served once on the retry, never a 402 after the charge")
}

func TestProbeFlushErrorAfterClaimNetHTTP(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /paid", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "paid") })
	inner := g.Middleware()(mux)
	var fail atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { inner.ServeHTTP(flushFails{w, &fail}, r) })
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	fail.Store(true)
	testkit.Do(h, "GET", "/paid", sig)
	fail.Store(false)
	retry := testkit.Do(h, "GET", "/paid", sig)
	t.Logf("net/http: retry after a failed flush = %d %q (settles=%d)", retry.Code, retry.Body.String(), f.SettleCalls.Load())
	assert.Equal(t, 200, retry.Code)
}
