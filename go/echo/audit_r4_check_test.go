package echo_test

// Round-4 audit probes (lens: echo): Check after the round-3 fixes. The fixes
// exempt every OPTIONS route on a paid path and probe a non-key method only when
// the router sends it to a route registered for that method.

import (
	"io"
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

func r4Echo() *echolib.Echo {
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.HideBanner, e.HidePort = true, true
	return e
}

// The key's own handler registered for OPTIONS as well (e.Match with OPTIONS,
// or e.OPTIONS with the same handler). README: Check "fails when the key's
// handler is also registered for another method". The gate never charges
// OPTIONS, so the paid answer goes out free.
func TestR4CheckPassesTheKeysHandlerRegisteredForOptions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(e *echolib.Echo, h echolib.HandlerFunc)
	}{
		{"e.Match GET and OPTIONS", func(e *echolib.Echo, h echolib.HandlerFunc) {
			e.Match([]string{http.MethodGet, http.MethodOptions}, "/paid", h)
		}},
		{"e.GET and e.OPTIONS with the same handler", func(e *echolib.Echo, h echolib.HandlerFunc) {
			e.GET("/paid", h)
			e.OPTIONS("/paid", h)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			var hits atomic.Int32
			e := r4Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			tc.register(e, func(c echolib.Context) error { hits.Add(1); return c.String(200, "PAID CONTENT") })

			checkErr := cantonecho.Check(e, testkit.Routes())
			unpaid := testkit.Do(e, http.MethodOptions, "/paid", "")
			t.Logf("Check=%v; unpaid OPTIONS /paid: %d %q hits=%d verify=%d settles=%d",
				checkErr, unpaid.Code, unpaid.Body.String(), hits.Load(), f.VerifyCalls.Load(), f.SettleCalls.Load())
			assert.Error(t, checkErr, "the key's handler is registered for OPTIONS and serves the paid answer free")
		})
	}
}

// echo's usual static root next to a paid route that sells one of its files:
// a RouteNotFound file server, or middleware.Static inside the gate. Both serve
// the file for any method. README: Check fails "when a method no route serves
// (POST, PUT, DELETE, OPTIONS) is answered with content, as a NoRoute file
// server does". Either Check reports the setup, or POST /paid is not the file.
func TestR4CheckPassesAFileServerThatServesThePaidPathForAnotherMethod(t *testing.T) {
	for _, tc := range []struct {
		name      string
		postRoute bool // most APIs have a POST route somewhere
		register  func(e *echolib.Echo, dir string)
	}{
		{"RouteNotFound file server", false, func(e *echolib.Echo, dir string) {
			e.RouteNotFound("/*", echolib.WrapHandler(http.FileServer(http.Dir(dir))))
		}},
		{"RouteNotFound file server, a POST route elsewhere", true, func(e *echolib.Echo, dir string) {
			e.RouteNotFound("/*", echolib.WrapHandler(http.FileServer(http.Dir(dir))))
		}},
		{"middleware.Static inside the gate", false, func(e *echolib.Echo, dir string) {
			e.Use(middleware.Static(dir))
		}},
		{"middleware.Static inside the gate, a POST route elsewhere", true, func(e *echolib.Echo, dir string) {
			e.Use(middleware.Static(dir))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("paid content"), 0o600))
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			e := r4Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			tc.register(e, dir)
			e.GET("/paid", func(c echolib.Context) error { return c.File(filepath.Join(dir, "paid")) })
			if tc.postRoute {
				e.POST("/feedback", func(c echolib.Context) error { return c.NoContent(http.StatusNoContent) })
			}

			require.Equal(t, 402, testkit.Do(e, http.MethodGet, "/paid", "").Code, "GET /paid is gated")
			checkErr := cantonecho.Check(e, testkit.Routes())
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions, "FOO"} {
				w := testkit.Do(e, method, "/paid", "")
				t.Logf("unpaid %s /paid: %d %q", method, w.Code, w.Body.String())
			}
			post := testkit.Do(e, http.MethodPost, "/paid", "")
			t.Logf("Check=%v; settles=%d", checkErr, f.SettleCalls.Load())
			assert.True(t, checkErr != nil || post.Body.String() != "paid content",
				"POST /paid gets the paid file free and Check passes")
		})
	}
}

// README (gin and echo): "Check does not run a free handler of its own for
// another method (an ingest, a CORS preflight): it must not return paid
// content." A free ingest on the paid path, its own handler, is such a setup.
func TestR4CheckRefusesAFreeHandlerOfItsOwnOnThePaidPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(e *echolib.Echo, ingest echolib.HandlerFunc)
		target   string
	}{
		{"e.POST on the paid path", func(e *echolib.Echo, ingest echolib.HandlerFunc) {
			e.POST("/paid", ingest)
		}, "/paid"},
		{"e.POST on a param route", func(e *echolib.Echo, ingest echolib.HandlerFunc) {
			e.POST("/:collection", ingest)
		}, "/paid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			var ingested atomic.Int32
			e := r4Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID CONTENT") })
			tc.register(e, func(c echolib.Context) error {
				ingested.Add(1) // stores a record: a side effect at every Check
				return c.JSON(http.StatusCreated, map[string]string{"id": "rec-1"})
			})

			checkErr := cantonecho.Check(e, testkit.Routes())
			t.Logf("Check=%v; ingest calls made by Check: %d", checkErr, ingested.Load())
			assert.NoError(t, checkErr, "a free handler of its own for another method passes Check, per README")
			assert.Zero(t, ingested.Load(), "Check must not run a free handler of its own")
		})
	}
}
