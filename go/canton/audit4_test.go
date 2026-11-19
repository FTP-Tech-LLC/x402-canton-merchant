package canton

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the round-4 audits.

// ---------------------------------------------------------------- 1: every token per key

func TestSecondGiveUpOfTheSameClaimDoesNotOrphanTheFirst(t *testing.T) {
	ctx := context.Background()
	redis := newFakeRedis()
	led := newLedger(NewRedisStore(redis, "x:"), nopLogger())
	require.NoError(t, redis.Set(ctx, "x:claimed:u", "t1", time.Hour)) // the first claim, still in the Store
	require.True(t, led.rememberClaim("u", "t1", time.Hour))
	require.True(t, led.rememberClaim("u", "t2", time.Hour)) // a second give-up for the same updateId
	assert.True(t, led.isWithheld("u", "t1"), "the first token is not replaced")
	assert.True(t, led.isWithheld("u", "t2"))

	led.reconcileOnce(ctx)
	_, held, _ := redis.Get(ctx, "x:claimed:u")
	assert.False(t, held, "the first claim is released, not orphaned for 26 h")
	assert.False(t, led.isWithheld("u", "t1"))
	assert.False(t, led.isWithheld("u", "t2"))
}

func TestSecondGiveUpOfTheSameLockDoesNotOrphanTheFirst(t *testing.T) {
	ctx := context.Background()
	redis := newFakeRedis()
	led := newLedger(NewRedisStore(redis, "x:"), nopLogger())
	require.NoError(t, redis.Set(ctx, "x:lock:h", "l1", time.Hour))
	led.rememberLock("h", "l1", time.Hour)
	led.rememberLock("h", "l2", time.Hour)
	assert.True(t, led.isPendingLock("h", "l1"))
	led.reconcileOnce(ctx)
	_, held, _ := redis.Get(ctx, "x:lock:h")
	assert.False(t, held)
}

func TestForgettingOneTokenLeavesTheOthers(t *testing.T) {
	led := newLedger(NewMemoryStore(), nopLogger())
	led.rememberClaim("u", "t1", time.Hour)
	led.rememberClaim("u", "t2", time.Hour)
	led.forgetClaim("u", "t1")
	assert.False(t, led.isWithheld("u", "t1"))
	assert.True(t, led.isWithheld("u", "t2"))
	assert.Equal(t, 1, led.claims.size)
}

func TestPendingTableIsBoundedInTotalAcrossTokens(t *testing.T) {
	led := newLedger(NewMemoryStore(), nopLogger())
	for i := 0; i < maxPendingReleases; i++ {
		require.True(t, led.rememberClaim("u", fmt.Sprintf("t%d", i), time.Hour))
	}
	assert.False(t, led.rememberClaim("u", "one-more", time.Hour))
}

// ---------------------------------------------------------------- 2: only a marked claim is a delivery

func TestDeliveredClaimIsMarkedAfterTheResponse(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
	holder, found, _ := redis.Get(context.Background(), "x:claimed:"+testUpdateID)
	require.True(t, found)
	assert.True(t, strings.HasPrefix(holder, "delivered:"), "the claim carries the delivered mark: %s", holder)
	assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code, "a marked claim is the final answer")
}

func TestClaimWithoutTheMarkIs503NotTheFinal402(t *testing.T) {
	f := startMock(t)
	store := newFaultyStore(NewMemoryStore())
	store.breakOp("mark", true) // the mark write fails: the claim stays unmarked
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
	replay := do(h, "GET", "/paid", sig)
	assert.Equal(t, 503, replay.Code, "unconfirmed: never the final redeemed")
	assert.Contains(t, replay.Body.String(), "do NOT pay again")
	assert.Equal(t, int32(1), hits.Load(), "and never delivered twice")
}

func TestGivenUpClaimOfAnotherInstanceIsNotTakenForADelivery(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	ctx, leave := context.WithCancel(context.Background())
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			redis.loseNextReplies(1)
			settleOK(w)
			_ = leave
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	storeA := newFaultyStore(NewRedisStore(redis, "x:"))
	var hitsB atomic.Int32
	a := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = storeA })
	b := newGate(t, f, okHandler(&hitsB), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	_ = ctx
	storeA.breakOp("claimant", true)
	storeA.breakOp("unclaim", true)
	sig := signatureFor(t, a, testPreparedTx)
	require.Equal(t, 503, do(a, "GET", "/paid", sig).Code)
	w := do(b, "GET", "/paid", sig)
	assert.Equal(t, 503, w.Code, "inside the deadline B tells the payer to retry, not that it was redeemed")
	assert.NotContains(t, w.Body.String(), "redeemed")
}

// ---------------------------------------------------------------- 3: gate lifetime

func TestGateCloseRunsAFinalPassAndStopsTheReconciler(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	old := reconcileInterval
	reconcileInterval = time.Hour // only Close reconciles
	defer func() { reconcileInterval = old }()
	startup, cancelStartup := context.WithTimeout(context.Background(), 5*time.Second)
	g, err := NewGate(startup, Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: nopLogger(),
		Store: NewRedisStore(redis, "x:")})
	require.NoError(t, err)
	cancelStartup() // a startup context may end: the gate has its own lifetime
	require.NoError(t, redis.Set(context.Background(), "x:claimed:u", "tok", time.Hour))
	g.g.ledger.rememberClaim("u", "tok", time.Hour)

	require.NoError(t, g.Close(context.Background()))
	_, held, _ := redis.Get(context.Background(), "x:claimed:u")
	assert.False(t, held, "the last pass released what was left")
	require.NoError(t, g.Close(context.Background()), "Close is safe to call twice")
	assert.NotNil(t, g.Middleware())
}

func TestGateReconcilerOutlivesTheStartupContext(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	old := reconcileInterval
	reconcileInterval = 5 * time.Millisecond
	defer func() { reconcileInterval = old }()
	startup, cancelStartup := context.WithTimeout(context.Background(), time.Minute)
	g, err := NewGate(startup, Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: nopLogger(),
		Store: NewRedisStore(redis, "x:")})
	require.NoError(t, err)
	defer func() { _ = g.Close(context.Background()) }()
	cancelStartup()
	require.NoError(t, redis.Set(context.Background(), "x:claimed:late", "tok", time.Hour))
	g.g.ledger.rememberClaim("late", "tok", time.Hour)
	require.Eventually(t, func() bool {
		_, held, _ := redis.Get(context.Background(), "x:claimed:late")
		return !held
	}, 2*time.Second, 5*time.Millisecond)
}

func TestCloseLogsWhatIsLeftBehindAtError(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	logger, logs := capturingLogger()
	old := reconcileInterval
	reconcileInterval = time.Hour
	defer func() { reconcileInterval = old }()
	g, err := NewGate(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: logger,
		Store: NewRedisStore(redis, "x:")})
	require.NoError(t, err)
	redis.fail(true)
	g.g.ledger.rememberClaim("u", "tok", time.Hour)
	require.NoError(t, g.Close(context.Background()))
	assert.True(t, logsContain(logs, "level=ERROR", "stopped with work left", "claims=1"))
}

func TestMemoryStoreWarnsThatAReplacementGateNeedsTheSameStore(t *testing.T) {
	f := startMock(t)
	logger, logs := capturingLogger()
	newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Logger = logger })
	assert.True(t, logsContain(logs, "level=WARN", "replaces another", "same Config.Store"))
}

func TestReconcilerSurvivesAPanickingStore(t *testing.T) {
	store := newFaultyStore(NewMemoryStore())
	led := newLedger(store, nopLogger())
	led.rememberClaim("u", "tok", time.Hour)
	store.breakOp("panicUnclaim", true)
	assert.NotPanics(t, func() { led.reconcileGuarded(context.Background()) })
}

func TestReconcilerStopsBetweenItemsWhenItsContextEnds(t *testing.T) {
	led := newLedger(NewMemoryStore(), nopLogger())
	for i := 0; i < 5; i++ {
		led.rememberClaim("u", fmt.Sprintf("t%d", i), time.Hour)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	led.reconcileOnce(ctx)
	assert.Equal(t, 5, led.claims.size, "a cancelled pass releases nothing")
}

// ---------------------------------------------------------------- 4: delivered/claim vs the reconciler

// racyStore releases and forgets a pending claim between the two reads of a
// claim holder, as the reconciler can.
type racyStore struct {
	Store
	led   *ledger
	reads atomic.Int32
	token string
}

func (s *racyStore) Claimant(ctx context.Context, updateID string) (string, bool, error) {
	holder, found, err := s.Store.Claimant(ctx, updateID)
	if s.reads.Add(1) == 1 && found {
		// The reconciler's tick lands right after this read: delete in the Store, then forget.
		_ = s.Unclaim(ctx, updateID, s.token)
		s.led.forgetClaim(updateID, s.token)
	}
	return holder, found, err
}

func TestClaimStateIsNotFooledByTheReconcilerReleasingBetweenReads(t *testing.T) {
	ctx := context.Background()
	inner := NewMemoryStore()
	led := newLedger(inner, nopLogger())
	racy := &racyStore{Store: inner, led: led, token: "t1"}
	led.store = racy
	_, _ = inner.Claim(ctx, "u", "t1", recordTTL)
	led.rememberClaim("u", "t1", time.Hour)
	ownership, err := led.claimState(ctx, "u")
	require.NoError(t, err)
	assert.Equal(t, claimNone, ownership, "a just-released given-up claim is not a delivery")
}

func TestClaimIsNotFooledByTheReconcilerEither(t *testing.T) {
	ctx := context.Background()
	inner := NewMemoryStore()
	led := newLedger(inner, nopLogger())
	led.store = &racyStore{Store: inner, led: led, token: "t1"}
	_, _ = inner.Claim(ctx, "u", "t1", recordTTL)
	// The pending entry is not in the table yet when claim() reads the holder; the
	// release lands between its two reads.
	claimed, err := led.claim(ctx, "u", "t2")
	require.NoError(t, err)
	assert.True(t, claimed, "the claim is retried after the holder vanished")
}

// ---------------------------------------------------------------- 5: stale record snapshot

func TestReconcilerNeverOverwritesANewerSettledRecord(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	led := newLedger(store, nopLogger())
	led.toFallback(ctx, "h", Record{State: StateAttempt, Digest: "d"})
	require.NoError(t, store.PutRecord(ctx, "h", Record{State: StateSettled, UpdateID: "u", Digest: "d"}, recordTTL))
	led.reconcileOnce(ctx)
	got, _, _ := store.Record(ctx, "h")
	assert.Equal(t, StateSettled, got.State, "the request's newer record stands")
	_, still := led.fromFallback("h")
	assert.False(t, still, "and the stale fallback entry is dropped")
}

func TestReconcilerSkipsAPaymentARequestHolds(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	led := newLedger(store, nopLogger())
	led.toFallback(ctx, "h", Record{State: StateAttempt, Digest: "d"})
	ok, _ := store.Lock(ctx, "h", "request-token", time.Minute)
	require.True(t, ok)
	led.reconcileOnce(ctx)
	_, found, _ := store.Record(ctx, "h")
	assert.False(t, found, "the request's own hooks write its record")
	_, still := led.fromFallback("h")
	assert.True(t, still)
}

func TestReconcilerNeverResurrectsAClearedAttempt(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	led := newLedger(store, nopLogger())
	led.toFallback(ctx, "h", Record{State: StateAttempt, Digest: "d"})
	led.clear(ctx, "h") // a definite refusal cleared it
	led.reconcileOnce(ctx)
	_, found, _ := store.Record(ctx, "h")
	assert.False(t, found)
}

func TestAttemptOlderThanTheFacilitatorsRecordIsNotClearedByARefusal(t *testing.T) {
	f := startMock(t)
	store := NewMemoryStore()
	f.settle = func(_ int32, w http.ResponseWriter) { settleRefused(w, "invalid_exact_canton_expired") }
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	// An unknown-outcome attempt from 24 h ago.
	payload := mustAttemptDigest(t, h, sig, store)
	_ = payload
	w := do(h, "GET", "/paid", sig)
	assert.Equal(t, 503, w.Code, "expired after 24 h does not prove nothing moved")
	_, found, _ := store.Record(context.Background(), testPreparedTx)
	assert.True(t, found, "the attempt is kept")
}

// mustAttemptDigest runs the payment once to learn its digest, then rewrites the
// attempt as if it were 24 hours old.
func mustAttemptDigest(t *testing.T, h http.Handler, sig string, store Store) string {
	t.Helper()
	ctx := context.Background()
	// Seed an attempt with the digest the gate computes: take it from a first, refused run.
	// The first run clears its own (fresh) attempt, so seed by a settle that stays unknown.
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { w.WriteHeader(502) }
	seedGate := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store; c.SettleCalls = 1 })
	seedSig := signatureFor(t, seedGate, testPreparedTx)
	do(seedGate, "GET", "/paid", seedSig)
	record, found, _ := store.Record(ctx, testPreparedTx)
	require.True(t, found)
	record.At = time.Now().Add(-24 * time.Hour).Unix()
	require.NoError(t, store.PutRecord(ctx, testPreparedTx, record, recordTTL))
	_ = h
	_ = sig
	return record.Digest
}

// ---------------------------------------------------------------- 7: the response does not wait for the Store

func TestSlowLockReleaseDoesNotHoldTheResponseOpen(t *testing.T) {
	old := releaseWait
	releaseWait = 20 * time.Millisecond
	t.Cleanup(func() { releaseWait = old }) // runs after the server (registered later) has closed
	f := startMock(t)
	store := newFaultyStore(NewMemoryStore())
	store.mu.Lock()
	store.unlockDelay = 600 * time.Millisecond
	store.mu.Unlock()
	server, gate := realGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	sig := signatureWithPayload(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forward(server, w, r) }), "GET", "/paid", payloadJSON(testPreparedTx))
	_ = gate

	req, _ := http.NewRequestWithContext(context.Background(), "GET", server.URL+"/paid", nil)
	req.Header.Set(headerPaymentSig, sig)
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	elapsed := time.Since(started)
	assert.Equal(t, 200, resp.StatusCode)
	assert.JSONEq(t, `{"data":42}`, string(body))
	assert.Equal(t, fmt.Sprint(len(body)), resp.Header.Get("Content-Length"), "the response carries its length: complete at the gate's Flush")
	assert.Less(t, elapsed, 400*time.Millisecond, "the client has the whole response long before the 600 ms Unlock ends")
}

func TestPaidAnswersCarryTheirContentLength(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, fmt.Sprint(w.Body.Len()), w.Header().Get("Content-Length"))
	unpaid := do(h, "GET", "/paid", "")
	assert.Equal(t, fmt.Sprint(unpaid.Body.Len()), unpaid.Header().Get("Content-Length"))
}

func TestPostResponseWorkIsWaitedForByClose(t *testing.T) {
	old := releaseWait
	releaseWait = time.Millisecond
	defer func() { releaseWait = old }()
	f := startMock(t)
	store := newFaultyStore(NewMemoryStore())
	store.mu.Lock()
	store.unlockDelay = 150 * time.Millisecond
	store.mu.Unlock()
	g, err := NewGate(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: nopLogger(), Store: store})
	require.NoError(t, err)
	h := g.Middleware()(okHandler(new(atomic.Int32)))
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
	require.NoError(t, g.Close(context.Background()))
	ok, _ := store.Store.Lock(context.Background(), testPreparedTx, "t", time.Minute)
	assert.True(t, ok, "Close waited for the lock release")
}

// ---------------------------------------------------------------- 8: a different digest for the same payment

func TestDigestIgnoresTheFacilitatorInjectedFields(t *testing.T) {
	payload := []byte(`{"payload":{"preparedTxHash":"ab"}}`)
	a := []byte(`{"scheme":"exact","amount":"1","extra":{"feePayer":"old::1220","synchronizerId":"s1","instrumentId":{"id":"Amulet"}}}`)
	b := []byte(`{"scheme":"exact","amount":"1","extra":{"feePayer":"new::1220","synchronizerId":"s2","instrumentId":{"id":"Amulet"}}}`)
	c := []byte(`{"scheme":"exact","amount":"2","extra":{"feePayer":"old::1220","synchronizerId":"s1","instrumentId":{"id":"Amulet"}}}`)
	assert.Equal(t, paymentDigest(payload, a), paymentDigest(payload, b))
	assert.NotEqual(t, paymentDigest(payload, a), paymentDigest(payload, c), "a different amount is another payment")
	assert.True(t, digestMatches(legacyPaymentDigest(payload, a), payload, b) == false, "legacy digests only match the requirements they were made from")
	assert.True(t, digestMatches(legacyPaymentDigest(payload, a), payload, a), "records written before the change still match")
	assert.False(t, digestMatches("", payload, a))
}

func TestWithheldPaymentIsServedByAGateWithANewerSupportedPin(t *testing.T) {
	f := startMock(t)
	store := NewMemoryStore()
	ctx, leave := context.WithCancel(context.Background())
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			leave()
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	a := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	sig := signatureFor(t, a, testPreparedTx)
	require.Equal(t, 503, doCtx(ctx, a, "GET", "/paid", sig).Code)

	// The facilitator rotated its fee payer; a gate started now pins the new one.
	f.supported = func(_ int32, w http.ResponseWriter) {
		body := supportedBody(nil)
		extra := body["kinds"].([]any)[0].(map[string]any)["extra"].(map[string]any)
		extra["feePayer"] = "rotated-fee-payer::1220" + strings.Repeat("c", 64)
		reply(w, 200, body)
	}
	var hitsB atomic.Int32
	b := newGate(t, f, okHandler(&hitsB), func(c *Config) { c.Store = store })
	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	retry := do(b, "GET", "/paid", sig)
	assert.Equal(t, 200, retry.Code, "the same payment under another pin is the same payment")
	assert.Equal(t, int32(1), hitsB.Load())
}

func TestHandlerChainReturnsBeforeASlowLockRelease(t *testing.T) {
	// For HTTP/2 and chunked answers the stream ends when the handler returns:
	// the post-response work must not hold it.
	old := releaseWait
	releaseWait = 20 * time.Millisecond
	defer func() { releaseWait = old }()
	f := startMock(t)
	store := newFaultyStore(NewMemoryStore())
	store.mu.Lock()
	store.unlockDelay = 600 * time.Millisecond
	store.mu.Unlock()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	started := time.Now()
	require.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
	assert.Less(t, time.Since(started), 400*time.Millisecond)
}
