package echo_test

import (
	"errors"
	"net/http"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestProbeOuterSeesFinalStatusAndSize(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	type seen struct {
		status    int
		size      int64
		committed bool
	}
	var last seen
	e.Use(func(next echolib.HandlerFunc) echolib.HandlerFunc {
		return func(c echolib.Context) error {
			err := next(c)
			r := c.Response()
			last = seen{r.Status, r.Size, r.Committed}
			return err
		}
	})
	e.Use(cantonecho.Middleware(g.Middleware()))
	mode := "ok"
	e.GET("/paid", func(c echolib.Context) error {
		switch mode {
		case "err":
			_ = c.String(200, "partial")
			return errors.New("x")
		case "204":
			return c.NoContent(204)
		}
		return c.String(200, "paid-body")
	})
	check := func(name string, w interface {
		Result() *http.Response
	}, body int) {
	}
	_ = check
	w402 := testkit.Do(e, "GET", "/paid", "")
	assert.Equal(t, seen{402, int64(w402.Body.Len()), true}, last, "402")
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	w := testkit.Do(e, "GET", "/paid", sig)
	assert.Equal(t, seen{200, int64(w.Body.Len()), true}, last, "200")
	mode = "err"
	sig2 := testkit.SignatureFor(t, e, "bbcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	w = testkit.Do(e, "GET", "/paid", sig2)
	assert.Equal(t, seen{500, int64(w.Body.Len()), true}, last, "500")
	mode = "204"
	f.OnSettle(func(_ int32, w http.ResponseWriter) {
		testkit.Reply(w, 200, map[string]any{"success": true, "transaction": "1220aabbccdd0022", "network": testkit.Network, "payer": testkit.Payer})
	})
	sig3 := testkit.SignatureFor(t, e, "cbcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	w = testkit.Do(e, "GET", "/paid", sig3)
	t.Logf("204 case body=%q", w.Body.String())
	assert.Equal(t, 204, w.Code)
	assert.Equal(t, seen{204, 0, true}, last, "204")
	t.Logf("last=%+v", last)
}
