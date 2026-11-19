package echo_test

// Round-2 probes: a real net/http server whose connection breaks at the gate's
// final write (writes fail like a broken pipe while the request context is
// still live), and cantonecho.Check.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type brokenPipeConn struct {
	net.Conn
	fail *atomic.Bool
}

func (c brokenPipeConn) Write(p []byte) (int, error) {
	if c.fail.Load() {
		return 0, errors.New("write: broken pipe")
	}
	return c.Conn.Write(p)
}

type brokenPipeListener struct {
	net.Listener
	fail *atomic.Bool
}

func (l brokenPipeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return brokenPipeConn{Conn: c, fail: l.fail}, nil
}

func getWith(t *testing.T, url, sig string, gzip bool) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PAYMENT-SIGNATURE", sig)
	if gzip {
		req.Header.Set("Accept-Encoding", "gzip") // set by hand: the transport does not decode it
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

// R2-1 on a real server: the socket breaks right after /settle, while the
// payer is still connected (no context cancel). Without a wrapper the gate sees
// net/http's flush error and the retry is served once; with echo's Gzip outside
// the gate the error is dropped, the claim is marked, and the retry gets 402.
func TestR2RealServerBrokenPipeAtTheFinalWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		gzip bool
	}{{"no wrapper (control)", false}, {"Gzip outside the gate", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var fail atomic.Bool
			f := testkit.StartFacilitator(t)
			f.OnSettle(func(n int32, w http.ResponseWriter) {
				if n == 1 {
					fail.Store(true) // the payer's connection breaks while the gate settles
					testkit.SettleOK(w)
					return
				}
				testkit.SettleAlreadySettled(w)
			})
			g := testkit.NewGate(t, f)
			e := r2Echo()
			e.Use(middleware.Recover())
			if tc.gzip {
				e.Use(middleware.Gzip())
			}
			e.Use(cantonecho.Middleware(g.Middleware()))
			var hits atomic.Int32
			e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid") })
			sig := testkit.SignatureFor(t, e, testkit.PreparedTx)

			server := httptest.NewUnstartedServer(e)
			server.Listener = brokenPipeListener{Listener: server.Listener, fail: &fail}
			server.Start()
			defer server.Close()

			code, body, err := getWith(t, server.URL+"/paid", sig, tc.gzip)
			t.Logf("first: code=%d body=%q err=%v settles=%d", code, body, err, f.SettleCalls.Load())
			fail.Store(false)
			code, body, err = getWith(t, server.URL+"/paid", sig, false)
			t.Logf("retry of the same payment: code=%d body=%q err=%v hits=%d", code, body, err, hits.Load())
			require.NoError(t, err)
			assert.Equal(t, 200, code, "nothing reached the payer: the retry is served once")
		})
	}
}

// R2-3: cantonecho.Check reads e.Routes(), the default router only. A paid path
// registered with Any on an e.Host router passes Check, and POST is served free.
func TestR2CheckMissesAnyOnAHostRouter(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	e.Use(cantonecho.Middleware(g.Middleware()))
	var hits atomic.Int32
	api := e.Host("api.example.com")
	api.Any("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid content") })

	err := cantonecho.Check(e, testkit.Routes())
	r := httptest.NewRequestWithContext(context.Background(), "POST", "http://api.example.com/paid", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	t.Logf("Check=%v; POST api.example.com/paid without payment: %d %q hits=%d", err, w.Code, w.Body.String(), hits.Load())
	assert.Error(t, err, "Check must report Any on a paid path")
}

// R2-4: Check passes setups in which every paid request is refused after
// /verify (no charge, but the paid route never works): the gate in e.Pre, a
// param route serving the paid path, and the gate registered twice.
func TestR2CheckPassesSetupsThatNeverDeliver(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(e *echolib.Echo, mw echolib.MiddlewareFunc, h echolib.HandlerFunc)
	}{
		{"gate in e.Pre", func(e *echolib.Echo, mw echolib.MiddlewareFunc, h echolib.HandlerFunc) {
			e.Pre(mw)
			e.GET("/paid", h)
		}},
		{"param route", func(e *echolib.Echo, mw echolib.MiddlewareFunc, h echolib.HandlerFunc) {
			e.Use(mw)
			e.GET("/:name", h)
		}},
		{"gate on e.Use and on the group", func(e *echolib.Echo, mw echolib.MiddlewareFunc, h echolib.HandlerFunc) {
			e.Use(mw)
			grp := e.Group("")
			grp.Use(mw)
			grp.GET("/paid", h)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			e := r2Echo()
			var hits atomic.Int32
			tc.build(e, cantonecho.Middleware(g.Middleware()), func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid") })
			checkErr := cantonecho.Check(e, testkit.Routes())
			w := testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
			t.Logf("Check=%v; paid request: %d %q hits=%d verify=%d settles=%d", checkErr, w.Code, w.Body.String(),
				hits.Load(), f.VerifyCalls.Load(), f.SettleCalls.Load())
			switch tc.name {
			case "gate in e.Pre":
				// Check cannot see e.Pre (documented): nothing is charged, and the adapter
				// logs a warning on the first paid request.
				assert.Zero(t, f.SettleCalls.Load())
				assert.NotEqual(t, 200, w.Code)
			case "gate on e.Use and on the group":
				// A second registration passes the request through: the outer gate serves it.
				assert.Equal(t, 200, w.Code)
				assert.Equal(t, int32(1), f.SettleCalls.Load())
				assert.NoError(t, checkErr)
			default:
				assert.Zero(t, f.SettleCalls.Load())
				assert.Error(t, checkErr, "Check passed a setup whose paid route never delivers")
			}
		})
	}
}

// R2-5: Check reports e.Match([GET, HEAD]) on a paid GET path as "served free",
// but the SDK gates HEAD through the GET key: HEAD gets the 402 offer without a
// payment and 405 with one.
func TestR2CheckFlagsHeadThatIsGated(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r2Echo()
	e.Use(cantonecho.Middleware(g.Middleware()))
	var hits atomic.Int32
	e.Match([]string{"GET", "HEAD"}, "/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "paid") })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	unpaid := testkit.Do(e, "HEAD", "/paid", "")
	paid := testkit.Do(e, "HEAD", "/paid", sig)
	err := cantonecho.Check(e, testkit.Routes())
	t.Logf("HEAD unpaid=%d, HEAD paid=%d, hits=%d, settles=%d; Check=%v", unpaid.Code, paid.Code, hits.Load(), f.SettleCalls.Load(), err)
	assert.Zero(t, hits.Load())
	if err != nil {
		assert.NotContains(t, err.Error(), "HEAD", "HEAD is gated by the GET key: not served free")
	}
}

// R2-6: Check with a group-level gate on a paid group root ("GET /api"): echo's
// Group.Use registers RouteNotFound routes for the group root, and Check reads
// them as a method that is not a paid key.
func TestR2CheckWithAGroupGateOnAPaidGroupRoot(t *testing.T) {
	f := testkit.StartFacilitator(t)
	routes := map[string]canton.Route{"GET /api": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
	e := r2Echo()
	api := e.Group("/api")
	api.Use(cantonecho.Middleware(g.Middleware()))
	api.GET("", func(c echolib.Context) error { return c.String(200, "paid") })
	unpaid := testkit.Do(e, "GET", "/api", "")
	err := cantonecho.Check(e, routes)
	t.Logf("GET /api unpaid=%d; Check=%v", unpaid.Code, err)
	if err != nil {
		assert.False(t, strings.Contains(err.Error(), echolib.RouteNotFound), "a RouteNotFound route is not a method")
	}
}
