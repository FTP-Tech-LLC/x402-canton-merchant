package echo_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// The example's WriteTimeout (30 s) is below a slow settle; the gate lifts it
// through echo's Response.Unwrap. Real server, WriteTimeout 300 ms, settle 1 s.
func TestProbeWriteDeadlineLiftThroughEcho(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid") })
	f.OnSettle(func(_ int32, w http.ResponseWriter) { time.Sleep(time.Second); testkit.SettleOK(w) })
	srv := httptest.NewUnstartedServer(e)
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	req, _ := http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PAYMENT-SIGNATURE", sig)
	resp, err := http.DefaultClient.Do(req)
	if assert.NoError(t, err) {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("status=%d body=%q", resp.StatusCode, b)
		assert.Equal(t, 200, resp.StatusCode)
	}
}
