package echo_test

// Round-2 probe of the echo "own exact route" rule, for every routing result
// echo can produce. A "leaky" handler or middleware stands for a proxy or file
// server that answers the paid resource for whatever path reaches it; every
// request is unpaid. The same file runs against the previous rule (route
// snapshot) and the new one (ContextKeyHeaderAllow), and the outputs are
// compared.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func z2Echo() *echolib.Echo {
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.HideBanner, e.HidePort = true, true
	return e
}

type z2Case struct {
	name    string
	host    string
	method  string
	path    string
	setup   func(e *echolib.Echo, leaky echolib.HandlerFunc, leakyMW echolib.MiddlewareFunc, dir string)
	wantOwn bool // the verdict for method+path: a route of the merchant's own
	// leaks: the unpaid requests (method path) allowed to reach the leaky handler
	// or carry paid content: only the merchant's own exact route.
	leaks []string
}

func z2Cases() []z2Case {
	paid := func(c echolib.Context) error { return c.String(200, "PAID CONTENT (charged route)") }
	noop := func(next echolib.HandlerFunc) echolib.HandlerFunc { return next }
	return []z2Case{
		{name: "exact own POST route", method: "POST", path: "/paid", wantOwn: true, leaks: []string{"POST /paid"},
			setup: func(e *echolib.Echo, leaky echolib.HandlerFunc, _ echolib.MiddlewareFunc, _ string) {
				e.GET("/paid", paid)
				e.POST("/paid", leaky)
			}},
		{name: "route added before the first request", method: "POST", path: "/paid", wantOwn: true, leaks: []string{"POST /paid"},
			setup: func(e *echolib.Echo, leaky echolib.HandlerFunc, _ echolib.MiddlewareFunc, _ string) {
				e.GET("/paid", paid)
				e.GET("/other", paid)
				e.POST("/paid", leaky)
			}},
		{name: "the key's own GET route", method: "GET", path: "/paid", wantOwn: true,
			setup: func(e *echolib.Echo, _ echolib.HandlerFunc, _ echolib.MiddlewareFunc, _ string) { e.GET("/paid", paid) }},
		{name: "param route", method: "POST", path: "/paid", wantOwn: true, // echo routes it to /:name: the guard handles wildcards itself
			setup: func(e *echolib.Echo, leaky echolib.HandlerFunc, _ echolib.MiddlewareFunc, _ string) {
				e.GET("/paid", paid)
				e.Any("/:name", leaky)
			}},
		{name: "any route", method: "POST", path: "/paid", wantOwn: true,
			setup: func(e *echolib.Echo, leaky echolib.HandlerFunc, _ echolib.MiddlewareFunc, _ string) {
				e.GET("/paid", paid)
				e.Any("/*", leaky)
			}},
		{name: "RouteNotFound /*", method: "POST", path: "/paid", wantOwn: false,
			setup: func(e *echolib.Echo, leaky echolib.HandlerFunc, _ echolib.MiddlewareFunc, _ string) {
				e.GET("/paid", paid)
				e.RouteNotFound("/*", leaky)
			}},
		{name: "RouteNotFound exact /paid", method: "POST", path: "/paid", wantOwn: false,
			setup: func(e *echolib.Echo, leaky echolib.HandlerFunc, _ echolib.MiddlewareFunc, _ string) {
				e.GET("/paid", paid)
				e.RouteNotFound("/paid", leaky)
			}},
		{name: "group prefix on the paid path", method: "POST", path: "/paid", wantOwn: false,
			setup: func(e *echolib.Echo, _ echolib.HandlerFunc, leakyMW echolib.MiddlewareFunc, _ string) {
				g := e.Group("/paid", leakyMW)
				g.GET("", paid)
				g.GET("/:id", func(c echolib.Context) error { return c.String(200, "sub "+c.Param("id")) })
			}},
		{name: "group (no-op middleware) with Static inside the gate", method: "POST", path: "/paid", wantOwn: false,
			setup: func(e *echolib.Echo, _ echolib.HandlerFunc, _ echolib.MiddlewareFunc, dir string) {
				e.Use(middleware.Static(dir))
				e.GET("/paid", func(c echolib.Context) error { return c.File(filepath.Join(dir, "paid")) })
				e.Group("/paid", noop).GET("/:id", func(c echolib.Context) error { return c.String(200, "sub "+c.Param("id")) })
			}},
		{name: "405 (only a GET route), Static inside the gate", method: "POST", path: "/paid", wantOwn: false,
			setup: func(e *echolib.Echo, _ echolib.HandlerFunc, _ echolib.MiddlewareFunc, dir string) {
				e.Use(middleware.Static(dir))
				e.GET("/paid", func(c echolib.Context) error { return c.File(filepath.Join(dir, "paid")) })
			}},
		{name: "OPTIONS (echo's own answer)", method: "OPTIONS", path: "/paid", wantOwn: false,
			setup: func(e *echolib.Echo, _ echolib.HandlerFunc, _ echolib.MiddlewareFunc, dir string) {
				e.Use(middleware.Static(dir))
				e.GET("/paid", func(c echolib.Context) error { return c.File(filepath.Join(dir, "paid")) })
			}},
		{name: "404 (no route at all)", method: "GET", path: "/missing", wantOwn: false,
			setup: func(e *echolib.Echo, _ echolib.HandlerFunc, _ echolib.MiddlewareFunc, _ string) { e.GET("/paid", paid) }},
		{name: "host router: RouteNotFound exact and Any /x/*", host: "api.example", method: "POST", path: "/paid", wantOwn: false,
			setup: func(e *echolib.Echo, leaky echolib.HandlerFunc, _ echolib.MiddlewareFunc, _ string) {
				h := e.Host("api.example")
				h.GET("/paid", paid)
				h.RouteNotFound("/paid", leaky)
				h.Any("/x/*", leaky)
			}},
	}
}

func TestZ2EchoOwnRouteRule(t *testing.T) {
	for _, tc := range z2Cases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("PAID FILE"), 0o600))
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			var leakyRuns atomic.Int32
			leaky := func(c echolib.Context) error {
				leakyRuns.Add(1)
				return c.String(200, "PAID CONTENT via "+c.Request().Method+" "+c.Request().URL.Path)
			}
			leakyMW := func(next echolib.HandlerFunc) echolib.HandlerFunc {
				return func(c echolib.Context) error {
					if strings.EqualFold(strings.Trim(c.Request().URL.Path, "/"), "paid") {
						leakyRuns.Add(1)
						return c.String(200, "PAID CONTENT via middleware "+c.Request().Method+" "+c.Request().URL.Path)
					}
					return next(c)
				}
			}
			e := z2Echo()
			e.Use(middleware.Recover())
			var verdict atomic.Value // what the rule says for the probed request
			e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc {
				return func(c echolib.Context) error {
					if c.Request().Method == tc.method && c.Request().URL.Path == tc.path && c.Request().Header.Get("X-Verdict") != "" {
						verdict.Store(cantonecho.HasOwnRoute(c))
					}
					return next(c)
				}
			})
			e.Use(cantonecho.Middleware(g.Middleware())) // after the verdict probe, so it always runs
			tc.setup(e, leaky, leakyMW, dir)

			probe := httptest.NewRequest(tc.method, tc.path, nil)
			probe.Header.Set("X-Verdict", "1")
			if tc.host != "" {
				probe.Host = tc.host
			}
			e.ServeHTTP(httptest.NewRecorder(), probe)
			if got, ok := verdict.Load().(bool); ok {
				assert.Equal(t, tc.wantOwn, got, "own-route verdict for %s %s", tc.method, tc.path)
			} else if tc.name != "404 (no route at all)" {
				t.Fatalf("the verdict middleware never ran for %s %s", tc.method, tc.path)
			}

			var leaks []string
			for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "PROPFIND"} {
				for _, target := range []string{"/paid", "/PAID", "/paid/", "//paid", "/%70aid", "/paid%2F", "/%2570aid"} {
					r := httptest.NewRequest(m, target, nil)
					if tc.host != "" {
						r.Host = tc.host
					}
					w := httptest.NewRecorder()
					before := leakyRuns.Load()
					e.ServeHTTP(w, r)
					ran := leakyRuns.Load() > before
					if (strings.Contains(w.Body.String(), "PAID") && w.Code != http.StatusPaymentRequired) || ran {
						leaks = append(leaks, m+" "+target)
					}
				}
			}
			sort.Strings(leaks)
			assert.Equal(t, tc.leaks, leaks, "unpaid requests that carried paid content or ran the leaky handler")
			assert.Zero(t, f.SettleCalls.Load())
		})
	}
}
