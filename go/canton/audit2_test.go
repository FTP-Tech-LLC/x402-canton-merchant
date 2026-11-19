package canton

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402http "github.com/x402-foundation/x402/go/v2/http"
)

// Regression tests for the round-2 audits.

// realGate runs the gate behind a real net/http server, so writes fail the way
// they do in production.
func realGate(t *testing.T, f *mockFacilitator, handler http.Handler, mutate ...func(*Config)) (*httptest.Server, http.Handler) {
	t.Helper()
	cfg := Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 2 * time.Second, Logger: nopLogger()}
	for _, change := range mutate {
		change(&cfg)
	}
	mw, err := NewMiddleware(context.Background(), cfg)
	require.NoError(t, err)
	gate := mw(handler)
	server := httptest.NewServer(gate)
	t.Cleanup(server.Close)
	return server, gate
}

func sendReal(t *testing.T, server *httptest.Server, method, sig string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, server.URL+"/paid", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	if sig != "" {
		req.Header.Set(headerPaymentSig, sig)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error(), nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func receiptSuccess(t *testing.T, header http.Header) bool {
	t.Helper()
	if header == nil || header.Get(headerPaymentResponse) == "" {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(header.Get(headerPaymentResponse))
	require.NoError(t, err)
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(raw, &receipt))
	return receipt["success"] == true
}

// ---------------------------------------------------------------- 1: memory store admission

func TestFullMemoryStoreRefusesBeforeSettleInsteadOfChargingAndWithholding(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	// Bound 4 with 2 live entries: no room for a payment's lock, record and claim.
	store := NewMemoryStore(WithMaxEntries(4))
	for i := 0; i < 2; i++ {
		_, _ = store.Claim(context.Background(), fmt.Sprintf("old%d", i), "t", recordTTL)
	}
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 503, w.Code)
	assert.JSONEq(t, `{"error":"payment state unavailable: nothing was charged; retry the same payment"}`, w.Body.String())
	assert.Equal(t, int32(0), f.settleCalls.Load(), "nothing was settled")
	assert.Equal(t, int32(0), hits.Load())
	assert.Equal(t, int32(0), f.verifyCalls.Load())
}

func TestMemoryStoreWithRoomForOnePaymentDeliversIt(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	store := NewMemoryStore(WithMaxEntries(5))
	for i := 0; i < 2; i++ {
		_, _ = store.Claim(context.Background(), fmt.Sprintf("old%d", i), "t", recordTTL)
	}
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "room for lock, record and claim: charged and served")
	assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
	assert.Equal(t, int32(1), f.settleCalls.Load())
}

func TestSteadyStateOnAFullStoreNeverChargesWithoutDelivering(t *testing.T) {
	f := startMock(t)
	store := NewMemoryStore(WithMaxEntries(60))
	var served, charged atomic.Int32
	f.settle = func(_ int32, w http.ResponseWriter) { charged.Add(1); settleOK(w) }
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) { served.Add(1); _, _ = w.Write([]byte("ok")) },
		func(c *Config) { c.Store = store; c.SettleCalls = 1 })
	// Each payment is a fresh hash and a fresh updateId, until the store is full.
	for i := 0; i < 40; i++ {
		hash := fmt.Sprintf("%064x", i+1)
		f.settle = func(_ int32, w http.ResponseWriter) {
			charged.Add(1)
			reply(w, 200, map[string]any{"success": true, "transaction": "u" + hash, "network": testNetworkName, "payer": testPayer})
		}
		w := do(h, "GET", "/paid", signatureFor(t, h, hash))
		assert.Contains(t, []int{200, 503}, w.Code)
	}
	assert.Equal(t, charged.Load(), served.Load(), "every payment that was settled was delivered")
	assert.Positive(t, served.Load())
}

// ---------------------------------------------------------------- 2: 204 and 304 with a body

func TestNoContentWithABodyKeepsTheClaim(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	server, _ := realGate(t, f, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
		_, _ = w.Write([]byte(`{"ok":true}`)) // a common slip: net/http drops it with ErrBodyNotAllowed
	}))
	sig := signatureWithPayload(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forward(server, w, r)
	}), "GET", "/paid", payloadJSON(testPreparedTx))
	deliveries := 0
	var trail []string
	for i := 0; i < 4; i++ {
		code, body, header := sendReal(t, server, "GET", sig)
		if receiptSuccess(t, header) {
			deliveries++
		}
		trail = append(trail, fmt.Sprintf("%d %q", code, body))
	}
	assert.Equal(t, 1, deliveries, "one payment, one delivery: %v", trail)
	assert.Equal(t, int32(1), hits.Load(), "replays do not run the handler")
}

// forward sends a request to a real test server and copies the answer, so tests
// that need an http.Handler can use a real server.
func forward(server *httptest.Server, w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), r.Method, server.URL+r.URL.RequestURI(), nil)
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.WriteHeader(599)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func TestNotModifiedWithABodyKeepsTheClaimWhenSettled(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	server, _ := realGate(t, f, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotModified)
		_, _ = w.Write([]byte("body"))
	}), func(c *Config) { c.SettleOnRedirect = true })
	sig := signatureWithPayload(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forward(server, w, r) }), "GET", "/paid", payloadJSON(testPreparedTx))
	first, _, _ := sendReal(t, server, "GET", sig)
	second, _, _ := sendReal(t, server, "GET", sig)
	assert.Equal(t, 304, first)
	assert.Equal(t, 402, second)
	assert.Equal(t, int32(1), hits.Load())
}

func TestEmptyBodyIsDroppedForNoContent(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "11")
		w.WriteHeader(204)
		_, _ = w.Write([]byte("hello world"))
	})
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 204, w.Code)
	assert.Empty(t, w.Body.String())
	assert.Empty(t, w.Header().Get("Content-Length"))
}

func TestOnlyANoBodyAnswerKeepsTheClaimAfterAWriteError(t *testing.T) {
	assert.True(t, reachedClient(http.ErrBodyNotAllowed))
	assert.False(t, reachedClient(http.ErrContentLength), "a body net/http refused did not reach the client")
	assert.False(t, reachedClient(io.ErrClosedPipe))
	assert.False(t, reachedClient(nil))
}

// ---------------------------------------------------------------- 3: Store outage that fails reads

func TestStoreReadFailureKeepsTheRecordInProcess(t *testing.T) {
	for _, outcome := range []string{"unknown", "success"} {
		t.Run(outcome, func(t *testing.T) {
			f := startMock(t)
			store := newFaultyStore(NewMemoryStore())
			f.settle = func(n int32, w http.ResponseWriter) {
				switch {
				case outcome == "success" && n == 1:
					settleOK(w)
				case outcome == "unknown" && n <= 4:
					w.WriteHeader(502)
				default:
					settleAlreadySettled(w, testUpdateID)
				}
			}
			var hits atomic.Int32
			h := newGate(t, f, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if hits.Add(1) == 1 { // the blip starts while the handler runs
					for _, op := range []string{"record", "put", "claim", "claimant"} {
						store.breakOp(op, true)
					}
				}
				_, _ = w.Write([]byte(`{"data":42}`))
			}), func(c *Config) { c.Store = store })
			sig := signatureFor(t, h, testPreparedTx)
			require.Equal(t, 503, do(h, "GET", "/paid", sig).Code)

			for _, op := range []string{"record", "put", "claim", "claimant"} {
				store.breakOp(op, false)
			}
			f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
			retry := do(h, "GET", "/paid", sig)
			assert.Equal(t, 200, retry.Code, "the retry reaches /settle instead of a 402 expired")
		})
	}
}

func TestStaleFallbackAttemptDoesNotHideADeliveredPayment(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	led := newLedger(store, nopLogger())
	led.toFallback(ctx, "h", Record{State: StateAttempt, Digest: "d"})
	require.NoError(t, store.PutRecord(ctx, "h", Record{State: StateSettled, UpdateID: "u", Digest: "d"}, recordTTL))
	got, found, err := led.lookup(ctx, "h")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, StateSettled, got.State, "the Store's settled record beats the in-process attempt")
	_, still := led.fromFallback("h")
	assert.False(t, still, "and the stale entry is dropped")

	led.toFallback(ctx, "h2", Record{State: StateSettled, UpdateID: "u2", Digest: "d"})
	require.NoError(t, store.PutRecord(ctx, "h2", Record{State: StateAttempt, Digest: "d"}, recordTTL))
	got, _, _ = led.lookup(ctx, "h2")
	assert.Equal(t, StateSettled, got.State, "a settled record in process beats the Store's attempt")
}

func TestStoreErrorOnTheRetryOfASettledPaymentDoesNotSayNothingWasSettled(t *testing.T) {
	f := startMock(t)
	ctx, leave := context.WithCancel(context.Background())
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			leave()
		}
		settleOK(w)
	}
	store := newFaultyStore(NewMemoryStore())
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 503, doCtx(ctx, h, "GET", "/paid", sig).Code)
	store.breakOp("lock", true)
	w := do(h, "GET", "/paid", sig)
	assert.Equal(t, 503, w.Code)
	assert.NotContains(t, w.Body.String(), "nothing was settled")
	assert.Contains(t, w.Body.String(), "do NOT pay again")
	assert.Equal(t, testUpdateID, paymentResponse(t, w)["transaction"], "the updateId the gate holds")
}

// ---------------------------------------------------------------- 4: lost claim reply with a failed read-back

func TestLostClaimReplyWithAFailedReadBackLeavesNothingBehind(t *testing.T) {
	for _, unclaimWorks := range []bool{true, false} {
		t.Run(fmt.Sprintf("unclaim works=%v", unclaimWorks), func(t *testing.T) {
			f := startMock(t)
			redis := newFakeRedis()
			store := newFaultyStore(NewRedisStore(redis, "x:"))
			f.settle = func(n int32, w http.ResponseWriter) {
				if n == 1 {
					redis.loseNextReplies(1) // the claim's SET NX applies, the reply is lost
					store.breakOp("claimant", true)
					if !unclaimWorks {
						store.breakOp("unclaim", true)
					}
					settleOK(w)
					return
				}
				settleAlreadySettled(w, testUpdateID)
			}
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
			sig := signatureFor(t, h, testPreparedTx)
			first := do(h, "GET", "/paid", sig)
			require.Equal(t, 503, first.Code)
			assert.NotContains(t, first.Body.String(), "data")

			store.breakOp("claimant", false) // the blip is over
			store.breakOp("unclaim", false)
			retry := do(h, "GET", "/paid", sig)
			assert.Equal(t, 200, retry.Code, "nothing was delivered: the retry is served once")
			assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
			assert.Equal(t, int32(2), hits.Load())
		})
	}
}

func TestClaimRetryAfterADelayedWriteRecognisesItsOwnToken(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	f.settle = func(_ int32, w http.ResponseWriter) {
		redis.set(func(r *fakeRedis) { r.delayNext = true }) // the claim's first SET NX errors and lands late
		settleOK(w)
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 200, w.Code, "the second SET NX says taken: the holder is us")
	assert.Equal(t, int32(1), hits.Load())
}

func TestClaimReadBackNeverAcceptsAForeignHolder(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	redis.set(func(r *fakeRedis) {
		r.errorOnTaken = true
		r.values["x:claimed:"+testUpdateID] = fakeRedisValue{value: "delivered:another-instance", expires: time.Now().Add(time.Hour)}
	})
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 402, w.Code)
	assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, w.Body.String())
	assert.NotContains(t, w.Body.String(), "data")
}

func TestLockReadBackNeverAcceptsAForeignHolder(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	redis.set(func(r *fakeRedis) {
		r.errorOnTaken = true
		r.values["x:lock:"+testPreparedTx] = fakeRedisValue{value: "another-request", expires: time.Now().Add(time.Hour)}
	})
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 503, w.Code)
	assert.JSONEq(t, `{"error":"payment in progress: retry the same payment"}`, w.Body.String())
	assert.Equal(t, int32(0), hits.Load())
}

// lostReplyLockStore applies the first real Lock and then answers with a result a
// client library gives when the reply was lost: an error, or (after a re-send)
// "exists".
type lostReplyLockStore struct {
	Store
	answer func() (bool, error)
	used   atomic.Bool
}

func (s *lostReplyLockStore) Lock(ctx context.Context, hash, token string, ttl time.Duration) (bool, error) {
	acquired, err := s.Store.Lock(ctx, hash, token, ttl)
	if hash == testPreparedTx && s.used.CompareAndSwap(false, true) {
		return s.answer()
	}
	return acquired, err
}

func TestLockAppliedWithReplyAndReadBackLostIsReleased(t *testing.T) {
	f := startMock(t)
	inner := NewMemoryStore()
	store := newFaultyStore(&lostReplyLockStore{Store: inner, answer: func() (bool, error) { return false, errStoreDown }})
	store.breakOp("lockholder", true)
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	assert.Equal(t, 503, do(h, "GET", "/paid", sig).Code)
	_, held, _ := inner.LockHolder(context.Background(), testPreparedTx)
	assert.False(t, held, "the applied lock was released: it does not block the retry")
	store.breakOp("lockholder", false)
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
}

// A re-sent write whose first send was applied answers "exists" for our own lock:
// the gate reads the holder back and carries on, for every Store.
func TestLockExistsForOurOwnTokenIsOurs(t *testing.T) {
	for name, answer := range map[string]func() (bool, error){
		"first send lost, re-send says exists": func() (bool, error) { return false, nil },
		"first send lost, error":               func() (bool, error) { return false, errStoreDown },
	} {
		t.Run(name, func(t *testing.T) {
			f := startMock(t)
			store := &lostReplyLockStore{Store: NewMemoryStore(), answer: answer}
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
			sig := signatureFor(t, h, testPreparedTx)
			assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "the payment is not made to wait for its own lock")
			assert.Equal(t, int32(1), hits.Load())
		})
	}
}

func TestTransientLockErrorWithAnAbsentKeyIsRetried(t *testing.T) {
	f := startMock(t)
	store := newFaultyStore(NewMemoryStore())
	store.failNext("lock", 1)
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	assert.Equal(t, 200, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code)
}

// ---------------------------------------------------------------- 5: settled record answers without /settle

func TestSettledRecordIsDeliveredEvenWhenTheFacilitatorForgot(t *testing.T) {
	for _, reason := range []string{"invalid_exact_canton_expired", "invalid_exact_canton_input_contention"} {
		t.Run(reason, func(t *testing.T) {
			f := startMock(t)
			ctx, leave := context.WithCancel(context.Background())
			f.settle = func(n int32, w http.ResponseWriter) {
				if n == 1 {
					leave()
					settleOK(w)
					return
				}
				settleRefused(w, reason) // the facilitator's record is gone
			}
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits))
			sig := signatureFor(t, h, testPreparedTx)
			require.Equal(t, 503, doCtx(ctx, h, "GET", "/paid", sig).Code)
			settles := f.settleCalls.Load()

			retry := do(h, "GET", "/paid", sig)
			assert.Equal(t, 200, retry.Code, "the gate's own record proves the payment moved")
			assert.Equal(t, testUpdateID, paymentResponse(t, retry)["transaction"])
			assert.Equal(t, settles, f.settleCalls.Load(), "/settle is not asked for a payment the gate holds as settled")
			assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code, "and it is delivered once")
		})
	}
}

// ---------------------------------------------------------------- 6: transport timeouts

func TestTransportResponseHeaderTimeoutDoesNotCapSettle(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond)
		settleOK(w)
	}
	transport := &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond}
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) {
		c.HTTPClient = &http.Client{Transport: transport}
		c.SettleTimeout = 2 * time.Second
	})
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, int32(1), f.settleCalls.Load(), "not cut and retried")
	assert.Equal(t, 100*time.Millisecond, transport.ResponseHeaderTimeout, "the merchant's transport is not mutated")
}

// ---------------------------------------------------------------- 7: browser paywall

func TestBrowserGetsATinyCantonPageNotTheEVMPaywall(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	r := httptest.NewRequest("GET", "/paid", nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	r.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Equal(t, 402, w.Code)
	assert.Less(t, w.Body.Len(), 2048, "a few hundred bytes, not 2.5 MB")
	assert.Contains(t, w.Body.String(), "0.01 CC", "the right Canton amount")
	assert.NotContains(t, w.Body.String(), "USDC")
	assert.NotContains(t, strings.ToLower(w.Body.String()), "eip155")
	assert.NotEmpty(t, w.Header().Get(headerPaymentRequired), "the offer is still in the header")
}

func TestDisplayAmount(t *testing.T) {
	for atomic, want := range map[string]string{"100000000": "0.01", "10000000000": "1", "15000000000": "1.5", "1": "0.0000000001", "123456": "0.0000123456", "x": "x", "": ""} {
		assert.Equal(t, want, displayAmount(atomic), atomic)
	}
}

// ---------------------------------------------------------------- 8: handler 4xx on a retry

func TestHandler4xxOnTheRetryOfASettledPaymentDoesNotHideTheCharge(t *testing.T) {
	f := startMock(t)
	ctx, leave := context.WithCancel(context.Background())
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			leave()
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	var calls atomic.Int32
	var retryFlags []bool
	var mu sync.Mutex
	h := newGate(t, f, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		retryFlags = append(retryFlags, IsRetry(r.Context()))
		mu.Unlock()
		if n := calls.Add(1); n == 2 { // a per-payer limiter that counts every handler run
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 503, doCtx(ctx, h, "GET", "/paid", sig).Code)

	limited := do(h, "GET", "/paid", sig)
	assert.Equal(t, 503, limited.Code, "a bare 429 would hide that this payment was charged")
	assert.Contains(t, limited.Body.String(), "do NOT pay again")
	receipt := paymentResponse(t, limited)
	assert.Equal(t, "settlement_pending", receipt["errorReason"])
	assert.Equal(t, testUpdateID, receipt["transaction"])

	served := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, served.Code, "the record was kept: the next retry is served once")
	assert.Equal(t, []bool{false, true, true}, retryFlags)
}

func TestFreshPaymentStillPassesAHandler4xxThrough(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) })
	assert.Equal(t, 429, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code)
	assert.Equal(t, int32(0), f.settleCalls.Load())
}

func TestIsRetryOutsideAPaidRequestIsFalse(t *testing.T) {
	assert.False(t, IsRetry(context.Background()))
}

// ---------------------------------------------------------------- 9: lows

func TestHeadWithAPaymentIsRefusedAndSpendsNothing(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /paid", okHandler(&hits))
	h := newGate(t, f, mux.ServeHTTP)
	sig := signatureFor(t, h, testPreparedTx)
	assert.Equal(t, 402, do(h, "HEAD", "/paid", "").Code, "an unpaid HEAD is still priced")
	head := do(h, "HEAD", "/paid", sig)
	assert.Equal(t, 405, head.Code)
	assert.Equal(t, "GET", head.Header().Get("Allow"))
	assert.Equal(t, int32(0), f.settleCalls.Load())
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "the payment is still good for the GET")
}

func TestSuccessFalseWithoutAReasonIsUnknown(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) {
		reply(w, 200, map[string]any{"success": false, "transaction": "", "network": testNetworkName})
	}
	store := NewMemoryStore()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 503, w.Code, "no verdict: unknown")
	assert.Equal(t, int32(4), f.settleCalls.Load())
	record, found, _ := store.Record(context.Background(), testPreparedTx)
	assert.True(t, found, "the attempt is kept")
	assert.Equal(t, StateAttempt, record.State)
}

func TestGateFailsClosedWhenTheSDKCallsTheHandlerWithoutAVerifiedPayment(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	g, err := buildGate(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: nopLogger()})
	require.NoError(t, err)
	// A hook that grants free access makes the SDK call the paid handler with no payment.
	g.httpServer.OnProtectedRequest(func(context.Context, x402http.HTTPRequestContext, x402http.RouteConfig) (*x402http.ProtectedRequestHookResult, error) {
		return &x402http.ProtectedRequestHookResult{GrantAccess: true}, nil
	})
	h := g.wrap(okHandler(&hits))
	w := do(h, "GET", "/paid", "")
	assert.Equal(t, 502, w.Code)
	assert.JSONEq(t, `{"error":"payment not verified; nothing was charged"}`, w.Body.String())
	assert.Equal(t, int32(0), hits.Load(), "the paid handler never ran")
}

func TestStartupReadsSupportedOncePerAttemptAndHandsTheSDKTheValidatedAnswer(t *testing.T) {
	f := startMock(t)
	f.supported = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			reply(w, 200, supportedBody(nil))
			return
		}
		reply(w, 200, map[string]any{"kinds": []any{}}) // a glitch on any later read
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	assert.Equal(t, 402, do(h, "GET", "/paid", "").Code)
	assert.Equal(t, int32(1), f.supportedCalls.Load())
}

func TestHangingSupportedDoesNotOutliveSupportedWait(t *testing.T) {
	f := startMock(t)
	release := make(chan struct{})
	defer close(release)
	f.supported = func(_ int32, _ http.ResponseWriter) { <-release }
	started := time.Now()
	_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 200 * time.Millisecond, Logger: nopLogger()})
	require.Error(t, err)
	assert.Less(t, time.Since(started), 2*time.Second, "the attempt is bounded by the time left in SupportedWait, not the 10 s default")
}

func TestSDKStartupPathNeverExitsTheProcess(t *testing.T) {
	if os.Getenv("CANTON_STARTUP_CHILD") == "1" {
		t.Skip("the child runs TestStartupChildProcess")
	}
	// The child runs NewMiddleware against a /supported that drops feePayer after
	// the first read. If the plugin let the SDK initialize itself again, the SDK
	// would call os.Exit(1); the child must exit 0.
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestStartupChildProcess$")
	cmd.Env = append(os.Environ(), "CANTON_STARTUP_CHILD=1")
	out, err := cmd.CombinedOutput()
	assert.NoError(t, err, string(out))
}

func TestStartupChildProcess(t *testing.T) {
	if os.Getenv("CANTON_STARTUP_CHILD") != "1" {
		t.Skip("run by TestSDKStartupPathNeverExitsTheProcess")
	}
	f := startMock(t)
	f.supported = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			reply(w, 200, supportedBody(nil))
			return
		}
		body := supportedBody(nil)
		delete(body["kinds"].([]any)[0].(map[string]any)["extra"].(map[string]any), "feePayer")
		reply(w, 200, body)
	}
	_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: nopLogger()})
	require.NoError(t, err)
}

func TestHeadOfALooseSpellingIsRefused(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	assert.Equal(t, 404, serveRaw(h, "HEAD", "/PAID").Code)
}

func TestDrainReadsAtMostTheBound(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }, func(c *Config) {
		c.Routes = map[string]Route{"POST /paid": validRoute()}
		c.Routes["POST /paid"] = Route{PayTo: testPayTo, Price: CC("0.01")}
	})
	sig := signatureWithPayload(t, h, "POST", "/paid", payloadJSON(testPreparedTx))
	body := &countingBody{limit: 4 << 20}
	r := httptest.NewRequest("POST", "/paid", body)
	r.Header.Set(headerPaymentSig, sig)
	h.ServeHTTP(httptest.NewRecorder(), r)
	assert.Equal(t, int64(drainBound), body.read.Load(), "a streamed body cannot hold the request open past the bound")
}

type countingBody struct {
	limit int64
	read  atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.read.Load() >= b.limit {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.limit-b.read.Load())
	b.read.Add(n)
	return int(n), nil
}

func TestPayerLeftOnTheRetryOfAnAttemptedPaymentSaysDoNotPayAgain(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { w.WriteHeader(502) }
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.SettleCalls = 1 })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 503, do(h, "GET", "/paid", sig).Code) // an attempt is recorded
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := doCtx(ctx, h, "GET", "/paid", sig)
	assert.Equal(t, 503, w.Code)
	assert.Contains(t, w.Body.String(), "do NOT pay again")
}

func TestClearAttemptNeverDeletesAnotherPaymentsRecord(t *testing.T) {
	ctx := context.Background()
	led := newLedger(NewMemoryStore(), nopLogger())
	scheme := newFTPScheme(led, nopLogger())
	payload, requirements := []byte(`{"p":1}`), []byte(`{"r":1}`)
	mine := paymentDigest(payload, requirements)
	require.NoError(t, led.write(ctx, "h", Record{State: StateSettled, UpdateID: "u", Digest: mine}))
	state := &requestState{requestCtx: ctx, hasIdentity: true, identity: paymentIdentity{canonicalHash: "h"}}
	scheme.clearAttempt(ctx, state, payload, requirements)
	_, found, _ := led.lookup(ctx, "h")
	assert.True(t, found, "a settled record is not an attempt")
	require.NoError(t, led.write(ctx, "h", Record{State: StateAttempt, Digest: "other"}))
	scheme.clearAttempt(ctx, state, payload, requirements)
	_, found, _ = led.lookup(ctx, "h")
	assert.True(t, found, "an attempt of other bytes is not mine to clear")
	require.NoError(t, led.write(ctx, "h", Record{State: StateAttempt, Digest: mine}))
	scheme.clearAttempt(ctx, state, payload, requirements)
	_, found, _ = led.lookup(ctx, "h")
	assert.False(t, found)
}

// ---------------------------------------------------------------- test-suite gaps the audit named

func TestClaimsAndRecordsGetTheFullTTLThroughTheGate(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	require.Equal(t, 200, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code)
	redis.mu.Lock()
	defer redis.mu.Unlock()
	assert.Equal(t, recordTTL, redis.ttls["x:claimed:"+testUpdateID])
	assert.Equal(t, recordTTL, redis.ttls["x:record:"+testPreparedTx])
}

func TestStoreCallsSurviveTheSDKDeadline(t *testing.T) {
	// The fake Redis honours its context like go-redis: a settled write that ran
	// on the request context would fail once the SDK's Timeout has passed.
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond)
		settleOK(w)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) {
		c.Store = NewRedisStore(newFakeRedis(), "x:")
		c.Timeout = 100 * time.Millisecond
	})
	assert.Equal(t, 200, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code)
}

// gatedStore blocks its first Lock call until released.
type gatedStore struct {
	Store
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (s *gatedStore) Lock(ctx context.Context, hash, token string, ttl time.Duration) (bool, error) {
	if hash == testPreparedTx && s.calls.Add(1) == 1 { // not the startup probe
		close(s.entered)
		<-s.release
	}
	return s.Store.Lock(ctx, hash, token, ttl)
}

func TestADuplicateThatWinsTheLockLateRunsNoHandler(t *testing.T) {
	f := startMock(t)
	store := &gatedStore{Store: NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)

	late := make(chan *httptest.ResponseRecorder, 1)
	go func() { late <- do(h, "GET", "/paid", sig) }() // read the record (none), then blocks in Lock
	<-store.entered
	require.Equal(t, 200, do(h, "GET", "/paid", sig).Code) // the first request delivers and unlocks
	verifies := f.verifyCalls.Load()
	close(store.release)
	w := <-late
	assert.Equal(t, 402, w.Code, "the second look under the lock sees the delivery")
	assert.Equal(t, int32(1), hits.Load(), "one handler run")
	assert.Equal(t, verifies, f.verifyCalls.Load(), "and no second /verify")
}

func TestWrongContentLengthFromTheHandlerStillDeliversTheWholeBody(t *testing.T) {
	// The gate holds the whole body, so a wrong Content-Length of the handler's
	// own is dropped and net/http frames the real one.
	for _, declared := range []string{"2", "999"} {
		f := startMock(t)
		server, _ := realGate(t, f, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", declared)
			_, _ = w.Write([]byte("hello"))
		}))
		sig := signatureWithPayload(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forward(server, w, r) }), "GET", "/paid", payloadJSON(testPreparedTx))
		code, body, _ := sendReal(t, server, "GET", sig)
		assert.Equal(t, 200, code)
		assert.Equal(t, "hello", body, "declared %s", declared)
		again, _, _ := sendReal(t, server, "GET", sig)
		assert.Equal(t, 402, again)
	}
}
