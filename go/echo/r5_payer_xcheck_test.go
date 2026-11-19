package echo_test

// Cross-check from the gin lens (round 5): the echo adapter also reads "paid"
// from canton.PayerFrom, which is false for a valid /verify answer without a
// payer (x402 v2: payer is optional).

import (
	"net/http"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR5XEchoValidVerifyWithoutPayerHandler500IsNotSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	f.OnVerify(func(_ int32, w http.ResponseWriter) { testkit.Reply(w, 200, map[string]any{"isValid": true}) })
	g := testkit.NewGate(t, f)
	e := newEcho(g.Middleware(), func(c echolib.Context) error { return c.String(500, "database down") })
	w := testkit.Do(e, "GET", "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
	t.Logf("echo, verify without payer, handler 500: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load(), "a handler 500 must never be settled")
}
