package gin_test

// Round-2 audit probes (lens: gin): binding errors on a paid route.

import (
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type r2Query struct {
	N int `form:"n" binding:"required"`
}

func TestR2BindErrorIsNotSettled(t *testing.T) {
	cases := map[string]gingonic.HandlerFunc{
		"c.BindQuery returns": func(c *gingonic.Context) {
			var q r2Query
			if err := c.BindQuery(&q); err != nil {
				return
			}
			c.String(200, "paid content")
		},
		"c.BindQuery error ignored, then 200": func(c *gingonic.Context) {
			var q r2Query
			_ = c.BindQuery(&q)
			c.String(200, "paid content")
		},
		"c.ShouldBindQuery error answered 422": func(c *gingonic.Context) {
			var q r2Query
			if err := c.ShouldBindQuery(&q); err != nil {
				c.JSON(422, gingonic.H{"error": err.Error()})
				return
			}
			c.String(200, "paid content")
		},
	}
	for name, h := range cases {
		f, r := paidEngine(t, h)
		w := testkit.Do(r, "GET", "/paid?n=abc", testkit.SignatureFor(t, r, testkit.PreparedTx))
		t.Logf("%s: code=%d body=%q settles=%d", name, w.Code, w.Body.String(), f.SettleCalls.Load())
		assert.Zero(t, f.SettleCalls.Load(), name)
		assert.GreaterOrEqual(t, w.Code, 400, name)
	}
}

// gin's c.Bind* already answered 400 and aborted; a handler that ignores the
// returned error and goes on writes paid content after it. The adapter replaces
// the body of a bind-failure 4xx, so the paid content never leaves, and the 400
// is not charged.
func TestIgnoredBindErrorNeverLeaksPaidContentIn400(t *testing.T) {
	f, r := paidEngine(t, func(c *gingonic.Context) {
		var q r2Query
		_ = c.BindQuery(&q) // optional filters: the error is ignored
		c.String(200, "paid content")
	})
	w := testkit.Do(r, "GET", "/paid?n=abc", testkit.SignatureFor(t, r, testkit.PreparedTx))
	assert.Equal(t, 400, w.Code)
	assert.NotContains(t, w.Body.String(), "paid content")
	assert.Zero(t, f.SettleCalls.Load())
}
