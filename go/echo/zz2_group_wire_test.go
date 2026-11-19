package echo_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

const z2PaidFile = "PAID REPORT FILE"

// A paid path whose echo routing is not a plain route of its own for the other
// methods: a group on the path (which adds a RouteNotFound route at its prefix),
// an explicit RouteNotFound on the path, and the same on a host router. Static
// inside the gate (no Skipper) serves the paid file for whatever reaches it.
func TestZ2UnpaidOtherMethodsNeverGetThePaidFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		host  string
		setup func(e *echolib.Echo, file echolib.HandlerFunc)
	}{
		{"group on the paid path", "", func(e *echolib.Echo, file echolib.HandlerFunc) {
			e.GET("/report", file)
			e.Group("/report", func(next echolib.HandlerFunc) echolib.HandlerFunc { return next }).
				GET("/:id", func(c echolib.Context) error { return c.String(200, "report "+c.Param("id")) })
		}},
		{"explicit RouteNotFound on the path", "", func(e *echolib.Echo, file echolib.HandlerFunc) {
			e.GET("/report", file)
			e.RouteNotFound("/report", file)
		}},
		{"host router with RouteNotFound on the path", "api.example", func(e *echolib.Echo, file echolib.HandlerFunc) {
			h := e.Host("api.example")
			h.GET("/report", file)
			h.RouteNotFound("/report", file)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "report"), []byte(z2PaidFile), 0o600))
			f := testkit.StartFacilitator(t)
			routes := map[string]canton.Route{"GET /report": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
			g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
			e := z2Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.Use(middleware.Static(dir))
			tc.setup(e, func(c echolib.Context) error { return c.File(filepath.Join(dir, "report")) })
			do := func(method string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, "/report", nil)
				if tc.host != "" {
					r.Host = tc.host
				}
				w := httptest.NewRecorder()
				e.ServeHTTP(w, r)
				return w
			}
			assert.Equal(t, http.StatusPaymentRequired, do(http.MethodGet).Code, "the key is gated")
			for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "PROPFIND"} {
				w := do(method)
				assert.NotContains(t, w.Body.String(), z2PaidFile, method)
				assert.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, w.Code, method)
			}
			assert.Zero(t, f.VerifyCalls.Load())
			assert.Zero(t, f.SettleCalls.Load())
		})
	}
}
