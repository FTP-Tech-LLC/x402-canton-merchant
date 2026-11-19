package gin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// realServer starts h with the given WriteTimeout.
func realServer(t *testing.T, h http.Handler, writeTimeout time.Duration) string {
	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

func get(t *testing.T, url, sig string) (int, string, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/json")
	if sig != "" {
		req.Header.Set("PAYMENT-SIGNATURE", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// P23: http.Server.WriteTimeout below a slow settle. The gate lifts the
// deadline through gin's own writer (it has Unwrap).
func TestProbeWriteDeadlineLiftThroughGin(t *testing.T) {
	f := testkit.StartFacilitator(t)
	f.OnSettle(func(_ int32, w http.ResponseWriter) { time.Sleep(1500 * time.Millisecond); testkit.SettleOK(w) })
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	url := realServer(t, r, time.Second)
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	code, body, err := get(t, url+"/paid", sig)
	t.Logf("lift through gin: code=%d body=%q err=%v settles=%d", code, body, err, f.SettleCalls.Load())
	assert.NoError(t, err)
	assert.Equal(t, 200, code)
}

// gzipLike is a gin middleware writer wrapper in the style of gin-contrib/gzip:
// it embeds gin.ResponseWriter (no Unwrap) and passes writes through.
type gzipLike struct{ gingonic.ResponseWriter }

func (w *gzipLike) Write(b []byte) (int, error) { return w.ResponseWriter.Write(b) }

// P24: the same with a gin writer wrapper outside the gate: the lift fails, the
// response is cut after the charge (the core README's WriteTimeout fallback).
func TestProbeWriteDeadlineLiftThroughGinWrapper(t *testing.T) {
	f := testkit.StartFacilitator(t)
	var slow atomic.Bool
	slow.Store(true)
	f.OnSettle(func(n int32, w http.ResponseWriter) {
		if slow.Load() {
			time.Sleep(1500 * time.Millisecond)
		}
		if n == 1 {
			testkit.SettleOK(w)
			return
		}
		testkit.SettleAlreadySettled(w)
	})
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(func(c *gingonic.Context) { c.Writer = &gzipLike{c.Writer}; c.Next() })
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	url := realServer(t, r, time.Second)
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	code, body, err := get(t, url+"/paid", sig)
	t.Logf("lift through a gin wrapper: code=%d body=%q err=%v settles=%d", code, body, err, f.SettleCalls.Load())
	slow.Store(false)
	code2, body2, err2 := get(t, url+"/paid", sig)
	t.Logf("retry: code=%d body=%q err=%v settles=%d", code2, body2, err2, f.SettleCalls.Load())
}

// P25: c.Stream on a paid route over a real server, client leaves mid-stream.
func TestProbeStreamClientLeaves(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	var returned atomic.Bool
	r.GET("/paid", func(c *gingonic.Context) {
		c.Stream(func(w io.Writer) bool {
			_, _ = w.Write([]byte("tick\n"))
			time.Sleep(20 * time.Millisecond)
			return true
		})
		returned.Store(true)
	})
	url := realServer(t, r, 0)
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url+"/paid", nil)
	req.Header.Set("PAYMENT-SIGNATURE", sig)
	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	t.Logf("stream: client err=%v", err)
	require.Eventually(t, returned.Load, 3*time.Second, 20*time.Millisecond, "the streaming handler returns when the payer leaves")
	time.Sleep(200 * time.Millisecond)
	t.Logf("stream: settles=%d", f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load(), "a payer who left is not charged")
}

// r.Any on a paid path serves the other methods free (documented); Check reports it.
func TestAnyOnAPaidPathIsReportedByCheck(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(cantongin.Middleware(g.Middleware()))
	var hits atomic.Int32
	r.Any("/paid", func(c *gingonic.Context) { hits.Add(1); c.String(200, "paid content") })
	assert.Equal(t, 200, testkit.Do(r, "POST", "/paid", "").Code, "POST /paid is not a paid key: served free")
	err := cantongin.Check(r, testkit.Routes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a paid key")
}

func TestCheckPassesForACorrectSetup(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	assert.NoError(t, cantongin.Check(r, testkit.Routes()))
}

// P27: canton.PayerFrom(c) with the gin context itself.
func TestProbePayerFromGinContext(t *testing.T) {
	var fromGin, fromRequest string
	var okGin bool
	_, r := paidEngine(t, func(c *gingonic.Context) {
		fromGin, okGin = canton.PayerFrom(c)
		fromRequest, _ = canton.PayerFrom(c.Request.Context())
		c.String(200, "ok")
	})
	require.Equal(t, 200, testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx)).Code)
	t.Logf("PayerFrom(c)=%q ok=%v PayerFrom(c.Request.Context())=%q", fromGin, okGin, fromRequest)
}

// P28: the adapter as a per-route handler.
func TestProbePerRouteRegistration(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.GET("/paid", cantongin.Middleware(g.Middleware()), func(c *gingonic.Context) { c.String(200, "paid content") })
	r.GET("/free", func(c *gingonic.Context) { c.String(200, "free") })
	assert.Equal(t, 402, testkit.Do(r, "GET", "/paid", "").Code)
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	assert.Equal(t, "paid content", w.Body.String())
	assert.Equal(t, int32(1), f.SettleCalls.Load())
	// "//paid" never reaches the per-route chain: gin answers 404 itself.
	w2 := testkit.Do(r, "GET", "//paid", "")
	t.Logf("per-route //paid: code=%d", w2.Code)
}

// brokenConn is a ResponseWriter shaped like net/http's own: writes are
// buffered (no error), the connection error shows only in FlushError, and the
// plain Flush() swallows it, exactly as *http.response does.
type brokenConn struct{ *httptest.ResponseRecorder }

func (w brokenConn) FlushError() error { return io.ErrClosedPipe }
func (w brokenConn) Flush()            { _ = w.FlushError() }

// P29: the payer's connection breaks while the paid answer is written. The
// core detects it through FlushError, gives the claim back and serves the
// retry once. Through gin, gin's Flush() swallows the error: the claim is
// marked delivered and the retry of the charged payment gets the final 402.
func TestProbeBrokenConnectionAfterSettle(t *testing.T) {
	paidOK := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("paid content")) }
	run := func(t *testing.T, h http.Handler) (first, retry int, body string) {
		sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
		r := httptest.NewRequest("GET", "/paid", nil)
		r.Header.Set("Accept", "application/json")
		r.Header.Set("PAYMENT-SIGNATURE", sig)
		w := brokenConn{httptest.NewRecorder()}
		h.ServeHTTP(w, r)
		again := testkit.Do(h, "GET", "/paid", sig)
		return w.Code, again.Code, again.Body.String()
	}
	t.Run("net/http core", func(t *testing.T) {
		f := testkit.StartFacilitator(t)
		f.OnSettle(func(n int32, w http.ResponseWriter) {
			if n == 1 {
				testkit.SettleOK(w)
				return
			}
			testkit.SettleAlreadySettled(w)
		})
		g := testkit.NewGate(t, f)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /paid", paidOK)
		first, retry, body := run(t, g.Middleware()(mux))
		t.Logf("core: first=%d retry=%d %q", first, retry, body)
		assert.Equal(t, 200, retry, "the retry of an undelivered payment is served once")
	})
	t.Run("gin", func(t *testing.T) {
		f := testkit.StartFacilitator(t)
		f.OnSettle(func(n int32, w http.ResponseWriter) {
			if n == 1 {
				testkit.SettleOK(w)
				return
			}
			testkit.SettleAlreadySettled(w)
		})
		g := testkit.NewGate(t, f)
		r := gingonic.New()
		r.Use(gingonic.Recovery())
		r.Use(cantongin.Middleware(g.Middleware()))
		r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
		first, retry, body := run(t, r)
		t.Logf("gin: first=%d retry=%d %q", first, retry, body)
		assert.Equal(t, 200, retry, "the retry of an undelivered payment is served once")
	})
}

// The gate registered twice (globally and on the route): the second registration
// passes the request through, the outer gate serves it once.
func TestGateRegisteredTwiceIsServedOnce(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	var hits atomic.Int32
	r.GET("/paid", cantongin.Middleware(g.Middleware()), func(c *gingonic.Context) { hits.Add(1); c.String(200, "paid content") })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "paid content", w.Body.String())
	assert.Equal(t, int32(1), f.SettleCalls.Load())
	assert.Equal(t, int32(1), hits.Load())
}

// P5b: the c.Error pattern over a real server.
func TestProbeErrorMiddlewareOutsideRealServer(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(func(c *gingonic.Context) {
		c.Next()
		if len(c.Errors) > 0 {
			c.JSON(http.StatusInternalServerError, gingonic.H{"error": c.Errors.Last().Error()})
		}
	})
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { _ = c.Error(io.ErrUnexpectedEOF) })
	url := realServer(t, r, 0)
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	req, _ := http.NewRequest("GET", url+"/paid", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PAYMENT-SIGNATURE", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	t.Logf("real server c.Error: status=%d body=%q receipt=%v settles=%d", resp.StatusCode, b, resp.Header.Get("PAYMENT-RESPONSE") != "", f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load())
}
