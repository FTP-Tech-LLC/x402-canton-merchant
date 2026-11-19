package gin_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// The round-4 group-gate setup (the README forbids it when NoRoute serves
// files), with a NoRoute that serves one directory by prefix. Check's one
// "//path" request does not have the prefix, so Check passes, while
// "/premium//report" and "/premium/./report" get the premium file free.
func TestR5GroupGateCheckMissesAPrefixNoRoute(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "premium"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "premium", "report"), []byte("premium report"), 0o600))
	f := testkit.StartFacilitator(t)
	premiumRoutes := map[string]canton.Route{"GET /premium/report": {PayTo: testkit.PayTo, Price: canton.CC("5")}}
	premium := testkit.NewGate(t, f, func(c *canton.Config) { c.Routes = premiumRoutes })
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Group("/premium", cantongin.Middleware(premium.Middleware())).
		GET("/report", func(c *gingonic.Context) { c.File(filepath.Join(dir, "premium", "report")) })
	r.NoRoute(func(c *gingonic.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/premium/") { // the premium assets directory
			c.File(filepath.Join(dir, filepath.Clean(c.Request.URL.Path)))
			return
		}
		c.JSON(http.StatusNotFound, gingonic.H{"error": "not found"})
	})
	checkErr := cantongin.Check(r, premiumRoutes)
	free := 0
	for _, target := range []string{"//premium/report", "/premium//report", "/premium/./report"} {
		w := testkit.Do(r, "GET", target, "")
		t.Logf("unpaid GET %s: code=%d body=%q", target, w.Code, w.Body.String())
		if w.Body.String() == "premium report" {
			free++
		}
	}
	t.Logf("Check(premium)=%v; spellings served free: %d; settles=%d", checkErr, free, f.SettleCalls.Load())
	assert.False(t, checkErr == nil && free > 0, "Check passes while a spelling of the paid key is served free")
}
