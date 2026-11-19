package echo_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"

	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

const fixture = "testdata/report.txt"

func TestProbeEchoWriteMethods(t *testing.T) {
	cases := []struct {
		name    string
		h       echolib.HandlerFunc
		code    int
		body    string
		settles int32
	}{
		{"File", func(c echolib.Context) error { return c.File(fixture) }, 200, "paid file content", 1},
		{"Attachment", func(c echolib.Context) error { return c.Attachment(fixture, "r.txt") }, 200, "paid file content", 1},
		{"Stream", func(c echolib.Context) error { return c.Stream(200, "text/plain", strings.NewReader("streamed")) }, 200, "streamed", 1},
		{"File missing", func(c echolib.Context) error { return c.File(fixture + ".nope") }, 404, "", 0},
		{"wrapped HTTPError", func(c echolib.Context) error { return fmt.Errorf("auth: %w", echolib.ErrUnauthorized) }, 500, "", 0},
		{"Hijack refused", func(c echolib.Context) error {
			_, _, err := c.Response().Hijack()
			if err == nil {
				return c.String(200, "hijacked")
			}
			return err
		}, 500, "", 0},
		{"panic after write, Recover outside", func(c echolib.Context) error { _ = c.String(200, "partial"); panic("bug") }, 500, "", 0},
		{"Redirect invalid code", func(c echolib.Context) error { return c.Redirect(200, "/x") }, 500, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.StartFacilitator(t)
			g := testkit.NewGate(t, f)
			e := quietEcho()
			e.Use(middleware.Recover())
			e.Use(cantonecho.Middleware(g.Middleware()))
			e.GET("/paid", tc.h)
			sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
			w := testkit.Do(e, "GET", "/paid", sig)
			t.Logf("%s: status=%d body=%q cl=%q settles=%d", tc.name, w.Code, w.Body.String(), w.Header().Get("Content-Length"), f.SettleCalls.Load())
			assert.Equal(t, tc.code, w.Code)
			if tc.body != "" {
				assert.Equal(t, tc.body, w.Body.String())
			}
			assert.Equal(t, tc.settles, f.SettleCalls.Load())
		})
	}
	_ = errors.New
	_ = http.StatusOK
}
