package gin_test

// Round-4 audit cross-check (lens: echo): the README's shared Check paragraph
// against gin, for the setups the echo probes use. These are expected to pass:
// they show what the README describes.

import (
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func r4GinEngine(t *testing.T) *gingonic.Engine {
	t.Helper()
	g := testkit.NewGate(t, testkit.StartFacilitator(t))
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	return r
}

func TestR4XGinFreeHandlerOfItsOwnPassesCheck(t *testing.T) {
	for _, param := range []bool{false, true} {
		r := r4GinEngine(t)
		var ingested atomic.Int32
		r.GET("/paid", func(c *gingonic.Context) { c.String(200, "PAID CONTENT") })
		ingest := func(c *gingonic.Context) { ingested.Add(1); c.JSON(http.StatusCreated, gingonic.H{"id": "rec-1"}) }
		if param {
			r.POST("/:collection", ingest)
		} else {
			r.POST("/paid", ingest)
		}
		err := cantongin.Check(r, testkit.Routes())
		t.Logf("gin, param=%v: Check=%v ingest calls=%d", param, err, ingested.Load())
		assert.NoError(t, err)
		assert.Zero(t, ingested.Load())
	}
}

func TestR4XGinKeysHandlerOnOptionsFailsCheck(t *testing.T) {
	r := r4GinEngine(t)
	paid := func(c *gingonic.Context) { c.String(200, "PAID CONTENT") }
	r.GET("/paid", paid)
	r.OPTIONS("/paid", paid)
	err := cantongin.Check(r, testkit.Routes())
	t.Logf("gin, the key's handler on OPTIONS: Check=%v", err)
	assert.Error(t, err)
}

func TestR4XGinNoRouteFileServerForAnotherMethod(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("paid content"), 0o600))
	r := r4GinEngine(t)
	r.GET("/paid", func(c *gingonic.Context) { c.File(filepath.Join(dir, "paid")) })
	r.NoRoute(gingonic.WrapH(http.FileServer(http.Dir(dir))))
	err := cantongin.Check(r, testkit.Routes())
	post := testkit.Do(r, http.MethodPost, "/paid", "")
	t.Logf("gin NoRoute file server: Check=%v; unpaid POST /paid: %d %q", err, post.Code, post.Body.String())
	assert.NotEqual(t, "paid content", post.Body.String())
}

// The same writer-identity test in gin (gin.go: w == outer): a net/http
// middleware composed into the gate function marks free requests too.
func TestR4XGinComposedGateFunctionBypassesASecondGate(t *testing.T) {
	f := testkit.StartFacilitator(t)
	siteGate := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /report": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	})
	apiGate := testkit.NewGate(t, f, func(c *canton.Config) {
		c.Routes = map[string]canton.Route{"GET /api/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
	})
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(func(next http.Handler) http.Handler {
		return http.TimeoutHandler(siteGate.Middleware()(next), 10*time.Minute, "timeout")
	}))
	r.GET("/users/:id", func(c *gingonic.Context) { c.String(200, "user "+c.Param("id")) })
	api := r.Group("/api", cantongin.Middleware(apiGate.Middleware()))
	api.GET("/data", func(c *gingonic.Context) { c.String(200, "PAID API DATA") })
	user := testkit.Do(r, http.MethodGet, "/users/7", "")
	data := testkit.Do(r, http.MethodGet, "/api/data", "")
	t.Logf("gin composed: free /users/7 %d %q; unpaid /api/data %d %q settles=%d", user.Code, user.Body.String(), data.Code, data.Body.String(), f.SettleCalls.Load())
	assert.Equal(t, 402, data.Code)
}
