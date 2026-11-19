package canton

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rule 4: only the authorization flow. Rule 5: handler status decides settle.

func TestSettleRunsAfterTheHandlerNotBefore(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		f.record("handler")
		_, _ = w.Write([]byte(`{"data":42}`))
	})
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	require.Equal(t, 200, w.Code)
	assert.Equal(t, []string{"verify", "handler", "settle"}, f.events())
	assert.JSONEq(t, `{"data":42}`, w.Body.String())
	receipt := paymentResponse(t, w)
	assert.Equal(t, true, receipt["success"])
	assert.Equal(t, testUpdateID, receipt["transaction"])
	assert.Equal(t, testPayer, receipt["payer"])
	assert.Equal(t, testNetworkName, receipt["network"])
}

func TestEveryHandlerStatusBelow300IsSettled(t *testing.T) {
	for _, status := range []int{200, 201, 202, 204} {
		f := startMock(t)
		h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
		assert.Equal(t, status, w.Code)
		assert.Equal(t, int32(1), f.settleCalls.Load(), "status %d", status)
	}
}

func TestHandlerErrorsAreNotSettledAndPassThroughUnchanged(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 429, 500, 502, 503} {
		f := startMock(t)
		h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Handler", "kept")
			w.Header().Set("Retry-After", "7")
			w.Header().Set("Set-Cookie", "a=b")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("handler says no"))
		})
		w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
		assert.Equal(t, status, w.Code)
		assert.Equal(t, "handler says no", w.Body.String(), "status %d body passes through", status)
		assert.Equal(t, "kept", w.Header().Get("X-Handler"))
		assert.Equal(t, "7", w.Header().Get("Retry-After"))
		assert.Equal(t, "a=b", w.Header().Get("Set-Cookie"))
		assert.Equal(t, int32(0), f.settleCalls.Load(), "status %d must not be settled", status)
		assert.Empty(t, w.Header().Get(headerPaymentResponse))
	}
}

func TestHandlerPanicIsNotSettledAndReleasesTheLock(t *testing.T) {
	f := startMock(t)
	var calls atomic.Int32
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		_, _ = w.Write([]byte("ok"))
	})
	sig := signatureFor(t, h, testPreparedTx)
	require.Panics(t, func() { do(h, "GET", "/paid", sig) })
	assert.Equal(t, int32(0), f.settleCalls.Load())
	w := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, w.Code, "the payment is not stuck behind a leaked lock")
	assert.Equal(t, int32(1), f.settleCalls.Load())
}

func TestRedirectFromAPaidHandlerIsNotChargedAndBecomes502(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		f := startMock(t)
		h := newGate(t, f, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/elsewhere")
			w.Header().Set("Set-Cookie", "session=secret")
			w.WriteHeader(status)
		})
		w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
		assert.Equal(t, 502, w.Code, "status %d", status)
		assert.Equal(t, int32(0), f.settleCalls.Load())
		assert.Equal(t, msgRedirectNotCharged, errorMessage(t, w))
		assert.Empty(t, w.Header().Get("Location"), "the handler's redirect does not leak")
		assert.Empty(t, w.Header().Get("Set-Cookie"))
		assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
	}
}

func TestRedirectIsSettledWhenSettleOnRedirectIsSet(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(302)
	}, func(c *Config) { c.SettleOnRedirect = true })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 302, w.Code)
	assert.Equal(t, "/elsewhere", w.Header().Get("Location"))
	assert.Equal(t, int32(1), f.settleCalls.Load())
}

// Rule 11: payer for the handler.

func TestHandlerSeesTheProvenPayerAndCanRefuseWithoutCharging(t *testing.T) {
	f := startMock(t)
	var seen string
	var calls atomic.Int32
	h := newGate(t, f, func(w http.ResponseWriter, r *http.Request) {
		payer, ok := PayerFrom(r.Context())
		require.True(t, ok)
		seen = payer
		if calls.Add(1) > 1 { // a per-payer limiter: the second call is over the limit
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	first := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	second := do(h, "GET", "/paid", signatureFor(t, h, "ffff"+testPreparedTx[4:]))
	assert.Equal(t, testPayer, seen)
	assert.Equal(t, 200, first.Code)
	assert.Equal(t, 429, second.Code)
	assert.Equal(t, int32(1), f.settleCalls.Load(), "the 429 was not settled")
}

func TestPayerFromOutsideAPaidRequestIsEmpty(t *testing.T) {
	payer, ok := PayerFrom(t.Context())
	assert.False(t, ok)
	assert.Empty(t, payer)
}

// Rule 13: caching.

func TestPaidResponsesAreCachePrivateMergedWithTheHandlersValue(t *testing.T) {
	cases := map[string]string{"": "private", "max-age=60": "max-age=60, private", "no-cache, private": "no-cache, private"}
	for handlerValue, want := range cases {
		f := startMock(t)
		h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
			if handlerValue != "" {
				w.Header().Set(headerCacheControl, handlerValue)
			}
			_, _ = w.Write([]byte("ok"))
		})
		w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
		assert.Equal(t, want, w.Header().Get(headerCacheControl), "handler value %q", handlerValue)
	}
}

func TestPaidResponseKeepsTheHandlersHeaders(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "abc")
		w.Header().Set("Set-Cookie", "session=1")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, "abc", w.Header().Get("X-Request-Id"))
	assert.Equal(t, "session=1", w.Header().Get("Set-Cookie"))
	assert.Equal(t, "text/plain", w.Header().Get("Content-Type"))
	assert.NotEmpty(t, w.Header().Get(headerPaymentResponse))
}

func TestUnpaidRoutesPassThroughUntouched(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Free", "1")
		_, _ = w.Write([]byte("free " + r.URL.Path))
	})
	w := do(h, "GET", "/free", "")
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "free /free", w.Body.String())
	assert.Equal(t, "1", w.Header().Get("X-Free"))
	assert.Equal(t, 200, do(h, "OPTIONS", "/paid", "").Code, "CORS preflight is not gated")
	assert.Equal(t, int32(0), f.verifyCalls.Load())
}
