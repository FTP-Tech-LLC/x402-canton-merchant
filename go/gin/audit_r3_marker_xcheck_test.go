package gin_test

// Cross-check from the echo lens: gin's adapter marks every request the first
// gate lets through, the same way as echo's.

import (
	"sync/atomic"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR3XGinSecondDistinctGateOnAGroupIsBypassed(t *testing.T) {
	f := testkit.StartFacilitator(t)
	siteGate := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /report": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	})
	apiGate := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /api/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	})
	var hits atomic.Int32
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(siteGate.Middleware()))
	r.GET("/report", func(c *gingonic.Context) { c.String(200, "site report") })
	api := r.Group("/api")
	api.Use(cantongin.Middleware(apiGate.Middleware()))
	api.GET("/data", func(c *gingonic.Context) { hits.Add(1); c.String(200, "PAID API DATA") })

	w := testkit.Do(r, "GET", "/api/data", "")
	t.Logf("gin: unpaid GET /api/data: %d %q hits=%d", w.Code, w.Body.String(), hits.Load())
	assert.Equal(t, 402, w.Code)
}
