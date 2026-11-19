package gin_test

// Round-2 probe: gin's guard probes (NoRoute: AnyMethodGated; param and
// catch-all routes: OtherMethodGated) on free requests that carry a valid
// payment for the paid key and a body. A probe must not reach a handler,
// /verify, /settle or the Store.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gingonic "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	cantongin "github.com/FTP-Tech-LLC/x402-canton-merchant/go/gin"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

type z2GinStore struct {
	canton.Store
	calls atomic.Int32
}

func (s *z2GinStore) Lock(ctx context.Context, hash, token string, ttl time.Duration) (bool, error) {
	s.calls.Add(1)
	return s.Store.Lock(ctx, hash, token, ttl)
}
func (s *z2GinStore) LockHolder(ctx context.Context, hash string) (string, bool, error) {
	s.calls.Add(1)
	return s.Store.LockHolder(ctx, hash)
}
func (s *z2GinStore) Unlock(ctx context.Context, hash, token string) error {
	s.calls.Add(1)
	return s.Store.Unlock(ctx, hash, token)
}
func (s *z2GinStore) Record(ctx context.Context, hash string) (canton.Record, bool, error) {
	s.calls.Add(1)
	return s.Store.Record(ctx, hash)
}
func (s *z2GinStore) PutRecord(ctx context.Context, hash string, record canton.Record, ttl time.Duration) error {
	s.calls.Add(1)
	return s.Store.PutRecord(ctx, hash, record, ttl)
}
func (s *z2GinStore) DeleteRecord(ctx context.Context, hash string) error {
	s.calls.Add(1)
	return s.Store.DeleteRecord(ctx, hash)
}
func (s *z2GinStore) Claim(ctx context.Context, updateID, token string, ttl time.Duration) (bool, error) {
	s.calls.Add(1)
	return s.Store.Claim(ctx, updateID, token, ttl)
}
func (s *z2GinStore) Claimant(ctx context.Context, updateID string) (string, bool, error) {
	s.calls.Add(1)
	return s.Store.Claimant(ctx, updateID)
}
func (s *z2GinStore) Unclaim(ctx context.Context, updateID, token string) error {
	s.calls.Add(1)
	return s.Store.Unclaim(ctx, updateID, token)
}
func (s *z2GinStore) MarkDelivered(ctx context.Context, updateID, token string, ttl time.Duration) error {
	s.calls.Add(1)
	return s.Store.MarkDelivered(ctx, updateID, token, ttl)
}

func TestZ2GinProbesHaveNoEffect(t *testing.T) {
	f := testkit.StartFacilitator(t)
	store := &z2GinStore{Store: canton.NewMemoryStore()}
	g := testkit.NewGate(t, f, func(c *canton.Config) { c.Store = store })
	var runs atomic.Int32
	count := func(name string) gingonic.HandlerFunc {
		return func(c *gingonic.Context) {
			runs.Add(1)
			body, _ := io.ReadAll(c.Request.Body)
			c.String(200, "%s body=%s", name, body)
		}
	}
	r := gingonic.New()
	r.Use(gingonic.Recovery())
	r.Use(cantongin.Middleware(g.Middleware()))
	r.GET("/paid", count("paid"))
	r.Any("/v/:name", count("param"))
	r.Any("/files/*rest", count("any"))
	r.NoRoute(count("noroute"))
	sig := testkit.SignatureFor(t, r, testkit.PreparedTx)
	store.calls.Store(0)
	verify0, settle0 := f.VerifyCalls.Load(), f.SettleCalls.Load()
	var requests, maxRuns int32
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "PROPFIND"} {
		for _, target := range []string{"/v/x", "/v/paid", "/files/paid", "/files/x", "/nowhere", "/PAID", "/paid/", "//paid", "/%2570aid", "/v/%2570aid", "/paid%3Fx"} {
			req := httptest.NewRequest(m, target, strings.NewReader("request body"))
			req.Header.Set("PAYMENT-SIGNATURE", sig)
			before, storeBefore, verifyBefore := runs.Load(), store.calls.Load(), f.VerifyCalls.Load()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			requests++
			if d := runs.Load() - before; d > maxRuns {
				maxRuns = d
			}
			if store.calls.Load() != storeBefore || f.VerifyCalls.Load() != verifyBefore {
				t.Logf("%s %s -> %d store+%d verify+%d (path %q)", m, target, w.Code, store.calls.Load()-storeBefore, f.VerifyCalls.Load()-verifyBefore, req.URL.Path)
				// The SDK charges GET/HEAD "//paid", "/paid/", "/paid?x": real paid requests, not probes.
				store.calls.Store(storeBefore)
				verify0 += f.VerifyCalls.Load() - verifyBefore
			}
		}
	}
	t.Logf("requests=%d handler runs=%d (max per request %d) store=%d verify=%d settle=%d",
		requests, runs.Load(), maxRuns, store.calls.Load(), f.VerifyCalls.Load()-verify0, f.SettleCalls.Load()-settle0)
	require.LessOrEqual(t, maxRuns, int32(1))
	require.Zero(t, f.SettleCalls.Load()-settle0)
	_ = http.MethodGet
}
