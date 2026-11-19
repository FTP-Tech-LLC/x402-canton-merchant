package canton

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
)

// Rule 12: gate errors never carry headers the paid handler set.

func leakyHandler(hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Set-Cookie", "access=paid")
		w.Header().Set("Content-Length", "11")
		w.Header().Set("X-Internal", "secret")
		w.Header().Set("ETag", "abc")
		w.Header().Set("Location", "/x")
		_, _ = w.Write([]byte("paid-body!!"))
	}
}

func assertOnlyGateHeaders(t *testing.T, w *httptest.ResponseRecorder, label string) {
	t.Helper()
	for name := range w.Header() {
		assert.True(t, gateAllowedHeaders[http.CanonicalHeaderKey(name)], "%s: header %q must not be on a gate error", label, name)
	}
	assert.Empty(t, w.Header().Get("Set-Cookie"), label)
	assert.Equal(t, fmt.Sprint(w.Body.Len()), w.Header().Get("Content-Length"), "%s: the gate's own length, not the handler's 11", label)
	assert.NotContains(t, w.Body.String(), "paid-body", label)
}

func TestGateErrorsCarryNoHandlerHeaders(t *testing.T) {
	scenarios := map[string]struct {
		settle func(n int32, w http.ResponseWriter)
		store  func() Store
		want   int
	}{
		"refusal":         {settle: func(_ int32, w http.ResponseWriter) { settleRefused(w, "invalid_exact_canton_insufficient_balance") }, want: 402},
		"unknown outcome": {settle: func(_ int32, w http.ResponseWriter) { w.WriteHeader(502) }, want: 503},
		"already settled, claimed elsewhere": {
			settle: func(_ int32, w http.ResponseWriter) { settleAlreadySettled(w, testUpdateID) },
			store: func() Store {
				store := NewMemoryStore()
				_, _ = store.Claim(context.Background(), testUpdateID, "other-instance", recordTTL)
				_ = store.MarkDelivered(context.Background(), testUpdateID, "other-instance", recordTTL)
				return store
			},
			want: 402,
		},
		"claim aborted": {
			store: func() Store {
				store := NewMemoryStore()
				_, _ = store.Claim(context.Background(), testUpdateID, "other-instance", recordTTL)
				_ = store.MarkDelivered(context.Background(), testUpdateID, "other-instance", recordTTL)
				return store
			},
			want: 402,
		},
		"store failure": {
			store: func() Store {
				store := newFaultyStore(NewMemoryStore())
				store.breakOp("claim", true)
				return store
			},
			want: 503,
		},
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			f := startMock(t)
			if scenario.settle != nil {
				f.settle = scenario.settle
			}
			var hits atomic.Int32
			h := newGate(t, f, leakyHandler(&hits), func(c *Config) {
				if scenario.store != nil {
					c.Store = scenario.store()
				}
			})
			w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
			require.Equal(t, scenario.want, w.Code)
			assert.Equal(t, int32(1), hits.Load(), "the handler did run")
			assertOnlyGateHeaders(t, w, name)
			assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
			assert.Equal(t, contentTypeJSON, w.Header().Get(headerContentType))
		})
	}
}

func TestRedirectAnswerCarriesNoHandlerHeaders(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "access=paid")
		w.Header().Set("Location", "/x")
		w.WriteHeader(302)
	})
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	require.Equal(t, 502, w.Code)
	assertOnlyGateHeaders(t, w, "redirect")
}

func TestGateErrorBeforeTheHandlerKeepsOnlyGateHeaders(t *testing.T) {
	f := startMock(t)
	f.verify = func(_ int32, w http.ResponseWriter) { reply(w, 503, map[string]string{"error": "scan_busy"}) }
	h := newGate(t, f, leakyHandler(new(atomic.Int32)))
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	require.Equal(t, 503, w.Code)
	assertOnlyGateHeaders(t, w, "verify outage")
}

func TestUpstreamMiddlewareHeadersSurviveGateAnswers(t *testing.T) {
	// A CORS or request-id middleware in front of the gate is not the paid
	// handler: its headers stay on a gate answer.
	f := startMock(t)
	f.verify = func(_ int32, w http.ResponseWriter) { reply(w, 503, map[string]string{"error": "scan_busy"}) }
	inner := newGate(t, f, okHandler(new(atomic.Int32)))
	sig := signatureFor(t, inner, testPreparedTx)
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		inner.ServeHTTP(w, r)
	})
	w := do(outer, "GET", "/paid", sig)
	assert.Equal(t, 503, w.Code)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestScrubKeepsOnlyTheAllowList(t *testing.T) {
	header := http.Header{}
	for _, name := range []string{"PAYMENT-RESPONSE", "payment-required", "Retry-After", "cache-control", "content-type", "Set-Cookie", "Content-Length", "X-A"} {
		header.Set(name, "v")
	}
	scrubToGateHeaders(header)
	assert.Len(t, header, 6)
	assert.Empty(t, header.Get("Set-Cookie"))
}

// Rule 14: hooks come from an FTP type that embeds the mirror scheme.

func TestFTPSchemeEmbedsTheMirrorAndProvidesTheHooks(t *testing.T) {
	scheme := newFTPScheme(newLedger(NewMemoryStore(), nopLogger()), nopLogger())
	assert.Equal(t, "exact", scheme.Scheme())
	assert.Equal(t, "transfer-factory", scheme.DefaultAssetTransferMethod())
	assert.Equal(t, []string{"feePayer", "synchronizerId"}, scheme.DynamicExtraFields())
	assert.NotNil(t, scheme.ExactCantonScheme)

	var asServer x402.SchemeNetworkServer = scheme
	_, isBeforeVerify := asServer.(x402.BeforeVerifyHookProvider)
	_, isAfterVerify := asServer.(x402.AfterVerifyHookProvider)
	_, isAfterSettle := asServer.(x402.AfterSettleHookProvider)
	_, isOnSettleFailure := asServer.(x402.OnSettleFailureHookProvider)
	assert.True(t, isBeforeVerify && isAfterVerify && isAfterSettle && isOnSettleFailure,
		"Register finds the hooks by type assertion and attaches them to this scheme's network only")
}

func TestRoutesNeverCarryAPaymentFlow(t *testing.T) {
	conf, err := Config{Routes: map[string]Route{"GET /a": validRoute(), "POST /b": validRoute()}}.resolve()
	require.NoError(t, err)
	for key, route := range sdkRoutes(conf.routes) {
		extra := route.Accepts[0].Price.(x402.AssetAmount).Extra
		assert.NotContains(t, extra, "paymentFlow", key)
	}
}
