package canton

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the round-3 audits.

func gateOf(t *testing.T, f *mockFacilitator, handler http.HandlerFunc, mutate ...func(*Config)) (*gate, http.Handler) {
	t.Helper()
	cfg := Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 2 * time.Second, Logger: nopLogger()}
	for _, change := range mutate {
		change(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g, err := buildGate(ctx, cfg)
	require.NoError(t, err)
	return g, g.wrap(handler)
}

// ---------------------------------------------------------------- 1: the reconciler

func TestWithheldClaimIsReleasedInTheBackgroundSoAnotherInstanceServesTheRetry(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	storeA := newFaultyStore(NewRedisStore(redis, "x:"))
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			redis.loseNextReplies(1) // the claim's SET NX applies, the reply is lost
			storeA.breakOp("claimant", true)
			storeA.breakOp("unclaim", true) // and neither the read-back nor the release get through
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	var hitsB atomic.Int32
	gA, a := gateOf(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = storeA })
	_, b := gateOf(t, f, okHandler(&hitsB), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	sig := signatureFor(t, a, testPreparedTx)
	require.Equal(t, 503, do(a, "GET", "/paid", sig).Code)

	// Instance B knows nothing of A's token: while Redis holds the unmarked claim it answers
	// 503 "do NOT pay again", never the final 402 (the claim is not a confirmed delivery).
	foreign := do(b, "GET", "/paid", sig)
	assert.Equal(t, 503, foreign.Code)
	assert.Contains(t, foreign.Body.String(), "do NOT pay again")

	// Redis heals; the reconciler releases the claim, and B serves the payment.
	storeA.breakOp("claimant", false)
	storeA.breakOp("unclaim", false)
	gA.ledger.reconcileOnce(context.Background())
	_, held, _ := redis.Get(context.Background(), "x:claimed:"+testUpdateID)
	assert.False(t, held, "the given-up claim is gone from the Store")
	assert.Equal(t, 200, do(b, "GET", "/paid", sig).Code, "another instance serves the retry")
	assert.Equal(t, int32(1), hitsB.Load())
	assert.Equal(t, 402, do(b, "GET", "/paid", sig).Code)
}

func TestFailedLockReleaseIsRetriedInTheBackground(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	store := newFaultyStore(NewRedisStore(redis, "x:"))
	var hits atomic.Int32
	g, h := gateOf(t, f, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		store.breakOp("unlock", true) // the blip starts while the request runs
		w.WriteHeader(500)
	}, func(c *Config) { c.Store = store })
	_, other := gateOf(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 500, do(h, "GET", "/paid", sig).Code)

	blocked := do(other, "GET", "/paid", sig)
	assert.Equal(t, 503, blocked.Code)
	assert.Contains(t, blocked.Body.String(), "payment in progress", "the lock is still in the Store")

	store.breakOp("unlock", false)
	g.ledger.reconcileOnce(context.Background())
	assert.Equal(t, 200, do(other, "GET", "/paid", sig).Code, "the lock lapsed seconds after the Store came back")
}

func TestLockLeftBehindByThisProcessIsTakenBackOnRetry(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	store := newFaultyStore(NewRedisStore(redis, "x:"))
	var hits atomic.Int32
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			store.breakOp("unlock", true)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}, func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 500, do(h, "GET", "/paid", sig).Code)
	store.breakOp("unlock", false)
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "the retry on the same instance takes its own stale lock back")
}

func TestRecordKeptOnlyInProcessReachesTheStoreAndIsServedByAnotherInstance(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	store := newFaultyStore(NewRedisStore(redis, "x:"))
	f.settle = func(n int32, w http.ResponseWriter) {
		if n <= 4 {
			w.WriteHeader(502)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	var hitsB atomic.Int32
	gA, a := gateOf(t, f, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		store.breakOp("record", true) // Redis goes away after the lock
		store.breakOp("put", true)
		_, _ = w.Write([]byte("ok"))
	}), func(c *Config) { c.Store = store })
	_, b := gateOf(t, f, okHandler(&hitsB), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	sig := signatureFor(t, a, testPreparedTx)
	require.Equal(t, 503, do(a, "GET", "/paid", sig).Code)

	store.breakOp("record", false)
	store.breakOp("put", false)
	gA.ledger.reconcileOnce(context.Background())
	record, found, _ := NewRedisStore(redis, "x:").Record(context.Background(), testPreparedTx)
	require.True(t, found, "the in-process record was written to the shared Store")
	assert.Equal(t, StateAttempt, record.State)

	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	assert.Equal(t, 200, do(b, "GET", "/paid", sig).Code, "another instance serves it past the deadline")
}

func TestReconcilerRunsByItselfAndStopsWithItsContext(t *testing.T) {
	old := reconcileInterval
	reconcileInterval = 5 * time.Millisecond
	defer func() { reconcileInterval = old }()
	f := startMock(t)
	redis := newFakeRedis()
	store := newFaultyStore(NewRedisStore(redis, "x:"))
	ctx, cancel := context.WithCancel(context.Background())
	g, err := buildGate(ctx, Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: nopLogger(), Store: store})
	require.NoError(t, err)
	go g.ledger.run(ctx, reconcileInterval, true)
	g.ledger.rememberClaim("u-pending", "tok", time.Hour)
	require.NoError(t, redis.Set(context.Background(), "x:claimed:u-pending", "tok", time.Hour))
	require.Eventually(t, func() bool {
		_, held, _ := redis.Get(context.Background(), "x:claimed:u-pending")
		return !held
	}, 2*time.Second, 5*time.Millisecond, "released without any request")

	cancel()
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, redis.Set(context.Background(), "x:claimed:u-after", "tok2", time.Hour))
	g.ledger.rememberClaim("u-after", "tok2", time.Hour)
	time.Sleep(100 * time.Millisecond)
	_, held, _ := redis.Get(context.Background(), "x:claimed:u-after")
	assert.True(t, held, "after ctx ends the reconciler is stopped")
}

func TestPendingReleaseLapsesWithItsTTLAndForgetsOnlyItsOwnToken(t *testing.T) {
	led := newLedger(NewMemoryStore(), nopLogger())
	now := time.Unix(1000, 0)
	led.now = func() time.Time { return now }
	require.True(t, led.rememberClaim("u", "tok", time.Minute))
	led.forgetClaim("u", "someone-else")
	assert.True(t, led.isWithheld("u", "tok"), "another token does not clear the entry")
	now = now.Add(2 * time.Minute)
	assert.False(t, led.isWithheld("u", "tok"), "lapsed with its TTL")
	assert.Empty(t, led.snapshot(led.claims))
}

func TestRememberReportsWhenTheTableIsFull(t *testing.T) {
	led := newLedger(NewMemoryStore(), nopLogger())
	for i := 0; i < maxPendingReleases; i++ {
		require.True(t, led.rememberClaim(fmt.Sprintf("u%d", i), "t", time.Hour))
	}
	assert.False(t, led.rememberClaim("one-too-many", "t", time.Hour), "the caller can log what really happened")
}

func TestUnreadableHolderOfATakenClaimIs503NotTheFinal402(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	store := newFaultyStore(NewRedisStore(redis, "x:"))
	require.NoError(t, redis.Set(context.Background(), "x:claimed:"+testUpdateID, "maybe-our-own-given-up-token", time.Hour))
	store.breakOp("claimant", true)
	// The delivered check cannot read the claimant either, so no record is shown first:
	// give the claim path its answer directly.
	led := newLedger(store, nopLogger())
	claimed, err := led.claim(context.Background(), testUpdateID, "new-token")
	assert.False(t, claimed)
	assert.Error(t, err, "an unreadable holder is an error the gate answers with 503, not a final redeemed")
	_ = f
}

// ---------------------------------------------------------------- 2: full memory store

func TestRetryOfARecordedPaymentIsAdmittedOnAFullMemoryStore(t *testing.T) {
	f := startMock(t)
	ctx, leave := context.WithCancel(context.Background())
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			leave()
		}
		settleOK(w)
	}
	store := NewMemoryStore(WithMaxEntries(20))
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 503, doCtx(ctx, h, "GET", "/paid", sig).Code)
	// Other payments take every remaining slot.
	for i := 0; ; i++ {
		if _, err := store.Lock(context.Background(), fmt.Sprintf("filler-%d", i), "t", time.Hour); err != nil {
			break
		}
	}
	retry := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, retry.Code, "the payment was admitted once: its retry needs no room")
}

func TestRecordedPaymentOnAStoreThatRefusesLocksIsNeverToldNothingWasCharged(t *testing.T) {
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
	store.breakOp("lockFull", true)
	w := do(h, "GET", "/paid", sig)
	assert.Equal(t, 503, w.Code)
	assert.NotContains(t, w.Body.String(), "nothing was charged")
	assert.Contains(t, w.Body.String(), "do NOT pay again")
	assert.Equal(t, testUpdateID, paymentResponse(t, w)["transaction"])
}

func TestFreshPaymentOnAFullStoreIsStillToldNothingWasCharged(t *testing.T) {
	f := startMock(t)
	store := newFaultyStore(NewMemoryStore())
	store.breakOp("lockFull", true)
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Contains(t, w.Body.String(), "nothing was charged")
}

func TestMemoryBoundBelowThreeIsRaisedSoAPaymentCanBeAdmitted(t *testing.T) {
	store := NewMemoryStore(WithMaxEntries(1)).(*memoryStore)
	assert.Equal(t, 3, store.maxEntries)
	ok, err := store.Lock(context.Background(), "h", "t", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestStalePriorIsClearedWhenTheSecondLookFindsNothing(t *testing.T) {
	f := startMock(t)
	store := NewMemoryStore()
	g, _ := gateOf(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	state := &requestState{identity: paymentIdentity{canonicalHash: testPreparedTx}}
	require.NoError(t, store.PutRecord(context.Background(), testPreparedTx, Record{State: StateAttempt, Digest: "d"}, recordTTL))
	require.False(t, g.answerIfDeliveredOrFailed(context.Background(), httptest.NewRecorder(), state))
	require.NotNil(t, state.prior)
	require.NoError(t, store.DeleteRecord(context.Background(), testPreparedTx)) // a concurrent refusal cleared it
	require.False(t, g.answerIfDeliveredOrFailed(context.Background(), httptest.NewRecorder(), state))
	assert.Nil(t, state.prior, "the next look under the lock sees no record: no skipped /verify, no false retry")
}

// ---------------------------------------------------------------- 3: route keys

func TestServeMuxWildcardsAndAPaidRootAreRefusedWithAClearError(t *testing.T) {
	for _, key := range []string{"GET /items/{id}", "GET /files/{path...}", "GET /{$}", "GET /"} {
		_, problems := checkRoutes(map[string]Route{key: validRoute()})
		require.NotEmpty(t, problems, key)
		message := fmt.Sprint(problems)
		assert.True(t, strings.Contains(message, "wildcard") || strings.Contains(message, "paid root"), "%s: %s", key, message)
	}
}

// ---------------------------------------------------------------- 4: write deadline through a wrapper

type statusRecorder struct{ http.ResponseWriter } // no Unwrap, as a hand-written logging wrapper

func TestWrapperWithoutUnwrapIsWarnedAboutOncePerProcess(t *testing.T) {
	writeDeadlineWarned.Store(false)
	f := startMock(t)
	logger, logs := capturingLogger()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Logger = logger })
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(&statusRecorder{w}, r) })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 200, do(wrapped, "GET", "/paid", sig).Code)
	assert.True(t, logsContain(logs, "level=WARN", "cannot lift the write deadline", "writerType=*canton.statusRecorder", "Unwrap", "RequestBudget"))

	before := strings.Count(logs.String(), "cannot lift the write deadline")
	other := signatureFor(t, h, "ee"+testPreparedTx[2:])
	do(wrapped, "GET", "/paid", other)
	assert.Equal(t, before, strings.Count(logs.String(), "cannot lift the write deadline"), "once per process")
	writeDeadlineWarned.Store(false)
}

// ---------------------------------------------------------------- 6: refusals on a retry

func TestRedirectAndInvalidStatusOnTheRetryOfAChargedPaymentSay503DoNotPayAgain(t *testing.T) {
	for _, status := range []int{303, 42} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := startMock(t)
			ctx, leave := context.WithCancel(context.Background())
			f.settle = func(n int32, w http.ResponseWriter) {
				if n == 1 {
					leave()
				}
				settleOK(w)
			}
			var calls atomic.Int32
			h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 2 {
					w.Header().Set("Location", "/created")
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte("ok"))
			})
			sig := signatureFor(t, h, testPreparedTx)
			require.Equal(t, 503, doCtx(ctx, h, "GET", "/paid", sig).Code)
			w := do(h, "GET", "/paid", sig)
			assert.Equal(t, 503, w.Code)
			assert.NotContains(t, w.Body.String(), "nothing was charged")
			assert.Contains(t, w.Body.String(), "do NOT pay again")
			assert.Equal(t, testUpdateID, paymentResponse(t, w)["transaction"])
			assert.Empty(t, w.Header().Get("Location"))
			assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "the record was kept")
		})
	}
}

// ---------------------------------------------------------------- lows

func TestPaywallNamesATokenByItsIDNotItsAdminHint(t *testing.T) {
	assert.Equal(t, "CBTC", displayAsset(testTokenAdmin+"::CBTC"))
	assert.Equal(t, "CC", displayAsset("CC"))
	f := startMock(t)
	f.supported = func(_ int32, w http.ResponseWriter) { reply(w, 200, supportedBody([]string{testTokenAdmin})) }
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) {
		c.Routes = map[string]Route{"GET /paid": {PayTo: testPayTo, Price: Token(testTokenAdmin, "CBTC", "1.5")}}
	})
	r := httptest.NewRequest("GET", "/paid", nil)
	r.Header.Set("Accept", "text/html")
	r.Header.Set("User-Agent", "Mozilla/5.0")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Contains(t, w.Body.String(), "1.5 CBTC")
	assert.NotContains(t, w.Body.String(), "cbtc-network")
}

func TestTokenIDMustMatchTheListedInstrument(t *testing.T) {
	f := startMock(t)
	f.supported = func(_ int32, w http.ResponseWriter) { reply(w, 200, supportedBody([]string{testTokenAdmin})) } // lists CBTC
	cfg := Config{FacilitatorURL: f.URL, SupportedWait: time.Second, Logger: nopLogger(),
		Routes: map[string]Route{"GET /paid": {PayTo: testPayTo, Price: Token(testTokenAdmin, "CBTX", "1")}}}
	_, err := NewMiddleware(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token id")
	cfg.Routes["GET /paid"] = Route{PayTo: testPayTo, Price: Token(testTokenAdmin, "CBTC", "1")}
	_, err = NewMiddleware(context.Background(), cfg)
	require.NoError(t, err)
}

func TestStartupRefusesAnUnusableStore(t *testing.T) {
	f := startMock(t)
	// A nil client is a configuration error: refused at once, whatever the load.
	started := time.Now()
	_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Minute,
		Logger: nopLogger(), Store: NewRedisStore(nil, "x:")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil RedisClient")
	assert.Less(t, time.Since(started), 10*time.Second, "not retried for SupportedWait")

	// An unreachable Redis is retried for SupportedWait and then refused. The wait is
	// generous so a loaded machine cannot cut the last attempt before it reads the Store.
	redis := newFakeRedis()
	redis.fail(true)
	_, err = NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 2 * time.Second,
		Logger: nopLogger(), Store: NewRedisStore(redis, "x:")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unusable after")
}

func TestRequestBudgetStaysPositiveForAnInvalidConfig(t *testing.T) {
	assert.Positive(t, RequestBudget(Config{Routes: testRoutes(), SettleCalls: -3}))
	assert.Equal(t, 505*time.Second, RequestBudget(Config{Routes: testRoutes()}), "defaults: 120+4x65+3x5+11 store windows of 10 s")
}

func TestShortSettleTimeoutIsWarnedAbout(t *testing.T) {
	f := startMock(t)
	logger, logs := capturingLogger()
	newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Logger = logger; c.SettleTimeout = 10 * time.Second })
	assert.True(t, logsContain(logs, "level=WARN", "SettleTimeout is below"))
}

func TestTransportLiftHandlesH2HooksAndWarnsAboutWrappedTransports(t *testing.T) {
	logger, logs := capturingLogger()
	conf := resolved{Config: Config{SettleTimeout: 65 * time.Second, Logger: logger}}

	h2 := &http.Transport{ResponseHeaderTimeout: 10 * time.Second}
	h2.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{"h2": nil}
	lifted, ok := transportFor(conf, h2).(*http.Transport)
	require.True(t, ok)
	assert.Zero(t, lifted.ResponseHeaderTimeout)
	assert.Nil(t, lifted.TLSNextProto, "the clone drops the x/net hook that kept the timeout")
	assert.True(t, lifted.ForceAttemptHTTP2)
	assert.Equal(t, 10*time.Second, h2.ResponseHeaderTimeout, "the original is untouched")

	untouched := &http.Transport{ResponseHeaderTimeout: 90 * time.Second}
	assert.Same(t, untouched, transportFor(conf, untouched))

	_ = transportFor(conf, roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil }))
	assert.True(t, logsContain(logs, "level=WARN", "wrapped"))
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInstrumentIDIsAvailableFromThePluginPackage(t *testing.T) {
	assert.Equal(t, "a::b", InstrumentID{Admin: "a", ID: "b"}.String())
}

func TestDeadlineOnTheStartupContextIsWarnedAbout(t *testing.T) {
	f := startMock(t)
	logger, logs := capturingLogger()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := NewMiddleware(ctx, Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: logger})
	require.NoError(t, err)
	assert.True(t, logsContain(logs, "level=WARN", "has a deadline", "reconciler stops", "lives as long as the server"))

	quiet, quietLogs := capturingLogger()
	longLived, stop := context.WithCancel(context.Background())
	defer stop()
	_, err = NewMiddleware(longLived, Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: quiet})
	require.NoError(t, err)
	assert.NotContains(t, quietLogs.String(), "has a deadline")
}
