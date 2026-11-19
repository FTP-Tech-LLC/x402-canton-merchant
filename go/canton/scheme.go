package canton

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"

	cantonmech "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
	exactserver "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton/exact/server"
)

// Abort reasons the scheme gives the SDK. The gate's error handler turns each
// into its own answer; none of them reaches the payer as a 402 for money that
// may have moved.
const (
	abortPayerLeft      = "payer_left"
	abortStateWithheld  = "payment_delivery_withheld"
	abortSettleNotFinal = "settlement_pending"
)

// ftpScheme is the mirror scheme plus the FTP hooks. Embedding keeps the
// mirror untouched; the SDK's Register finds the hook providers by type
// assertion and attaches them to canton:mainnet/exact only.
type ftpScheme struct {
	*exactserver.ExactCantonScheme
	ledger *ledger
	logger *slog.Logger
}

var (
	_ x402.SchemeNetworkServer         = (*ftpScheme)(nil)
	_ x402.BeforeVerifyHookProvider    = (*ftpScheme)(nil)
	_ x402.AfterVerifyHookProvider     = (*ftpScheme)(nil)
	_ x402.BeforeSettleHookProvider    = (*ftpScheme)(nil)
	_ x402.AfterSettleHookProvider     = (*ftpScheme)(nil)
	_ x402.OnSettleFailureHookProvider = (*ftpScheme)(nil)
	_ x402.FacilitatorSupportValidator = (*ftpScheme)(nil)
)

func newFTPScheme(ledger *ledger, logger *slog.Logger) *ftpScheme {
	return &ftpScheme{ExactCantonScheme: exactserver.NewExactCantonScheme(), ledger: ledger, logger: logger}
}

// ValidateFacilitatorSupport makes a facilitator that cannot build a Canton 402
// a startup error instead of a payment-time one.
func (s *ftpScheme) ValidateFacilitatorSupport(_ x402.Network, kind types.SupportedKind, _ []string) error {
	return validateKindExtra(kind)
}

// BeforeVerifyHook skips /verify for a payment this merchant already attempted
// or saw settle, when the presented payload and requirements are the very
// bytes recorded. After the payment deadline /verify would answer expired and
// the payer would have paid for nothing. /settle still decides: the
// facilitator answers a repeat from its own record. Bytes that differ from the
// record, or a route with other terms, go through /verify as usual.
func (s *ftpScheme) BeforeVerifyHook() x402.BeforeVerifyHook {
	return func(ctx x402.VerifyContext) (*x402.BeforeHookResult, error) {
		state := stateFrom(ctx.Ctx)
		if state == nil || state.prior == nil || !digestMatches(state.prior.Digest, ctx.PayloadBytes, ctx.RequirementsBytes) {
			return nil, nil
		}
		return &x402.BeforeHookResult{
			Skip:             true,
			SkipVerifyResult: &x402.VerifyResponse{IsValid: true, Payer: state.prior.Payer},
		}, nil
	}
}

// AfterVerifyHook exposes the proven payer to the handler and lifts the write
// deadline. A payer from an isValid:false answer is never used: it can be a
// forged act_as. After a skipped /verify the payer is the one recorded when the
// payment was attempted.
func (s *ftpScheme) AfterVerifyHook() x402.AfterVerifyHook {
	return func(ctx x402.VerifyResultContext) (*x402.AfterVerifyResult, error) {
		state := stateFrom(ctx.Ctx)
		if state == nil || ctx.Result == nil || !ctx.Result.IsValid {
			return nil, nil
		}
		state.markVerified()
		if ctx.Result.Payer != "" {
			state.setPayer(ctx.Result.Payer)
		}
		s.extendWriteDeadline(ctx.Ctx, state)
		return nil, nil
	}
}

// extendWriteDeadline lets the paid request outlive http.Server.WriteTimeout.
// It runs only after /verify passed, so unpaid requests cannot hold a connection
// longer. A writer without deadline support is fine: OPERATIONS.md states the
// WriteTimeout the server needs then.
func (s *ftpScheme) extendWriteDeadline(ctx context.Context, state *requestState) {
	if state.outer == nil || state.budget <= 0 {
		return
	}
	err := http.NewResponseController(state.outer).SetWriteDeadline(timeNow().Add(state.budget))
	switch {
	case err == nil:
	case errors.Is(err, http.ErrNotSupported):
		warnNoWriteDeadline(s.logger, ctx, state.outer)
	default:
		s.logger.WarnContext(ctx, "cannot extend the write deadline", slog.String("error", err.Error()))
	}
}

// writeDeadlineWarned makes warnNoWriteDeadline log once per process.
var writeDeadlineWarned atomic.Bool

// warnNoWriteDeadline says why the gate cannot lift the write deadline. The
// usual cause is a ResponseWriter wrapper in front of the gate that does not
// implement Unwrap() http.ResponseWriter. With a WriteTimeout shorter than a
// paid request, a delivery that was claimed can then be lost.
func warnNoWriteDeadline(logger *slog.Logger, ctx context.Context, writer http.ResponseWriter) {
	if writeDeadlineWarned.CompareAndSwap(false, true) {
		logger.WarnContext(ctx, "the gate cannot lift the write deadline: the ResponseWriter does not support it, "+
			"usually a wrapper in front of the gate without Unwrap() http.ResponseWriter. "+
			"Put the gate outermost, add Unwrap to the wrapper, or set http.Server.WriteTimeout to at least canton.RequestBudget",
			slog.String("writerType", fmt.Sprintf("%T", writer)))
	}
}

// BeforeSettleHook decides whether /settle is called at all.
//
//   - Payer already gone: nothing is submitted; the payment expires unused.
//   - The gate holds a settled record for these very bytes: the payment moved,
//     so /settle is not asked (the facilitator may have forgotten it): the
//     recorded updateId is delivered once, after the claim.
//   - Otherwise the attempt is recorded before the first /settle call, so a
//     retry after an unknown outcome, a crash or a Store failure can skip /verify.
func (s *ftpScheme) BeforeSettleHook() x402.BeforeSettleHook {
	return func(ctx x402.SettleContext) (*x402.BeforeHookResult, error) {
		state := stateFrom(ctx.Ctx)
		if state == nil {
			return nil, nil
		}
		if state.payerGone() {
			state.markNotCharged(msgPayerLeft)
			s.logger.WarnContext(ctx.Ctx, "payer left before settle: not settled",
				slog.String("canonicalHash", state.identity.canonicalHash))
			return &x402.BeforeHookResult{Abort: true, Reason: abortPayerLeft, Message: "payer left before settle"}, nil
		}
		if !state.hasIdentity {
			return nil, nil
		}
		digest := paymentDigest(ctx.PayloadBytes, ctx.RequirementsBytes)
		if prior := state.prior; prior != nil && prior.State == StateSettled && prior.UpdateID != "" && digestMatches(prior.Digest, ctx.PayloadBytes, ctx.RequirementsBytes) {
			return &x402.BeforeHookResult{Skip: true, SkipResult: &x402.SettleResponse{
				Success: true, Transaction: prior.UpdateID, Payer: prior.Payer, Network: cantonmech.CantonMainnetCAIP2,
			}}, nil
		}
		attempt := Record{State: StateAttempt, Digest: digest, Payer: state.currentPayer(), At: timeNow().Unix()}
		if err := s.ledger.saveAttempt(ctx.Ctx, state.identity.canonicalHash, attempt); err != nil {
			// Settle goes on: the attempt is kept in process and the reconciler writes it
			// to the Store when it answers. The payer must not be refused for our Store.
			s.logger.ErrorContext(ctx.Ctx, "attempt not confirmed in the store: kept in process",
				slog.String("canonicalHash", state.identity.canonicalHash), slog.String("error", err.Error()))
		}
		return nil, nil
	}
}

// AfterSettleHook runs on success:true. It records the payment as settled and
// claims the updateId so the response is served once. Nothing is delivered
// without a claim: a Store failure, or a payer who looks gone, withholds the
// body and answers 503; the settled record lets the retry be served once.
func (s *ftpScheme) AfterSettleHook() x402.AfterSettleHook {
	return func(ctx x402.SettleResultContext) error {
		state := stateFrom(ctx.Ctx)
		if state == nil || ctx.Result == nil {
			return nil
		}
		updateID := ctx.Result.Transaction
		if updateID == "" {
			return s.failUnknown(ctx.Ctx, state, "success without a transaction")
		}
		record := Record{State: StateSettled, UpdateID: updateID, Payer: firstNonEmpty(ctx.Result.Payer, state.currentPayer()),
			Digest: paymentDigest(ctx.PayloadBytes, ctx.RequirementsBytes)}
		if err := s.saveSettled(ctx.Ctx, state, record); err != nil {
			return s.withhold(ctx.Ctx, state, updateID, err)
		}
		return s.claim(ctx.Ctx, state, updateID)
	}
}

// OnSettleFailureHook handles the facilitator's answers to a repeat. An
// already_settled answer becomes one delivery: its updateId is claimed and only
// the first claim is served. A definite refusal clears the attempt record.
func (s *ftpScheme) OnSettleFailureHook() x402.OnSettleFailureHook {
	return func(ctx x402.SettleFailureContext) (*x402.SettleFailureHookResult, error) {
		state := stateFrom(ctx.Ctx)
		var settleError *x402.SettleError
		if state == nil || !errors.As(ctx.Error, &settleError) {
			return nil, nil
		}
		digest := paymentDigest(ctx.PayloadBytes, ctx.RequirementsBytes)
		switch {
		case settleError.ErrorReason == errReasonAlreadySettled && settleError.Transaction != "":
			return s.recoverAlreadySettled(ctx.Ctx, state, settleError, digest), nil
		case settleError.ErrorReason != cantonmech.ErrSettlementPending && settleError.ErrorReason != errReasonAlreadySettled:
			s.clearAttempt(ctx.Ctx, state, ctx.PayloadBytes, ctx.RequirementsBytes)
		}
		return nil, nil
	}
}

func (s *ftpScheme) recoverAlreadySettled(ctx context.Context, state *requestState, answer *x402.SettleError, digest string) *x402.SettleFailureHookResult {
	updateID := answer.Transaction
	record := Record{State: StateSettled, UpdateID: updateID, Payer: firstNonEmpty(answer.Payer, state.currentPayer()), Digest: digest}
	if err := s.saveSettled(ctx, state, record); err != nil {
		_ = s.withhold(ctx, state, updateID, err)
		return nil
	}
	if state.payerGone() {
		_ = s.withhold(ctx, state, updateID, errPayerGone)
		return nil
	}
	claimed, err := s.ledger.claim(ctx, updateID, state.token)
	if err != nil {
		_ = s.withhold(ctx, state, updateID, err)
		return nil
	}
	result := &x402.SettleResponse{Success: claimed, Transaction: updateID, Payer: answer.Payer, Network: answer.Network}
	if claimed {
		state.setClaimed(updateID)
	} else {
		result.ErrorReason = errReasonAlreadyRedeemed
		state.markRedeemed(updateID)
	}
	return &x402.SettleFailureHookResult{Recovered: true, Result: result}
}

// claim takes the delivery claim for a fresh success.
func (s *ftpScheme) claim(ctx context.Context, state *requestState, updateID string) error {
	if state.payerGone() {
		return s.withhold(ctx, state, updateID, errPayerGone)
	}
	claimed, err := s.ledger.claim(ctx, updateID, state.token)
	if err != nil {
		return s.withhold(ctx, state, updateID, err)
	}
	if !claimed {
		state.markRedeemed(updateID)
		return x402.NewAfterSettleAbort(errReasonAlreadyRedeemed, "this payment was already delivered")
	}
	state.setClaimed(updateID)
	return nil
}

var errPayerGone = errors.New("the payer's request context is done")

// saveSettled records the settled payment. A request whose header carried no
// parsable hash has no hash; the updateId claim still guards it.
func (s *ftpScheme) saveSettled(ctx context.Context, state *requestState, record Record) error {
	if !state.hasIdentity {
		return nil
	}
	return s.ledger.saveSettled(ctx, state.identity.canonicalHash, record)
}

// attemptTrustWindow is how long a definite refusal from /settle proves that an
// unknown-outcome attempt never moved. The facilitator keeps its record for 24
// hours: after that it validates from scratch and answers "expired" even for a
// payment that committed.
const attemptTrustWindow = 23 * time.Hour

// clearAttempt removes this request's own attempt record after a definite
// refusal: nothing moved, so a retry goes through /verify again. An attempt
// older than the facilitator's own record is kept, and the payer is told the
// outcome is unknown.
func (s *ftpScheme) clearAttempt(ctx context.Context, state *requestState, payload, requirements []byte) {
	if !state.hasIdentity {
		return
	}
	hash := state.identity.canonicalHash
	record, found, err := s.ledger.lookup(ctx, hash)
	if err != nil || !found || record.State != StateAttempt || !digestMatches(record.Digest, payload, requirements) {
		return
	}
	if record.At > 0 && timeNow().Sub(time.Unix(record.At, 0)) > attemptTrustWindow {
		state.mu.Lock()
		state.settleUnknown = true
		state.mu.Unlock()
		s.logger.WarnContext(ctx, "refusal for an attempt older than the facilitator's record: outcome stays unknown",
			slog.String("canonicalHash", hash))
		return
	}
	s.ledger.clear(ctx, hash)
}

// withhold keeps the body back after money moved. The gate answers 503 with the
// updateId; the settled record lets a retry of the same payment be served once.
func (s *ftpScheme) withhold(ctx context.Context, state *requestState, updateID string, cause error) error {
	state.markWithheld(updateID)
	level := slog.LevelError
	if errors.Is(cause, errPayerGone) {
		level = slog.LevelWarn
	}
	s.logger.Log(ctx, level, "settled but not delivered: a retry of the same payment is served once; reconcile by updateId",
		slog.String("canonicalHash", state.identity.canonicalHash), slog.String("updateId", updateID), slog.String("cause", cause.Error()))
	return x402.NewAfterSettleAbort(abortStateWithheld, "payment delivery withheld")
}

func (s *ftpScheme) failUnknown(ctx context.Context, state *requestState, why string) error {
	state.mu.Lock()
	state.settleUnknown = true
	state.mu.Unlock()
	s.logger.WarnContext(ctx, "settle answer unusable: outcome unknown", slog.String("canonicalHash", state.identity.canonicalHash),
		slog.String("why", why))
	return x402.NewAfterSettleAbort(abortSettleNotFinal, why)
}

// validateKindExtra requires the two fields a Canton 402 cannot do without.
func validateKindExtra(kind types.SupportedKind) error {
	for _, field := range []string{"feePayer", "synchronizerId"} {
		if value, ok := kind.Extra[field].(string); !ok || value == "" {
			return fmt.Errorf("facilitator /supported kind has no extra.%s", field)
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
