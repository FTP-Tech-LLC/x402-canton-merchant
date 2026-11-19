package gin_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// Odd request targets (asterisk form, absolute form, CONNECT) through the guard:
// no panic, no paid bytes, no settle.
func TestR5OddRequestTargets(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "paid"), []byte("PAID-FILE"), 0o600))
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(200, "PAID-ROUTE") })
	r.Any("/x/*rest", func(c *gingonic.Context) { c.String(200, "free x") })
	r.NoRoute(gingonic.WrapH(httpFileServer(dir)))
	srv := httptest.NewServer(r)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for _, mt := range [][2]string{
		{"OPTIONS", "*"}, {"GET", "*"}, {"GET", "http://x/paid"}, {"POST", "http://x//paid"}, {"CONNECT", "/paid"},
		{"CONNECT", "x:443"}, {"PRI", "/paid"}, {"GET", "/paid#frag"}, {"PUT", "/x/../paid"}, {"DELETE", "/x/%2e%2e/paid"},
	} {
		code, body := r4RawRequest(t, addr, mt[0], mt[1])
		t.Logf("%s %s: %d %q", mt[0], mt[1], code, body)
		assert.NotContains(t, body, "PAID-", mt)
	}
	assert.Zero(t, f.SettleCalls.Load())
}
