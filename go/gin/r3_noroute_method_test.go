package gin_test

// Round-3 audit probes (lens: core flush + gin): the unpaid-clone check for an
// empty FullPath asks the gate about the request's own method only. A paid GET
// path requested with another method reaches NoRoute, and gin's usual static
// root (NoRoute + http.FileServer, which serves a file for any method) returns
// the paid file. Check probes only the methods some gin route registers.

import (
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func r3StaticRoot(t *testing.T) (*testkit.Facilitator, *gingonic.Engine) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("paid content"), 0o600))
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.File(filepath.Join(dir, "paid")) })
	r.NoRoute(gingonic.WrapH(http.FileServer(http.Dir(dir)))) // the static site at the root
	return f, r
}

func TestR3NoRouteFileServerDoesNotServeAPaidPathForAnotherMethod(t *testing.T) {
	f, r := r3StaticRoot(t)
	require.NoError(t, cantongin.Check(r, testkit.Routes()), "the setup passes the self-check")
	require.Equal(t, 402, testkit.Do(r, "GET", "/paid", "").Code)
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS", "FOO"} {
		w := testkit.Do(r, method, "/paid", "")
		t.Logf("unpaid %s /paid through NoRoute: code=%d body=%q", method, w.Code, w.Body.String())
		assert.NotEqual(t, "paid content", w.Body.String(), "%s /paid: paid content served free", method)
	}
	assert.Zero(t, f.SettleCalls.Load())
}

// Either the adapter keeps POST /paid away from NoRoute, or Check reports the
// setup. Today neither happens: POST /paid is served free and Check passes.
func TestR3CheckReportsTheNoRouteFileServerForAnotherMethod(t *testing.T) {
	_, r := r3StaticRoot(t)
	err := cantongin.Check(r, testkit.Routes())
	post := testkit.Do(r, "POST", "/paid", "")
	t.Logf("Check: %v; unpaid POST /paid: code=%d body=%q", err, post.Code, post.Body.String())
	assert.True(t, err != nil || post.Body.String() != "paid content", "POST /paid is served free and Check passes")
}

// Observation: with one POST route anywhere in the app, Check probes POST and
// reports the setup. The gap is the method set, taken from engine.Routes().
func TestR3CheckControlWithAPOSTRouteElsewhere(t *testing.T) {
	_, r := r3StaticRoot(t)
	var hits atomic.Int32
	r.POST("/feedback", func(c *gingonic.Context) { hits.Add(1); c.Status(204) })
	err := cantongin.Check(r, testkit.Routes())
	post := testkit.Do(r, "POST", "/paid", "")
	t.Logf("Check with a POST route elsewhere: %v; unpaid POST /paid: code=%d", err, post.Code)
	assert.True(t, err != nil || post.Body.String() != "paid content")
}

// The gate only on a group: the NoRoute chain has no gate, so a paid path
// requested with another method reaches the file server. The key itself still
// answers 402, so only the other-method probe can see it: Check must fail.
func TestR3CheckCatchesAGatelessNoRouteForAnotherMethod(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("paid content"), 0o600))
	g := testkit.NewGate(t, testkit.StartFacilitator(t))
	r := gingonic.New()
	r.Group("", cantongin.Middleware(g.Middleware())).GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	r.NoRoute(gingonic.WrapH(http.FileServer(http.Dir(dir))))
	err := cantongin.Check(r, testkit.Routes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GET //paid")
	assert.Contains(t, err.Error(), "NoRoute")
}
