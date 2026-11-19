package echo_test

// Round-4 audit probe (lens: echo): the adapter tells a paid request from a
// free one by the writer the gate hands to the handler (w == out). A net/http
// middleware composed into the gate function (here http.TimeoutHandler, the
// usual way to bound a net/http handler) hands every request its own writer.

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR4ComposedGateFunctionTreatsFreeRequestsAsPaid(t *testing.T) {
	f := testkit.StartFacilitator(t)
	siteGate := testkit.NewGate(t, f, r4Routes("GET /report"))
	apiGate := testkit.NewGate(t, f, r4Routes("GET /api/data"))

	var apiHits atomic.Int32
	e := r4Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(func(next http.Handler) http.Handler {
		return http.TimeoutHandler(siteGate.Middleware()(next), 10*time.Minute, "timeout")
	}))
	e.GET("/report", func(c echolib.Context) error { return c.String(200, "site report") })
	e.GET("/users/:id", func(c echolib.Context) error { return c.String(200, "user "+c.Param("id")) })
	api := e.Group("/api", cantonecho.Middleware(apiGate.Middleware()))
	api.GET("/data", func(c echolib.Context) error { apiHits.Add(1); return c.String(200, "PAID API DATA") })

	user := testkit.Do(e, http.MethodGet, "/users/7", "")
	data := testkit.Do(e, http.MethodGet, "/api/data", "")
	report := testkit.Do(e, http.MethodGet, "/report", "")
	t.Logf("free GET /users/7: %d %q", user.Code, user.Body.String())
	t.Logf("unpaid GET /api/data (the group gate's key): %d %q hits=%d settles=%d", data.Code, data.Body.String(), apiHits.Load(), f.SettleCalls.Load())
	t.Logf("unpaid GET /report (the site gate's key): %d", report.Code)
	assert.Equal(t, 200, user.Code, "a free param route still works")
	assert.Equal(t, 402, data.Code, "the group gate gates its own key")
}

func r4Routes(key string) func(*canton.Config) {
	return func(c *canton.Config) {
		c.Routes = map[string]canton.Route{key: {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	}
}
