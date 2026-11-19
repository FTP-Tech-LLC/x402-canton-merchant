package echo_test

// Round-5 audit (lens: echo): the timing dependency of
// TestR3ConnectionClosedDuringSettle. The payer's first request ends for the
// client the moment the facilitator stub closes the connection, which is in the
// middle of /settle. The server still has to read the settle answer, claim,
// write (and fail), give the claim back and release the lock. The test then
// sleeps a fixed 150 ms and retries once. When the server-side work takes longer
// (CPU starvation under go test -race ./... while other packages build and run),
// the retry meets the first request's lock and is answered 503 "payment in
// progress: retry the same payment": correct for the gate, a failure for the test.
//
// settleStall stands for that stall: the settle answer is held back after the
// connection was closed, so the server-side work outlives the 150 ms sleep.

import (
	"crypto/tls"
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

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type r5closedRun struct {
	h2, viaEcho bool
	stall       time.Duration // held back after the connection closed, before the settle answer
	robustWait  bool          // the fix: wait for the server, then retry while "payment in progress"
	sawClose    bool          // the fix for HTTP/2: answer settle only after the server saw the close
}

func r5ConnectionClosedDuringSettle(t *testing.T, run r5closedRun) {
	listener := &closingListener{}
	var paidCtx atomic.Value // the first paid request's context, as the handler saw it
	f := testkit.StartFacilitator(t)
	f.OnSettle(func(n int32, w http.ResponseWriter) {
		if n == 1 {
			listener.closeAll()
			if run.sawClose {
				// The server notices a closed connection asynchronously (HTTP/2: its
				// serve loop). Release the settle answer only once it has: the paid
				// request's context ends then.
				if ctx, ok := paidCtx.Load().(interface{ Done() <-chan struct{} }); ok {
					select {
					case <-ctx.Done():
					case <-time.After(5 * time.Second):
						t.Error("the server never saw the closed connection")
					}
				}
			}
			time.Sleep(run.stall)
			testkit.SettleOK(w)
			return
		}
		testkit.SettleAlreadySettled(w)
	})
	g := testkit.NewGate(t, f)
	var hits atomic.Int32
	var inner http.Handler
	if run.viaEcho {
		e := r3Echo()
		e.Use(middleware.Recover())
		e.Use(cantonecho.Middleware(g.Middleware()))
		e.GET("/paid", func(c echolib.Context) error {
			if hits.Add(1) == 1 {
				paidCtx.Store(c.Request().Context())
			}
			return c.String(200, "paid")
		})
		inner = e
	} else {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /paid", func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) == 1 {
				paidCtx.Store(r.Context())
			}
			_, _ = w.Write([]byte("paid"))
		})
		inner = g.Middleware()(mux)
	}
	served := make(chan struct{}, 16)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { served <- struct{}{} }()
		inner.ServeHTTP(w, r)
	})
	sig := testkit.SignatureFor(t, inner, testkit.PreparedTx)
	server := httptest.NewUnstartedServer(h)
	listener.Listener = server.Listener
	server.Listener = listener
	if run.h2 {
		server.EnableHTTP2 = true
		server.StartTLS()
	} else {
		server.Start()
	}
	defer server.Close()
	get := func() (int, string, string, error) {
		tr := server.Client().Transport.(*http.Transport).Clone()
		tr.DisableKeepAlives = !run.h2
		if run.h2 {
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
	start := time.Now()
	code, body, proto, err := get()
	t.Logf("first: code=%d body=%q proto=%s err=%v after %s settles=%d", code, body, proto, err, time.Since(start).Round(time.Millisecond), f.SettleCalls.Load())
	if !run.robustWait {
		time.Sleep(150 * time.Millisecond) // as TestR3ConnectionClosedDuringSettle
		code, body, proto, err = get()
	} else {
		select { // the first request's handler has returned on the server
		case <-served:
		case <-time.After(30 * time.Second):
			t.Fatal("the first request never finished on the server")
		}
		// Post-response Store work may outlive the handler (releaseWait): retry the
		// same payment as the gate tells a client to, while it says "in progress".
		deadline := time.Now().Add(10 * time.Second)
		for {
			code, body, proto, err = get()
			if err != nil || code != http.StatusServiceUnavailable || !strings.Contains(body, "payment in progress") || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Logf("retry: code=%d body=%q proto=%s err=%v hits=%d", code, body, proto, err, hits.Load())
	require.NoError(t, err)
	assert.Equal(t, 200, code, "nothing reached the payer: the retry is served once")
}

func r5Variants(t *testing.T, stall time.Duration, robust bool, sawClose ...bool) {
	for _, h2 := range []bool{false, true} {
		for _, viaEcho := range []bool{true, false} {
			name := map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2] + " " + map[bool]string{true: "echo", false: "net/http gate"}[viaEcho]
			t.Run(name, func(t *testing.T) {
				r5ConnectionClosedDuringSettle(t, r5closedRun{h2: h2, viaEcho: viaEcho, stall: stall, robustWait: robust, sawClose: len(sawClose) > 0 && sawClose[0]})
			})
		}
	}
}

// The connection is closed in the middle of /settle: the settle answer is held
// until the server has seen the close, and the retry waits for the server. This
// replaces fixed sleeps, which failed under load.
func TestR5ClosedDuringSettle(t *testing.T) { r5Variants(t, 0, true, true) }

// The same with a stall after the close.
func TestR5ClosedDuringSettleWithAStall(t *testing.T) {
	r5Variants(t, 300*time.Millisecond, true, true)
}
