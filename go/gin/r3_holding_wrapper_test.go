package gin_test

// Round-3 audit probe (lens: core flush + gin): a wrapper in front of the gate
// that holds the status and body until it is flushed (an ETag or signature
// middleware: headers must be final before the status goes out). It follows
// the README: Unwrap() http.ResponseWriter, and a Flush that writes what it
// holds. The walker flushes it before reading the connection's error, but the
// gin adapter's outerWriter.FlushError first calls WriteHeaderNow, which sends
// gin's default 200 before the wrapper writes its held status.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type r3HoldingGinWriter struct {
	gingonic.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (w *r3HoldingGinWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *r3HoldingGinWriter) Write(b []byte) (int, error)       { return w.buf.Write(b) }
func (w *r3HoldingGinWriter) WriteString(s string) (int, error) { return w.buf.WriteString(s) }
func (w *r3HoldingGinWriter) Flush() {
	if w.status != 0 {
		w.ResponseWriter.WriteHeader(w.status)
		w.status = 0
	}
	if w.buf.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		w.buf.Reset()
	}
	w.ResponseWriter.Flush()
}
func (w *r3HoldingGinWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestR3StatusHoldingWrapperKeepsTheGateStatus(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(func(c *gingonic.Context) {
		w := &r3HoldingGinWriter{ResponseWriter: c.Writer}
		c.Writer = w
		c.Next()
		w.Header().Set("ETag", `"v1"`)
		w.Flush()
	})
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/paid", nil)
	req.Header.Set("Accept", "application/json")
	r.ServeHTTP(rec, req)
	t.Logf("gin, holding wrapper, unpaid: code=%d body=%q PAYMENT-REQUIRED=%t", rec.Code, rec.Body.String(), rec.Header().Get("PAYMENT-REQUIRED") != "")
	assert.Equal(t, 402, rec.Code, "the offer must stay a 402")
}

// Control: the same wrapper shape in front of the net/http core gate.
type r3HoldingWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (w *r3HoldingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *r3HoldingWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *r3HoldingWriter) Flush() {
	if w.status != 0 {
		w.ResponseWriter.WriteHeader(w.status)
		w.status = 0
	}
	if w.buf.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		w.buf.Reset()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *r3HoldingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestR3StatusHoldingWrapperControlCore(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /paid", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("paid content")) })
	gated := g.Middleware()(mux)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hw := &r3HoldingWriter{ResponseWriter: w}
		gated.ServeHTTP(hw, r)
		hw.Header().Set("ETag", `"v1"`)
		hw.Flush()
	})
	rec := testkit.Do(h, "GET", "/paid", "")
	t.Logf("core, holding wrapper, unpaid: code=%d", rec.Code)
	assert.Equal(t, 402, rec.Code)
}

// The answer that matters most: settle outcome unknown is a 503 "do NOT pay
// again; retry the same payment". Through the holding wrapper it arrives as a
// 200, which an x402 client takes as the paid answer and does not retry.
func TestR3StatusHoldingWrapperKeepsTheOutcomeUnknown503(t *testing.T) {
	f := testkit.StartFacilitator(t)
	f.OnSettle(func(_ int32, w http.ResponseWriter) { testkit.Reply(w, 503, map[string]string{"error": "scan_busy"}) })
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(func(c *gingonic.Context) {
		w := &r3HoldingGinWriter{ResponseWriter: c.Writer}
		c.Writer = w
		c.Next()
		w.Flush()
	})
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	plain := gingonic.New() // same gate, no wrapper: only to build the payment from a real 402
	plain.Use(cantongin.Middleware(g.Middleware()))
	plain.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	sig := testkit.SignatureFor(t, plain, testkit.PreparedTx)
	w := testkit.Do(r, "GET", "/paid", sig)
	t.Logf("gin, holding wrapper, settle outcome unknown: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Equal(t, 503, w.Code, "outcome unknown must stay a 503 (do NOT pay again, retry)")
}
