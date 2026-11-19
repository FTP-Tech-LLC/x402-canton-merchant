package echo_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestProbeTimeoutHandlerPanicsOnEveryGatedRequest(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	e := quietEcho()
	var panics atomic.Int32
	var last atomic.Value
	e.Use(middleware.RecoverWithConfig(middleware.RecoverConfig{LogErrorFunc: func(_ echolib.Context, err error, _ []byte) error {
		panics.Add(1)
		last.Store(err.Error())
		return err
	}}))
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.GET("/paid", func(c echolib.Context) error { return c.String(200, "paid") })
	e.GET("/free", func(c echolib.Context) error { return c.String(200, "free") })
	h := http.TimeoutHandler(e, 10*time.Minute, "timeout")
	offer := testkit.Do(h, "GET", "/paid", "")
	afterOffer := panics.Load()
	testkit.Do(h, "GET", "/free", "")
	afterFree := panics.Load()
	t.Logf("402 offer: status=%d, panics after offer=%d, after a free request=%d, last=%v", offer.Code, afterOffer, afterFree, last.Load())
	assert.Zero(t, afterOffer, "the 402 offer must not panic")
}
