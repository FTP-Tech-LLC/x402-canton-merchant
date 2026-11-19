package gin_test

// Round-2 audit probes (lens: gin): the c.Errors rule and cantongin.Check.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// README: "An error recorded with c.Error ... [is] answered 500 and not
// charged." A handler that streams rows and records the database error it hit
// half way (the usual gin pattern: record, let the error middleware render)
// without calling Abort is settled for the partial body.
func TestR2ErrorAfterPartialWriteWithoutAbortIsNotSettled(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) {
		c.Status(200)
		_, _ = c.Writer.WriteString(`{"rows":[1,2,`)
		_ = c.Error(errors.New("db: connection reset")) // rows.Err()
	})
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("partial write + c.Error, no Abort: code=%d body=%q settles=%d receipt=%v",
		w.Code, w.Body.String(), f.SettleCalls.Load(), w.Header().Get("PAYMENT-RESPONSE") != "")
	assert.Zero(t, f.SettleCalls.Load(), "an answer the handler marked failed must not be settled")
}

// Errors recorded by middleware before the gate do not refuse a paid answer
// (control: errorsBefore is taken inside the gate).
func TestR2ErrorsBeforeTheGateDoNotRefuse(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(func(c *gingonic.Context) { _ = c.Error(errors.New("outer: non-fatal")); c.Next() })
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.Abort(); c.String(200, "paid content") })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("outer error + aborted 2xx: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, int32(1), f.SettleCalls.Load())
}

// Check with a paid route that also answers HEAD (r.Match GET+HEAD, as
// r.StaticFile does). The gate gates HEAD through the GET key, so HEAD is not
// served free, yet Check reports it.
func TestR2CheckFlagsAGatedHead(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(cantongin.Middleware(g.Middleware()))
	r.Match([]string{"GET", "HEAD"}, "/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	head := testkit.Do(r, "HEAD", "/paid", "")
	t.Logf("HEAD /paid unpaid: code=%d", head.Code)
	err := cantongin.Check(r, testkit.Routes())
	t.Logf("Check: %v", err)
	assert.Equal(t, 402, head.Code, "HEAD is gated")
	assert.NoError(t, err, "HEAD on a paid GET route is gated, not served free")
}

// Check misses another method on a paid path served through a param route.
func TestR2CheckMissesAParamRouteForAnotherMethod(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(cantongin.Middleware(g.Middleware()))
	var hits atomic.Int32
	paid := func(c *gingonic.Context) { hits.Add(1); c.String(200, "paid content") }
	r.GET("/paid", paid)
	r.POST("/:name", paid) // e.g. a generic RPC-style dispatcher on the same handlers
	w := testkit.Do(r, "POST", "/paid", "")
	err := cantongin.Check(r, testkit.Routes())
	t.Logf("POST /paid unpaid: code=%d body=%q; Check: %v", w.Code, w.Body.String(), err)
	assert.Error(t, err, "POST /paid serves the paid handler free and Check passes")
}

// Check panics instead of returning an error when the route it reports runs a
// handler that needs CloseNotify (c.Stream, httputil.ReverseProxy): the
// in-process recorder has none and gin's CloseNotify asserts it.
func TestR2CheckPanicsOnAStreamingHandler(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.GET("/paid", func(c *gingonic.Context) { // registered before Use: the bug Check exists for
		c.Stream(func(w io.Writer) bool { _, _ = w.Write([]byte("tick")); return false })
	})
	r.Use(cantongin.Middleware(g.Middleware()))
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		return cantongin.Check(r, testkit.Routes())
	}()
	t.Logf("Check: %v", err)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "panic", "Check must report, not crash")
	_ = http.StatusOK
}
