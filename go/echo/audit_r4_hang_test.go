package echo_test

// Round-4 audit probe (lens: echo): Check against a handler that streams or
// waits for the client, where the gate is missing (the case Check exists for)
// or where a catch-all route serves the paid path for another method. gin's
// Check bounds each request at 5 s; echo's sends with context.Background() and
// a recorder.

import (
	"net/http"
	"testing"
	"time"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR4CheckReturnsOnAHandlerThatWaitsForTheClient(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(e *echolib.Echo, gate echolib.MiddlewareFunc, wait echolib.HandlerFunc)
	}{
		{"paid route outside the gated group", func(e *echolib.Echo, gate echolib.MiddlewareFunc, wait echolib.HandlerFunc) {
			e.Group("/api", gate).GET("/status", func(c echolib.Context) error { return c.String(200, "ok") })
			e.GET("/paid", wait) // the gate is missing here: Check must say so
		}},
		{"catch-all event stream for other methods", func(e *echolib.Echo, gate echolib.MiddlewareFunc, wait echolib.HandlerFunc) {
			e.Use(gate)
			e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID") })
			e.POST("/*", wait) // a long-poll / SSE ingest on every other path
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := testkit.NewGate(t, testkit.StartFacilitator(t))
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			e := r4Echo()
			e.Use(middleware.Recover())
			tc.register(e, cantonecho.Middleware(g.Middleware()), func(c echolib.Context) error {
				c.Response().Header().Set("Content-Type", "text/event-stream")
				c.Response().WriteHeader(http.StatusOK)
				c.Response().Flush()
				select { // until the client leaves (or the test ends)
				case <-c.Request().Context().Done():
				case <-release:
				}
				return nil
			})

			done := make(chan error, 1)
			go func() { done <- cantonecho.Check(e, testkit.Routes()) }()
			select {
			case err := <-done:
				t.Logf("Check returned: %v", err)
			case <-time.After(10 * time.Second):
				t.Log("Check has not returned after 10 s")
				assert.Fail(t, "Check hangs: a server that calls it before serving never starts")
			}
		})
	}
}
