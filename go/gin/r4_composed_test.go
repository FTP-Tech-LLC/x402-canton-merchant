package gin_test

// Round-4 audit probe (lens: gin): the adapter tells a free request from a
// gated one by writer identity (w == outer). Middleware takes any
// func(http.Handler) http.Handler, so a merchant may pass the gate composed
// with a net/http middleware that wraps the writer (a status recorder, a
// tracing wrapper). Every free request then looks gated: it is marked, and a
// second, different gate later in the chain passes it through (the round-3
// high comes back).

import (
	"net/http"
	"sync/atomic"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type r4StatusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *r4StatusRecorder) WriteHeader(code int)        { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *r4StatusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func r4WithStatusMetrics(next http.Handler) http.Handler { // a net/http metrics middleware
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&r4StatusRecorder{ResponseWriter: w}, r)
	})
}

func TestR4ComposedGateDoesNotMarkFreeRequests(t *testing.T) {
	f := testkit.StartFacilitator(t)
	basic := testkit.NewGate(t, f) // GET /paid
	premium := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /premium/report": {PayTo: testkit.PayTo, Price: canton.CC("5")}}
	})
	var hits atomic.Int32
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(func(h http.Handler) http.Handler { return basic.Middleware()(r4WithStatusMetrics(h)) }))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "basic content") })
	r.Group("/premium", cantongin.Middleware(premium.Middleware())).
		GET("/report", func(c *gingonic.Context) { hits.Add(1); c.String(200, "premium content") })

	w := testkit.Do(r, "GET", "/premium/report", "")
	t.Logf("unpaid GET /premium/report: code=%d body=%q hits=%d settles=%d", w.Code, w.Body.String(), hits.Load(), f.SettleCalls.Load())
	assert.Equal(t, 402, w.Code)
	assert.Zero(t, hits.Load(), "the premium handler ran without payment")
}

// The same with the net/http middleware outside the gate (metrics around it).
func TestR4ComposedGateOutsideWrapperDoesNotMarkFreeRequests(t *testing.T) {
	f := testkit.StartFacilitator(t)
	basic := testkit.NewGate(t, f)
	premium := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /premium/report": {PayTo: testkit.PayTo, Price: canton.CC("5")}}
	})
	var hits atomic.Int32
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(func(h http.Handler) http.Handler { return r4WithStatusMetrics(basic.Middleware()(h)) }))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "basic content") })
	r.Group("/premium", cantongin.Middleware(premium.Middleware())).
		GET("/report", func(c *gingonic.Context) { hits.Add(1); c.String(200, "premium content") })
	w := testkit.Do(r, "GET", "/premium/report", "")
	t.Logf("metrics outside the gate, unpaid GET /premium/report: code=%d body=%q hits=%d", w.Code, w.Body.String(), hits.Load())
	assert.Equal(t, 402, w.Code)
	assert.Zero(t, hits.Load())
	paid := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("metrics outside the gate, paid GET /paid: code=%d body=%q settles=%d", paid.Code, paid.Body.String(), f.SettleCalls.Load())
}
