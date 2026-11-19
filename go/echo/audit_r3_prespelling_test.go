package echo_test

import (
	"bytes"
	"os"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/gommon/log"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// One spelling per process (R3_TARGET), gate on e.Use, logger at DEBUG: which
// unrouted spellings trigger the e.Pre warning on a correct setup.
func TestR3PreWarningPerSpelling(t *testing.T) {
	target := os.Getenv("R3_TARGET")
	if target == "" {
		t.Skip("set R3_TARGET")
	}
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r3Echo()
	var logs bytes.Buffer
	e.Logger.SetOutput(&logs)
	e.Logger.SetLevel(log.DEBUG)
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "PAID") })
	w := testkit.Do(e, "GET", target, testkit.SignatureFor(t, e, testkit.PreparedTx))
	t.Logf("%s: %d; e.Pre warning logged: %v", target, w.Code, bytes.Contains(logs.Bytes(), []byte("e.Pre")))
}
