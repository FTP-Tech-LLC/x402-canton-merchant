package gin_test

// Round-5 echo-lens cross-check: gin tells paid from free by canton.PayerFrom
// as echo does (gin/gin.go:71). A /verify answer with isValid:true and no payer
// (x402 v2: payer is optional) runs the paid handler on the free branch.

import (
	"net/http"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestR5XGinVerifiedPaymentWithoutPayer(t *testing.T) {
	f := testkit.StartFacilitator(t)
	f.OnVerify(func(_ int32, w http.ResponseWriter) { testkit.Reply(w, 200, map[string]any{"isValid": true}) })
	g := testkit.NewGate(t, f)
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", func(c *gingonic.Context) { c.String(http.StatusInternalServerError, "backend down") })
	w := testkit.Do(r, http.MethodGet, "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("gin, verify isValid without payer, handler 500: %d %q settles=%d", w.Code, w.Body.String(), f.SettleCalls.Load())
	assert.Zero(t, f.SettleCalls.Load(), "a 500 must not be charged")
}
