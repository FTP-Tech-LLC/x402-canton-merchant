package gin_test

// Round-2 audit probes (lens: gin): the FullPath pass-through. A request gin
// routes to no handler skips the gate entirely, also when a NoRoute handler
// serves content for that path.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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

// A static site served by NoRoute (gin's documented SPA / file-server
// fallback) and a paid route for one of its files. Check passes. A paid
// spelling the SDK treats as the paid route ("//paid") reaches NoRoute, skips
// the gate, and http.FileServer cleans the path and serves the paid file free.
func TestR2NoRouteFileServerServesPaidSpellingFree(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("paid content"), 0o600))
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.File(filepath.Join(dir, "paid")) })
	r.NoRoute(gingonic.WrapH(http.FileServer(http.Dir(dir))))
	require.NoError(t, cantongin.Check(r, testkit.Routes()), "the setup passes the self-check")

	assert.Equal(t, 402, testkit.Do(r, "GET", "/paid", "").Code, "the paid path is gated")
	for _, spelling := range []string{"//paid", "/./paid", "/x/../paid"} {
		w := testkit.Do(r, "GET", spelling, "")
		t.Logf("unpaid %s through NoRoute: code=%d body=%q", spelling, w.Code, w.Body.String())
		assert.NotEqual(t, "paid content", w.Body.String(), "%s: paid content served free", spelling)
	}
	assert.Zero(t, f.SettleCalls.Load())
}

// The net/http core with the same site (mux "/" file server + paid route):
// the same spellings are gated or refused. Control for the contract.
func TestR2NoRouteFileServerControlCore(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("paid content"), 0o600))
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /paid", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Join(dir, "paid")) })
	mux.Handle("/", http.FileServer(http.Dir(dir)))
	h := g.Middleware()(mux)
	for _, spelling := range []string{"//paid", "/./paid", "/x/../paid"} {
		w := testkit.Do(h, "GET", spelling, "")
		t.Logf("core unpaid %s: code=%d body=%q", spelling, w.Code, w.Body.String())
		assert.NotEqual(t, "paid content", w.Body.String(), "%s: paid content served free", spelling)
	}
}

// gin as a paying front for an upstream API: NoRoute proxies everything gin does
// not route. "//paid" skips the gate and the upstream (which normalizes slashes,
// as nginx's default merge_slashes does) serves the paid answer free.
func TestR2NoRouteReverseProxyServesPaidSpellingFree(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if filepath.Clean(r.URL.Path) == "/paid" {
			_, _ = w.Write([]byte("paid content"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)

	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", gingonic.WrapH(proxy))
	r.NoRoute(gingonic.WrapH(proxy))
	require.NoError(t, cantongin.Check(r, testkit.Routes()))

	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	code, body := r2Get(t, front.URL+"/paid")
	_ = body
	assert.Equal(t, 402, code)
	code, body = r2Get(t, front.URL+"//paid")
	t.Logf("unpaid //paid through a NoRoute proxy: code=%d body=%q upstreamHits=%d", code, body, upstreamHits.Load())
	assert.NotEqual(t, "paid content", body, "//paid: paid content served free")
	assert.Zero(t, f.SettleCalls.Load())
}

func r2Get(t *testing.T, target string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", target, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A paid key served only by the NoRoute proxy (no gin route of its own): free at
// the exact key path. Check reports it, with a message about registration order.
func TestR2PaidKeyServedOnlyByNoRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("paid content"))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(cantongin.Middleware(g.Middleware()))
	r.NoRoute(gingonic.WrapH(httputil.NewSingleHostReverseProxy(target)))
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	code, body := r2Get(t, front.URL+"/paid")
	t.Logf("paid key only on NoRoute: code=%d body=%q", code, body)
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("Check panicked: %v", p)
			}
		}()
		return cantongin.Check(r, testkit.Routes())
	}()
	t.Logf("Check: %v", err)
	assert.NotEqual(t, "paid content", body, "the paid key is served free")
}
