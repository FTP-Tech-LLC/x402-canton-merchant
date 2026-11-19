package echo_test

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// With the FlushError prototype, Gzip outside still delivers an intact gzip body.
func TestR2FixGzipOutsideSuccessIsIntact(t *testing.T) {
	for _, cfg := range []middleware.GzipConfig{{}, {MinLength: 1024}} {
		f := testkit.StartFacilitator(t)
		g := testkit.NewGate(t, f)
		e := r2Echo()
		e.Use(middleware.Recover())
		e.Use(middleware.GzipWithConfig(cfg))
		e.Use(cantonecho.Middleware(g.Middleware()))
		e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid content paid content") })
		sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
		server := httptest.NewServer(e)
		req, _ := http.NewRequestWithContext(context.Background(), "GET", server.URL+"/paid", nil)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("PAYMENT-SIGNATURE", sig)
		resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
		require.NoError(t, err)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		zr, err := gzip.NewReader(bytesReader(raw))
		require.NoError(t, err, "CE=%q raw=%q", resp.Header.Get("Content-Encoding"), raw)
		plain, err := io.ReadAll(zr)
		t.Logf("minLength=%d status=%d ce=%q plain=%q err=%v settles=%d", cfg.MinLength, resp.StatusCode, resp.Header.Get("Content-Encoding"), plain, err, f.SettleCalls.Load())
		assert.NoError(t, err)
		assert.Equal(t, "paid content paid content", string(plain))
		server.Close()
	}
}
