package echo_test

// Round 3 probes: the flush walk on a real net/http server whose connection
// breaks at the gate's final write, through the writer stacks echo users have.

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// unwrapOnly is a status-recording wrapper with Unwrap and no Flush.
type unwrapOnly struct {
	http.ResponseWriter
	status int
}

func (w *unwrapOnly) WriteHeader(code int)        { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *unwrapOnly) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func unwrapOnlyMiddleware(next echolib.HandlerFunc) echolib.HandlerFunc {
	return func(c echolib.Context) error {
		c.Response().Writer = &unwrapOnly{ResponseWriter: c.Response().Writer}
		return next(c)
	}
}

func TestR3RealServerBrokenPipeThroughWriterStacks(t *testing.T) {
	big := strings.Repeat("x", 64<<10)
	for _, tc := range []struct {
		name  string
		outer []echolib.MiddlewareFunc
		gzip  bool
		body  string
	}{
		{"no wrapper, 64 KiB body", nil, false, big},
		{"BodyDump outside", []echolib.MiddlewareFunc{middleware.BodyDump(func(echolib.Context, []byte, []byte) {})}, false, "paid"},
		{"Gzip then BodyDump outside", []echolib.MiddlewareFunc{middleware.Gzip(), middleware.BodyDump(func(echolib.Context, []byte, []byte) {})}, true, "paid"},
		{"Gzip MinLength 1024, short body", []echolib.MiddlewareFunc{middleware.GzipWithConfig(middleware.GzipConfig{MinLength: 1024})}, true, "paid"},
		{"Gzip outside, 64 KiB body", []echolib.MiddlewareFunc{middleware.Gzip()}, true, big},
		{"wrapper with Unwrap and no Flush", []echolib.MiddlewareFunc{unwrapOnlyMiddleware}, false, "paid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fail atomic.Bool
			f := testkit.StartFacilitator(t)
			f.OnSettle(func(n int32, w http.ResponseWriter) {
				if n == 1 {
					fail.Store(true)
					testkit.SettleOK(w)
					return
				}
				testkit.SettleAlreadySettled(w)
			})
			g := testkit.NewGate(t, f)
			e := r3Echo()
			e.Use(middleware.Recover())
			for _, m := range tc.outer {
				e.Use(m)
			}
			e.Use(cantonecho.Middleware(g.Middleware()))
			var hits atomic.Int32
			e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, tc.body) })
			sig := testkit.SignatureFor(t, e, testkit.PreparedTx)

			server := httptest.NewUnstartedServer(e)
			server.Listener = brokenPipeListener{Listener: server.Listener, fail: &fail}
			server.Start()
			defer server.Close()

			code, body, err := getWith(t, server.URL+"/paid", sig, tc.gzip)
			t.Logf("first: code=%d len=%d err=%v settles=%d", code, len(body), err, f.SettleCalls.Load())
			fail.Store(false)
			code, body, err = retryWhileInProgress(func() (int, string, error) { return getWith(t, server.URL+"/paid", sig, false) })
			t.Logf("retry: code=%d len=%d err=%v hits=%d", code, len(body), err, hits.Load())
			if code != 200 {
				t.Logf("retry body: %q", body)
			}
			require.NoError(t, err)
			assert.Equal(t, 200, code, "nothing reached the payer: the retry is served once")
			code, _, _ = retryWhileInProgress(func() (int, string, error) { return getWith(t, server.URL+"/paid", sig, false) })
			assert.Equal(t, 402, code, "then the payment is redeemed")
		})
	}
}

// closingListener keeps the accepted connections so a test can close them: the
// connection really goes away (reads and writes fail, a blocked read returns).
type closingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *closingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *closingListener) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		_ = c.Close()
	}
	l.conns = nil
}

// closeAtWrite closes the connection when the gate starts its final write (after
// the claim): an outer echo hook, or a net/http wrapper with Unwrap.
type closeAtWrite struct {
	http.ResponseWriter
	close func()
}

func (w closeAtWrite) WriteHeader(code int) {
	if code == http.StatusOK {
		w.close()
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w closeAtWrite) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestR3ConnectionClosedAtTheFinalWrite(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, viaEcho := range []bool{true, false} {
			name := map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2] + " " + map[bool]string{true: "echo", false: "net/http gate"}[viaEcho]
			if h2 && !closeWait {
				continue // over HTTP/2 the server notices a close asynchronously: see the README
			}
			t.Run(name, func(t *testing.T) {
				listener := &closingListener{}
				var armed atomic.Bool
				closeOnce := func(ctx context.Context) {
					if armed.CompareAndSwap(true, false) {
						listener.closeAll()
						if closeWait { // the server has seen the close once the request's context ends
							select {
							case <-ctx.Done():
							case <-time.After(5 * time.Second):
								t.Error("the server never saw the closed connection")
							}
						}
					}
				}
				f := testkit.StartFacilitator(t)
				f.OnSettle(func(n int32, w http.ResponseWriter) {
					if n == 1 {
						armed.Store(true)
						testkit.SettleOK(w)
						return
					}
					testkit.SettleAlreadySettled(w)
				})
				g := testkit.NewGate(t, f)
				var hits atomic.Int32
				var h http.Handler
				if viaEcho {
					e := r3Echo()
					e.Use(middleware.Recover())
					e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc {
						return func(c echolib.Context) error {
							c.Response().Before(func() {
								if c.Response().Status == http.StatusOK {
									closeOnce(c.Request().Context())
								}
							})
							return next(c)
						}
					})
					e.Use(cantonecho.Middleware(g.Middleware()))
					e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid") })
					h = e
				} else {
					mux := http.NewServeMux()
					mux.HandleFunc("GET /paid", func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); _, _ = w.Write([]byte("paid")) })
					inner := g.Middleware()(mux)
					h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						inner.ServeHTTP(closeAtWrite{ResponseWriter: w, close: func() { closeOnce(r.Context()) }}, r) //nolint:contextcheck // r is the request being served
					})
				}
				sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
				server := httptest.NewUnstartedServer(h)
				listener.Listener = server.Listener
				server.Listener = listener
				if h2 {
					server.EnableHTTP2 = true
					server.StartTLS()
				} else {
					server.Start()
				}
				defer server.Close()
				get := func() (int, string, string, error) {
					tr := server.Client().Transport.(*http.Transport).Clone()
					tr.DisableKeepAlives = !h2
					if h2 {
						tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test server
						tr.ForceAttemptHTTP2 = true
					}
					req, _ := http.NewRequest("GET", server.URL+"/paid", nil)
					req.Header.Set("Accept", "application/json")
					req.Header.Set("PAYMENT-SIGNATURE", sig)
					resp, err := (&http.Client{Transport: tr}).Do(req)
					if err != nil {
						return 0, "", "", err
					}
					defer resp.Body.Close()
					b, _ := io.ReadAll(resp.Body)
					return resp.StatusCode, string(b), resp.Proto, nil
				}
				code, body, proto, err := get()
				t.Logf("first: code=%d body=%q proto=%s err=%v settles=%d", code, body, proto, err, f.SettleCalls.Load())
				code, body, err = retryWhileInProgress(func() (int, string, error) {
					c, b, p, e := get()
					proto = p
					return c, b, e
				})
				t.Logf("retry: code=%d body=%q proto=%s err=%v hits=%d", code, body, proto, err, hits.Load())
				require.NoError(t, err)
				assert.Equal(t, 200, code, "nothing reached the payer: the retry is served once")
			})
		}
	}
}

// closeWait makes the connection-closing hook wait until the server has seen the
// close (the request's context ends), instead of a fixed sleep.
var closeWait = true

// The same without waiting: the connection is closed at the very moment of the
// gate's final write. HTTP/1.1 only: HTTP/2 learns of a close asynchronously.
func TestR3ConnectionClosedAtTheFinalWriteNoWait(t *testing.T) {
	old := closeWait
	closeWait = false
	defer func() { closeWait = old }()
	TestR3ConnectionClosedAtTheFinalWrite(t)
}

// retryWhileInProgress repeats the request while the gate answers 503 "payment in
// progress" or "retry the same payment" (the claim is not yet marked): the first request's post-response Store work may outlive its client's
// EOF, and the gate's answer is then correct. Bounded.
func retryWhileInProgress(get func() (int, string, error)) (code int, body string, err error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body, err = get()
		if err != nil || code != http.StatusServiceUnavailable || (!strings.Contains(body, "payment in progress") && !strings.Contains(body, "retry the same payment")) || time.Now().After(deadline) {
			return code, body, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
