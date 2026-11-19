package canton

// Final diff-scoped probes for canton.Verified.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type seenByHandler struct {
	mu       sync.Mutex
	calls    int
	verified []bool
	payer    []string
	hasPayer []bool
	retry    []bool
}

func (s *seenByHandler) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls++
		s.verified = append(s.verified, Verified(r.Context()))
		p, ok := PayerFrom(r.Context())
		s.payer = append(s.payer, p)
		s.hasPayer = append(s.hasPayer, ok)
		s.retry = append(s.retry, IsRetry(r.Context()))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"data":42}`))
	}
}

func (s *seenByHandler) last() (bool, string, bool, bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == 0 {
		return false, "", false, false, 0
	}
	i := s.calls - 1
	return s.verified[i], s.payer[i], s.hasPayer[i], s.retry[i], s.calls
}

func TestFinalVerifiedExactness(t *testing.T) {
	routes := func(c *Config) {
		c.Routes = map[string]Route{"GET /paid": {PayTo: testPayTo, Price: CC("0.01")}}
	}

	t.Run("free route: false", func(t *testing.T) {
		f := startMock(t)
		var s seenByHandler
		h := newGate(t, f, s.handler(200), routes)
		w := do(h, "GET", "/free", "")
		require.Equal(t, 200, w.Code)
		v, _, _, _, n := s.last()
		require.Equal(t, 1, n)
		require.False(t, v)
	})

	t.Run("free route carrying a payment header: false, nothing verified", func(t *testing.T) {
		f := startMock(t)
		var s seenByHandler
		h := newGate(t, f, s.handler(200), routes)
		sig := signatureFor(t, h, testPreparedTx)
		for _, target := range []string{"/free", "/paid/x", "/paidx"} {
			w := do(h, "GET", target, sig)
			require.Equal(t, 200, w.Code, target)
			v, _, _, _, _ := s.last()
			require.False(t, v, target)
		}
		w := do(h, "OPTIONS", "/paid", sig)
		require.Equal(t, 200, w.Code)
		v, _, _, _, _ := s.last()
		require.False(t, v, "OPTIONS on a paid path with a payment is passed through unverified")
		require.Zero(t, f.verifyCalls.Load())
		require.Zero(t, f.settleCalls.Load())
	})

	t.Run("valid payment with payer: true", func(t *testing.T) {
		f := startMock(t)
		var s seenByHandler
		h := newGate(t, f, s.handler(200), routes)
		w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
		require.Equal(t, 200, w.Code, w.Body.String())
		v, p, ok, retry, n := s.last()
		require.Equal(t, 1, n)
		require.True(t, v)
		require.True(t, ok)
		require.Equal(t, testPayer, p)
		require.False(t, retry)
	})

	t.Run("valid payment without payer: true, PayerFrom false", func(t *testing.T) {
		f := startMock(t)
		f.verify = func(_ int32, w http.ResponseWriter) { reply(w, 200, map[string]any{"isValid": true}) }
		var s seenByHandler
		h := newGate(t, f, s.handler(200), routes)
		w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
		require.Equal(t, 200, w.Code, w.Body.String())
		v, p, ok, _, n := s.last()
		require.Equal(t, 1, n)
		require.True(t, v)
		require.False(t, ok)
		require.Empty(t, p)
		require.Equal(t, int32(1), f.settleCalls.Load())
	})

	t.Run("invalid payment, verify outage, unpaid, loose spelling: handler never sees the request", func(t *testing.T) {
		f := startMock(t)
		var s seenByHandler
		h := newGate(t, f, s.handler(200), routes)
		sig := signatureFor(t, h, testPreparedTx)
		f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_payload") }
		require.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
		f.verify = func(_ int32, w http.ResponseWriter) { reply(w, 500, map[string]any{"error": "down"}) }
		require.Equal(t, 503, do(h, "GET", "/paid", sig).Code)
		require.Equal(t, 402, do(h, "GET", "/paid", "").Code)
		require.Equal(t, 404, do(h, "GET", "/PAID", sig).Code)
		_, _, _, _, n := s.last()
		require.Zero(t, n)
		require.Zero(t, f.settleCalls.Load())
	})

	t.Run("recorded retry that skips verify: true and IsRetry", func(t *testing.T) {
		f := startMock(t)
		var s seenByHandler
		h := newGate(t, f, s.handler(200), routes)
		sig := signatureFor(t, h, testPreparedTx)
		f.settle = func(_ int32, w http.ResponseWriter) { settlePending(w, "") }
		w := do(h, "GET", "/paid", sig)
		require.Equal(t, 503, w.Code, w.Body.String())
		f.settle = func(_ int32, w http.ResponseWriter) { settleOK(w) }
		w = do(h, "GET", "/paid", sig)
		require.Equal(t, 200, w.Code, w.Body.String())
		v, _, _, retry, n := s.last()
		require.Equal(t, 2, n)
		require.True(t, v)
		require.True(t, retry)
		require.Equal(t, int32(1), f.verifyCalls.Load(), "the retry skipped /verify")
	})

	t.Run("the caller's own request never carries the state", func(t *testing.T) {
		f := startMock(t)
		var s seenByHandler
		h := newGate(t, f, s.handler(200), routes)
		sig := signatureFor(t, h, testPreparedTx)
		r := httptest.NewRequest("GET", "/paid", nil)
		r.Header.Set(headerPaymentSig, sig)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		require.Equal(t, 200, w.Code)
		require.False(t, Verified(r.Context()), "only the derived request inside the gate is verified")
		require.False(t, Verified(context.Background()))
	})

	t.Run("nested gates: an outer gate's verification is visible through an inner gate's pass-through", func(t *testing.T) {
		fA, fB := startMock(t), startMock(t)
		var s seenByHandler
		inner := newGate(t, fB, s.handler(200), func(c *Config) {
			c.Routes = map[string]Route{"GET /other": {PayTo: testPayTo, Price: CC("0.01")}}
		})
		outer := newGate(t, fA, inner.ServeHTTP, routes)
		w := do(outer, "GET", "/paid", signatureFor(t, outer, testPreparedTx))
		require.Equal(t, 200, w.Code, w.Body.String())
		v, _, _, _, _ := s.last()
		t.Logf("inner gate passed /paid through; Verified in the handler = %v (verified by the outer gate)", v)
		require.True(t, v)
		require.Zero(t, fB.verifyCalls.Load())
	})

	t.Run("handler status does not change what Verified said", func(t *testing.T) {
		f := startMock(t)
		var s seenByHandler
		h := newGate(t, f, s.handler(500), routes)
		w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
		require.Equal(t, 500, w.Code)
		v, _, _, _, _ := s.last()
		require.True(t, v)
		require.Zero(t, f.settleCalls.Load(), "a 5xx is never settled")
	})
}

// Verified is safe to call concurrently with the gate's own state writes.
func TestFinalVerifiedConcurrentReads(t *testing.T) {
	f := startMock(t)
	var reads atomic.Int32
	h := newGate(t, f, func(w http.ResponseWriter, r *http.Request) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { //nolint:contextcheck // test goroutine
				defer wg.Done()
				if Verified(r.Context()) {
					reads.Add(1)
				}
			}()
		}
		wg.Wait()
		_, _ = w.Write([]byte("ok"))
	})
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	require.Equal(t, 200, w.Code)
	require.Equal(t, int32(8), reads.Load())
}
