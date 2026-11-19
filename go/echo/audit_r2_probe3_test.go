package echo_test

// Round-2 probes: HTTP/2, http.TimeoutHandler in front of echo with echo's own
// writer wrappers outside the gate, and the package-doc order.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

func doClient(t *testing.T, client *http.Client, url, sig string) (int, string, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	if sig != "" {
		req.Header.Set("PAYMENT-SIGNATURE", sig)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Proto, nil
}

// HTTP/2 through the adapter, with and without Gzip outside: delivered once,
// the replay is the final 402, nothing panics.
func TestR2HTTP2DeliversOnce(t *testing.T) {
	for _, gz := range []bool{false, true} {
		f := testkit.StartFacilitator(t)
		g := testkit.NewGate(t, f)
		e := r2Echo()
		e.Use(middleware.Recover())
		if gz {
			e.Use(middleware.Gzip())
		}
		e.Use(cantonecho.Middleware(g.Middleware()))
		e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid") })
		sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
		server := httptest.NewUnstartedServer(e)
		server.EnableHTTP2 = true
		server.StartTLS()
		client := server.Client()
		code, body, proto, err := doClient(t, client, server.URL+"/paid", sig)
		require.NoError(t, err)
		replay, _, _, err := doClient(t, client, server.URL+"/paid", sig)
		require.NoError(t, err)
		t.Logf("gzip=%v proto=%s first=%d %q replay=%d settles=%d", gz, proto, code, body, replay, f.SettleCalls.Load())
		assert.Equal(t, "HTTP/2.0", proto)
		assert.Equal(t, 200, code)
		assert.Equal(t, 402, replay)
		server.Close()
	}
}

// http.TimeoutHandler (longer than the budget) in front of echo, echo's BodyDump
// outside the gate: a real server, Recover outermost. The gate's final flush
// panics inside BodyDump; the payer got the answer, but its replay is 503.
func TestR2TimeoutHandlerAndBodyDumpRealServer(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	var panics atomic.Int32
	e.Use(middleware.RecoverWithConfig(middleware.RecoverConfig{
		LogErrorFunc: func(_ echolib.Context, err error, _ []byte) error {
			panics.Add(1)
			t.Logf("recovered: %v", err)
			return err
		},
	}))
	e.Use(middleware.BodyDump(func(echolib.Context, []byte, []byte) {}))
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid") })
	server := httptest.NewServer(http.TimeoutHandler(e, 10*time.Minute, "timeout"))
	defer server.Close()
	client := server.Client()

	offer, _, _, err := doClient(t, client, server.URL+"/paid", "")
	require.NoError(t, err)
	t.Logf("offer=%d panics=%d", offer, panics.Load())
	sig := testkit.SignatureForPath(t, e, "/paid", testkit.PreparedTx) // in-process: the recorder flushes
	before := panics.Load()
	code, body, _, err := doClient(t, client, server.URL+"/paid", sig)
	require.NoError(t, err)
	replay, replayBody, _, err := doClient(t, client, server.URL+"/paid", sig)
	require.NoError(t, err)
	t.Logf("paid=%d %q (panics %d) settles=%d; replay=%d %q", code, body, panics.Load()-before, f.SettleCalls.Load(), replay, replayBody)
	assert.Zero(t, panics.Load(), "no request on a paid route panics")
	assert.Equal(t, 402, replay, "the replay of a delivered payment is the final 402")
}

// The package doc now says Recover goes BEFORE (outside) the gate. Recover inside
// the gate recovers a panic below the adapter and cannot be seen: unsupported.
func TestRecoverOutsideTheGateIsTheDocumentedOrder(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error {
		_ = c.String(200, "partial paid content")
		panic("bug after writing")
	})
	w := testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
	assert.Equal(t, 500, w.Code)
	assert.Zero(t, f.SettleCalls.Load(), "a panic is never settled")
}
