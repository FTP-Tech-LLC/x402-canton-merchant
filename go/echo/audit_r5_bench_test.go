package echo_test

// Round-5 audit (lens: echo): per-request cost of the runtime guard on free
// requests. Same source runs on the round-4 code (no guard) for comparison.

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func benchEcho(b *testing.B, keys int) *echolib.Echo {
	b.Helper()
	f := testkit.StartFacilitator(&testing.T{})
	routes := map[string]canton.Route{}
	for i := 0; i < keys; i++ {
		routes[fmt.Sprintf("GET /v1/paid%d", i)] = canton.Route{PayTo: testkit.PayTo, Price: canton.CC("0.01")}
	}
	g, err := canton.NewGate(b.Context(), canton.Config{FacilitatorURL: f.URL, Routes: routes, Logger: discardLogger()})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = g.Close(b.Context()) })
	e := echolib.New()
	e.Logger.SetOutput(io.Discard)
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	for i := 0; i < keys; i++ {
		e.GET(fmt.Sprintf("/v1/paid%d", i), func(c echolib.Context) error { return c.String(200, "paid") })
	}
	e.GET("/free", func(c echolib.Context) error { return c.String(200, "free") })
	e.GET("/users/:id", func(c echolib.Context) error { return c.String(200, "user") })
	e.GET("/assets/*", func(c echolib.Context) error { return c.String(200, "asset") })
	return e
}

func BenchmarkR5FreeRequests(b *testing.B) {
	for _, keys := range []int{1, 50} {
		for _, tc := range []struct{ name, method, target string }{
			{"exact", http.MethodGet, "/free"},
			{"param", http.MethodGet, "/users/7"},
			{"catch-all", http.MethodGet, "/assets/app.js"},
			{"unrouted", http.MethodGet, "/missing"},
			{"OPTIONS on a paid path", http.MethodOptions, "/v1/paid0"},
		} {
			b.Run(fmt.Sprintf("keys=%d/%s", keys, tc.name), func(b *testing.B) {
				e := benchEcho(b, keys)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					r := httptest.NewRequest(tc.method, tc.target, nil)
					w := httptest.NewRecorder()
					e.ServeHTTP(w, r)
				}
			})
		}
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
