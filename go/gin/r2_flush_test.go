package gin_test

// Round-2 audit probes (lens: gin): the flush error through gin writer
// wrappers registered outside (before) the gate. Each probe asserts the money
// rule; a failing probe is a finding.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// r2BrokenConn is shaped like net/http's *response after the peer went away:
// writes are buffered (no error), FlushError reports the broken connection and
// the plain Flush() swallows it.
type r2BrokenConn struct{ *httptest.ResponseRecorder }

func (w r2BrokenConn) FlushError() error { return io.ErrClosedPipe }
func (w r2BrokenConn) Flush()            { _ = w.FlushError() }

// r2UnwrapWrapper is a gin writer wrapper that follows the README's rule for a
// wrapper in front of the gate: it implements Unwrap() http.ResponseWriter.
// (A status or metrics recorder, a body logger.)
type r2UnwrapWrapper struct {
	gingonic.ResponseWriter
	status int
}

func (w *r2UnwrapWrapper) WriteHeader(code int)        { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *r2UnwrapWrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func r2BrokenThenRetry(t *testing.T, wrap gingonic.HandlerFunc) (retry int, body string, settles int32) {
	t.Helper()
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
	if wrap != nil {
		r.Use(wrap)
	}
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	req := httptest.NewRequest("GET", "/paid", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PAYMENT-SIGNATURE", sig)
	r.ServeHTTP(r2BrokenConn{httptest.NewRecorder()}, req) // the payer's connection broke
	again := testkit.Do(r, "GET", "/paid", sig)
	return again.Code, again.Body.String(), f.SettleCalls.Load()
}

// Control: gin's own writer, no wrapper outside the gate (round-1 finding 4 fix).
func TestR2BrokenConnectionNoWrapperControl(t *testing.T) {
	code, body, settles := r2BrokenThenRetry(t, nil)
	t.Logf("no wrapper: retry=%d %q settles=%d", code, body, settles)
	assert.Equal(t, 200, code, "the retry of an undelivered payment is served once")
}

// A wrapper outside the gate that implements Unwrap, as the README asks: the
// adapter unwraps one level, lands on gin's own writer, and
// http.NewResponseController prefers its Flush() (which drops the error) over
// Unwrap. The undelivered answer is marked delivered.
func TestR2BrokenConnectionThroughUnwrapWrapper(t *testing.T) {
	code, body, settles := r2BrokenThenRetry(t, func(c *gingonic.Context) {
		c.Writer = &r2UnwrapWrapper{ResponseWriter: c.Writer}
		c.Next()
	})
	t.Logf("Unwrap wrapper outside: retry=%d %q settles=%d", code, body, settles)
	assert.Equal(t, 200, code, "the retry of an undelivered payment is served once")
}
