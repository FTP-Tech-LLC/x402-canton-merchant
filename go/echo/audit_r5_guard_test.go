package echo_test

// Round-5 audit probes (lens: echo): the runtime guard (echo/echo.go:237-250,
// internal/paidpath).

import (
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// The guard leaves an exact route of the merchant's own for another method
// alone (reachesPaidPath returns false before any probe). Middleware inside the
// gate still runs for that request, before the route's handler: middleware.Static
// serves the paid file for any method and never reaches the handler. README:
// the runtime guard covers "echo's own 404/405 (middleware.Static)", and an
// exact route of your own "is not touched and must not return paid content":
// here the handler returns nothing, Static does.
func TestR5StaticInsideTheGateServesThePaidFileThroughAnExemptExactRoute(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("PAID FILE"), 0o600))
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			var own atomic.Int32
			e := r4Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			// The site's static root, inside the gate, told to leave paid paths alone: the
			// adapter cannot stop an inner middleware on an exact route of the merchant's own.
			e.Use(middleware.StaticWithConfig(middleware.StaticConfig{Root: dir, Skipper: cantonecho.SkipPaidPaths(g.Middleware())}))
			e.GET("/paid", func(c echolib.Context) error { return c.File(filepath.Join(dir, "paid")) })
			// A free handler of its own on the paid path (an ingest, an upload, a CORS answer).
			e.Add(method, "/paid", func(c echolib.Context) error { own.Add(1); return c.NoContent(http.StatusAccepted) })

			checkErr := cantonecho.Check(e, testkit.Routes())
			unpaidGet := testkit.Do(e, http.MethodGet, "/paid", "")
			w := testkit.Do(e, method, "/paid", "")
			t.Logf("Check=%v; unpaid GET /paid: %d; unpaid %s /paid: %d %q own handler calls=%d verify=%d settles=%d",
				checkErr, unpaidGet.Code, method, w.Code, w.Body.String(), own.Load(), f.VerifyCalls.Load(), f.SettleCalls.Load())
			require.Equal(t, http.StatusPaymentRequired, unpaidGet.Code)
			assert.NotContains(t, w.Body.String(), "PAID FILE", "the paid file must not be served free for another method")
			assert.Equal(t, int32(1), own.Load(), "the merchant's own handler answers its method")
		})
	}
}

// Control: without the exact route for the other method, the guard answers 404.
func TestR5StaticInsideTheGateControlWithoutAnExactRoute(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("PAID FILE"), 0o600))
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r4Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.Use(middleware.Static(dir))
	e.GET("/paid", func(c echolib.Context) error { return c.File(filepath.Join(dir, "paid")) })
	w := testkit.Do(e, http.MethodPost, "/paid", "")
	t.Logf("unpaid POST /paid (no POST route): %d %q", w.Code, w.Body.String())
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// Check collects "key names" from every route for the key's method that matches
// the paid path (echo/check.go:111-116), so a catch-all registered for all
// methods (e.Any("/*", proxy), an SPA or reverse-proxy fallback) lends its own
// name to the key: its POST, PUT... routes are then reported as "the paid
// handler served free", although the runtime guard answers them 404. The server
// does not start (examples/echo returns Check's error).
func TestR5CheckRefusesACatchAllFallbackForAllMethods(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(e *echolib.Echo, h echolib.HandlerFunc)
	}{
		{"e.Any(\"/*\", proxy)", func(e *echolib.Echo, h echolib.HandlerFunc) { e.Any("/*", h) }},
		{"e.Match(GET, POST on /*)", func(e *echolib.Echo, h echolib.HandlerFunc) {
			e.Match([]string{http.MethodGet, http.MethodPost}, "/*", h)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			var proxied atomic.Int32
			e := r4Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID CONTENT") })
			tc.register(e, func(c echolib.Context) error { proxied.Add(1); return c.String(200, "proxied "+c.Request().URL.Path) })

			checkErr := cantonecho.Check(e, testkit.Routes())
			unpaid := testkit.Do(e, http.MethodGet, "/paid", "")
			post := testkit.Do(e, http.MethodPost, "/paid", "")
			other := testkit.Do(e, http.MethodPost, "/other", "")
			t.Logf("Check=%v", checkErr)
			t.Logf("unpaid GET /paid: %d; POST /paid: %d %q; POST /other: %d %q; proxy calls=%d",
				unpaid.Code, post.Code, post.Body.String(), other.Code, other.Body.String(), proxied.Load())
			require.Equal(t, http.StatusPaymentRequired, unpaid.Code)
			require.NotContains(t, post.Body.String(), "PAID CONTENT")
			assert.NoError(t, checkErr, "a fallback with its own handler is not the paid handler")
		})
	}
}

// The route set the guard uses to exempt exact routes is built once, on the
// first request through the adapter (Check's own request included). An exact
// route added after it is treated as unrouted.
func TestR5RouteAddedAfterTheFirstRequestIsNotExempt(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	var ingested atomic.Int32
	e := r4Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID CONTENT") })
	e.GET("/health", func(c echolib.Context) error { return c.String(200, "ok") })
	require.Equal(t, http.StatusOK, testkit.Do(e, http.MethodGet, "/health", "").Code) // a load-balancer probe before the last route
	e.POST("/paid", func(c echolib.Context) error { ingested.Add(1); return c.NoContent(http.StatusAccepted) })
	w := testkit.Do(e, http.MethodPost, "/paid", "")
	t.Logf("free POST /paid (exact route added after the first request): %d %q ingest calls=%d", w.Code, w.Body.String(), ingested.Load())
	assert.Equal(t, http.StatusAccepted, w.Code)
}

// echo names every echolib.WrapHandler route "github.com/labstack/echo/v4.WrapHandler.func1":
// a net/http paid handler and a net/http ingest on the same path look alike.
func TestR5CheckRefusesTwoWrappedNetHTTPHandlers(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r4Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", echolib.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("PAID")) })))
	e.POST("/paid", echolib.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })))
	checkErr := cantonecho.Check(e, testkit.Routes())
	post := testkit.Do(e, http.MethodPost, "/paid", "")
	t.Logf("Check=%v; free POST /paid: %d %q; names: %s / %s", checkErr, post.Code, post.Body.String(), e.Routes()[0].Name, e.Routes()[1].Name)
	assert.NoError(t, checkErr, "the ingest is a handler of its own")
}

// The converse: one net/http paid handler wrapped by two echolib.WrapHandler
// calls (GET, the key, and POST). README (gin and echo, Check): "echo only has
// names, so two closures from one call site look alike there"; only gin is said
// to miss "one net/http handler wrapped by two gin.WrapH calls". In a default
// build the compiler inlines WrapHandler at each call site and the closures get
