package gin_test

// Round-5 audit (lens: gin): what the runtime guard costs a FREE request. The
// guard runs the whole gate on unpaid copies of the request: 4-5 copies on a
// param or catch-all route, 5-6 on NoRoute. Each copy runs the SDK's route
// match, a regexp per paid key.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gingonic "github.com/gin-gonic/gin"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

func r5Routes(n int) map[string]canton.Route {
	routes := map[string]canton.Route{}
	for i := 0; i < n; i++ {
		routes[fmt.Sprintf("GET /reports/r%d", i)] = canton.Route{PayTo: testkit.PayTo, Price: canton.CC("0.01")}
	}
	return routes
}

func r5Engine(tb testing.TB, facilitatorURL string, n int, withGate bool, gateRuns *atomic.Int64) *gingonic.Engine {
	r := gingonic.New()
	if withGate {
		g, err := canton.NewGate(context.Background(), canton.Config{FacilitatorURL: facilitatorURL, Routes: r5Routes(n),
			SupportedWait: 2 * time.Second, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = g.Close(context.Background()) })
		mw := g.Middleware()
		counted := func(next http.Handler) http.Handler {
			h := mw(next)
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if gateRuns != nil {
					gateRuns.Add(1)
				}
				h.ServeHTTP(w, req)
			})
		}
		if gateRuns != nil {
			r.Use(cantongin.Middleware(counted))
		} else {
			r.Use(cantongin.Middleware(mw))
		}
	}
	r.GET("/health", func(c *gingonic.Context) { c.String(200, "ok") })
	r.GET("/users/:id", func(c *gingonic.Context) { c.String(200, "user") })
	r.GET("/assets/*filepath", func(c *gingonic.Context) { c.String(200, "asset") })
	return r
}

// TestR5GateRunsPerFreeRequest counts how often the gate middleware runs for
// one free request (1 = the request itself).
func TestR5GateRunsPerFreeRequest(t *testing.T) {
	f := testkit.StartFacilitator(t)
	var runs atomic.Int64
	r := r5Engine(t, f.URL, 10, true, &runs)
	for _, target := range []string{"/health", "/users/7", "/assets/app.js", "/nope", "/reports/r3/x"} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			runs.Store(0)
			w := testkit.Do(r, method, target, "")
			t.Logf("%s %s: code=%d gate runs=%d", method, target, w.Code, runs.Load())
		}
	}
}

func BenchmarkR5FreeRequest(b *testing.B) {
	fac := startBenchFacilitator(b)
	for _, n := range []int{1, 100, 1000} {
		for _, withGate := range []bool{false, true} {
			if !withGate && n != 1 {
				continue
			}
			r := r5Engine(b, fac, n, withGate, nil)
			for _, target := range []string{"/health", "/users/7", "/assets/app.js", "/nope"} {
				b.Run(fmt.Sprintf("keys=%d/gate=%v%s", n, withGate, target), func(b *testing.B) {
					req := httptest.NewRequest("GET", target, nil)
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						w := httptest.NewRecorder()
						r.ServeHTTP(w, req)
					}
				})
			}
		}
	}
}

func startBenchFacilitator(b *testing.B) string {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /supported", func(w http.ResponseWriter, _ *http.Request) {
		testkit.Reply(w, 200, map[string]any{
			"kinds": []any{map[string]any{"x402Version": 2, "scheme": "exact", "network": testkit.Network,
				"extra": map[string]any{"feePayer": testkit.FeePayer, "synchronizerId": testkit.SyncID}}},
			"extensions": []string{}, "signers": map[string]any{"canton:*": []string{testkit.FeePayer}},
		})
	})
	server := httptest.NewServer(mux)
	b.Cleanup(server.Close)
	return server.URL
}

// BenchmarkR5FreeRequestParallel: throughput of free requests on all cores.
func BenchmarkR5FreeRequestParallel(b *testing.B) {
	fac := startBenchFacilitator(b)
	for _, n := range []int{1, 1000} {
		r := r5Engine(b, fac, n, true, nil)
		for _, target := range []string{"/health", "/users/7", "/assets/app.js", "/nope"} {
			b.Run(fmt.Sprintf("keys=%d%s", n, target), func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					req := httptest.NewRequest("GET", target, nil)
					for pb.Next() {
						w := httptest.NewRecorder()
						r.ServeHTTP(w, req)
					}
				})
			})
		}
	}
}

// TestR5LongPathCost times one free NoRoute request and one param-route
// request with a long path (Go's default MaxHeaderBytes allows ~1 MB).
func TestR5LongPathCost(t *testing.T) {
	f := testkit.StartFacilitator(t)
	for _, n := range []int{1, 1000} {
		r := r5Engine(t, f.URL, n, true, nil)
		for _, size := range []int{1 << 10, 64 << 10, 512 << 10} {
			long := strings.Repeat("a", size)
			for _, target := range []string{"/" + long, "/users/" + long} {
				start := time.Now()
				w := testkit.Do(r, "GET", target, "")
				t.Logf("keys=%d path=%d KiB %s: code=%d took %s", n, size>>10, target[:7], w.Code, time.Since(start).Round(time.Microsecond))
			}
		}
	}
}
