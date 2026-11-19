package echo_test

// Final audit (lens: gin/echo round-5 fixes): cantonecho.SkipPaidPaths as the
// Skipper of middleware.Static inside the gate. Sweep spellings x methods,
// unpaid, and flag any answer that carries the paid file.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func finalSpellings(p string) []string {
	set := map[string]bool{p: true, strings.ToUpper(p): true, p + "/": true, "/" + p: true, "/." + p: true, "/x/.." + p: true,
		p + "%2F": true, p + "%3Fx": true, p + "%23x": true, p + "?q=1": true, p + ";x": true, p + "%20": true, p + ".": true}
	for i := 1; i < len(p); i++ {
		c := p[i]
		if c == '/' {
			continue
		}
		h := strings.ToUpper(hexByte(c))
		set[p[:i]+"%"+h+p[i+1:]] = true                  // single
		set[p[:i]+"%25"+h+p[i+1:]] = true                // double
		set[p[:i]+"%2525"+h+p[i+1:]] = true              // triple
		set[p[:i]+"%"+strings.ToLower(h)+p[i+1:]] = true // lower-case escape
	}
	if i := strings.LastIndex(p, "/"); i > 0 {
		set[p[:i]+"/"+p[i:]] = true
		set[p[:i]+"%2F"+p[i+1:]] = true
		set[p[:i]+"%252F"+p[i+1:]] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	return out
}

func hexByte(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&15]})
}

type staticCase struct {
	name     string
	unescape bool
	ownPost  bool
}

func TestFinalStaticSkipPaidPathsSweep(t *testing.T) {
	for _, sc := range []staticCase{
		{name: "default", ownPost: true},
		{name: "default, no own route"},
		{name: "EnablePathUnescaping", unescape: true, ownPost: true},
		{name: "EnablePathUnescaping, no own route", unescape: true},
	} {
		t.Run(sc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "v1"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "v1", "data"), []byte("PAID FILE"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "free.txt"), []byte("FREE FILE"), 0o600))
			f := testkit.StartFacilitator(t)
			routes := map[string]canton.Route{"GET /v1/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
			g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
			var own atomic.Int32
			e := r4Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.Use(middleware.StaticWithConfig(middleware.StaticConfig{Root: dir, Skipper: cantonecho.SkipPaidPaths(g.Middleware()), EnablePathUnescaping: sc.unescape}))
			e.GET("/v1/data", func(c echolib.Context) error { return c.File(filepath.Join(dir, "v1", "data")) })
			if sc.ownPost {
				e.POST("/v1/data", func(c echolib.Context) error { own.Add(1); return c.NoContent(http.StatusAccepted) })
			}
			checkErr := cantonecho.Check(e, routes)
			leaks := 0
			for _, s := range finalSpellings("/v1/data") {
				for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "PROPFIND"} {
					r := httptest.NewRequest(m, s, nil)
					w := httptest.NewRecorder()
					e.ServeHTTP(w, r)
					if strings.Contains(w.Body.String(), "PAID FILE") {
						leaks++
						t.Logf("LEAK unpaid %s %s (path %q raw %q): %d %q", m, s, r.URL.Path, r.URL.RawPath, w.Code, w.Body.String())
					}
				}
			}
			free := testkit.Do(e, "GET", "/free.txt", "")
			t.Logf("Check=%v; leaks=%d; free GET /free.txt: %d %q; settles=%d", checkErr, leaks, free.Code, free.Body.String(), f.SettleCalls.Load())
			assert.Equal(t, "FREE FILE", free.Body.String(), "a free static file is still served")
			assert.Zero(t, leaks, "the paid file is served free with SkipPaidPaths as the Skipper")
		})
	}
}

// The same family through a route: e.Static with Echo.EnablePathUnescapingStaticFiles
// (StaticDirectoryHandler decodes the wildcard param once more). The paid key has
// its own exact route, as Check requires; e.Static serves the rest of the
// directory. The runtime guard covers the catch-all route for other methods only,
// and asks the index with the path as net/http decoded it once.
func TestFinalEchoStaticRouteDoubleDecode(t *testing.T) {
	for _, unescape := range []bool{false, true} {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "v1"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "v1", "data"), []byte("PAID FILE"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "v1", "free.txt"), []byte("FREE FILE"), 0o600))
		f := testkit.StartFacilitator(t)
		routes := map[string]canton.Route{"GET /v1/data": {PayTo: testkit.PayTo, Price: canton.CC("0.01")}}
		g := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = routes })
		e := r4Echo()
		e.EnablePathUnescapingStaticFiles = unescape
		e.Use(middleware.Recover())
		e.Use(cantonecho.Middleware(g.Middleware()))
		e.GET("/v1/data", func(c echolib.Context) error { return c.File(filepath.Join(dir, "v1", "data")) })
		e.Static("/v1", filepath.Join(dir, "v1"))
		checkErr := cantonecho.Check(e, routes)
		leaks := 0
		for _, s := range finalSpellings("/v1/data") {
			for _, m := range []string{"GET", "HEAD"} {
				r := httptest.NewRequest(m, s, nil)
				w := httptest.NewRecorder()
				e.ServeHTTP(w, r)
				if strings.Contains(w.Body.String(), "PAID FILE") {
					leaks++
					t.Logf("unescape=%v LEAK unpaid %s %s (path %q raw %q): %d %q", unescape, m, s, r.URL.Path, r.URL.RawPath, w.Code, w.Body.String())
				}
			}
		}
		free := testkit.Do(e, "GET", "/v1/free.txt", "")
		unpaid := testkit.Do(e, "GET", "/v1/data", "")
		t.Logf("unescape=%v Check=%v; unpaid GET /v1/data: %d; leaks=%d; free GET /v1/free.txt: %d %q; settles=%d", unescape, checkErr, unpaid.Code, leaks, free.Code, free.Body.String(), f.SettleCalls.Load())
		assert.Zero(t, leaks, "unescape=%v: the paid file is served free through e.Static", unescape)
	}
}

// Control: the paid request itself, through Static with SkipPaidPaths inside the
// gate: delivered once and settled once.
func TestFinalStaticSkipperPaidRequestDelivered(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("PAID FILE"), 0o600))
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	var hits atomic.Int32
	e := r4Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.Use(middleware.StaticWithConfig(middleware.StaticConfig{Root: dir, Skipper: cantonecho.SkipPaidPaths(g.Middleware())}))
	e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return c.File(filepath.Join(dir, "paid")) })
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := testkit.Do(e, "GET", "/paid", sig)
	again := testkit.Do(e, "GET", "/paid", sig)
	t.Logf("paid GET /paid: %d %q hits=%d verify=%d settles=%d; same payment again: %d", w.Code, w.Body.String(), hits.Load(), f.VerifyCalls.Load(), f.SettleCalls.Load(), again.Code)
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "PAID FILE", w.Body.String())
	assert.Equal(t, int32(1), hits.Load(), "the route's handler serves it (Static skipped)")
	assert.Equal(t, int32(1), f.SettleCalls.Load())
	assert.Equal(t, 402, again.Code, "one payment, one delivery")
}
