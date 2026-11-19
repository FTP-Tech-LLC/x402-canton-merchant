package echo_test

// Round 3 probes: the request-context marker that makes a second gate pass a
// request through. The marker is set for every request the first gate lets
// through (paid or not) and is not tied to that gate.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func r3Echo() *echolib.Echo {
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.HideBanner, e.HidePort = true, true
	return e
}

func routesFor(key string) func(*canton.Config) {
	return func(c *canton.Config) {
		c.Routes = map[string]canton.Route{key: {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	}
}

// (a) Two distinct gates: a site-wide one and one on a group. The group's paid
// route must answer 402 to an unpaid request.
func TestR3SecondDistinctGateOnAGroupIsBypassed(t *testing.T) {
	f := testkit.StartFacilitator(t)
	siteGate := testkit.NewGate(t, f, routesFor("GET /report"))
	apiGate := testkit.NewGate(t, f, routesFor("GET /api/data"))

	var hits atomic.Int32
	e := r3Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(siteGate.Middleware()))
	e.GET("/report", func(c echolib.Context) error { return c.String(200, "site report") })
	api := e.Group("/api")
	api.Use(cantonecho.Middleware(apiGate.Middleware()))
	api.GET("/data", func(c echolib.Context) error { hits.Add(1); return c.String(200, "PAID API DATA") })

	checkErr := cantonecho.Check(e, map[string]canton.Route{"GET /api/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}})
	w := testkit.Do(e, "GET", "/api/data", "")
	t.Logf("Check(api routes)=%v", checkErr)
	t.Logf("unpaid GET /api/data: %d %q hits=%d verify=%d settles=%d", w.Code, w.Body.String(), hits.Load(), f.VerifyCalls.Load(), f.SettleCalls.Load())
	assert.Equal(t, 402, w.Code, "the group's gate must gate its own paid route")
	assert.NotContains(t, w.Body.String(), "PAID API DATA")
}

// (b) Two echo apps, each with its own gate: the main app mounts the second
// under a path (echo.WrapHandler). The second app's paid route must answer 402.
func TestR3MountedEchoAppWithItsOwnGateIsBypassed(t *testing.T) {
	f := testkit.StartFacilitator(t)
	mainGate := testkit.NewGate(t, f, routesFor("GET /report"))
	subGate := testkit.NewGate(t, f, routesFor("GET /v2/data"))

	var hits atomic.Int32
	sub := r3Echo()
	sub.Use(middleware.Recover())
	sub.Use(cantonecho.Middleware(subGate.Middleware()))
	sub.GET("/v2/data", func(c echolib.Context) error { hits.Add(1); return c.String(200, "PAID V2 DATA") })

	e := r3Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(mainGate.Middleware()))
	e.GET("/report", func(c echolib.Context) error { return c.String(200, "site report") })
	e.Any("/v2/*", echolib.WrapHandler(sub))

	// Control: the sub app alone gates its route, and Check passes on both apps.
	alone := testkit.Do(sub, "GET", "/v2/data", "")
	assert.Equal(t, 402, alone.Code)
	t.Logf("Check(sub, sub routes)=%v; Check(main, main routes)=%v",
		cantonecho.Check(sub, map[string]canton.Route{"GET /v2/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}),
		cantonecho.Check(e, map[string]canton.Route{"GET /report": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}))

	w := testkit.Do(e, "GET", "/v2/data", "")
	t.Logf("sub alone: %d; through the main app: %d %q hits=%d settles=%d", alone.Code, w.Code, w.Body.String(), hits.Load(), f.SettleCalls.Load())
	assert.Equal(t, 402, w.Code, "the mounted app's gate must gate its paid route")
	assert.NotContains(t, w.Body.String(), "PAID V2 DATA")
}

// (c) One gate. A free alias route forwards internally to the paid route with
// the request's context (c.Echo().ServeHTTP with a clone of the request). The
// internal request carries no payment: it must get the 402, not the content.
func TestR3InternalForwardToAPaidRouteIsServedFree(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	var hits atomic.Int32
	e := r3Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.String(200, "PAID CONTENT") })
	e.GET("/latest", func(c echolib.Context) error {
		forward := c.Request().Clone(c.Request().Context())
		forward.URL.Path = "/paid"
		forward.RequestURI = "/paid"
		c.Echo().ServeHTTP(c.Response(), forward)
		return nil
	})

	// Control: an outside client that forwards the same way with a fresh context
	// gets the offer.
	direct := httptest.NewRecorder()
	e.ServeHTTP(direct, httptest.NewRequest(http.MethodGet, "/paid", nil))
	assert.Equal(t, 402, direct.Code)

	t.Logf("Check(e, routes)=%v", cantonecho.Check(e, testkit.Routes()))
	w := testkit.Do(e, "GET", "/latest", "")
	t.Logf("direct unpaid /paid: %d; /latest forwarding to /paid: %d %q hits=%d verify=%d settles=%d",
		direct.Code, w.Code, w.Body.String(), hits.Load(), f.VerifyCalls.Load(), f.SettleCalls.Load())
	assert.NotContains(t, w.Body.String(), "PAID CONTENT", "a forwarded request without payment must not get paid content")
	assert.Zero(t, hits.Load())
}
