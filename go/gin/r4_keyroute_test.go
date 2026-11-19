package gin_test

// Round-4 audit probes (lens: gin). Check now judges other methods by handler
// name, and collects "the key's own handler" only from a route whose path is
// exactly the key's path. A key served by a catch-all or param route (r.Any on
// a file server or a reverse proxy, r.Match on a param route) has no such
// route: the same handler, registered for every other method, is not judged,
// and those methods are not probed either (a route matches them). At runtime
// the adapter asks the gate only about the request's own method on a matched
// route, so the paid content is served free to POST, PUT, DELETE and the rest.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func r4Routes(key string) map[string]canton.Route {
	return map[string]canton.Route{key: {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
}

// Files under /files, one of them sold: the usual gin way to expose a directory
// for every method a client may use, with the gate in front (r.Use).
func TestR4AnyCatchAllFileServerServingAPaidKey(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("paid report"), 0o600))
	f := testkit.StartFacilitator(t)
	routes := r4Routes("GET /files/report.pdf")
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.Any("/files/*filepath", gingonic.WrapH(http.StripPrefix("/files", http.FileServer(http.Dir(dir)))))

	require.Equal(t, 402, testkit.Do(r, "GET", "/files/report.pdf", "").Code, "the key itself is gated")
	err := cantongin.Check(r, routes)
	free := 0
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		w := testkit.Do(r, method, "/files/report.pdf", "")
		t.Logf("unpaid %s /files/report.pdf: code=%d body=%q", method, w.Code, w.Body.String())
		if w.Body.String() == "paid report" {
			free++
		}
	}
	t.Logf("Check: %v; methods served the paid file free: %d; settles=%d", err, free, f.SettleCalls.Load())
	assert.Zero(t, free, "the paid file is served free to other methods")
	assert.Error(t, err, "Check passes although the key's own handler (r.Any) serves every other method free")
}

// The same with a reverse proxy in front of an existing API (the paywall-in-
// front-of-my-backend setup): the upstream serves /api/report for any method.
func TestR4AnyCatchAllProxyServingAPaidKey(t *testing.T) {
	f := testkit.StartFacilitator(t)
	routes := r4Routes("GET /api/report")
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
	var upstreamHits atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { // method-agnostic, as many are
		upstreamHits.Add(1)
		_, _ = fmt.Fprint(w, "report data")
	})
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.Any("/api/*path", gingonic.WrapH(upstream))

	require.Equal(t, 402, testkit.Do(r, "GET", "/api/report", "").Code)
	err := cantongin.Check(r, routes)
	post := testkit.Do(r, "POST", "/api/report", "")
	t.Logf("Check: %v; unpaid POST /api/report: code=%d body=%q upstreamHits=%d", err, post.Code, post.Body.String(), upstreamHits.Load())
	assert.NotEqual(t, "report data", post.Body.String(), "POST /api/report served free through the catch-all")
	assert.Error(t, err, "Check passes")
}

// r.Match on a param route, the shape Check's doc names ("r.Match, a param
// route"): the key GET /v1/data is gated through /v1/:name, and POST /v1/data
// reaches the same handler free.
func TestR4MatchOnAParamRouteServingAPaidKey(t *testing.T) {
	f := testkit.StartFacilitator(t)
	routes := r4Routes("GET /v1/data")
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
	var hits atomic.Int32
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.Match([]string{http.MethodGet, http.MethodPost}, "/v1/:name", func(c *gingonic.Context) {
		hits.Add(1)
		c.String(200, "dataset "+c.Param("name"))
	})

	require.Equal(t, 402, testkit.Do(r, "GET", "/v1/data", "").Code, "the key itself is gated")
	require.Zero(t, hits.Load())
	err := cantongin.Check(r, routes)
	post := testkit.Do(r, "POST", "/v1/data", "")
	t.Logf("Check: %v; unpaid POST /v1/data: code=%d body=%q hits=%d", err, post.Code, post.Body.String(), hits.Load())
	assert.NotEqual(t, "dataset data", post.Body.String(), "POST /v1/data served free")
	assert.Error(t, err, "Check passes although r.Match registers the key's handler for POST")
}

// Control: the same r.Match on the key's exact path is reported by Check.
func TestR4MatchOnTheExactPathControl(t *testing.T) {
	f := testkit.StartFacilitator(t)
	routes := r4Routes("GET /v1/data")
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.Match([]string{http.MethodGet, http.MethodPost}, "/v1/data", func(c *gingonic.Context) { c.String(200, "dataset") })
	err := cantongin.Check(r, routes)
	t.Logf("Check (exact path): %v", err)
	assert.Error(t, err)
}
