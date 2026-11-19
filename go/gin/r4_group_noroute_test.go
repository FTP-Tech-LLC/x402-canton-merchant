package gin_test

// Round-4 audit probes (lens: gin): the NoRoute protection lives in the
// adapter, and gin runs only the ENGINE's middleware before NoRoute. A gate on
// a group (the README's "second, different gate", or the only gate, as in
// TestPaidRouteInAGroupIsKeyedByItsFullPath) is not in that chain. Another
// method is reported by Check (the implementer's own test), but a GET
// spelling (//path, /./path, /x/../path) reaches the NoRoute file server,
// which cleans the path and serves the paid file. Check sends only the
// canonical path, so it passes.

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// A site served from a directory at the root (NoRoute + http.FileServer), a
// cheap global gate, and a premium group with a gate of its own.
func r4GroupGateSite(t *testing.T) (*testkit.Facilitator, *gingonic.Engine, map[string]canton.Route) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "premium"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "premium", "report"), []byte("premium report"), 0o600))
	f := testkit.StartFacilitator(t)
	basic := testkit.NewGate(t, f) // GET /paid
	premiumRoutes := map[string]canton.Route{"GET /premium/report": {PayTo: testkit.PayTo, Price: canton.CC("5")}}
	premium := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = premiumRoutes })
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(basic.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	group := r.Group("/premium", cantongin.Middleware(premium.Middleware()))
	group.GET("/report", func(c *gingonic.Context) { c.File(filepath.Join(dir, "premium", "report")) })
	r.NoRoute(gingonic.WrapH(http.FileServer(http.Dir(dir))))
	return f, r, premiumRoutes
}

func TestR4GroupGateSpellingNeverReachesNoRoute(t *testing.T) {
	f, r, premiumRoutes := r4GroupGateSite(t)
	require.Equal(t, 402, testkit.Do(r, "GET", "/premium/report", "").Code, "the premium key is gated")
	checkErr := cantongin.Check(r, premiumRoutes)
	free := 0
	for _, target := range []string{"//premium/report", "/./premium/report", "/x/../premium/report", "/premium//report"} {
		w := testkit.Do(r, "GET", target, "")
		t.Logf("unpaid GET %s: code=%d body=%q", target, w.Code, w.Body.String())
		if w.Body.String() == "premium report" {
			free++
		}
	}
	t.Logf("Check(premium routes): %v; spellings served free: %d; settles=%d", checkErr, free, f.SettleCalls.Load())
	// A gate on a group is not in gin's NoRoute chain, so no adapter can stop this at
	// request time: Check is the net, and the README says to register the gate with r.Use.
	assert.True(t, free == 0 || checkErr != nil, "the paid file is served free and Check passes")
}

// Control: the same spellings for the global gate's key get gin's 404.
func TestR4GlobalGateSpellingControl(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("paid content"), 0o600))
	g := testkit.NewGate(t, testkit.StartFacilitator(t))
	r := gingonic.New()
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.File(filepath.Join(dir, "paid")) })
	r.NoRoute(gingonic.WrapH(http.FileServer(http.Dir(dir))))
	for _, target := range []string{"//paid", "/./paid", "/x/../paid"} {
		w := testkit.Do(r, "GET", target, "")
		t.Logf("global gate, unpaid GET %s: code=%d", target, w.Code)
		assert.NotEqual(t, "paid content", w.Body.String())
	}
}

// The same site with a NoRoute that serves files for GET and HEAD only (a
// static site or SPA host usually refuses other methods): Check now passes,
// and the GET spellings still return the premium file free.
func TestR4GroupGateSpellingWithAGetOnlyNoRouteAndCheckPasses(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "premium"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "premium", "report"), []byte("premium report"), 0o600))
	f := testkit.StartFacilitator(t)
	basic := testkit.NewGate(t, f)
	premiumRoutes := map[string]canton.Route{"GET /premium/report": {PayTo: testkit.PayTo, Price: canton.CC("5")}}
	premium := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = premiumRoutes })
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(basic.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "paid content") })
	r.Group("/premium", cantongin.Middleware(premium.Middleware())).
		GET("/report", func(c *gingonic.Context) { c.File(filepath.Join(dir, "premium", "report")) })
	files := http.FileServer(http.Dir(dir))
	r.NoRoute(func(c *gingonic.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.AbortWithStatus(http.StatusMethodNotAllowed)
			return
		}
		files.ServeHTTP(c.Writer, c.Request)
	})

	require.Equal(t, 402, testkit.Do(r, "GET", "/premium/report", "").Code)
	checkErr := cantongin.Check(r, premiumRoutes)
	checkAll := cantongin.Check(r, testkit.Routes())
	w := testkit.Do(r, "GET", "//premium/report", "")
	t.Logf("Check(premium)=%v Check(basic)=%v; unpaid GET //premium/report: code=%d body=%q settles=%d",
		checkErr, checkAll, w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.False(t, checkErr == nil && w.Body.String() == "premium report", "the premium file is served free and Check passes")
}

// Check's NoRoute probe with a gate on a group and a NoRoute reverse proxy to
// the merchant's backend: at every startup Check sends an unpaid DELETE, PUT,
// PATCH and POST for the paid path to the backend (the global gate would
// stop them in the adapter; a group gate is not in the NoRoute chain).
func TestR4CheckSendsDestructiveMethodsThroughANoRouteProxy(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		seen = append(seen, req.Method+" "+req.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	f := testkit.StartFacilitator(t)
	premiumRoutes := map[string]canton.Route{"GET /premium/report": {PayTo: testkit.PayTo, Price: canton.CC("5")}}
	premium := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = premiumRoutes })
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Group("/premium", cantongin.Middleware(premium.Middleware())).
		GET("/report", func(c *gingonic.Context) { c.String(200, "premium report") })
	r.NoRoute(gingonic.WrapH(httputil.NewSingleHostReverseProxy(target)))

	checkErr := cantongin.Check(r, premiumRoutes)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("Check: %v; requests the backend received during Check: %v", checkErr, seen)
	assert.NotContains(t, seen, "DELETE /premium/report", "Check sent an unpaid DELETE to the backend at startup")
}
