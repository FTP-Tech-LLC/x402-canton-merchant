package canton

import (
	"bytes"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rule 7: settle retries live in the facilitator client wrapper.

func TestPendingThenSuccessIsDeliveredOnceWithIdenticalBytes(t *testing.T) {
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n <= 3 {
			settlePending(w, "")
			return
		}
		settleOK(w)
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.SettleCalls = 4 })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, int32(1), hits.Load(), "one delivery")
	assert.Equal(t, int32(4), f.settleCalls.Load())
	for _, body := range f.settleBodies[1:] {
		assert.True(t, bytes.Equal(f.settleBodies[0], body), "a retry sends the identical bytes")
	}
}

func TestEveryUnknownOutcomeKindIsRetriedThenSucceeds(t *testing.T) {
	kinds := map[string]func(w http.ResponseWriter){
		"pending 503": func(w http.ResponseWriter) { settlePending(w, "") },
		"pending in a 200": func(w http.ResponseWriter) {
			reply(w, 200, map[string]any{"success": false, "errorReason": "settlement_pending", "transaction": testPreparedTx, "network": testNetworkName})
		},
		"transport error":    dropConnection,
		"429":                func(w http.ResponseWriter) { reply(w, 429, map[string]string{"error": "rate_limited"}) },
		"bodyless 502":       func(w http.ResponseWriter) { w.WriteHeader(502) },
		"bodyless 504":       func(w http.ResponseWriter) { w.WriteHeader(504) },
		"503 without reason": func(w http.ResponseWriter) { reply(w, 503, map[string]string{"error": "scan_busy"}) },
		"500":                func(w http.ResponseWriter) { reply(w, 500, map[string]string{"error": "Internal Server Error"}) },
		"400 without reason": func(w http.ResponseWriter) { reply(w, 400, map[string]string{"error": "bad body"}) },
		"403 from a proxy":   func(w http.ResponseWriter) { w.WriteHeader(403) },
		"408":                func(w http.ResponseWriter) { w.WriteHeader(408) },
		"200 with junk body": func(w http.ResponseWriter) { _, _ = w.Write([]byte("<html>proxy</html>")) },
	}
	for name, failure := range kinds {
		t.Run(name, func(t *testing.T) {
			f := startMock(t)
			f.settle = func(n int32, w http.ResponseWriter) {
				if n == 1 {
					failure(w)
					return
				}
				settleOK(w)
			}
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits))
			w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
			assert.Equal(t, 200, w.Code)
			assert.Equal(t, int32(1), hits.Load())
			assert.Equal(t, int32(2), f.settleCalls.Load())
		})
	}
}

func TestRetryAfterIsHonouredAndCapped(t *testing.T) {
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			settlePending(w, "3") // the test cap is 60 ms, the real cap 5 s
			return
		}
		settleOK(w)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	require.Equal(t, 200, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code)
	gap := f.settleTimes[1].Sub(f.settleTimes[0])
	assert.GreaterOrEqual(t, gap, retryAfterCap-5*time.Millisecond, "waited for the capped Retry-After")
	assert.Less(t, gap, 2*time.Second, "did not wait the full 3 s")
}

func TestRetryAfterCapIsFiveSecondsInProduction(t *testing.T) {
	// TestMain shortens the cap for speed; the shipped value is the spec's 5 s
	// and the default backoff 1, 2, 4 s.
	assert.Equal(t, 60*time.Millisecond, retryAfterCap)
	assert.Equal(t, 5*time.Second, productionRetryAfterCap)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, productionSettleBackoff)
}

func TestWaitUsesBackoffWithoutRetryAfter(t *testing.T) {
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n <= 3 {
			settlePending(w, "")
			return
		}
		settleOK(w)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	require.Equal(t, 200, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code)
	for i, want := range settleBackoff {
		gap := f.settleTimes[i+1].Sub(f.settleTimes[i])
		assert.GreaterOrEqual(t, gap, want-time.Millisecond, "wait %d", i)
	}
}

func TestSettleBudgetCountsTheSDKsOwnRetry(t *testing.T) {
	for _, calls := range []int{1, 2, 3, 4, 6} {
		f := startMock(t)
		f.settle = func(_ int32, w http.ResponseWriter) { settlePending(w, "") }
		h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.SettleCalls = calls })
		w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
		assert.Equal(t, 503, w.Code)
		assert.Equal(t, int32(calls), f.settleCalls.Load(), "SettleCalls=%d is a hard cap, the SDK's immediate retry included", calls)
	}
}

func TestDefaultSettleBudgetIsFour(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { settlePending(w, "") }
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, int32(4), f.settleCalls.Load())
}

func TestBudgetIsPerRequestNotPerProcess(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { settlePending(w, "") }
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.SettleCalls = 2 })
	do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	do(h, "GET", "/paid", signatureFor(t, h, "11"+testPreparedTx[2:]))
	assert.Equal(t, int32(4), f.settleCalls.Load())
}

func TestDefiniteAnswersReturnImmediately(t *testing.T) {
	answers := map[string]func(w http.ResponseWriter){
		"insufficient balance": func(w http.ResponseWriter) { settleRefused(w, "invalid_exact_canton_insufficient_balance") },
		"expired":              func(w http.ResponseWriter) { settleRefused(w, "invalid_exact_canton_expired") },
		"execute failed":       func(w http.ResponseWriter) { settleRefused(w, "invalid_exact_canton_execute_failed") },
		"500 with reason": func(w http.ResponseWriter) {
			reply(w, 500, map[string]any{"success": false, "errorReason": "unexpected_canton_ledger_error", "network": testNetworkName})
		},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			f := startMock(t)
			f.settle = func(_ int32, w http.ResponseWriter) { answer(w) }
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits))
			w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
			assert.Equal(t, 402, w.Code, "a definite refusal stays a 402")
			assert.Equal(t, int32(1), f.settleCalls.Load(), "no retry of a definite answer")
			assert.JSONEq(t, `{"error":"settlement_failed"}`, w.Body.String())
			assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
		})
	}
}

func TestRefusalKeepsTheFacilitatorsReasonInPaymentResponse(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { settleRefused(w, "invalid_exact_canton_insufficient_balance") }
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	receipt := paymentResponse(t, w)
	assert.Equal(t, false, receipt["success"])
	assert.Equal(t, "invalid_exact_canton_insufficient_balance", receipt["errorReason"])
}

// Rule 6: settle runs on a detached context with its own timeout.

func TestSlowSettleOutlivesTheGateTimeout(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond)
		settleOK(w)
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Timeout = 100 * time.Millisecond })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 200, w.Code, "the SDK deadline must not cut an in-flight settle")
	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, testUpdateID, paymentResponse(t, w)["transaction"])
}

func TestSettleTimeoutAppliesPerCall(t *testing.T) {
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			time.Sleep(400 * time.Millisecond) // longer than SettleTimeout: the client gives up
			return
		}
		settleOK(w)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.SettleTimeout = 100 * time.Millisecond })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 200, w.Code, "a timed-out call is an unknown outcome: retried, then delivered")
	assert.Equal(t, int32(2), f.settleCalls.Load())
}

// Rule 8: unknown outcome after the budget.

func TestUnknownOutcomeAfterTheBudgetIs503DoNotPayAgain(t *testing.T) {
	failures := map[string]func(w http.ResponseWriter){
		"pending forever":  func(w http.ResponseWriter) { settlePending(w, "") },
		"transport errors": dropConnection,
		"429 forever":      func(w http.ResponseWriter) { reply(w, 429, map[string]string{"error": "rate_limited"}) },
		"bodyless 502":     func(w http.ResponseWriter) { w.WriteHeader(502) },
		"503 scan_busy":    func(w http.ResponseWriter) { reply(w, 503, map[string]string{"error": "scan_busy"}) },
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			f := startMock(t)
			f.settle = func(_ int32, w http.ResponseWriter) { failure(w) }
			logger, logs := capturingLogger()
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Logger = logger })
			w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))

			assert.Equal(t, 503, w.Code, "never a 402 here")
			assert.Equal(t, "3", w.Header().Get(headerRetryAfter))
			assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
			assert.JSONEq(t, `{"error":"payment outcome unknown: do NOT pay again; retry the same payment"}`, w.Body.String())
			receipt := paymentResponse(t, w)
			assert.Equal(t, false, receipt["success"])
			assert.Equal(t, "settlement_pending", receipt["errorReason"])
			assert.Equal(t, testPreparedTx, receipt["transaction"], "the prepared tx hash")
			assert.Equal(t, testNetworkName, receipt["network"])
			assert.True(t, logsContain(logs, "level=WARN", "settle outcome unknown", "canonicalHash="+testPreparedTx))
			assert.Equal(t, int32(4), f.settleCalls.Load())
			assert.Equal(t, int32(1), hits.Load(), "the handler ran once; the body was withheld")
			assert.NotContains(t, w.Body.String(), "data", "the paid body is not delivered")
		})
	}
}

func TestUnknownOutcomeUsesThePreparedHashWhenTheAnswerHasNone(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { w.WriteHeader(502) }
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	w := do(h, "GET", "/paid", signatureFor(t, h, "  1220"+strings.ToUpper(testPreparedTx[:60])+"  "))
	assert.Equal(t, 503, w.Code)
	assert.Equal(t, "1220"+testPreparedTx[:60], paymentResponse(t, w)["transaction"], "trimmed and lower-case, prefix kept")
}

func TestThePaidBodyIsNeverDeliveredOnAnyFailure(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { settleRefused(w, "invalid_exact_canton_insufficient_balance") }
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("SECRET-PAID-BODY")) })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.NotContains(t, w.Body.String(), "SECRET-PAID-BODY")
}

// Rule 9: /verify failures.

func TestVerifyOutagesAre503NotThe402(t *testing.T) {
	failures := map[string]func(w http.ResponseWriter){
		"429":             func(w http.ResponseWriter) { reply(w, 429, map[string]string{"error": "rate_limited"}) },
		"503 scan_busy":   func(w http.ResponseWriter) { reply(w, 503, map[string]string{"error": "scan_busy"}) },
		"500":             func(w http.ResponseWriter) { reply(w, 500, map[string]string{"error": "Internal Server Error"}) },
		"502 bodyless":    func(w http.ResponseWriter) { w.WriteHeader(502) },
		"transport error": dropConnection,
		"200 junk":        func(w http.ResponseWriter) { _, _ = w.Write([]byte("junk")) },
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			f := startMock(t)
			f.verify = func(_ int32, w http.ResponseWriter) { failure(w) }
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits))
			w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
			assert.Equal(t, 503, w.Code)
			assert.Equal(t, "3", w.Header().Get(headerRetryAfter))
			assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
			assert.JSONEq(t, `{"error":"verification unavailable: nothing was settled; retry the same payment"}`, w.Body.String())
			assert.Equal(t, int32(0), hits.Load())
			assert.Equal(t, int32(0), f.settleCalls.Load())
		})
	}
}

func TestVerifyTimeoutIs503(t *testing.T) {
	f := startMock(t)
	f.verify = func(_ int32, w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond)
		verifyOK(w)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Timeout = 80 * time.Millisecond })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 503, w.Code)
	assert.Equal(t, int32(0), f.settleCalls.Load())
}

func TestDefiniteVerifyRefusalStaysTheSDKs402(t *testing.T) {
	f := startMock(t)
	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 402, w.Code)
	assert.NotEmpty(t, w.Header().Get(headerPaymentRequired))
	assert.Equal(t, int32(0), hits.Load())
	assert.Equal(t, int32(0), f.settleCalls.Load())
}

func TestVerify400StaysA402(t *testing.T) {
	f := startMock(t)
	f.verify = func(_ int32, w http.ResponseWriter) { reply(w, 400, map[string]string{"error": "malformed"}) }
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	assert.Equal(t, 402, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code)
}

func TestVerifyOutageClearsAfterRecovery(t *testing.T) {
	f := startMock(t)
	f.verify = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			reply(w, 503, map[string]string{"error": "scan_busy"})
			return
		}
		verifyOK(w)
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)
	assert.Equal(t, 503, do(h, "GET", "/paid", sig).Code)
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "the same payment goes through after the outage")
	assert.Equal(t, int32(1), hits.Load())
}

// M6: after an unknown call only a parsed facilitator verdict ends the loop.

func TestBareClientErrorAfterAnUnknownCallIsStillUnknown(t *testing.T) {
	for _, status := range []int{400, 403, 404, 408, 413} {
		f := startMock(t)
		f.settle = func(n int32, w http.ResponseWriter) {
			if n == 1 {
				w.WriteHeader(502) // may have committed
				return
			}
			w.WriteHeader(status) // a proxy or WAF, not the facilitator
		}
		var hits atomic.Int32
		h := newGate(t, f, okHandler(&hits))
		w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
		assert.Equal(t, 503, w.Code, "status %d after an unknown call must not become a 402", status)
		assert.Equal(t, int32(4), f.settleCalls.Load())
		assert.Contains(t, w.Body.String(), "do NOT pay again")
	}
}

func TestParsedVerdictAfterAnUnknownCallIsDefinite(t *testing.T) {
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			w.WriteHeader(502)
			return
		}
		settleRefused(w, "invalid_exact_canton_insufficient_balance")
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 402, w.Code)
	assert.Equal(t, int32(2), f.settleCalls.Load())
}
