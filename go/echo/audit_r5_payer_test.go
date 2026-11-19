package echo_test

// Round-5 audit probes (lens: echo): the adapter tells paid from free by
// canton.PayerFrom. The gate marks a payment verified on any isValid:true
// answer, but sets the payer only when the answer names one; x402 v2 makes
// VerifyResponse.payer optional. A verified payment without a payer then runs
// on the adapter's free branch: the handler writes to echo's own Response (the
// connection), not to the gate's buffer, and the gate settles an empty 200.

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// verifyOKNoPayer is a spec-valid /verify answer: payer is optional in x402 v2.
func verifyOKNoPayer(_ int32, w http.ResponseWriter) {
	testkit.Reply(w, 200, map[string]any{"isValid": true})
}

func TestR5VerifiedPaymentWithoutPayerRunsOnTheFreeBranch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		handler   echolib.HandlerFunc
		settle    func(n int32, w http.ResponseWriter)
		noSettle  bool   // the money rules forbid any /settle call here
		forbidden string // body the payer must not get
	}{
		{"handler answers 500", func(c echolib.Context) error {
			return c.String(http.StatusInternalServerError, "backend down")
		}, settleOK, true, ""},
		{"handler returns an error", func(c echolib.Context) error {
			return errors.New("db timeout")
		}, settleOK, true, ""},
		{"handler writes 200 then returns an error", func(c echolib.Context) error {
			_ = c.String(http.StatusOK, "partial paid content")
			return errors.New("failed after writing")
		}, settleOK, true, "partial paid content"},
		{"settle refused after a 200", func(c echolib.Context) error {
			return c.String(http.StatusOK, "PAID CONTENT")
		}, func(_ int32, w http.ResponseWriter) {
			testkit.SettleRefused(w, "invalid_exact_canton_payment_expired")
		}, false, "PAID CONTENT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			f.OnVerify(verifyOKNoPayer)
			f.OnSettle(tc.settle)
			g := testkit.NewGate(t, f)
			var hits atomic.Int32
			e := r4Echo()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.GET("/paid", func(c echolib.Context) error { hits.Add(1); return tc.handler(c) })

			w := testkit.Do(e, http.MethodGet, "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
			t.Logf("paid GET /paid (verify isValid without payer): %d %q PAYMENT-RESPONSE=%t hits=%d verify=%d settles=%d",
				w.Code, w.Body.String(), w.Header().Get("PAYMENT-RESPONSE") != "", hits.Load(), f.VerifyCalls.Load(), f.SettleCalls.Load())
			if tc.noSettle {
				assert.Zero(t, f.SettleCalls.Load(), "an error, a non-2xx or a refused settle must not be charged")
			}
			if tc.forbidden != "" {
				assert.NotContains(t, w.Body.String(), tc.forbidden, "paid content must not go out unless settle succeeded")
			}
		})
	}
}

func settleOK(_ int32, w http.ResponseWriter) { testkit.SettleOK(w) }

// Control: the same handlers with a /verify answer that names the payer.
func TestR5VerifiedPaymentWithPayerControl(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := r4Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(http.StatusInternalServerError, "backend down") })
	w := testkit.Do(e, http.MethodGet, "/paid", testkit.SignatureFor(t, e, testkit.PreparedTx))
	t.Logf("control (payer named): %d %q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load())
}
