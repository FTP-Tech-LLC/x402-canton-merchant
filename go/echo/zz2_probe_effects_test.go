package echo_test

// Round-2 probe: guard probes (reachesPaidPath, SkipPaidPaths) on free requests
// that carry a valid payment for the paid key and a body. A probe must not reach
// a handler, /verify, /settle or the Store.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	echolib "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantonecho "github.com/FTP-Tech-LLC/x402-canton-merchant/go/echo"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type z2CountingStore struct {
	canton.Store
	calls atomic.Int32
}

func (s *z2CountingStore) Lock(ctx context.Context, hash, token string, ttl time.Duration) (bool, error) {
	s.calls.Add(1)
	return s.Store.Lock(ctx, hash, token, ttl)
}
func (s *z2CountingStore) LockHolder(ctx context.Context, hash string) (string, bool, error) {
	s.calls.Add(1)
	return s.Store.LockHolder(ctx, hash)
}
func (s *z2CountingStore) Unlock(ctx context.Context, hash, token string) error {
	s.calls.Add(1)
	return s.Store.Unlock(ctx, hash, token)
}
func (s *z2CountingStore) Record(ctx context.Context, hash string) (canton.Record, bool, error) {
	s.calls.Add(1)
	return s.Store.Record(ctx, hash)
}
func (s *z2CountingStore) PutRecord(ctx context.Context, hash string, record canton.Record, ttl time.Duration) error {
	s.calls.Add(1)
	return s.Store.PutRecord(ctx, hash, record, ttl)
}
func (s *z2CountingStore) DeleteRecord(ctx context.Context, hash string) error {
	s.calls.Add(1)
	return s.Store.DeleteRecord(ctx, hash)
}
func (s *z2CountingStore) Claim(ctx context.Context, updateID, token string, ttl time.Duration) (bool, error) {
	s.calls.Add(1)
	return s.Store.Claim(ctx, updateID, token, ttl)
}
func (s *z2CountingStore) Claimant(ctx context.Context, updateID string) (string, bool, error) {
	s.calls.Add(1)
	return s.Store.Claimant(ctx, updateID)
}
func (s *z2CountingStore) Unclaim(ctx context.Context, updateID, token string) error {
	s.calls.Add(1)
	return s.Store.Unclaim(ctx, updateID, token)
}
func (s *z2CountingStore) MarkDelivered(ctx context.Context, updateID, token string, ttl time.Duration) error {
	s.calls.Add(1)
	return s.Store.MarkDelivered(ctx, updateID, token, ttl)
}

func TestZ2EchoProbesHaveNoEffect(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "free.txt"), []byte("FREE FILE"), 0o600))
	f := testkit.StartFacilitator(t)
	store := &z2CountingStore{Store: canton.NewMemoryStore()}
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Store = store })
	var runs atomic.Int32
	count := func(name string) echolib.HandlerFunc {
		return func(c echolib.Context) error {
			runs.Add(1)
			body, _ := io.ReadAll(c.Request().Body)
			return c.String(200, name+" body="+string(body))
		}
	}
	e := z2Echo()
	e.Use(middleware.Recover())
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.Use(middleware.StaticWithConfig(middleware.StaticConfig{Root: dir, Skipper: cantonecho.SkipPaidPaths(g.Middleware())}))
	e.GET("/paid", count("paid"))
	e.Any("/v/:name", count("param"))
	e.Any("/files/*", count("any"))
	e.RouteNotFound("/*", count("notfound"))
	sig := testkit.SignatureFor(t, e, testkit.PreparedTx)
	store.calls.Store(0)
	verify0, settle0 := f.VerifyCalls.Load(), f.SettleCalls.Load()
	var requests, maxRunsPerRequest int32
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "PROPFIND"} {
		for _, target := range []string{"/v/x", "/v/paid", "/files/paid", "/files/x", "/nowhere", "/PAID", "/paid/", "//paid", "/%2570aid", "/free.txt", "/paid%3Fx", "/v/%2570aid"} {
			if (m == "GET" || m == "HEAD") && (target == "/PAID" || target == "/paid/" || target == "//paid" || target == "/%2570aid") {
				continue // charged or refused by the core itself (the SDK runs): not a probe-only request
			}
			r := httptest.NewRequest(m, target, strings.NewReader("request body"))
			r.Header.Set("PAYMENT-SIGNATURE", sig)
			before, storeBefore, verifyBefore := runs.Load(), store.calls.Load(), f.VerifyCalls.Load()
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			requests++
			if store.calls.Load() != storeBefore || f.VerifyCalls.Load() != verifyBefore {
				charged := w.Code == http.StatusPaymentRequired || w.Header().Get("PAYMENT-RESPONSE") != ""
				t.Logf("%s %s -> %d store+%d verify+%d (SDK-charged spelling: %v, path %q)", m, target, w.Code,
					store.calls.Load()-storeBefore, f.VerifyCalls.Load()-verifyBefore, charged, r.URL.Path)
				if strings.Contains(r.URL.Path, "?") && (m == "GET" || m == "HEAD") {
					store.calls.Store(storeBefore) // the SDK matches "/paid?x" (cut at ?): a real paid request, not a probe
					verify0 += f.VerifyCalls.Load() - verifyBefore
				}
			}
			if d := runs.Load() - before; d > maxRunsPerRequest {
				maxRunsPerRequest = d
			}
			if w.Code == http.StatusPaymentRequired {
				t.Logf("%s %s -> 402 (charged by the SDK, not a probe-only request)", m, target)
			}
		}
	}
	t.Logf("requests=%d handler runs=%d (max per request %d) store calls=%d verify=%d settle=%d",
		requests, runs.Load(), maxRunsPerRequest, store.calls.Load(), f.VerifyCalls.Load()-verify0, f.SettleCalls.Load()-settle0)
	require.LessOrEqual(t, maxRunsPerRequest, int32(1), "a probe ran a handler")
	require.Zero(t, store.calls.Load(), "a probe touched the Store")
	require.Zero(t, f.VerifyCalls.Load()-verify0, "a probe called /verify")
	require.Zero(t, f.SettleCalls.Load()-settle0, "a probe called /settle")
}

// A function that does not pass its handler through (README: pass the gate's own
// Middleware and compose nothing into it): SkipPaidPaths sends its probe through
// that function, and the probe reaches the real handler the function wraps.
func TestZ2EchoProbeWithAFunctionThatIgnoresItsHandler(t *testing.T) {
	f := testkit.StartFacilitator(t)
	g := testkit.NewGate(t, f)
	var sideEffects atomic.Int32
	real := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sideEffects.Add(1)
		_, _ = w.Write([]byte("real " + r.Method + " " + r.URL.Path))
	})
	misuse := func(http.Handler) http.Handler { return g.Middleware()(real) }
	e := z2Echo()
	e.Use(cantonecho.Middleware(g.Middleware()))
	e.Use(middleware.StaticWithConfig(middleware.StaticConfig{Root: t.TempDir(), Skipper: cantonecho.SkipPaidPaths(misuse)}))
	e.Any("/v/:name", func(c echolib.Context) error { return c.String(200, "param") })
	w := testkit.Do(e, http.MethodDelete, "/v/item", "")
	t.Logf("DELETE /v/item -> %d %q; the real handler inside the misused function ran %d time(s) for this one request", w.Code, w.Body.String(), sideEffects.Load())
}
