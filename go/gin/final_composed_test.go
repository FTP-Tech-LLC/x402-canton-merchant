package gin_test

// Final probe: two canton gates composed into the one function handed to the
// adapter (README: "compose nothing into it"). The probe fast path answers from
// the outer gate only; the per-method slow path ran both.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestFinalComposedGatesInnerKeyReachesNoRoute(t *testing.T) {
	f := testkit.StartFacilitator(t)
	outer := testkit.NewGate(t, f) // GET /paid
	inner := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /report": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	})
	composed := func(h http.Handler) http.Handler { return outer.Middleware()(inner.Middleware()(h)) }
	r := gingonic.New()
	r.Use(cantongin.Middleware(composed))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid A") })
	r.GET("/report", func(c *gingonic.Context) { c.String(200, "paid B") })
	served := 0
	r.NoRoute(func(c *gingonic.Context) { served++; c.String(200, "proxied %s", c.Request.URL.Path) }) // a proxy / file server
	for _, target := range []string{"/REPORT", "/report/.", "/Report"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		t.Logf("GET %s -> %d %q", target, w.Code, w.Body.String())
	}
	// The same spellings of the OUTER gate's key are refused.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/PAID", nil))
	t.Logf("GET /PAID -> %d %q", w.Code, w.Body.String())
	assert.Zero(t, served, "a loose spelling of a paid key must not reach NoRoute, whichever composed gate owns the key")
}
