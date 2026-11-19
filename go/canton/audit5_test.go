package canton

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the round-5 audits.

func newTestGate(t *testing.T, f *mockFacilitator, handler http.HandlerFunc, mutate ...func(*Config)) (*Gate, http.Handler) {
	t.Helper()
	cfg := Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: nopLogger()}
	for _, change := range mutate {
		change(&cfg)
	}
	g, err := NewGate(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	return g, g.Middleware()(handler)
}

// ---------------------------------------------------------------- 1: Close and paid requests in flight

func TestCloseWaitsForPaidRequestsStillInFlight(t *testing.T) {
	f := startMock(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g, h := newTestGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		_, _ = w.Write([]byte("ok"))
	})
	sig := signatureFor(t, h, testPreparedTx)
	served := make(chan int, 1)
	go func() { served <- do(h, "GET", "/paid", sig).Code }()
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- g.Close(context.Background()) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a paid request was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	assert.Equal(t, 200, <-served)
	select {
	case err := <-closed:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the request finished")
	}
}

func TestCloseIsBoundedByItsContextWhileARequestRuns(t *testing.T) {
	f := startMock(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g, h := newTestGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		<-release
	})
	go do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, g.Close(ctx), context.DeadlineExceeded)
	close(release)
}

func TestCloseUnderLoadNeverCrashes(t *testing.T) {
	f := startMock(t)
	g, h := newTestGate(t, f, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var served atomic.Int32
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				hash := fmt.Sprintf("%02x%062x", worker, i)
				if do(h, "GET", "/paid", signatureForRawPayment(h, hash)).Code == 200 {
					served.Add(1)
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	// Close while requests keep arriving: it must not panic (a WaitGroup Add during
	// Wait), and it is bounded by its ctx because the load never drains.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := g.Close(ctx); err != nil {
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	time.Sleep(50 * time.Millisecond) // requests are still served after Close
	close(stop)
	wg.Wait()
	assert.Positive(t, served.Load())
}

// signatureForRawPayment builds a header for the route without asking the gate
// for its 402 first (the load test cannot afford that round trip per payment).
func signatureForRawPayment(h http.Handler, hash string) string {
	w := do(h, "GET", "/paid", "")
	return rebuildSignature(w, hash)
}

func TestClaimGivenUpDuringARequestThatCloseWaitsForIsReleased(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	store := newFaultyStore(NewRedisStore(redis, "x:"))
	settling, release := make(chan struct{}), make(chan struct{})
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			close(settling)
			<-release
			redis.loseNextReplies(1)      // the claim applies, the reply is lost
			store.failNext("claimant", 1) // the read-back fails
			store.failNext("unclaim", 1)  // and so does the first release
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	old := reconcileInterval
	reconcileInterval = time.Hour // only Close's passes
	defer func() { reconcileInterval = old }()
	g, h := newTestGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	done := make(chan int, 1)
	go func() { done <- do(h, "GET", "/paid", sig).Code }()
	<-settling

	closed := make(chan error, 1)
	go func() { closed <- g.Close(context.Background()) }() // the reload starts while the request runs
	time.Sleep(30 * time.Millisecond)
	close(release)
	require.Equal(t, 503, <-done, "the claim was given up: the body is withheld")
	require.NoError(t, <-closed)

	_, held, _ := redis.Get(context.Background(), "x:claimed:"+testUpdateID)
	assert.False(t, held, "the given-up claim was released by Close's last pass, not orphaned for 26 h")
	var hitsB atomic.Int32
	_, hB := newTestGate(t, f, okHandler(&hitsB), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	assert.Equal(t, 200, do(hB, "GET", "/paid", sig).Code, "the new gate serves the retry")
}

// ---------------------------------------------------------------- 2: no background clear

func unknownThenCommittedSettle(f *mockFacilitator) {
	f.settle = func(n int32, w http.ResponseWriter) {
		switch {
		case n == 1:
			settleRefused(w, "invalid_exact_canton_transient_refusal") // definite: nothing moved
		case n <= 5:
			w.WriteHeader(502) // the second attempt: unknown outcome on all 4 calls
		default:
			settleAlreadySettled(w, testUpdateID) // it had committed
		}
	}
}

func TestFailedClearNeverHidesOrDeletesALaterAttemptOfTheSameBytes(t *testing.T) {
	f := startMock(t)
	store := newFaultyStore(NewMemoryStore())
	unknownThenCommittedSettle(f)
	g, err := buildGate(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: nopLogger(), Store: store})
	require.NoError(t, err)
	h := g.wrap(okHandler(new(atomic.Int32)))
	sig := signatureFor(t, h, testPreparedTx)

	store.breakOp("delete", true) // a Store blip at the clear after the refusal
	require.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
	store.breakOp("delete", false)

	require.Equal(t, 503, do(h, "GET", "/paid", sig).Code, "the same bytes again: unknown outcome")
	g.ledger.reconcileOnce(context.Background())
	_, found, _ := store.Store.Record(context.Background(), testPreparedTx)
	assert.True(t, found, "the attempt of the unknown outcome stays recorded")

	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") } // past the deadline
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "money may have moved: the retry is served, never a 402")
}

func TestFailedClearOnAnotherInstanceDoesNotDeleteTheAttemptARetryReliesOn(t *testing.T) {
	f := startMock(t)
	shared := NewMemoryStore()
	storeA := newFaultyStore(shared)
	unknownThenCommittedSettle(f)
	gA, err := buildGate(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: time.Second, Logger: nopLogger(), Store: storeA})
	require.NoError(t, err)
	hA := gA.wrap(okHandler(new(atomic.Int32)))
	hB := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = shared })
	sig := signatureFor(t, hA, testPreparedTx)

	storeA.breakOp("delete", true)
	require.Equal(t, 402, do(hA, "GET", "/paid", sig).Code)
	storeA.breakOp("delete", false)
	require.Equal(t, 503, do(hB, "GET", "/paid", sig).Code)
	gA.ledger.reconcileOnce(context.Background()) // A's next tick must not touch B's attempt
	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	assert.Equal(t, 200, do(hB, "GET", "/paid", sig).Code)
}
