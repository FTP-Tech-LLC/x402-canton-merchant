package gin_test

// Round-3 audit probes (lens: core flush + gin): the double-gate marker. The
// adapter marks c.Request as "already gated" whenever the outer gate calls its
// next handler, also when that gate passed the request through because the
// route is not one of its keys. Any later cantongin.Middleware, whatever gate
// it wraps, then runs c.Next() without gating.

import (
	"net/http"
	"sync/atomic"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func r3PremiumRoutes() map[string]canton.Route {
	return map[string]canton.Route{"GET /premium/report": {PayTo: testkit.PayTo, Price: canton.CC("5")}}
}

// Two different gates: a global one for the cheap route and a group gate for
// premium routes (a different price list, payTo, store or timeout). The
// premium route must answer 402 to an unpaid request.
func TestR3SecondDifferentGateIsNotBypassed(t *testing.T) {
	f := testkit.StartFacilitator(t)
	basic := testkit.NewGate(t, f)                                                            // GET /paid
	premium := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = r3PremiumRoutes() }) // GET /premium/report
	var hits atomic.Int32
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(basic.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "basic content") })
	group := r.Group("/premium", cantongin.Middleware(premium.Middleware()))
	group.GET("/report", func(c *gingonic.Context) { hits.Add(1); c.String(200, "premium content") })

	w := testkit.Do(r, "GET", "/premium/report", "")
	t.Logf("unpaid GET /premium/report: code=%d body=%q handlerHits=%d settles=%d", w.Code, w.Body.String(), hits.Load(), f.SettleCalls.Load())
	assert.Equal(t, 402, w.Code, "the premium gate must ask for payment")
	assert.NotEqual(t, "premium content", w.Body.String(), "premium content served free")
	assert.Zero(t, hits.Load(), "the premium handler ran without payment")

	// Observation only: Check reports it when it is called with the premium
	// gate's routes too, with advice that does not apply (the gate is on the group).
	t.Logf("Check(premium routes): %v", cantongin.Check(r, r3PremiumRoutes()))
}

// Control: the premium group gate alone answers 402.
func TestR3SecondDifferentGateControlAlone(t *testing.T) {
	f := testkit.StartFacilitator(t)
	premium := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = r3PremiumRoutes() })
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	group := r.Group("/premium", cantongin.Middleware(premium.Middleware()))
	group.GET("/report", func(c *gingonic.Context) { c.String(200, "premium content") })
	w := testkit.Do(r, "GET", "/premium/report", "")
	t.Logf("premium gate alone: code=%d", w.Code)
	assert.Equal(t, 402, w.Code)
}

// gin's documented internal redirect, r.HandleContext, from a free route to a
// paid one: c.Request keeps the marker the gate's pass-through set, so the
// adapter in the re-run chain does not gate the paid route.
func TestR3HandleContextFromAFreeRouteDoesNotServeAPaidRouteFree(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	var hits atomic.Int32
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { hits.Add(1); c.String(200, "paid content") })
	r.GET("/latest", func(c *gingonic.Context) { // a free alias
		c.Request.URL.Path = "/paid"
		r.HandleContext(c)
	})
	require.Equal(t, 402, testkit.Do(r, "GET", "/paid", "").Code)

	w := testkit.Do(r, "GET", "/latest", "")
	t.Logf("unpaid GET /latest -> HandleContext(/paid): code=%d body=%q hits=%d", w.Code, w.Body.String(), hits.Load())
	assert.NotEqual(t, "paid content", w.Body.String(), "paid content served free through an internal redirect")
	assert.Zero(t, hits.Load())
}

// The same gate registered twice still works once (the case the marker is for).
func TestR3SameGateTwiceControl(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", cantongin.Middleware(g.Middleware()), func(c *gingonic.Context) { c.String(200, "paid content") })
	assert.Equal(t, 402, testkit.Do(r, "GET", "/paid", "").Code)
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, int32(1), f.SettleCalls.Load())
	_ = http.StatusOK
}

// A gin sub-application with its own gate, mounted into a root engine that has
// a gate of its own (modular apps, a vendored module). The root gate passes the
// sub-app's path through and marks the request; the sub-app's adapter then
// skips its gate.
func TestR3MountedSubEngineKeepsItsGate(t *testing.T) {
	f := testkit.StartFacilitator(t)
	rootGate := testkit.NewGate(t, f) // GET /paid
	subGate := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /api/report": {PayTo: testkit.PayTo, Price: canton.CC("1")}}
	})
	sub := gingonic.New()
	sub.Use(cantongin.Middleware(subGate.Middleware()))
	var hits atomic.Int32
	sub.GET("/api/report", func(c *gingonic.Context) { hits.Add(1); c.String(200, "sub-app paid content") })
	require.Equal(t, 402, testkit.Do(sub, "GET", "/api/report", "").Code, "the sub-app alone is gated")

	root := gingonic.New()
	root.Use(gingonic.Recovery())
	root.Use(cantongin.Middleware(rootGate.Middleware()))
	root.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	root.Any("/api/*path", gingonic.WrapH(sub))

	w := testkit.Do(root, "GET", "/api/report", "")
	t.Logf("unpaid GET /api/report through the root engine: code=%d body=%q hits=%d", w.Code, w.Body.String(), hits.Load())
	assert.Equal(t, 402, w.Code)
	assert.Zero(t, hits.Load(), "the sub-app's paid handler ran without payment")
}
