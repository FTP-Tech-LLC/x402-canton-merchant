package echo_test

// Final audit (lens: gin/echo round-5 fixes): routeSets is created once per
// cantonecho.Middleware call and keys its snapshot by host only ("" for the
// default router). The same MiddlewareFunc on two echo instances (an API and an
// admin server, two ports) shares one snapshot: one instance's exact routes
// exempt the other instance's requests from the guard.

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestFinalSharedMiddlewareAcrossTwoEchoInstances(t *testing.T) {
	for _, shared := range []bool{false, true} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("PAID FILE"), 0o600))
		f := testkit.StartFacilitator(t)
		g := testkit.NewGate(t, f)
		mw := cantonecho.Middleware(g.Middleware())
		mw2 := mw
		if !shared {
			mw2 = cantonecho.Middleware(g.Middleware())
		}
		// e1: the paid key plus a free ingest of its own on POST /paid.
		e1 := r4Echo()
		e1.Use(middleware.Recover())
		e1.Use(mw)
		e1.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID") })
		e1.POST("/paid", func(c echolib.Context) error { return c.NoContent(http.StatusAccepted) })
		// e2: the paid key served from a static root inside the gate; no route of its
		// own for POST /paid, so (README) no Skipper is needed: the guard answers 404.
		e2 := r4Echo()
		e2.Use(middleware.Recover())
		e2.Use(mw2)
		e2.Use(middleware.Static(dir))
		e2.GET("/paid", func(c echolib.Context) error { return c.File(filepath.Join(dir, "paid")) })

		first := testkit.Do(e1, http.MethodPost, "/paid", "")
		w := testkit.Do(e2, http.MethodPost, "/paid", "")
		t.Logf("shared=%v: e1 POST /paid %d; e2 unpaid POST /paid: %d %q; settles=%d", shared, first.Code, w.Code, w.Body.String(), f.SettleCalls.Load())
		assert.NotContains(t, w.Body.String(), "PAID FILE", "shared=%v: e2 serves the paid file free", shared)
	}
}
