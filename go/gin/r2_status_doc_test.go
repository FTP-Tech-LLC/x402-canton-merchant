package gin_test

// Round-2 audit probes (lens: gin): README "a bare status (c.Status,
// WriteHeader) ... c.AbortWithStatus ... are not charged".

import (
	"net/http"
	"testing"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

// Documented: a bare 2xx is an empty paid answer and is charged (only a bare 3xx,
// 4xx or 5xx is not).
func TestBareTwoHundredStatusIsAnEmptyPaidAnswer(t *testing.T) {
	for name, handler := range map[string]gingonic.HandlerFunc{
		"c.Status(204)":             func(c *gingonic.Context) { c.Status(http.StatusNoContent) },
		"c.AbortWithStatus(204)":    func(c *gingonic.Context) { c.AbortWithStatus(http.StatusNoContent) },
		"c.AbortWithStatus(200)":    func(c *gingonic.Context) { c.AbortWithStatus(http.StatusOK) },
		"c.Writer.WriteHeader(202)": func(c *gingonic.Context) { c.Writer.WriteHeader(http.StatusAccepted) },
	} {
		f, r := paidEngine(t, handler)
		testkit.Do(r, "GET", "/paid", testkit.SignatureFor(t, r, testkit.PreparedTx))
		assert.Equal(t, int32(1), f.SettleCalls.Load(), name)
	}
}
