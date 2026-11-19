package canton

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	x402 "github.com/x402-foundation/x402/go/v2"

	cantonmech "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
)

const (
	// errReasonAlreadySettled is the facilitator's answer to a repeat of a settled
	// payment. It is not in the upstream spec table, so it lives in this layer.
	errReasonAlreadySettled = "invalid_exact_canton_payment_already_settled"
	// errReasonAlreadyRedeemed is what the gate answers for a delivered payment.
	errReasonAlreadyRedeemed = "payment_already_redeemed"
)

// productionRetryAfterCap bounds how long one Retry-After from the facilitator
// can hold a paid request.
const productionRetryAfterCap = 5 * time.Second

// retryAfterCap is productionRetryAfterCap; a variable so tests can run fast.
var retryAfterCap = productionRetryAfterCap

// productionSettleBackoff is the wait before settle retry 1, 2, 3 when the
// facilitator sends no Retry-After. It matches PENDING_RETRY_MS in the Express
// middleware.
var productionSettleBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// settleBackoff is productionSettleBackoff; a variable so tests can run fast.
var settleBackoff = productionSettleBackoff

// facilitatorClient wraps the SDK HTTP client with the rules the SDK lacks:
// bounded settle retries of the identical bytes on a detached context, and the
// classification of /verify failures. It is the only place that calls /settle.
type facilitatorClient struct {
	inner         x402.FacilitatorClient
	settleTimeout time.Duration
	settleCalls   int
	logger        *slog.Logger
}

var _ x402.FacilitatorClient = (*facilitatorClient)(nil)

func (c *facilitatorClient) GetSupported(ctx context.Context) (x402.SupportedResponse, error) {
	return c.inner.GetSupported(ctx)
}

// Verify calls /verify. A failure that is not a definite answer is flagged in
// the request state, so the gate answers 503 instead of the SDK's 402: a 402
// tells the payer the payment was refused, and a client may then pay again.
func (c *facilitatorClient) Verify(ctx context.Context, payload, requirements []byte) (*x402.VerifyResponse, error) {
	state := stateFrom(ctx)
	if state != nil {
		state.beginCall(false)
	}
	resp, err := c.inner.Verify(ctx, payload, requirements)
	if err == nil || state == nil {
		return resp, err
	}
	var verifyError *x402.VerifyError
	if errors.As(err, &verifyError) {
		return resp, err
	}
	status, _ := state.answer()
	if !isDefiniteRefusalStatus(status) {
		state.mu.Lock()
		state.verifyUnavailable = true
		state.mu.Unlock()
		c.logger.WarnContext(ctx, "verify unavailable", slog.Int("status", status), slog.String("error", err.Error()))
	}
	return resp, err
}

// Settle calls /settle with the identical bytes until the answer is definite or
// the per-request budget (SettleCalls) is spent. The SDK's own immediate retry
// after a pending answer lands here too and counts against the same budget.
func (c *facilitatorClient) Settle(ctx context.Context, payload, requirements []byte) (*x402.SettleResponse, error) {
	state := stateFrom(ctx)
	if state == nil {
		return c.settleOnce(ctx, payload, requirements)
	}
	for retry := 0; ; retry++ {
		if state.settleCallCount() >= c.settleCalls {
			return nil, c.unknownOutcome(ctx, state)
		}
		state.beginCall(true)
		resp, err := c.settleOnce(ctx, payload, requirements)
		if !isUnknownOutcome(resp, err) {
			return resp, err
		}
		state.noteUnknown(resp, err)
		if state.settleCallCount() >= c.settleCalls {
			return nil, c.unknownOutcome(ctx, state)
		}
		c.wait(state, retry)
	}
}

// settleOnce runs one /settle call on a context detached from the request: a
// payer who leaves, or the SDK deadline, must not cut a settle the facilitator
// may already be committing.
func (c *facilitatorClient) settleOnce(ctx context.Context, payload, requirements []byte) (*x402.SettleResponse, error) {
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.settleTimeout)
	defer cancel()
	return c.inner.Settle(callCtx, payload, requirements)
}

// isUnknownOutcome reports whether the answer leaves open that the payment
// settled. Only a parsed facilitator verdict is definite: success, or a
// failure with an errorReason other than settlement_pending. Everything else
// is unknown and is retried: settlement_pending in any form, a transport error
// or timeout, 429, and every other answer without an errorReason (a bare 4xx
// or 5xx from a proxy included), as the Express middleware does.
func isUnknownOutcome(resp *x402.SettleResponse, err error) bool {
	if err == nil {
		// A parsed answer is a verdict only when it succeeded or names a reason.
		return resp == nil || (!resp.Success && (resp.ErrorReason == "" || resp.ErrorReason == cantonmech.ErrSettlementPending))
	}
	var settleError *x402.SettleError
	if errors.As(err, &settleError) {
		return settleError.ErrorReason == cantonmech.ErrSettlementPending
	}
	return true
}

// isDefiniteRefusalStatus is true for a 4xx other than 429. Only /verify uses
// it: a refused /verify submitted nothing, so it is safe to call it definite.
func isDefiniteRefusalStatus(status int) bool {
	return status >= http.StatusBadRequest && status < http.StatusInternalServerError &&
		status != http.StatusTooManyRequests
}

// noteUnknown remembers the transaction and payer of a pending answer.
func (s *requestState) noteUnknown(resp *x402.SettleResponse, err error) {
	var tx, payer string
	var settleError *x402.SettleError
	switch {
	case resp != nil:
		tx, payer = resp.Transaction, resp.Payer
	case errors.As(err, &settleError):
		tx, payer = settleError.Transaction, settleError.Payer
	}
	s.mu.Lock()
	if tx != "" {
		s.lastTx = tx
	}
	if payer != "" {
		s.lastPayer = payer
	}
	s.mu.Unlock()
}

func (c *facilitatorClient) wait(state *requestState, retry int) {
	delay := settleBackoff[min(retry, len(settleBackoff)-1)]
	if _, retryAfter := state.answer(); retryAfter > 0 {
		delay = min(retryAfter, retryAfterCap)
	}
	time.Sleep(delay)
}

// unknownOutcome is returned once the budget is spent without a definite
// answer. It reads as settlement_pending to the SDK and to the gate's error
// handler, which turns it into a 503, never a 402.
func (c *facilitatorClient) unknownOutcome(ctx context.Context, state *requestState) error {
	state.mu.Lock()
	alreadyLogged := state.settleUnknown
	state.settleUnknown = true
	tx := state.identity.preparedTxHash
	if tx == "" {
		tx = state.lastTx
	}
	payer, hash := state.lastPayer, state.identity.canonicalHash
	calls := state.settleCalls
	state.mu.Unlock()
	if !alreadyLogged { // the SDK's own retry arrives here too: log once
		c.logger.WarnContext(ctx, "settle outcome unknown: do not pay again, retry the same payment",
			slog.String("canonicalHash", hash), slog.Int("settleCalls", calls))
	}
	return x402.NewSettleError(cantonmech.ErrSettlementPending, payer, cantonmech.CantonMainnetCAIP2, tx, "settle outcome unknown")
}
