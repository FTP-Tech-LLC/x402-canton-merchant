package canton

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rule 1: startup never exits the process; errors are values, /supported is retried.

func TestStartupRetriesSupportedUntilItAnswers(t *testing.T) {
	f := startMock(t)
	f.supported = func(n int32, w http.ResponseWriter) {
		if n <= 3 {
			reply(w, 500, map[string]string{"error": "boom"})
			return
		}
		reply(w, 200, supportedBody(nil))
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	assert.GreaterOrEqual(t, int(f.supportedCalls.Load()), 4)
	assert.Equal(t, 402, do(h, "GET", "/paid", "").Code)
}

func TestStartupReturnsErrorWhenFacilitatorStaysDown(t *testing.T) {
	f := startMock(t)
	f.supported = func(_ int32, w http.ResponseWriter) { reply(w, 503, map[string]string{"error": "down"}) }
	_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 50 * time.Millisecond})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/supported")
}

func TestStartupReturnsErrorWhenFacilitatorUnreachable(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: url, Routes: testRoutes(), SupportedWait: 50 * time.Millisecond})
	require.Error(t, err)
}

func TestStartupRefusesKindsThatCannotBuildACantonOffer(t *testing.T) {
	cases := map[string]map[string]any{
		"no kinds": {"kinds": []any{}},
		"wrong network": {"kinds": []any{map[string]any{"x402Version": 2, "scheme": "exact", "network": "canton:testnet",
			"extra": map[string]any{"feePayer": testFeePayer, "synchronizerId": testSyncID}}}},
		"wrong scheme": {"kinds": []any{map[string]any{"x402Version": 2, "scheme": "upto", "network": testNetworkName,
			"extra": map[string]any{"feePayer": testFeePayer, "synchronizerId": testSyncID}}}},
		"version 1": {"kinds": []any{map[string]any{"x402Version": 1, "scheme": "exact", "network": testNetworkName,
			"extra": map[string]any{"feePayer": testFeePayer, "synchronizerId": testSyncID}}}},
		"no feePayer": {"kinds": []any{map[string]any{"x402Version": 2, "scheme": "exact", "network": testNetworkName,
			"extra": map[string]any{"synchronizerId": testSyncID}}}},
		"no synchronizerId": {"kinds": []any{map[string]any{"x402Version": 2, "scheme": "exact", "network": testNetworkName,
			"extra": map[string]any{"feePayer": testFeePayer}}}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := startMock(t)
			f.supported = func(_ int32, w http.ResponseWriter) { reply(w, 200, body) }
			_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 30 * time.Millisecond})
			require.Error(t, err)
		})
	}
}

func TestStartupRunsSDKInitializationItself(t *testing.T) {
	// After NewMiddleware the SDK has its facilitator kinds: the first unpaid
	// request already carries feePayer, and a later /supported outage changes
	// nothing. The SDK's os.Exit path (background_init.go) is never reached
	// because the middleware is built with SyncFacilitatorOnStart off.
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	calls := f.supportedCalls.Load()
	f.supported = func(_ int32, w http.ResponseWriter) { reply(w, 500, nil) }
	w := do(h, "GET", "/paid", "")
	assert.Equal(t, 402, w.Code)
	var required map[string]any
	decodeHeader(t, w.Header().Get(headerPaymentRequired), &required)
	accepts := required["accepts"].([]any)[0].(map[string]any)
	assert.Equal(t, testFeePayer, accepts["extra"].(map[string]any)["feePayer"])
	assert.Equal(t, calls, f.supportedCalls.Load(), "no further /supported reads at request time")
}

func TestStartupBadConfigIsAnErrorNotAPanic(t *testing.T) {
	_, err := NewMiddleware(context.Background(), Config{})
	require.Error(t, err)
	_, err = NewMiddleware(context.Background(), Config{FacilitatorURL: "ftp://x", Routes: testRoutes()})
	require.Error(t, err)
}

func TestStartupCancelledContextStopsTheWait(t *testing.T) {
	f := startMock(t)
	f.supported = func(_ int32, w http.ResponseWriter) { reply(w, 503, nil) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewMiddleware(ctx, Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Minute})
	require.Error(t, err)
}
