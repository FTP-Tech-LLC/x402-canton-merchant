package echo_test

import (
	"errors"
	"io"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func TestProbeInnerMiddlewaresThatHandleErrors(t *testing.T) {
	cases := []struct {
		name  string
		mw    echolib.MiddlewareFunc
		route bool // route-level instead of e.Use after the gate
		panic bool
	}{
		{"RequestLogger HandleError", middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
			HandleError: true, LogStatus: true, LogValuesFunc: func(echolib.Context, middleware.RequestLoggerValues) error { return nil }}), false, false},
		{"BodyDump", middleware.BodyDump(func(echolib.Context, []byte, []byte) {}), false, false},
		{"Recover with DisableErrorHandler", middleware.RecoverWithConfig(middleware.RecoverConfig{DisableErrorHandler: true}), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			e := quietEcho()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			h := func(c echolib.Context) error {
				_ = c.String(200, "partial paid content")
				if tc.panic {
					panic("bug after writing")
				}
				return errors.New("failed after writing")
			}
			if tc.route {
				e.GET("/paid", h, tc.mw)
			} else {
				e.Use(tc.mw)
				e.GET("/paid", h)
			}
			sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
			w := testkit.Do(e, "GET", "/paid", sig)
			t.Logf("%s: status=%d body=%q settles=%d", tc.name, w.Code, w.Body.String(), f.SettleCalls.Load())
			assert.Zero(t, f.SettleCalls.Load())
		})
	}
	_ = io.Discard
}
