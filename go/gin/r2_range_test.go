package gin_test

// Round-2 audit probe (lens: gin): Range on a paid c.File, through gin and the
// net/http core, for the record (same contract expected).

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR2RangeOnAPaidFileMatchesTheCore(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "paid")
	require.NoError(t, os.WriteFile(file, []byte("paid content"), 0o600))
	f, r := paidEngine(t, func(c *gingonic.Context) { c.File(file) })
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	w := doReq(r, "GET", "/paid", sig, map[string]string{"Range": "bytes=0-3"})
	t.Logf("gin Range: code=%d body=%q CR=%q CL=%q settles=%d", w.Code, w.Body.String(), w.Header().Get("Content-Range"), w.Header().Get("Content-Length"), f.SettleCalls.Load())

	fc := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, fc)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /paid", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, file) })
	h := g.Middleware()(mux)
	sig2 := testkit.SignatureFor(t, h, testkit.PreparedTx)
	w2 := doReq(h, "GET", "/paid", sig2, map[string]string{"Range": "bytes=0-3"})
	t.Logf("core Range: code=%d body=%q CR=%q CL=%q settles=%d", w2.Code, w2.Body.String(), w2.Header().Get("Content-Range"), w2.Header().Get("Content-Length"), fc.SettleCalls.Load())
	assert.Equal(t, w2.Code, w.Code)
	assert.Equal(t, w2.Body.String(), w.Body.String())
	assert.Equal(t, fc.SettleCalls.Load(), f.SettleCalls.Load())
}
