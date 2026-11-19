package canton

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rule 10: one delivery per payment.

func TestCanonicalHashSpellings(t *testing.T) {
	bare := strings.Repeat("ab", 32)
	cases := map[string]string{
		bare:                           bare,
		"1220" + bare:                  bare, // 68 characters: the multihash prefix is stripped
		"  1220" + bare + "\n":         bare,
		strings.ToUpper(bare):          bare,
		"1220" + strings.ToUpper(bare): bare,
		"1220abcd":                     "1220abcd",           // not 68 characters: kept
		"1220" + bare + "00":           "1220" + bare + "00", // 70 characters: kept
		"":                             "",
	}
	for input, want := range cases {
		assert.Equal(t, want, canonicalHash(input), "input %q", input)
	}
}

func TestParsePaymentHeader(t *testing.T) {
	identity, outcome := parsePaymentHeader(signatureForRaw(" 1220" + strings.ToUpper(testPreparedTx) + " "))
	require.Equal(t, headerReadable, outcome)
	assert.Equal(t, "1220"+testPreparedTx, identity.preparedTxHash)
	assert.Equal(t, testPreparedTx, identity.canonicalHash)
	for _, junk := range []string{"", "not base64!!", "e30=", "bnVsbA=="} {
		_, outcome := parsePaymentHeader(junk)
		assert.Equal(t, headerUnreadable, outcome, "junk %q goes to the SDK as is", junk)
	}
}

func TestDifferentSpellingsOfOneHashAreOnePayment(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	first := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	require.Equal(t, 200, first.Code)
	replay := do(h, "GET", "/paid", signatureFor(t, h, "1220"+strings.ToUpper(testPreparedTx)))
	assert.Equal(t, 402, replay.Code)
	assert.Equal(t, int32(1), hits.Load())
}

func TestReplayInsideTheDeadlineIsRefusedBeforeVerifyAndRunsNoHandler(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
	verifies, settles := f.verifyCalls.Load(), f.settleCalls.Load()

	w := do(h, "GET", "/paid", sig)
	assert.Equal(t, 402, w.Code)
	assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, w.Body.String())
	receipt := paymentResponse(t, w)
	assert.Equal(t, testUpdateID, receipt["transaction"], "the updateId is in PAYMENT-RESPONSE")
	assert.Equal(t, false, receipt["success"])
	assert.Equal(t, int32(1), hits.Load(), "the handler did not run again")
	assert.Equal(t, verifies, f.verifyCalls.Load(), "refused before /verify")
	assert.Equal(t, settles, f.settleCalls.Load())
	assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
}

func TestConcurrentDuplicatesRunOneHandlerAndOneSettle(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte(`{"data":42}`))
	})
	sig := signatureFor(t, h, testPreparedTx)

	results := make(chan int, 1)
	go func() { results <- do(h, "GET", "/paid", sig).Code }()
	<-entered

	const duplicates = 20
	var wg sync.WaitGroup
	codes := make([]int, duplicates)
	bodies := make([]string, duplicates)
	for i := 0; i < duplicates; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := do(h, "GET", "/paid", sig)
			codes[i], bodies[i] = w.Code, w.Body.String()
			if w.Code == 503 {
				assert.Equal(t, "3", w.Header().Get(headerRetryAfter))
			}
		}()
	}
	wg.Wait()
	close(release)
	assert.Equal(t, 200, <-results)
	for i := range codes {
		assert.Equal(t, 503, codes[i])
		assert.JSONEq(t, `{"error":"payment in progress: retry the same payment"}`, bodies[i])
	}
	assert.Equal(t, int32(1), hits.Load(), "one handler run")
	assert.Equal(t, int32(1), f.settleCalls.Load(), "one settle")
	assert.Equal(t, int32(1), f.verifyCalls.Load(), "duplicates never reach /verify")
}

func TestRacingDuplicatesNeverDeliverTwice(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)
	var wg sync.WaitGroup
	var delivered atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if do(h, "GET", "/paid", sig).Code == 200 {
				delivered.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), delivered.Load())
	assert.Equal(t, int32(1), hits.Load())
}

func TestTheLockIsReleasedWhenTheRequestEnds(t *testing.T) {
	f := startMock(t)
	f.verify = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			verifyInvalid(w, "invalid_exact_canton_expired")
			return
		}
		verifyOK(w)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	sig := signatureFor(t, h, testPreparedTx)
	assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "a refused attempt leaves no lock behind")
}

func TestSettledAndDeliveredRecordsAreWritten(t *testing.T) {
	f := startMock(t)
	store := NewMemoryStore()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	require.Equal(t, 200, do(h, "GET", "/paid", signatureFor(t, h, "1220"+testPreparedTx)).Code)
	record, found, err := store.Record(context.Background(), testPreparedTx)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, testUpdateID, record.UpdateID, "keyed by the canonical hash")
	assert.Equal(t, StateSettled, record.State)
	assert.Equal(t, testPayer, record.Payer)
	assert.NotEmpty(t, record.Digest)
	_, delivered, _ := store.Claimant(context.Background(), testUpdateID)
	assert.True(t, delivered)
}

func TestLockTTLIsSizedFromTheRouteAndTheSettleBudget(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	started := make(chan struct{})
	release := make(chan struct{})
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	}, func(c *Config) {
		c.Store = NewRedisStore(redis, "p:")
		c.Timeout = 100 * time.Second
		c.SettleTimeout = 20 * time.Second
		c.SettleCalls = 3
	})
	sig := signatureFor(t, h, testPreparedTx)
	finished := make(chan struct{})
	go func() { do(h, "GET", "/paid", sig); close(finished) }()
	<-started
	redis.mu.Lock()
	ttl := redis.ttls["p:lock:"+testPreparedTx]
	redis.mu.Unlock()
	close(release)
	<-finished
	assert.Equal(t, 120*time.Second+3*20*time.Second+2*retryAfterCap+postVerifyStoreCalls*storeCallTimeout, ttl,
		"the larger of Timeout and the route lifetime, every settle call, the waits and the post-settle Store calls")
}

// A payer who leaves during settle: settled, not claimed; the retry is served once.

func TestPayerLeavesDuringSettleThenRetryIsServedOnce(t *testing.T) {
	f := startMock(t)
	ctx, leave := context.WithCancel(context.Background())
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			leave() // the payer disconnects while /settle is in flight
			time.Sleep(30 * time.Millisecond)
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID) // what the facilitator says to a repeat
	}
	var hits atomic.Int32
	store := NewMemoryStore()
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)

	doCtx(ctx, h, "GET", "/paid", sig) // the payer is gone: nobody reads this answer
	assert.Equal(t, int32(1), f.settleCalls.Load(), "the detached settle ran to the end")
	record, _, _ := store.Record(context.Background(), testPreparedTx)
	assert.Equal(t, testUpdateID, record.UpdateID, "settled is recorded")
	_, delivered, _ := store.Claimant(context.Background(), testUpdateID)
	assert.False(t, delivered, "but not claimed: nothing was delivered")

	retry := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, retry.Code, "the retry of the same signature is served once")
	assert.Equal(t, testUpdateID, paymentResponse(t, retry)["transaction"])
	assert.Equal(t, int32(2), hits.Load())

	third := do(h, "GET", "/paid", sig)
	assert.Equal(t, 402, third.Code)
	assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, third.Body.String())
	assert.Equal(t, int32(2), hits.Load())
}

func TestSettledButUndeliveredIsServedAfterTheDeadlineWithoutVerify(t *testing.T) {
	f := startMock(t)
	ctx, leave := context.WithCancel(context.Background())
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			leave()
			time.Sleep(20 * time.Millisecond)
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)
	doCtx(ctx, h, "GET", "/paid", sig)

	// Ten seconds past the deadline /verify says expired.
	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	verifies := f.verifyCalls.Load()
	w := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, w.Code, "paid for, so served once even after the deadline")
	assert.Equal(t, verifies, f.verifyCalls.Load(), "/verify is skipped for a payment this merchant saw settle")
	assert.Equal(t, int32(2), hits.Load())
	assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
}

func TestConcurrentRetriesOfASettledButUndeliveredPaymentServeOnce(t *testing.T) {
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
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)
	doCtx(ctx, h, "GET", "/paid", sig)
	hits.Store(0)

	var wg sync.WaitGroup
	var served atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if do(h, "GET", "/paid", sig).Code == 200 {
				served.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), served.Load())
	assert.Equal(t, int32(1), hits.Load(), "one handler run for the retry")
}

// already_settled from the facilitator (the lost-answer case).

func TestAlreadySettledIsDeliveredOnceWithTheOriginalUpdateID(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { settleAlreadySettled(w, "1220original") }
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)
	w := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, w.Code)
	receipt := paymentResponse(t, w)
	assert.Equal(t, true, receipt["success"])
	assert.Equal(t, "1220original", receipt["transaction"])
	assert.Equal(t, testPayer, receipt["payer"])
	assert.JSONEq(t, `{"data":42}`, w.Body.String())

	replay := do(h, "GET", "/paid", sig)
	assert.Equal(t, 402, replay.Code)
	assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, replay.Body.String())
	assert.Equal(t, int32(1), hits.Load())
}

func TestAfterALostAnswerTheRetryDeliversOnce(t *testing.T) {
	// The first /settle committed but its answer was lost (bodyless 502 until
	// the budget ran out); the payer's retry meets already_settled.
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n <= 4 {
			w.WriteHeader(502)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)
	first := do(h, "GET", "/paid", sig)
	require.Equal(t, 503, first.Code)
	assert.NotContains(t, first.Body.String(), "data")
	second := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, second.Code)
	assert.Equal(t, testUpdateID, paymentResponse(t, second)["transaction"])
	assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
}

func TestAlreadySettledWithoutATransactionIsNotDelivered(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { settleAlreadySettled(w, "") }
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 402, w.Code)
	assert.NotContains(t, w.Body.String(), "data")
}

func TestAlreadySettledWhoseUpdateIDWasClaimedStays402Redeemed(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { settleAlreadySettled(w, testUpdateID) }
	store := NewMemoryStore()
	_, _ = store.Claim(context.Background(), testUpdateID, "other-instance", recordTTL)
	_ = store.MarkDelivered(context.Background(), testUpdateID, "other-instance", recordTTL) // another instance served it
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 402, w.Code)
	assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, w.Body.String())
	assert.Equal(t, testUpdateID, paymentResponse(t, w)["transaction"])
	assert.NotContains(t, w.Body.String(), "data")
}

func TestSuccessWhoseUpdateIDWasAlreadyClaimedIsAborted(t *testing.T) {
	f := startMock(t)
	store := NewMemoryStore()
	_, _ = store.Claim(context.Background(), testUpdateID, "other-instance", recordTTL)
	_ = store.MarkDelivered(context.Background(), testUpdateID, "other-instance", recordTTL)
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 402, w.Code)
	assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, w.Body.String())
}

func TestSuccessWithoutATransactionIsNotDelivered(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) {
		reply(w, 200, map[string]any{"success": true, "transaction": "", "network": testNetworkName})
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 503, w.Code, "money may have moved: unknown, not refused")
	assert.NotContains(t, w.Body.String(), "data")
}

// Store failures: never deliver without a recorded claim.

func TestStoreFailuresNeverDeliverWithoutAClaim(t *testing.T) {
	for _, operation := range []string{"putSettled", "claim"} {
		t.Run(operation, func(t *testing.T) {
			f := startMock(t)
			store := newFaultyStore(NewMemoryStore())
			store.breakOp(operation, true)
			logger, logs := capturingLogger()
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store; c.Logger = logger })
			w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
			assert.Equal(t, 503, w.Code)
			assert.Equal(t, "3", w.Header().Get(headerRetryAfter))
			assert.JSONEq(t, `{"error":"payment outcome unknown: do NOT pay again; retry the same payment"}`, w.Body.String())
			receipt := paymentResponse(t, w)
			assert.Equal(t, testUpdateID, receipt["transaction"], "the updateId is in PAYMENT-RESPONSE")
			assert.Equal(t, false, receipt["success"])
			assert.NotContains(t, w.Body.String(), "data")
			assert.True(t, logsContain(logs, "level=ERROR", "updateId="+testUpdateID))
		})
	}
}

func TestStoreFailureOnAlreadySettledRecoveryIs503(t *testing.T) {
	for _, operation := range []string{"putSettled", "claim"} {
		t.Run(operation, func(t *testing.T) {
			f := startMock(t)
			f.settle = func(_ int32, w http.ResponseWriter) { settleAlreadySettled(w, testUpdateID) }
			store := newFaultyStore(NewMemoryStore())
			store.breakOp(operation, true)
			h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
			w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
			assert.Equal(t, 503, w.Code)
			assert.NotContains(t, w.Body.String(), "data")
			assert.Equal(t, testUpdateID, paymentResponse(t, w)["transaction"])
		})
	}
}

func TestStoreDownBeforeVerifyIs503AndNothingIsSettled(t *testing.T) {
	for _, operation := range []string{"lock", "record", "claimant"} {
		t.Run(operation, func(t *testing.T) {
			f := startMock(t)
			store := newFaultyStore(NewMemoryStore())
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
			sig := signatureFor(t, h, testPreparedTx)
			if operation == "claimant" { // only read once a settled record exists
				_ = store.Store.PutRecord(context.Background(), testPreparedTx, Record{State: StateSettled, UpdateID: testUpdateID, Digest: "d"}, recordTTL)
			}
			store.breakOp(operation, true)
			w := do(h, "GET", "/paid", sig)
			assert.Equal(t, 503, w.Code)
			assert.JSONEq(t, `{"error":"payment state unavailable: do NOT pay again; retry the same payment"}`, w.Body.String())
			assert.Equal(t, int32(0), hits.Load())
			assert.Equal(t, int32(0), f.settleCalls.Load())
		})
	}
}

// Two instances sharing Redis: the second refuses what the first delivered.

func TestTwoInstancesShareOneDeliveryThroughRedis(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	var hitsA, hitsB atomic.Int32
	shared := func(c *Config) { c.Store = NewRedisStore(redis, "x402:") }
	a := newGate(t, f, okHandler(&hitsA), shared)
	b := newGate(t, f, okHandler(&hitsB), shared)
	sig := signatureFor(t, a, testPreparedTx)
	assert.Equal(t, 200, do(a, "GET", "/paid", sig).Code)
	w := do(b, "GET", "/paid", sig)
	assert.Equal(t, 402, w.Code)
	assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, w.Body.String())
	assert.Equal(t, int32(0), hitsB.Load())
	assert.Equal(t, int32(1), f.settleCalls.Load())
}

func TestRedisOutageBeforeVerifyIs503(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = NewRedisStore(redis, "") })
	sig := signatureFor(t, h, testPreparedTx)
	redis.fail(true)
	w := do(h, "GET", "/paid", sig)
	assert.Equal(t, 503, w.Code)
	assert.Equal(t, int32(0), f.settleCalls.Load())
}

// A header that does not parse goes to the SDK as is.

func TestUnparsableHeaderGoesToTheSDK(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	w := do(h, "GET", "/paid", "this is not a payment")
	assert.Equal(t, 400, w.Code, "the SDK's own answer to a header it cannot read")
	assert.Contains(t, w.Body.String(), "invalid_payload", "the SDK's code, not the gate's")
	assert.NotContains(t, w.Body.String(), "preparedTxHash")
	assert.Equal(t, int32(0), hits.Load())
	assert.Equal(t, int32(0), f.settleCalls.Load())
}
