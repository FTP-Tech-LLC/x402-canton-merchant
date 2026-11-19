package echo_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// Pre middleware with e.Use: the gate sees the path the router used.
func TestProbePreRewritesNeverServeFree(t *testing.T) {
	cases := []struct {
		name   string
		pre    echolib.MiddlewareFunc
		route  string
		target string
	}{
		{"remove trailing slash", middleware.RemoveTrailingSlash(), "/paid", "/paid/"},
		{"add trailing slash", middleware.AddTrailingSlash(), "/paid/", "/paid"},
		{"rewrite alias", middleware.Rewrite(map[string]string{"/alias": "/paid"}), "/paid", "/alias"},
		{"remove slash, escaped", middleware.RemoveTrailingSlash(), "/paid", "/p%61id/"},
		{"method override", middleware.MethodOverride(), "/paid", "/paid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			e := quietEcho()
			e.Pre(tc.pre)
			e.Use(cantonecho.Middleware(g.Middleware()))
			var hits atomic.Int32
			e.GET(tc.route, func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid content") })
			method := "GET"
			r := httptest.NewRequest(method, tc.target, nil)
			if tc.name == "method override" {
				r = httptest.NewRequest("POST", tc.target, nil)
				r.Header.Set("X-HTTP-Method-Override", "GET")
			}
			r.Header.Set("Accept", "application/json")
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			t.Logf("%s %s -> %d %q hits=%d", r.Method, tc.target, w.Code, w.Body.String(), hits.Load())
			assert.Zero(t, hits.Load(), "the paid handler never runs unpaid")
		})
	}
}

// gzip inside the gate, success path: the compressed body is delivered intact.
func TestProbeGzipInsideGateSuccessIsIntact(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.Use(middleware.Gzip())
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid content paid content") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, paidRequest(sig, map[string]string{"Accept-Encoding": "gzip"}))
	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	var plain []byte
	if err == nil {
		plain, err = io.ReadAll(zr)
	}
	t.Logf("status=%d ce=%q cl=%q err=%v plain=%q", w.Code, w.Header().Get("Content-Encoding"), w.Header().Get("Content-Length"), err, plain)
	assert.NoError(t, err)
	assert.Equal(t, "paid content paid content", string(plain))
}

// gzip OUTSIDE the gate, error after a write: rendered, not settled.
func TestProbeGzipOutsideGateErrorAfterWrite(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(middleware.Gzip())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error {
		_ = c.String(200, "partial paid content")
		return errors.New("failed after writing")
	})
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, paidRequest(sig, map[string]string{"Accept-Encoding": "gzip"}))
	t.Logf("status=%d ce=%q settles=%d", w.Code, w.Header().Get("Content-Encoding"), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load())
	assert.Equal(t, 500, w.Code)
}

// gzip inside the gate, concurrent: the error is rendered into a gzip.Writer
// that was already Closed and returned to echo's sync.Pool (run with -race).
func TestProbeGzipInsideGatePooledWriterReuse(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.Use(middleware.Gzip())
	e.GET("/paid", func(c echolib.Context) error {
		_ = c.String(200, "partial paid content")
		return errors.New("failed after writing")
	})
	e.GET("/free", func(c echolib.Context) error { return c.String(200, "free free free free") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			e.ServeHTTP(w, paidRequest(sig, map[string]string{"Accept-Encoding": "gzip"}))
		}()
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("GET", "/free", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
			if err != nil {
				bad.Add(1)
				return
			}
			plain, err := io.ReadAll(zr)
			if err != nil || string(plain) != "free free free free" {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	t.Logf("corrupted free responses: %d / 50", bad.Load())
	assert.Zero(t, bad.Load())
}

// F4 money impact: a writer in front of the gate without Flush, and a payer
// whose connection drops at the final write. With net/http the claim is
// released and the retry is served once; with echo the flush panics first.
type brokenConn struct {
	http.ResponseWriter
	fail atomic.Bool
}

func (b *brokenConn) Write(p []byte) (int, error) {
	if b.fail.Load() {
		return 0, errors.New("write: broken pipe")
	}
	return b.ResponseWriter.Write(p)
}

func TestProbeDroppedConnectionRetryIsServedOnce(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	var drop atomic.Bool
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc {
		return func(c echolib.Context) error {
			bc := &brokenConn{ResponseWriter: c.Response().Writer}
			bc.fail.Store(drop.Load())
			c.Response().Writer = bc
			return next(c)
		}
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	var hits atomic.Int32
	e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	drop.Store(true)
	first := testkit.Do(e, "GET", "/paid", sig)
	drop.Store(false)
	retry := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("first(dropped)=%d settles=%d; retry=%d %q hits=%d", first.Code, f.SettleCalls.Load(), retry.Code, retry.Body.String(), hits.Load())
	assert.Equal(t, 200, retry.Code, "the retry of a charged, undelivered payment is served once")
}

// Same with net/http core for comparison.
func TestProbeDroppedConnectionRetryNetHTTP(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /paid", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "paid") })
	inner := g.Middleware()(mux)
	var drop atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bc := &brokenConn{ResponseWriter: w}
		bc.fail.Store(drop.Load())
		inner.ServeHTTP(bc, r)
	})
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	drop.Store(true)
	testkit.Do(h, "GET", "/paid", sig)
	drop.Store(false)
	retry := testkit.Do(h, "GET", "/paid", sig)
	t.Logf("net/http: retry=%d %q settles=%d", retry.Code, retry.Body.String(), f.SettleCalls.Load())
	assert.Equal(t, 200, retry.Code)
}
