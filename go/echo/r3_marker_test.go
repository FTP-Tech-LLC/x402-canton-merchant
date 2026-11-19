package echo_test

// Round-3 audit probe (lens: core flush + gin, cross-check): the echo adapter
// sets the same "already gated" marker for a request its gate passed through,
// so a second, different gate on a group never gates.

import (
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR3EchoSecondDifferentGateIsNotBypassed(t *testing.T) {
	f := testkit.StartFacilitator(t)
	basic := testkit.NewGate(t, f)
	premium := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /premium/report": {PayTo: testkit.PayTo, Price: canton.CC("5")}}
	})
	e := echolib.New()
	e.Use(cantonecho.Middleware(basic.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "basic content") })
	var hits atomic.Int32
	g := e.Group("/premium", cantonecho.Middleware(premium.Middleware()))
	g.GET("/report", func(c echolib.Context) error { hits.Add(1); return c.String(200, "premium content") })

	w := testkit.Do(e, "GET", "/premium/report", "")
	t.Logf("echo unpaid GET /premium/report: code=%d body=%q hits=%d", w.Code, w.Body.String(), hits.Load())
	assert.Equal(t, 402, w.Code)
	assert.Zero(t, hits.Load())
}
