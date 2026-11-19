package echo_test

import (
	"net/http"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/require"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// Routes added between requests (never concurrently: echo's router is not safe
// for that): before the POST route, POST /paid is guarded; after it, the ingest
// runs; a catch-all or RouteNotFound added later never serves the paid path.
func TestZ2RoutesAddedLater(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := z2Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID") })
	before := testkit.Do(e, http.MethodPost, "/paid", "")
	e.POST("/paid", func(c echolib.Context) error { return c.String(202, "ingest") })
	after := testkit.Do(e, http.MethodPost, "/paid", "")
	e.Any("/x/*", func(c echolib.Context) error { return c.String(200, "PAID via catch-all") })
	e.RouteNotFound("/*", func(c echolib.Context) error { return c.String(200, "PAID via notfound") })
	put := testkit.Do(e, http.MethodPut, "/PAID", "")
	require.Equal(t, http.StatusNotFound, before.Code, "no POST route yet: the guard answers")
	require.NotContains(t, before.Body.String(), "PAID")
	require.Equal(t, http.StatusAccepted, after.Code, "a route of its own added before the next request is the merchant's own")
	require.Equal(t, "ingest", after.Body.String())
	require.NotContains(t, put.Body.String(), "PAID", "a catch-all and RouteNotFound added later never serve the paid path")
	require.Zero(t, f.VerifyCalls.Load())
	require.Zero(t, f.SettleCalls.Load())
}
