package gin_test

// Round-4 audit probes (lens: gin): Check judges a route for another method by
// the NAME of its last handler (gin's RouteInfo.Handler). Every closure built
// by one function has the same name: gin.WrapH, gin.WrapF, and a merchant's own
// handler factory. A free handler of its own for another method then looks like
// "the key's own handler" and Check fails a correct setup, which the README
// turns into log.Fatal.

import (
	"net/http"
	"sync/atomic"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// net/http handlers mounted with gin.WrapF: a paid GET and a free POST ingest,
// two different functions.
func TestR4CheckDoesNotConflateTwoWrapFHandlers(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	var ingested atomic.Int32
	paidData := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("paid content")) }
	ingest := func(w http.ResponseWriter, _ *http.Request) { ingested.Add(1); w.WriteHeader(http.StatusAccepted) }
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", gingonic.WrapF(paidData))
	r.POST("/paid", gingonic.WrapF(ingest))

	post := testkit.Do(r, "POST", "/paid", "")
	err := cantongin.Check(r, testkit.Routes())
	for _, route := range r.Routes() {
		t.Logf("route %s %s handler=%s", route.Method, route.Path, route.Handler)
	}
	t.Logf("unpaid POST /paid: code=%d body=%q; Check: %v", post.Code, post.Body.String(), err)
	assert.NotEqual(t, "paid content", post.Body.String(), "control: POST serves no paid content")
	assert.NoError(t, err, "a free POST handler of its own is reported as the paid handler")
}

// A handler factory, the usual way to share JSON rendering between routes.
func TestR4CheckDoesNotConflateTwoClosuresOfOneFactory(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	reply := func(status int, body string) gingonic.HandlerFunc {
		return func(c *gingonic.Context) { c.String(status, body) }
	}
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", reply(200, "paid content"))
	r.DELETE("/paid", reply(202, "deletion queued")) // free, its own closure
	err := cantongin.Check(r, testkit.Routes())
	del := testkit.Do(r, "DELETE", "/paid", "")
	t.Logf("unpaid DELETE /paid: code=%d body=%q; Check: %v", del.Code, del.Body.String(), err)
	assert.NotEqual(t, "paid content", del.Body.String())
	assert.NoError(t, err, "a free DELETE closure from the same factory is reported as the paid handler")
}

// Table-driven registration (one call site for every route, as a router built
// from a spec or a list of net/http handlers is): every gin.WrapH closure then
// has the same name, whatever handler it wraps.
func TestR4CheckDoesNotConflateTableRegisteredWrapHHandlers(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	var ingested atomic.Int32
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	for _, route := range []struct {
		method  string
		handler http.Handler
	}{
		{http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("paid content")) })},
		{http.MethodPost, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { ingested.Add(1); w.WriteHeader(http.StatusAccepted) })},
	} {
		r.Handle(route.method, "/paid", gingonic.WrapH(route.handler))
	}
	post := testkit.Do(r, "POST", "/paid", "")
	err := cantongin.Check(r, testkit.Routes())
	for _, route := range r.Routes() {
		t.Logf("route %s %s handler=%s", route.Method, route.Path, route.Handler)
	}
	t.Logf("unpaid POST /paid: code=%d body=%q ingested=%d; Check: %v", post.Code, post.Body.String(), ingested.Load(), err)
	assert.NotEqual(t, "paid content", post.Body.String(), "control: POST serves no paid content")
	assert.NoError(t, err, "a free POST handler of its own is reported as the paid handler")
}
