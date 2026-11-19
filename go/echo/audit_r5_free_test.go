package echo_test

// Round-5 audit (lens: echo): free routes the guard must leave working. These
// pass: kept as evidence for "checked and correct".

import (
	"io"
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

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR5FreeRoutesKeepWorking(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o600))
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	var probesSeen atomic.Int32
	e := r4Echo()
	e.Use(middleware.Recover())
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{AllowOrigins: []string{"https://app.example"}})) // outside the gate
	e.Use(middleware.RateLimiter(middleware.NewRateLimiterMemoryStore(1000)))                              // outside: counts real requests only
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc {                                             // an echo metrics hook outside
		return func(c echolib.Context) error { probesSeen.Add(1); return next(c) }
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID CONTENT") })
	e.POST("/paid", func(c echolib.Context) error { // free ingest of its own on the paid path
		b, _ := io.ReadAll(c.Request().Body)
		return c.String(http.StatusAccepted, "ingested "+string(b))
	})
	e.POST("/collections/:name", func(c echolib.Context) error { // free param ingest
		b, _ := io.ReadAll(c.Request().Body)
		return c.String(http.StatusCreated, c.Param("name")+":"+string(b))
	})
	e.GET("/users/:id", func(c echolib.Context) error { return c.String(200, "user "+c.Param("id")) })
	e.GET("/healthz", func(c echolib.Context) error { return c.String(200, "ok") })
	e.Static("/assets", dir)

	do := func(method, target, body string, header map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		method, target, body string
		header               map[string]string
		code                 int
		contains             string
	}{
		{"POST", "/paid", "point-1", nil, http.StatusAccepted, "ingested point-1"},
		{"POST", "/collections/metrics", strings.Repeat("x", 100000), nil, http.StatusCreated, "metrics:xxxx"},
		{"GET", "/users/7", "", nil, 200, "user 7"},
		{"GET", "/healthz", "", nil, 200, "ok"},
		{"GET", "/assets/app.js", "", nil, 200, "console.log(1)"},
		{"OPTIONS", "/paid", "", map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "GET"}, http.StatusNoContent, ""},
		{"GET", "/paid", "", nil, http.StatusPaymentRequired, ""},
	} {
		before := probesSeen.Load()
		w := do(tc.method, tc.target, tc.body, tc.header)
		t.Logf("%s %s: %d %.40q (outer hook calls %d)", tc.method, tc.target, w.Code, w.Body.String(), probesSeen.Load()-before)
		assert.Equal(t, tc.code, w.Code, "%s %s", tc.method, tc.target)
		assert.Contains(t, w.Body.String(), tc.contains)
		if tc.method == "POST" && tc.target == "/collections/metrics" {
			assert.Len(t, w.Body.String(), len("metrics:")+100000, "the probe copies never touch the body")
		}
	}
	assert.Zero(t, f.VerifyCalls.Load())
	assert.Zero(t, f.SettleCalls.Load())
}

// Routing corners around a paid file under middleware.Static inside the gate:
// lowercase methods, TRACE, spellings and an e.Host router. All are charged or 404.
func TestR5PaidFileCornersAreChargedOrRefused(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("PAID FILE"), 0o600))
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r4Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.Use(middleware.Static(dir))
	e.GET("/paid", func(c echolib.Context) error { return c.File(filepath.Join(dir, "paid")) })
	api := e.Host("api.example")
	api.GET("/paid", func(c echolib.Context) error { return c.File(filepath.Join(dir, "paid")) })
	api.POST("/other", func(c echolib.Context) error { return c.NoContent(http.StatusAccepted) })
	for _, tc := range []struct{ method, host, target string }{
		{"get", "", "/paid"}, {"post", "", "/paid"}, {"TRACE", "", "/paid"}, {"HEAD", "", "/paid"},
		{"POST", "", "//paid"}, {"POST", "", "/paid/"}, {"POST", "", "/%70aid"}, {"POST", "", "/PAID"},
		{"GET", "api.example", "/paid"}, {"POST", "api.example", "/paid"}, {"PUT", "other.example", "/paid"},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		if tc.host != "" {
			r.Host = tc.host
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		t.Logf("%s %s%s: %d %.50q", tc.method, tc.host, tc.target, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "PAID FILE", "%s %s%s", tc.method, tc.host, tc.target)
	}
	assert.Zero(t, f.SettleCalls.Load())
}
