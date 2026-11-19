package canton

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the round-6 review.

func TestPaidRequestEnteringTheOldGateAfterCloseIsRefusedNotLeftUnreconciled(t *testing.T) {
	// The documented reload: swap the handler, then Close the old gate. A request the
	// old chain accepted before the swap reaches the old gate only after Close saw zero
	// work. It must not run a paid flow nothing would reconcile.
	f := startMock(t)
	redis := newFakeRedis()
	oldGate, err := NewGate(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 2 * time.Second,
		Logger: nopLogger(), Store: NewRedisStore(redis, "x:")})
	require.NoError(t, err)
	var oldRuns atomic.Int32
	paidOld := oldGate.Middleware()(okHandler(&oldRuns))
	inFront, proceed := make(chan struct{}), make(chan struct{})
	frontOld := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hold") != "" {
			close(inFront)
			<-proceed
		}
		paidOld.ServeHTTP(w, r)
	})
	var newRuns atomic.Int32
	_, newH := newTestGate(t, f, okHandler(&newRuns), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	sig := signatureFor(t, paidOld, testPreparedTx)

	straggler := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("GET", "/paid", nil)
		req.Header.Set(headerPaymentSig, sig)
		req.Header.Set("X-Hold", "1")
		w := httptest.NewRecorder()
		frontOld.ServeHTTP(w, req)
		straggler <- w
	}()
	<-inFront
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, oldGate.Close(closeCtx)) // sees zero work: the straggler is still in the front middleware
	close(proceed)

	w := <-straggler
	assert.Equal(t, 503, w.Code)
	assert.JSONEq(t, `{"error":"payment in progress: retry the same payment"}`, w.Body.String())
	assert.Zero(t, f.verifyCalls.Load(), "nothing was verified or settled on the closed gate")
	assert.Zero(t, f.settleCalls.Load())
	assert.Zero(t, oldRuns.Load())
	assert.Equal(t, 402, do(paidOld, "GET", "/paid", "").Code, "the unpaid offer is still answered")
	assert.Equal(t, 200, do(newH, "GET", "/paid", sig).Code, "the retry reaches the new gate and is served")
}

func TestCloseWhoseCtxEndsFirstStillReconcilesTheRequestItWasWaitingFor(t *testing.T) {
	old := reconcileInterval
	reconcileInterval = 10 * time.Millisecond
	defer func() { reconcileInterval = old }()
	f := startMock(t)
	redis := newFakeRedis()
	store := newFaultyStore(NewRedisStore(redis, "x:"))
	inHandler, proceed := make(chan struct{}), make(chan struct{})
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			redis.loseNextReplies(1)      // the claim applies, the reply is lost
			store.failNext("claimant", 1) // the read-back fails
			store.failNext("unclaim", 1)  // and the first release
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	var once sync.Once
	g, h := newTestGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(inHandler) })
		<-proceed
		_, _ = w.Write([]byte("ok"))
	}, func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	done := make(chan int, 1)
	go func() { done <- do(h, "GET", "/paid", sig).Code }()
	<-inHandler

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, g.Close(ctx), context.DeadlineExceeded, "this caller stops waiting")
	close(proceed) // the request finishes after Close's caller gave up
	assert.Equal(t, 503, <-done, "its claim was given up")

	require.NoError(t, g.Close(context.Background()), "the close work went on and finishes")
	_, held, _ := redis.Get(context.Background(), "x:claimed:"+testUpdateID)
	assert.False(t, held, "the reconciler still ran for it: the given-up claim is released")
}

func TestConcurrentCloseHonoursItsOwnCtx(t *testing.T) {
	f := startMock(t)
	inHandler, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g, h := newTestGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(inHandler) })
		<-proceed
		_, _ = w.Write([]byte("ok"))
	})
	sig := signatureFor(t, h, testPreparedTx)
	go do(h, "GET", "/paid", sig)
	<-inHandler
	first := make(chan error, 1)
	go func() { first <- g.Close(context.Background()) }()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := g.Close(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second, "a second Close does not wait for the first caller's ctx")
	close(proceed)
	require.NoError(t, <-first)
}

func TestUnpaidOfferIsStillAnsweredAfterClose(t *testing.T) {
	f := startMock(t)
	g, h := newTestGate(t, f, okHandler(new(atomic.Int32)))
	require.NoError(t, g.Close(context.Background()))
	assert.Equal(t, 402, do(h, "GET", "/paid", "").Code)
}
