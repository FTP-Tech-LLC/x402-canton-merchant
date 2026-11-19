package gin_test

import (
	"net/http"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// cors answers a preflight itself, as gin-contrib/cors does (AbortWithStatus).
func cors(c *gingonic.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	if c.Request.Method == http.MethodOptions {
		c.Header("Access-Control-Allow-Headers", "PAYMENT-SIGNATURE")
		c.AbortWithStatus(http.StatusNoContent)
		return
	}
	c.Next()
}

// Which free requests the runtime guard answers 404, on a correct setup.
func TestR5FreeRoutesUnderTheGuard(t *testing.T) {
	routes := map[string]canton.Route{"GET /v1/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	build := func(corsBeforeGate bool) *gingonic.Engine {
		g := testkit.NewGate(t, testkit.StartFacilitator(t), func(c *canton.Config) { c.Routes = routes })
		r := gingonic.New()
		r.Use(gingonic.Recovery())
		if corsBeforeGate {
			r.Use(cors)
		}
		r.Use(cantongin.Middleware(g.Middleware()))
		if !corsBeforeGate {
			r.Use(cors)
		}
		r.GET("/v1/data", func(c *gingonic.Context) { c.String(200, "paid") })
		r.POST("/v1/:name", func(c *gingonic.Context) { c.String(202, "ingested %s", c.Param("name")) })
		r.GET("/health", func(c *gingonic.Context) { c.String(200, "ok") })
		r.GET("/users/:id", func(c *gingonic.Context) { c.String(200, "user") })
		r.Static("/assets", t.TempDir())
		return r
	}
	for _, before := range []bool{true, false} {
		r := build(before)
		for _, mt := range [][2]string{
			{"GET", "/health"}, {"GET", "/users/7"}, {"POST", "/v1/other"}, {"POST", "/v1/data"},
			{"OPTIONS", "/v1/data"}, {"OPTIONS", "/users/7"}, {"GET", "/v1/data"},
		} {
			w := testkit.Do(r, mt[0], mt[1], "")
			t.Logf("cors before gate=%v: %s %s: %d %q", before, mt[0], mt[1], w.Code, w.Body.String())
		}
	}
	r := build(true)
	assert.Equal(t, 404, testkit.Do(r, "POST", "/v1/data", "").Code, "documented: a param route for another method on a paid path")
	assert.Equal(t, 204, testkit.Do(r, "OPTIONS", "/v1/data", "").Code, "CORS outside the gate answers the preflight")
	assert.Equal(t, 404, testkit.Do(build(false), "OPTIONS", "/v1/data", "").Code, "documented: CORS inside the gate")
}
