package gin_test

import (
	"errors"
	"net/http"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// x402 v2 spec 5.4.2: VerifyResponse.payer is Optional. A facilitator that
// answers {"isValid":true} without a payer is spec-compliant; the core marks the
// request verified and runs the handler, but canton.PayerFrom answers false
// ("when no payer is known"). The gin adapter reads that as "not a paid request".
func verifyWithoutPayer(w http.ResponseWriter) {
	testkit.Reply(w, 200, map[string]any{"isValid": true})
}

func ginPaid(mw func(http.Handler) http.Handler, h gingonic.HandlerFunc) *gingonic.Engine {
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(mw))
	r.GET("/paid", h)
	return r
}

func TestR5ValidVerifyWithoutPayerHandler500IsNotSettled(t *testing.T) {
	f := testkit.StartFacilitator(t)
	f.OnVerify(func(_ int32, w http.ResponseWriter) { verifyWithoutPayer(w) })
	g := testkit.NewGate(t, f)
	r := ginPaid(g.Middleware(), func(c *gingonic.Context) { c.String(500, "database down") })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("gin, verify without payer, handler 500: code=%d body=%q settles=%d PAYMENT-RESPONSE=%q",
		w.Code, w.Body.String(), f.SettleCalls.Load(), w.Header().Get("PAYMENT-RESPONSE"))
	assert.Zero(t, f.SettleCalls.Load(), "a handler 500 must never be settled")
}

func TestR5ValidVerifyWithoutPayerErrorAndAbortAreNotSettled(t *testing.T) {
	for name, h := range map[string]gingonic.HandlerFunc{
		"c.Error then 200":   func(c *gingonic.Context) { _ = c.Error(errors.New("db")); c.String(200, "partial") },
		"abort, no answer":   func(c *gingonic.Context) { c.Abort() },
		"AbortWithStatus503": func(c *gingonic.Context) { c.AbortWithStatus(503) },
	} {
		f := testkit.StartFacilitator(t)
		f.OnVerify(func(_ int32, w http.ResponseWriter) { verifyWithoutPayer(w) })
		g := testkit.NewGate(t, f)
		r := ginPaid(g.Middleware(), h)
		w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
		t.Logf("%s: code=%d body=%q settles=%d", name, w.Code, w.Body.String(), f.SettleCalls.Load())
		assert.Zero(t, f.SettleCalls.Load(), name)
	}
}

func TestR5ValidVerifyWithoutPayerSettleFailureDeliversNothing(t *testing.T) {
	f := testkit.StartFacilitator(t)
	f.OnVerify(func(_ int32, w http.ResponseWriter) { verifyWithoutPayer(w) })
	f.OnSettle(func(_ int32, w http.ResponseWriter) {
		testkit.SettleRefused(w, "invalid_exact_canton_insufficient_funds")
	})
	g := testkit.NewGate(t, f)
	r := ginPaid(g.Middleware(), func(c *gingonic.Context) { c.String(200, "paid content") })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("gin, verify without payer, settle refused: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.NotContains(t, w.Body.String(), "paid content", "a failed settle must not deliver the paid content")
}

// Control: the net/http core with the same facilitator does not settle a 500
// and does not deliver on a failed settle.
func TestR5ValidVerifyWithoutPayerCoreControl(t *testing.T) {
	f := testkit.StartFacilitator(t)
	f.OnVerify(func(_ int32, w http.ResponseWriter) { verifyWithoutPayer(w) })
	g := testkit.NewGate(t, f)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /paid", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("database down"))
	})
	h := g.Middleware()(mux)
	w := testkit.Do(h, "GET", "/paid", testkit.SignatureFor(t, h, testkit.PreparedTx))
	t.Logf("net/http core, verify without payer, handler 500: code=%d body=%q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Equal(t, 500, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
}

// Control: with a payer in the verify answer the gin adapter refuses to settle a 500.
func TestR5PayerPresentHandler500Control(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	r := ginPaid(g.Middleware(), func(c *gingonic.Context) { c.String(500, "database down") })
	w := testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("gin, payer present, handler 500: code=%d settles=%d", w.Code, f.SettleCalls.Load())
	assert.Equal(t, 500, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
}

// The same gate registered twice (README: "harmless") with a verify answer
// that has no payer: the outer adapter takes the free branch, sets no marker,
// and the inner adapter runs the gate again on the same payment.
func TestR5ValidVerifyWithoutPayerSameGateTwice(t *testing.T) {
	f := testkit.StartFacilitator(t)
	f.OnVerify(func(_ int32, w http.ResponseWriter) { verifyWithoutPayer(w) })
	g := testkit.NewGate(t, f)
	var hits int
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	api := r.Group("/")
	api.Use(cantongin.Middleware(g.Middleware()))
	api.GET("/paid", func(c *gingonic.Context) { hits++; c.String(200, "paid content") })
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	w := testkit.Do(r, "GET", "/paid", sig)
	t.Logf("first: code=%d body=%q hits=%d verifies=%d settles=%d", w.Code, w.Body.String(), hits, f.VerifyCalls.Load(), f.SettleCalls.Load())
	retry := testkit.Do(r, "GET", "/paid", sig)
	t.Logf("retry: code=%d body=%q hits=%d settles=%d", retry.Code, retry.Body.String(), hits, f.SettleCalls.Load())
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "paid content", w.Body.String(), "one payment, one clean delivery")
}
