package echo_test

import (
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// A spelling the gate charges ("/paid/", "//paid", "/%70aid") is not routed to
// the paid handler by echo; a 200 catch-all (SPA fallback, e.Static("/")) answers
// it and that answer is settled.
func TestProbeChargedSpellingServedByCatchAll(t *testing.T) {
	for _, target := range []string{"/paid/", "//paid", "/%70aid"} {
		t.Run(target, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			e := quietEcho()
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid content") })
			e.RouteNotFound("/*", func(c echolib.Context) error { return c.HTML(200, "<html>spa index</html>") })
			sig := testkit.SignatureForPath(t, e, target, testkit.PreparedTx)
			w := testkit.Do(e, "GET", target, sig)
			t.Logf("%s: status=%d body=%q settles=%d", target, w.Code, w.Body.String(), f.SettleCalls.Load())
			assert.False(t, f.SettleCalls.Load() == 1 && w.Body.String() != "paid content", "charged for an answer that is not the paid route")
		})
	}
}
