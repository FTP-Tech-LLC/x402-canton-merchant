package gin_test

// Round-3 audit probes (lens: core flush + gin): cantongin.Check's new method
// probing and its CloseNotify recorder.

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// A REST resource whose GET is paid and whose POST (submit a dataset) is free
// by design, with its own handler. Check runs the POST handler at startup with
// an empty body and fails the setup.
func TestR3CheckDoesNotRunOrFailAFreeHandlerForAnotherMethod(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	var submitted atomic.Int32
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	r.POST("/paid", func(c *gingonic.Context) { submitted.Add(1); c.Status(http.StatusAccepted) }) // free ingest, different handler
	err := cantongin.Check(r, testkit.Routes())
	t.Logf("Check: %v; POST handler runs at startup: %d", err, submitted.Load())
	assert.Zero(t, submitted.Load(), "Check ran a real handler with side effects at startup")
	assert.NoError(t, err, "a free POST with its own handler serves no paid content")
}

// CORS preflight answered by a route for every path (a common gin pattern).
func TestR3CheckDoesNotFailACORSPreflightRoute(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	r.OPTIONS("/*any", func(c *gingonic.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST")
		c.Status(http.StatusNoContent)
	})
	err := cantongin.Check(r, testkit.Routes())
	t.Logf("Check: %v", err)
	assert.NoError(t, err, "a 204 preflight serves no paid content")
}

// The trap Check exists for (a route registered before r.Use), on a route that
// streams until the client goes away (c.Stream with a keep-open step, as an
// SSE feed does). Check's recorder has a CloseNotify that never fires and a
// context that never ends, so Check never returns: the process hangs at
// startup instead of reporting the missing gate.
func TestR3CheckReturnsForAStreamingHandlerWithoutGate(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	var stop atomic.Bool
	t.Cleanup(func() { stop.Store(true) })
	r.GET("/paid", func(c *gingonic.Context) { // registered before Use: no gate
		c.Stream(func(w io.Writer) bool {
			_, _ = w.Write([]byte("data: tick\n\n"))
			time.Sleep(20 * time.Millisecond)
			return !stop.Load()
		})
	})
	r.Use(cantongin.Middleware(g.Middleware()))
	done := make(chan error, 1)
	go func() { done <- cantongin.Check(r, testkit.Routes()) }()
	select {
	case err := <-done:
		t.Logf("Check returned: %v", err)
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Errorf("Check did not return within 10 s: startup hangs on a streaming handler")
	}
}
