package gin_test

// Audit probes for the gin adapter (lens: gin). Each probe asserts the money
// rule; a failing probe is a finding.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// doReq sends a request with extra headers.
func doReq(h http.Handler, method, target, sig string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Accept", "application/json")
	if sig != "" {
		r.Header.Set("PAYMENT-SIGNATURE", sig)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// sigFor builds a payment from the 402 of any method and path.
func sigFor(t *testing.T, h http.Handler, method, path string) string {
	t.Helper()
	w := testkit.Do(h, method, path, "")
	require.Equal(t, 402, w.Code, "%s %s: %s", method, path, w.Body.String())
	var required map[string]any
	testkit.DecodeHeader(t, w.Header().Get("PAYMENT-REQUIRED"), &required)
	envelope, err := json.Marshal(map[string]any{
		"x402Version": 2, "resource": required["resource"], "accepted": required["accepts"].([]any)[0],
		"payload": map[string]any{"assetTransferMethod": "transfer-factory", "preparedTransaction": "H4sIAAAA",
			"preparedTxHash": testkit.PreparedTx, "signature": "c2lnbmF0dXJl"},
	})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(envelope)
}

func paidEngine(t *testing.T, handler gingonic.HandlerFunc, mutate ...func(*canton.Config)) (*testkit.Facilitator, *gingonic.Engine) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, mutate...)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", handler)
	return f, r
}

// P1: c.Status(code) with no body is gin's normal way to answer a bare status
// (gin's engine sends it with its final WriteHeaderNow). Through the adapter
// the status never reaches the gate's buffer: the gate sees 200 and settles.
func TestProbeStatusWithoutBodyIsNotSettled(t *testing.T) {
	for _, status := range []int{404, 500, 503} {
		f, r := paidEngine(t, func(c *gingonic.Context) { c.Status(status) })
		w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
		assert.Equal(t, status, w.Code, "client must get the handler's status %d", status)
		assert.Zero(t, f.SettleCalls.Load(), "a bare %d must not be settled", status)
	}
}

// P1b: c.Writer.WriteHeader(500) then return (net/http style) is the same.
func TestProbeWriteHeaderWithoutBodyIsNotSettled(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) { c.Writer.WriteHeader(http.StatusInternalServerError) })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	assert.Equal(t, 500, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
}

// P2: c.Redirect on a POST route: http.Redirect writes no body for POST, so
// the 302 stays pending in the gate writer and the gate settles a 200.
func TestProbeRedirectOnPostIsNotSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"POST /paid": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	})
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.POST("/paid", func(c *gingonic.Context) { c.Redirect(http.StatusFound, "/elsewhere") })
	sig := sigFor(t, r, "POST", "/paid")
	w := testkit.Do(r, "POST", "/paid", sig)
	t.Logf("POST redirect: code=%d location=%q body=%q settles=%d", w.Code, w.Header().Get("Location"), w.Body.String(), f.SettleCalls.Load())
	assert.Equal(t, 502, w.Code, "a redirect is 502 and not charged")
	assert.Zero(t, f.SettleCalls.Load())
}

// P2b: the same on GET when the handler set a Content-Type before redirecting.
func TestProbeRedirectWithContentTypeIsNotSettled(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) {
		c.Header("Content-Type", "application/json")
		c.Redirect(http.StatusFound, "/elsewhere")
	})
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("GET redirect with CT: code=%d location=%q settles=%d", w.Code, w.Header().Get("Location"), f.SettleCalls.Load())
	assert.Equal(t, 502, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
}

// P3: c.File with a conditional request: http.ServeFile answers 304 with
// WriteHeader only. The payer is charged for an empty 200.
func TestProbeFileNotModifiedIsNotSettled(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "report.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"report":1}`), 0o600))
	mod := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(file, mod, mod))
	f, r := paidEngine(t, func(c *gingonic.Context) { c.File(file) })
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	w := doReq(r, "GET", "/paid", sig, map[string]string{"If-Modified-Since": time.Now().UTC().Format(http.TimeFormat)})
	t.Logf("304 file: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.NotEqual(t, 200, w.Code, "a 304 must not become a charged empty 200")
	assert.Zero(t, f.SettleCalls.Load())
}

// "//paid" is a paid spelling for the SDK, but gin (RemoveExtraSlash false, the
// default) routes it to no handler: it runs the NoRoute chain. The adapter does
// not run the gate there: no offer, no charge, gin's own 404.
func TestDoubleSlashIsNeitherOfferedNorCharged(t *testing.T) {
	var hits int
	f, r := paidEngine(t, func(c *gingonic.Context) { hits++; c.String(200, "paid") })
	w := testkit.Do(r, "GET", "//paid", "")
	assert.Equal(t, 404, w.Code)
	assert.Empty(t, w.Header().Get("PAYMENT-REQUIRED"))
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	w = testkit.Do(r, "GET", "//paid", sig)
	assert.Equal(t, 404, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
	assert.Zero(t, hits)
}

func TestTrailingSlashWithoutRedirectIsNeitherOfferedNorCharged(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) { c.String(200, "paid") })
	r.RedirectTrailingSlash = false
	assert.Equal(t, 404, testkit.Do(r, "GET", "/paid/", "").Code)
	w := testkit.Do(r, "GET", "/paid/", testkit.SignatureFor(t, r, testkit.PreparedTx))
	assert.Equal(t, 404, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
}

// HandleMethodNotAllowed with a paid key the router does not serve for that method.
func TestMethodNotAllowedIsNeitherOfferedNorCharged(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{
			"GET /paid":  {PayTo: testkit.PayTo, Price: canton.CC("0.01")},
			"POST /paid": {PayTo: testkit.PayTo, Price: canton.CC("0.01")},
		}
	})
	r := gingonic.New()
	r.HandleMethodNotAllowed = true
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid") })
	w := testkit.Do(r, "POST", "/paid", "")
	assert.Equal(t, 405, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
}

// P5: the common gin error pattern: handlers call c.Error(err) and return,
// an error middleware registered with r.Use renders it after c.Next().
func TestProbeErrorMiddlewareOutsideTheGate(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(func(c *gingonic.Context) { // error renderer, as in the gin docs
		c.Next()
		if len(c.Errors) > 0 {
			c.JSON(http.StatusInternalServerError, gingonic.H{"error": c.Errors.Last().Error()})
		}
	})
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { _ = c.Error(errors.New("database down")) })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("c.Error: code=%d body=%q settles=%d receipt=%v", w.Code, w.Body.String(), f.SettleCalls.Load(), w.Header().Get("PAYMENT-RESPONSE") != "")
	assert.Zero(t, f.SettleCalls.Load(), "a handler error must not be settled")
}

// P6: a render error after part of the body was written (html/template
// streams into the writer): gin records c.Error and aborts; the adapter only
// refuses an abort with nothing written, so the truncated page is settled.
func TestProbeTemplateErrorMidPageIsNotSettled(t *testing.T) {
	tpl := template.Must(template.New("page").Parse(`<h1>report</h1>{{ .Missing.Field }}<p>end</p>`))
	f, r := paidEngine(t, func(c *gingonic.Context) { c.HTML(http.StatusOK, "page", map[string]any{"Missing": 7}) })
	r.SetHTMLTemplate(tpl)
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("template error: code=%d body=%q CL=%q settles=%d", w.Code, w.Body.String(), w.Header().Get("Content-Length"), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load(), "a render error must not be settled")
}

type failingReader struct{ sent bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "first half of the report "), nil
	}
	return 0, errors.New("upstream reset")
}

// P6b: DataFromReader whose reader fails midway.
func TestProbeReaderErrorMidBodyIsNotSettled(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) {
		c.DataFromReader(http.StatusOK, 100, "text/plain", &failingReader{}, nil)
	})
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("reader error: code=%d body=%q CL=%q settles=%d", w.Code, w.Body.String(), w.Header().Get("Content-Length"), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load(), "a truncated body must not be settled")
}

// recorder is a typical status-recording wrapper in front of the engine. It
// implements Unwrap, as the README asks, but not Flush.
type recorder struct {
	http.ResponseWriter
	status int
}

func (w *recorder) WriteHeader(code int)        { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func withRecorder(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(&recorder{ResponseWriter: w}, r) })
}

// P7: gin's own Flush asserts http.Flusher on its underlying writer. The gate
// flushes gin's writer after settle, so behind a non-Flusher wrapper every paid
// response panics after the charge.
func TestProbeNonFlusherWrapperInFrontOfGin(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) { c.String(200, "paid") })
	h := withRecorder(r)
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	var w *httptest.ResponseRecorder
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		w = testkit.Do(h, "GET", "/paid", sig)
	}()
	t.Logf("non-flusher: panic=%v settles=%d", panicked, f.SettleCalls.Load())
	if w != nil {
		t.Logf("code=%d body=%q", w.Code, w.Body.String())
	}
	assert.Nil(t, panicked)
	replay := testkit.Do(h, "GET", "/paid", sig)
	t.Logf("replay: code=%d body=%q", replay.Code, replay.Body.String())
	assert.Equal(t, 402, replay.Code, "a delivered payment's replay is the final 402")
}

// P7b: the same behind http.TimeoutHandler (its writer has no Flush).
func TestProbeTimeoutHandlerInFrontOfGin(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) { c.String(200, "paid") })
	h := http.TimeoutHandler(r, 10*time.Minute, "timeout")
	sig := testkit.SignatureFor(t, h, testkit.PreparedTx)
	var w *httptest.ResponseRecorder
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		w = testkit.Do(h, "GET", "/paid", sig)
	}()
	t.Logf("timeout handler: panic=%v settles=%d", panicked, f.SettleCalls.Load())
	if w != nil {
		t.Logf("code=%d body=%q", w.Code, w.Body.String())
	}
	assert.Nil(t, panicked)
	replay := testkit.Do(h, "GET", "/paid", sig)
	t.Logf("replay: code=%d body=%q", replay.Code, replay.Body.String())
	assert.Equal(t, 402, replay.Code)
}

// engine.HandleContext inside a paid chain resets c.Writer to gin's own writer:
// bytes it writes reach the client before the gate decides and cannot be taken
// back (documented: never call HandleContext on a paid route). What the adapter
// guarantees is that such an answer is never settled.
func TestHandleContextInsideAPaidChainIsNeverSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) {
		c.Request.URL.Path = "/impl"
		r.HandleContext(c)
	})
	r.GET("/impl", func(c *gingonic.Context) { c.String(200, "paid content") })
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	w := testkit.Do(r, "GET", "/paid", sig)
	assert.Zero(t, f.SettleCalls.Load(), "bytes sent behind the gate's back are never charged")
	assert.Empty(t, w.Header().Get("PAYMENT-RESPONSE"))
}

// A route registered before r.Use never runs the gate: gin leaves no hook, so
// the self-check Check reports it (documented).
func TestRouteRegisteredBeforeUseIsReportedByCheck(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	r.Use(cantongin.Middleware(g.Middleware()))
	assert.Equal(t, 200, testkit.Do(r, "GET", "/paid", "").Code, "gin gives the adapter no hook for this: it is served free")
	err := cantongin.Check(r, testkit.Routes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not 402")
}

// P10: AbortWithStatus(2xx) and the doc claim "an abort with a status ... never settled".
func TestProbeAbortWith2xxIsSettled(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) { c.AbortWithStatusJSON(http.StatusOK, gingonic.H{"cached": true}) })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("abort 200: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
}

// P11: c.Status(204) alone becomes a 200.
func TestProbeStatus204Only(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) { c.Status(http.StatusNoContent) })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("status 204 only: code=%d settles=%d", w.Code, f.SettleCalls.Load())
	assert.Equal(t, 204, w.Code)
}

var _ = bytes.NewBuffer
var _ = io.Discard
var _ = strings.Contains
