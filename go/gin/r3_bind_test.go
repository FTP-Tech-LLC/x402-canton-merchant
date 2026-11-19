package gin_test

// Round-3 audit probe (lens: core flush + gin): the bind-failure body
// replacement keeps every header the handler set after it ignored the error.
// A paid answer whose value is a header (a session or download cookie, a
// signed URL) still reaches the payer in the uncharged 400.

import (
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type r3Query struct {
	N int `form:"n" binding:"required"`
}

func TestR3IgnoredBindErrorLeaksNoPaidHeaders(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) {
		var q r3Query
		_ = c.BindQuery(&q) // optional filters: the error is ignored
		c.SetCookie("access", "paid-session-token", 3600, "/", "", true, true)
		c.Header("X-Download-Url", "https://cdn.example/report.pdf?sig=paid")
		c.String(200, "paid content")
	})
	w := testkit.Do(r, "GET", "/paid?n=abc", testkit.SignatureFor(t, r, testkit.PreparedTx))
	t.Logf("ignored bind error: code=%d body=%q Set-Cookie=%q X-Download-Url=%q settles=%d",
		w.Code, w.Body.String(), w.Header().Get("Set-Cookie"), w.Header().Get("X-Download-Url"), f.SettleCalls.Load())
	assert.Equal(t, 400, w.Code)
	assert.Zero(t, f.SettleCalls.Load())
	assert.NotContains(t, w.Body.String(), "paid content")
	assert.Empty(t, w.Header().Get("Set-Cookie"), "the paid session cookie reached the payer in an uncharged 400")
	assert.Empty(t, w.Header().Get("X-Download-Url"), "the paid URL reached the payer in an uncharged 400")
}
