package gin_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type ginConfig struct {
	name   string
	mutate func(*gingonic.Engine)
}

var ginConfigs = []ginConfig{
	{"default", func(*gingonic.Engine) {}},
	{"RemoveExtraSlash", func(e *gingonic.Engine) { e.RemoveExtraSlash = true }},
	{"RedirectFixedPath", func(e *gingonic.Engine) { e.RedirectFixedPath = true }},
	{"NoRedirectTrailingSlash", func(e *gingonic.Engine) { e.RedirectTrailingSlash = false }},
	{"UseRawPath", func(e *gingonic.Engine) { e.UseRawPath = true }},
	{"UseRawPath+Unescape", func(e *gingonic.Engine) { e.UseRawPath = true; e.UnescapePathValues = true }},
	{"RemoveExtraSlash+FixedPath", func(e *gingonic.Engine) { e.RemoveExtraSlash = true; e.RedirectFixedPath = true }},
	{"HandleMethodNotAllowed", func(e *gingonic.Engine) { e.HandleMethodNotAllowed = true }},
}

var spellings = []string{
	"/api/v1/data", "/api/v1/data/", "//api/v1/data", "/api//v1/data", "/api/v1//data", "/api/v1/data//",
	"/api/v1/%64ata", "/%61pi/v1/data", "/API/v1/data", "/api/V1/data", "/api/v1/Data",
	"/api/v1/./data", "/api/./v1/data", "/./api/v1/data", "/api/v1/x/../data", "/api/x/../v1/data",
	"/api/v1/data/.", "/api/v1/data/./", "/api/v1/data%2F", "/api/v1/data%20", "/api/v1/data;x",
	"/api/v1/data?x=1", "/api/v1/dat%61", "/api%2Fv1/data", "/api/v1%2Fdata", "/api/v1/data%00",
	"/api/v1/data.", "/api/v1/data%2e",
}

func rawRequest(method, target string) *http.Request {
	// httptest.NewRequest parses the target as a request line does (ParseRequestURI):
	// dot segments and a leading "//" stay in URL.Path, as on the wire.
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Accept", "application/json")
	return r
}

// TestProbeSpellingsNeverServePaidFree sends every spelling without a payment
// through a gin engine with the paid route in a group, under each gin router
// option, and fails when the paid handler runs (served free).
func TestProbeSpellingsNeverServePaidFree(t *testing.T) {
	for _, cfg := range ginConfigs {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(cfg.name+"/"+method, func(t *testing.T) {
				f := testkit.StartFacilitator(t)
				g := testkit.NewGate(t, f, func(c *canton.Config) {
					c.Routes = map[string]canton.Route{"GET /api/v1/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
				})
				var hits atomic.Int32
				r := gingonic.New()
				cfg.mutate(r)
				r.Use(gingonic.Recovery())
				r.Use(cantongin.Middleware(g.Middleware()))
				api := r.Group("/api").Group("/v1")
				api.GET("/data", func(c *gingonic.Context) { hits.Add(1); c.String(200, "PAID") })
				api.HEAD("/data", func(c *gingonic.Context) { hits.Add(1); c.Status(200) })
				api.GET("/open", func(c *gingonic.Context) { c.String(200, "open") })
				var outcomes []string
				for _, target := range spellings {
					before := hits.Load()
					w := httptest.NewRecorder()
					r.ServeHTTP(w, rawRequest(method, target))
					ran := hits.Load() > before
					outcomes = append(outcomes, fmt.Sprintf("%-22s %d%s", target, w.Code, map[bool]string{true: " HANDLER RAN", false: ""}[ran]))
					assert.False(t, ran, "%s %s served the paid handler without payment (code %d body %q)", method, target, w.Code, w.Body.String())
					assert.NotContains(t, w.Body.String(), "PAID", target)
				}
				t.Log("\n" + strings.Join(outcomes, "\n"))
			})
		}
	}
}

// TestProbeSpellingsNeverChargeForNothing pays every spelling that got a 402
// and fails when the payment is settled but the paid handler never ran.
func TestProbeSpellingsNeverChargeForNothing(t *testing.T) {
	var bad []string
	for _, cfg := range ginConfigs {
		t.Run(cfg.name, func(t *testing.T) {
			for _, target := range spellings {
				f := testkit.StartFacilitator(t)
				g := testkit.NewGate(t, f, func(c *canton.Config) {
					c.Routes = map[string]canton.Route{"GET /api/v1/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
				})
				var hits atomic.Int32
				r := gingonic.New()
				cfg.mutate(r)
				r.Use(gingonic.Recovery())
				r.Use(cantongin.Middleware(g.Middleware()))
				api := r.Group("/api").Group("/v1")
				api.GET("/data", func(c *gingonic.Context) { hits.Add(1); c.String(200, "PAID") })
				first := httptest.NewRecorder()
				r.ServeHTTP(first, rawRequest("GET", target))
				if first.Code != 402 {
					continue
				}
				sig := sigFor(t, r, "GET", target)
				w := testkit.Do(r, "GET", target, sig)
				if f.SettleCalls.Load() > 0 && hits.Load() == 0 {
					bad = append(bad, fmt.Sprintf("%s %s: code=%d body=%q", cfg.name, target, w.Code, w.Body.String()))
				}
			}
		})
	}
	t.Log("charged, paid handler never ran:\n" + strings.Join(bad, "\n"))
	assert.Empty(t, bad)
}
