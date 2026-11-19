package canton

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// timeNow is time.Now; tests replace it.
var timeNow = time.Now

type stateKey struct{}

// requestState is the per-request slot the gate puts into the request context.
// The SDK passes that context to every hook and to the facilitator client, so
// the hooks, the client wrapper and the outer gate share one view of a payment.
// Everything runs on one goroutine today; the mutex keeps it safe if the SDK
// ever calls hooks from another.
type requestState struct {
	mu sync.Mutex

	// requestCtx is the context of the incoming request. The SDK context also
	// ends at the gate's own Timeout, so "the payer left" is read from here.
	requestCtx context.Context

	identity    paymentIdentity
	hasIdentity bool
	token       string // random owner of this request's lock and claim
	// prior is the record found before /verify: a settled or attempted payment
	// this merchant saw earlier. It lets a retry skip /verify.
	prior  *Record
	outer  http.ResponseWriter // the real writer, for its write deadline
	budget time.Duration       // the longest this paid request may take

	payer string // proven payer, from an isValid:true verify

	lastStatus     int           // HTTP status of the last facilitator answer, 0 when none arrived
	lastRetryAfter time.Duration // its Retry-After, 0 when absent
	lastPayer      string
	lastTx         string

	settleCalls       int
	verifyUnavailable bool
	settleUnknown     bool // outcome unknown after the settle budget
	// withheld: money moved but the response is not delivered (a Store failure, or
	// a payer who looks gone). The gate answers 503 with the updateId.
	withheld      bool
	withheldTx    string
	notCharged    bool   // nothing was submitted to /settle (payer gone, or the attempt could not be stored)
	notChargedMsg string // what the gate tells the payer then
	verified      bool   // /verify passed, or was skipped for a recorded payment
	redeemed      bool   // the updateId was already delivered
	redeemedTx    string
	claimedTx     string // updateId this request claimed, released if the write fails

	handlerHeader http.Header
}

func stateFrom(ctx context.Context) *requestState {
	state, _ := ctx.Value(stateKey{}).(*requestState)
	return state
}

func withState(ctx context.Context, state *requestState) context.Context {
	return context.WithValue(ctx, stateKey{}, state)
}

// PayerFrom returns the payer the facilitator proved in /verify, for use inside
// a paid handler (for example a per-payer rate limit). On a retry that skips
// /verify it is the payer recorded when the payment was attempted. The second
// result is false outside a paid request, and when no payer is known.
//
// A handler that answers 429 or any other 4xx/5xx for an over-limit payer is
// not settled, so the payer is not charged. The exception is a retry of a
// payment that may already have moved (see IsRetry): there the gate does not
// pass a 4xx through, it answers 503 "do NOT pay again; retry the same payment".
func PayerFrom(ctx context.Context) (string, bool) {
	state := stateFrom(ctx)
	if state == nil {
		return "", false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.payer, state.payer != ""
}

// Verified reports whether ctx belongs to a paid request this package's gate
// verified (a payment /verify accepted, or a recorded retry that skipped it),
// whether or not the facilitator named the payer. A framework adapter uses it to
// tell a paid request from one the gate passed through: PayerFrom is false for a
// valid /verify answer without a payer (the payer field is optional in x402 v2).
func Verified(ctx context.Context) bool {
	state := stateFrom(ctx)
	return state != nil && state.isVerified()
}

func (s *requestState) setPayer(payer string) {
	s.mu.Lock()
	s.payer = payer
	s.mu.Unlock()
}

func (s *requestState) payerGone() bool {
	return s.requestCtx.Err() != nil
}

func (s *requestState) recordAnswer(status int, retryAfter time.Duration) {
	s.mu.Lock()
	s.lastStatus = status
	s.lastRetryAfter = retryAfter
	s.mu.Unlock()
}

// beginCall clears the answer of the previous facilitator call and counts the
// call when it is a settle.
func (s *requestState) beginCall(settle bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastStatus, s.lastRetryAfter = 0, 0
	if settle {
		s.settleCalls++
	}
}

func (s *requestState) answer() (status int, retryAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStatus, s.lastRetryAfter
}

func (s *requestState) settleCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settleCalls
}

func (s *requestState) markWithheld(updateID string) {
	s.mu.Lock()
	s.withheld, s.withheldTx = true, updateID
	s.mu.Unlock()
}

func (s *requestState) markRedeemed(updateID string) {
	s.mu.Lock()
	s.redeemed, s.redeemedTx = true, updateID
	s.mu.Unlock()
}

func (s *requestState) currentPayer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.payer
}

func (s *requestState) setClaimed(updateID string) {
	s.mu.Lock()
	s.claimedTx = updateID
	s.mu.Unlock()
}

// stateView is a copy of the outcome flags, safe to read without the lock.
type stateView struct {
	identity                                      paymentIdentity
	prior                                         *Record
	lastTx, withheldTx, redeemedTx                string
	notCharged, settleUnknown, withheld, redeemed bool
	notChargedMsg                                 string
}

func (s *requestState) view() stateView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return stateView{
		identity: s.identity, prior: s.prior, lastTx: s.lastTx, withheldTx: s.withheldTx, redeemedTx: s.redeemedTx,
		notCharged: s.notCharged, notChargedMsg: s.notChargedMsg, settleUnknown: s.settleUnknown, withheld: s.withheld, redeemed: s.redeemed,
	}
}

// IsRetry reports whether the paid request is the retry of a payment this
// merchant already attempted or settled but did not deliver. Such a payment may
// already have moved, so a limiter should not refuse it: a 4xx or 5xx from the
// handler is answered by the gate as 503, never passed through.
func IsRetry(ctx context.Context) bool {
	state := stateFrom(ctx)
	return state != nil && state.prior != nil
}

func (s *requestState) markVerified() {
	s.mu.Lock()
	s.verified = true
	s.mu.Unlock()
}

func (s *requestState) isVerified() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verified
}

func (s *requestState) markNotCharged(message string) {
	s.mu.Lock()
	s.notCharged, s.notChargedMsg = true, message
	s.mu.Unlock()
}
