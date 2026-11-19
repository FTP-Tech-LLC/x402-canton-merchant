package gin_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// P12: gin.Recovery registered inside the gate, panic after part of the body
// was written: Recovery's AbortWithStatus(500) cannot change a started 2xx,
// the adapter sees "aborted, written" and the partial body is settled.
func TestProbeRecoveryInsideAfterPartialWrite(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(cantongin.Middleware(g.Middleware()))
	r.Use(gingonic.Recovery())
	r.GET("/paid", func(c *gingonic.Context) {
		c.Status(200)
		_, _ = c.Writer.WriteString(`{"rows":[1,2,`)
		panic("bug after a partial write")
	})
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("recovery inside after write: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load(), "a panic is never settled")
}

// P12b: Recovery outside, panic after a partial write: not settled (control).
func TestProbeRecoveryOutsideAfterPartialWrite(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) {
		_, _ = c.Writer.WriteString(`{"rows":[1,2,`)
		panic("bug")
	})
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("recovery outside after write: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load())
	assert.Equal(t, 500, w.Code)
}

// P13: HandleContext from a paid route into another paid route.
func TestProbeHandleContextIntoAnotherPaidRoute(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{
			"GET /paid":    {PayTo: testkit.PayTo, Price: canton.CC("0.01")},
			"GET /v2/paid": {PayTo: testkit.PayTo, Price: canton.CC("0.01")},
		}
	})
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.Request.URL.Path = "/v2/paid"; r.HandleContext(c) })
	r.GET("/v2/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	w := testkit.Do(r, "GET", "/paid", sig)
	t.Logf("HandleContext into paid: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	retry := testkit.Do(r, "GET", "/paid", sig)
	t.Logf("HandleContext retry: code=%d body=%q settles=%d", retry.Code, retry.Body.String(), f.SettleCalls.Load())
	assert.False(t, f.SettleCalls.Load() > 0 && !strings.Contains(w.Body.String(), "paid content"), "charged without delivery")
}

// P14: no gin.Recovery, a non-Flusher wrapper in front of gin, a real server.
func TestProbeNonFlusherWithoutRecoveryRealServer(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New() // no Recovery: a panic goes to net/http
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	srv := httptest.NewUnstartedServer(withRecorder(r))
	srv.Config.ErrorLog = nil
	srv.Start()
	t.Cleanup(srv.Close)

	// The 402 offer itself panics behind this wrapper; read it from a recorder.
	offer := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		defer func() { _ = recover() }()
		withRecorder(r).ServeHTTP(w, q)
	})
	first, err0 := http.Get(srv.URL + "/paid")
	if err0 == nil {
		_ = first.Body.Close()
		t.Logf("402 offer over the real server: status=%d", first.StatusCode)
	} else {
		t.Logf("402 offer over the real server: client error=%v", err0)
	}
	sig := testkit.SignatureFor(t, offer, testkit.PreparedTx)
	req, _ := http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PAYMENT-SIGNATURE", sig)
	resp, err := http.DefaultClient.Do(req)
	var body []byte
	if err == nil {
		body, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Logf("real server: status=%d body=%q settles=%d", resp.StatusCode, body, f.SettleCalls.Load())
	} else {
		t.Logf("real server: client error=%v settles=%d", err, f.SettleCalls.Load())
	}
	assert.False(t, f.SettleCalls.Load() > 0 && !bytes.Contains(body, []byte("paid content")), "charged and nothing delivered")

	req2, _ := http.NewRequest("GET", srv.URL+"/paid", nil)
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("PAYMENT-SIGNATURE", sig)
	resp2, err2 := http.DefaultClient.Do(req2)
	if err2 != nil {
		t.Logf("retry of the same payment: client error=%v", err2)
		return
	}
	b2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	t.Logf("retry of the same payment: status=%d body=%q", resp2.StatusCode, b2)
}

// P15: http.ServeContent answers 412 with WriteHeader only.
func TestProbePreconditionFailedIsNotSettled(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) {
		c.Header("ETag", `"v2"`)
		http.ServeContent(c.Writer, c.Request, "report.json", time.Now(), strings.NewReader(`{"report":1}`))
	})
	w := doReq(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx), map[string]string{"If-Match": `"v1"`})
	t.Logf("412: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load())
}

// P15b: http.ServeFile on a directory without a trailing slash: 301 with WriteHeader only.
func TestProbeServeFileDirectoryRedirect(t *testing.T) {
	dir := t.TempDir()
	f, r := paidEngine(t, func(c *gingonic.Context) { c.File(dir) })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("dir redirect: code=%d location=%q body=%q settles=%d", w.Code, w.Header().Get("Location"), w.Body.String(), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load())
}

// valueWriter is a gin.ResponseWriter wrapper kept by value with a slice:
// an uncomparable dynamic type.
type valueWriter struct {
	gingonic.ResponseWriter
	tags []string
}

// P16: the adapter compares the gate's writer with gin's by ==; with an
// uncomparable writer type an earlier middleware installed, a free route panics.
func TestProbeUncomparableWriterOnFreeRoute(t *testing.T) {
	_, r := paidEngine(t, func(c *gingonic.Context) { c.String(200, "paid") })
	r2 := gingonic.New()
	r2.Use(func(c *gingonic.Context) {
		c.Writer = valueWriter{ResponseWriter: c.Writer, tags: []string{"a"}}
		c.Next()
	})
	_ = r
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r2.Use(cantongin.Middleware(g.Middleware()))
	r2.GET("/free", func(c *gingonic.Context) { c.String(200, "free") })
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		w := testkit.Do(r2, "GET", "/free", "")
		t.Logf("free route: code=%d body=%q", w.Code, w.Body.String())
	}()
	t.Logf("uncomparable writer: panic=%v", panicked)
	assert.Nil(t, panicked)
}

// P17: the adapter's own 500 for a bare Abort carries the handler's headers.
func TestProbeBareAbortCarriesHandlerCookie(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) {
		c.SetCookie("access", "paid-token", 3600, "/", "", true, true)
		c.Abort()
	})
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("bare abort: code=%d set-cookie=%q settles=%d", w.Code, w.Header().Get("Set-Cookie"), f.SettleCalls.Load())
	assert.Empty(t, w.Header().Get("Set-Cookie"), "an unpaid refusal carries no handler cookie")
}

// P18: AbortWithError(401, err) for an unauthenticated payer: not settled (control).
func TestProbeAbortWithErrorControl(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) { _ = c.AbortWithError(http.StatusUnauthorized, errors.New("no key")) })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("abort with error: code=%d settles=%d", w.Code, f.SettleCalls.Load())
	assert.Equal(t, 401, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
}

// gin's NoRoute handler that answers JSON 404: not offered, not settled.
func TestDoubleSlashWithANoRouteHandler(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) { c.String(200, "paid") })
	r.NoRoute(func(c *gingonic.Context) { c.JSON(404, gingonic.H{"error": "not found"}) })
	w := testkit.Do(r, "GET", "//paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	assert.Equal(t, 404, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
}

// P20: Logger outside: Status and Size seen after the gate.
func TestProbeOuterSeesFinalStatusAndSize(t *testing.T) {
	var status, size int
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(func(c *gingonic.Context) { c.Next(); status, size = c.Writer.Status(), c.Writer.Size() })
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "hello") })
	assert.Equal(t, 402, testkit.Do(r, "GET", "/paid", "").Code)
	t.Logf("after 402: status=%d size=%d", status, size)
	assert.Equal(t, 402, status)
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("after 200: code=%d status=%d size=%d", w.Code, status, size)
	assert.Equal(t, 200, status)
	assert.Equal(t, 5, size)
}

// The replay of a delivered payment through a NoRoute spelling is gin's 404 and
// leaves the delivered claim alone.
func TestReplayThroughDoubleSlash(t *testing.T) {
	var hits int
	f, r := paidEngine(t, func(c *gingonic.Context) { hits++; c.String(200, "paid") })
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	require.Equal(t, 200, testkit.Do(r, "GET", "/paid", sig).Code)
	w := testkit.Do(r, "GET", "//paid", sig)
	assert.Equal(t, 404, w.Code)
	assert.Equal(t, 402, testkit.Do(r, "GET", "/paid", sig).Code)
	assert.Equal(t, int32(1), f.SettleCalls.Load())
	assert.Equal(t, 1, hits)
}

// A group created before r.Use copies the engine's handlers at that moment: its
// routes have no gate. Check reports it.
func TestGroupCreatedBeforeUseIsReportedByCheck(t *testing.T) {
	f := testkit.StartFacilitator(t)
	routes := map[string]canton.Route{"GET /api/v1/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
	r := gingonic.New()
	api := r.Group("/api")
	r.Use(cantongin.Middleware(g.Middleware()))
	api.GET("/v1/data", func(c *gingonic.Context) { c.String(200, "paid content") })
	assert.Equal(t, 200, testkit.Do(r, "GET", "/api/v1/data", "").Code)
	require.Error(t, cantongin.Check(r, routes))
}
