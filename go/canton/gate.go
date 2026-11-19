package canton

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/flushwalk"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/paidpath"
	cantonmech "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
)

// gate is the outer wrapper around the SDK middleware. The SDK decides
// verify/handler/settle; the gate adds what the SDK cannot see: the payment
// lock, the handler's status, and the shape of every error answer.
type gate struct {
	httpServer       *x402http.HTTPServer
	sdk              func(http.Handler) http.Handler
	ledger           *ledger
	logger           *slog.Logger
	routes           looseRouteIndex
	timeout          time.Duration
	settleTimeout    time.Duration
	settleCalls      int
	settleOnRedirect bool
}

// drainBound is how much unread request body the gate reads after a paid
// handler returns: the same bound net/http uses. Reading to EOF is what makes
// net/http watch the connection, so a payer who disconnects during settle is
// seen before the claim.
const drainBound = 256 << 10

// wrap returns the middleware for a next handler.
func (g *gate) wrap(next http.Handler) http.Handler {
	sdkHandler := g.sdk(g.handlerWrapper(next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probe := paidpath.ProbeFrom(r.Context()); probe != nil {
			// An adapter's guard: which key methods are charged or refused for this
			// path, in any spelling. A superset of the SDK's match for literal keys.
			// Merge, and pass the probe on: a canton gate composed inside adds its own
			// keys. ask's next is a no-op, so nothing else runs for a probe.
			probe.Answered = true
			for method := range g.routes.gatedMethods(r.URL.Path) {
				if probe.Methods == nil {
					probe.Methods = map[string]bool{}
				}
				probe.Methods[method] = true
			}
			next.ServeHTTP(w, r)
			return
		}
		if !g.httpServer.RequiresPayment(g.requestContext(r)) {
			if g.routes.looksLikePaidRoute(r) {
				// A spelling the SDK does not gate but a loose router might serve
				// as the paid route: refuse it rather than serve paid content free.
				writeGateError(w, http.StatusNotFound, msgNotFound)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodHead && r.Header.Get(headerPaymentSig) != "" {
			// A HEAD cannot carry the paid response, so it must not spend the payment.
			w.Header().Set("Allow", http.MethodGet)
			writeGateError(w, http.StatusMethodNotAllowed, msgHeadWithPayment)
			return
		}
		g.servePaid(w, r, sdkHandler)
	})
}

func (g *gate) requestContext(r *http.Request) x402http.HTTPRequestContext {
	return x402http.HTTPRequestContext{
		Adapter:     nethttp.NewNetHTTPAdapter(r),
		Path:        r.URL.EscapedPath(),
		DecodedPath: r.URL.Path,
		Method:      r.Method,
	}
}

func (g *gate) servePaid(w http.ResponseWriter, r *http.Request, sdkHandler http.Handler) {
	if !g.ledger.enterRequest(r.Header.Get(headerPaymentSig) != "") {
		// This gate is closed (a reload swapped it out): nothing was verified or
		// settled here, and the retry of the same payment reaches the new gate.
		writeGateError(w, http.StatusServiceUnavailable, msgPaymentInProgress)
		return
	}
	defer g.ledger.exit()
	state := &requestState{requestCtx: r.Context(), token: newToken(), outer: w, budget: g.budgetFor(r)}
	var release func()
	if header := r.Header.Get(headerPaymentSig); header != "" {
		identity, outcome := parsePaymentHeader(header)
		switch outcome {
		case headerNoHash:
			writeGateError(w, http.StatusBadRequest, msgNoHash)
			return
		case headerReadable:
			state.identity, state.hasIdentity = identity, true
			var answered bool
			release, answered = g.guardPayment(w, r, state)
			if answered {
				return
			}
			// A panic in the handler chain still releases the lock.
			defer func() {
				if release != nil {
					release()
				}
			}()
		case headerUnreadable: // the SDK answers a header it cannot read
		}
	}
	buffered := newBufferWriter()
	sdkHandler.ServeHTTP(buffered, r.WithContext(withState(r.Context(), state)))
	delivered := g.flush(w, state, buffered)
	if release != nil {
		finish := release
		release = nil
		g.finishRequest(state, delivered, finish)
	}
}

// releaseWait is how long the response path waits for the post-response Store
// work (the delivered mark and the lock release). A slow Store must not delay
// the end of a response the client already has: past this the work goes on in
// the background. A variable so tests can use a small value.
var releaseWait = 100 * time.Millisecond

// finishRequest marks a delivered claim and releases the lock after the
// response was written. With a fast Store this completes before the handler
// returns; with a slow one it continues in the background.
func (g *gate) finishRequest(state *requestState, delivered bool, release func()) {
	state.mu.Lock()
	updateID := state.claimedTx
	state.mu.Unlock()
	g.ledger.background(func() {
		if delivered && updateID != "" {
			g.ledger.markDelivered(state.requestCtx, updateID, state.token)
		}
		release()
	}, releaseWait)
}

// budgetFor is the longest a paid request to this route may take: the larger of
// Timeout and the payment's own lifetime, then every settle call, the pauses
// between them, and the Store calls after settle. The in-flight lock and the
// write deadline both use it.
func (g *gate) budgetFor(r *http.Request) time.Duration {
	lifetime := g.routes.executeBefore(r)
	return max(g.timeout, lifetime) + time.Duration(g.settleCalls)*g.settleTimeout +
		time.Duration(max(g.settleCalls-1, 0))*retryAfterCap + postVerifyStoreCalls*storeCallTimeout
}

// guardPayment applies the one-delivery rules before /verify: a delivered
// payment is refused, a payment in flight is told to wait, and otherwise the
// in-flight lock is taken. The returned release must run when the request ends.
func (g *gate) guardPayment(w http.ResponseWriter, r *http.Request, state *requestState) (release func(), answered bool) {
	ctx, cancel := storeContext(r.Context())
	defer cancel()
	hash := state.identity.canonicalHash

	if g.answerIfDeliveredOrFailed(ctx, w, state) {
		return nil, true
	}
	acquired, err := g.ledger.lock(ctx, hash, state.token, state.budget)
	if err != nil {
		g.logStoreError(ctx, "lock", hash, err)
		g.writeStoreUnavailable(w, state, err)
		return nil, true
	}
	if !acquired {
		writeGateError(w, http.StatusServiceUnavailable, msgPaymentInProgress)
		return nil, true
	}
	release = func() { g.ledger.unlock(ctx, hash, state.token, state.budget) }
	// A request that finished between the first read and the lock may have
	// recorded or delivered the payment: look again under the lock.
	if g.answerIfDeliveredOrFailed(ctx, w, state) {
		release()
		return nil, true
	}
	return release, false
}

// answerIfDeliveredOrFailed reads the record of the payment. It answers 402 for
// a delivered payment and 503 for a Store failure, and returns true then. A
// payment that was attempted or settled but not delivered is noted in the
// state, so /verify can be skipped for the very same bytes.
func (g *gate) answerIfDeliveredOrFailed(ctx context.Context, w http.ResponseWriter, state *requestState) bool {
	hash := state.identity.canonicalHash
	record, found, err := g.ledger.lookup(ctx, hash)
	if err == nil && found && record.State == StateSettled && record.UpdateID != "" {
		var ownership claimOwnership
		if ownership, err = g.ledger.claimState(ctx, record.UpdateID); err == nil {
			switch ownership {
			case claimDelivered:
				g.writeRedeemed(w, record.UpdateID)
				return true
			case claimHeld:
				// Claimed by a request that has not confirmed its delivery: not the final
				// 402. The payer retries; a delivered claim is marked, a dead one is not.
				g.writeOutcomeUnknown(w, record.UpdateID)
				return true
			case claimNone:
			}
		}
	}
	if err != nil {
		g.logStoreError(ctx, "read", hash, err)
		g.writeStoreUnavailable(w, state, err)
		return true
	}
	if found {
		state.prior = &record
	} else {
		state.prior = nil // a record seen before the lock may be gone after it
	}
	return false
}

// writeStoreUnavailable answers a Store failure before /verify. A full memory
// store means nothing was charged. Otherwise the gate cannot know the state of
// the payment, so it never says "nothing was settled": it tells the payer not to
// pay again, and names the updateId when it already holds a settled record.
func (g *gate) writeStoreUnavailable(w http.ResponseWriter, state *requestState, err error) {
	switch {
	case state.prior != nil: // a recorded payment may have moved, whatever the Store says
		g.writeOutcomeUnknown(w, firstNonEmpty(state.prior.UpdateID, state.identity.preparedTxHash))
	case errors.Is(err, errMemoryStoreFull):
		writeGateError(w, http.StatusServiceUnavailable, msgStoreFull)
	default:
		writeGateError(w, http.StatusServiceUnavailable, msgStoreUnavailable)
	}
}

// postVerifyStoreCalls is how many Store calls one paid request can make after
// the lock: the look under the lock, read and write in BeforeSettle, read and
// write in AfterSettle, the claim, its release, and the lock release.
const postVerifyStoreCalls = 8

// preLockStoreCalls is how many Store calls come before the lock: the first look
// and the lock itself, plus one read-back. They count for a proxy's read timeout
// and the shutdown grace, not for the lock's lifetime.
const preLockStoreCalls = 3

func (g *gate) logStoreError(ctx context.Context, operation, hash string, err error) {
	g.logger.ErrorContext(ctx, "store call failed", slog.String("operation", operation),
		slog.String("canonicalHash", hash), slog.String("error", err.Error()))
}

// flush writes the buffered SDK answer, after rewriting the one answer the SDK
// gets wrong: a /verify outage is a 503, not a 402. If the write or the flush
// fails, the response was not delivered, so the claim is released and the
// retry of the same payment is served once.
func (g *gate) flush(w http.ResponseWriter, state *requestState, buffered *bufferWriter) bool {
	state.mu.Lock()
	verifyUnavailable := state.verifyUnavailable
	state.mu.Unlock()
	if verifyUnavailable && buffered.finalStatus() == http.StatusPaymentRequired {
		writeGateError(w, http.StatusServiceUnavailable, msgVerifyUnavailable)
		return false
	}
	for name, values := range buffered.header {
		w.Header()[name] = values
	}
	status := buffered.finalStatus()
	if status == http.StatusNoContent || status == http.StatusNotModified {
		w.Header().Del("Content-Length")
	} else {
		// The gate holds the whole body: with its length the response is complete for
		// an HTTP/1.1 client at the Flush below, before any Store work that follows.
		w.Header().Set("Content-Length", strconv.Itoa(buffered.body.Len()))
	}
	w.WriteHeader(status)
	var writeErr error
	if buffered.body.Len() > 0 {
		_, writeErr = w.Write(buffered.body.Bytes())
	}
	// Flush through the whole wrapper chain: ResponseController stops at the first
	// writer with a Flush method, whose error return is nothing, and a failed
	// write to the client would look delivered.
	seen, flushErr := flushwalk.Flush(w)
	state.mu.Lock()
	claimed := state.claimedTx != ""
	state.mu.Unlock()
	if !seen && (claimed || !paidpath.Synthetic(state.requestCtx)) {
		// Not an unpaid offer written into a synthetic writer (a startup check, an
		// adapter's probe): that says nothing about the connection.
		g.warnBlindFlush(state.requestCtx, w)
	}
	if reachedClient(writeErr) {
		writeErr = nil
	}
	if err := errors.Join(writeErr, flushErr); err != nil {
		g.releaseUndelivered(state, err)
		return false
	}
	return true
}

// warnBlindFlush says once per process that the gate cannot see the connection,
// and what that costs.
func (g *gate) warnBlindFlush(ctx context.Context, w http.ResponseWriter) {
	if blindFlushWarned.CompareAndSwap(false, true) {
		g.logger.WarnContext(ctx, "the gate cannot see the connection: a ResponseWriter wrapper in front of it has no Unwrap() http.ResponseWriter "+
			"(or the writer cannot flush), so a failed write after settle counts as delivered and the payer's retry is refused as redeemed. "+
			"Add Unwrap to the wrapper, or put the gate outermost",
			slog.String("writerType", fmt.Sprintf("%T", w)))
	}
}

// blindFlushWarned makes warnBlindFlush log once per process.
var blindFlushWarned atomic.Bool

// reachedClient is true for the write error that says the answer carried nothing
// to send: a body written after a 204 or 304. That is not a failed delivery, and
// the claim stays.
func reachedClient(writeErr error) bool {
	return errors.Is(writeErr, http.ErrBodyNotAllowed)
}

// releaseUndelivered gives the claim back after a failed write.
func (g *gate) releaseUndelivered(state *requestState, cause error) {
	state.mu.Lock()
	updateID := state.claimedTx
	state.mu.Unlock()
	if updateID == "" {
		return
	}
	ctx := state.requestCtx
	g.logger.ErrorContext(ctx, "response write failed after claim: claim released, a retry of the same payment is served once",
		slog.String("canonicalHash", state.identity.canonicalHash), slog.String("updateId", updateID), slog.String("error", cause.Error()))
	g.ledger.unclaim(ctx, updateID, state.token)
}

// handlerWrapper runs inside the SDK middleware. It gives the paid handler a
// private writer so it can read the final status, which hooks cannot see, and
// it keeps the handler's headers away from every gate error.
func (g *gate) handlerWrapper(next http.Handler) http.Handler {
	return http.HandlerFunc(func(sdkWriter http.ResponseWriter, r *http.Request) {
		state := stateFrom(r.Context())
		if !state.isVerified() {
			// Fail closed: the handler runs only for a payment /verify passed (or one
			// this merchant recorded). If the SDK ever calls the handler for a gated
			// route without that, nothing is delivered.
			g.logger.ErrorContext(r.Context(), "paid handler reached without a verified payment: refused", slog.String("path", r.URL.Path))
			writeGateError(sdkWriter, http.StatusBadGateway, msgNotVerified)
			return
		}
		handlerWriter := newBufferWriter()
		next.ServeHTTP(handlerWriter, r)
		// An unread body hides a disconnect: net/http watches the connection only
		// after the body hit EOF. Read what is left, bounded, before settle.
		if r.Body != nil { // a hand-built request in a unit test may have none
			_, _ = io.CopyN(io.Discard, r.Body, drainBound)
		}
		status := handlerWriter.finalStatus()

		refusal := status < http.StatusOK || status > maxHTTPStatus || status >= http.StatusBadRequest ||
			(status >= http.StatusMultipleChoices && !g.settleOnRedirect)
		switch {
		case refusal && state.prior != nil:
			// The retry of a payment that may already have moved: a bare 4xx, 5xx or
			// redirect would hide the charge and invite a second payment. The record
			// is kept, and the payer is told not to pay again.
			g.logger.WarnContext(r.Context(), "paid handler refused the retry of a recorded payment: answered 503, not passed through",
				slog.Int("handlerStatus", status), slog.String("canonicalHash", state.identity.canonicalHash))
			g.writeOutcomeUnknown(sdkWriter, firstNonEmpty(state.prior.UpdateID, state.identity.preparedTxHash))
		case status < http.StatusOK || status > maxHTTPStatus:
			g.logger.ErrorContext(r.Context(), "paid handler wrote an invalid status: not settled", slog.Int("status", status))
			writeGateError(sdkWriter, http.StatusBadGateway, msgInvalidStatus)
		case status >= http.StatusBadRequest:
			// Not settled by the SDK: the handler's answer passes through unchanged.
			copyHeader(sdkWriter.Header(), handlerWriter.header)
			sdkWriter.WriteHeader(status)
			_, _ = sdkWriter.Write(handlerWriter.body.Bytes())
		case status >= http.StatusMultipleChoices && !g.settleOnRedirect:
			g.logger.WarnContext(r.Context(), "paid route answered a redirect: not settled",
				slog.Int("status", status), slog.String("path", r.URL.Path))
			writeGateError(sdkWriter, http.StatusBadGateway, msgRedirectNotCharged)
		default:
			// The SDK settles after this returns. Handler headers wait in the
			// state and are added only if settle succeeds.
			body := handlerWriter.body.Bytes()
			if status == http.StatusNoContent || status == http.StatusNotModified {
				// No body is allowed: drop it, as net/http does, so the write cannot fail
				// after the payer already has the answer.
				body = nil
			}
			// The gate holds the whole body: a Content-Length of the handler's own
			// could disagree with it, and net/http would then refuse or truncate the
			// write after the payer was charged. net/http frames the real length.
			handlerWriter.header.Del("Content-Length")
			state.mu.Lock()
			state.handlerHeader = handlerWriter.header.Clone()
			state.mu.Unlock()
			sdkWriter.WriteHeader(status)
			_, _ = sdkWriter.Write(body)
		}
	})
}

// maxHTTPStatus is the largest status code net/http accepts.
const maxHTTPStatus = 999

// onSettled runs inside the SDK after a successful settle. It adds the
// handler's own headers to the response, and merges private into whatever
// Cache-Control the handler set.
func (g *gate) onSettled(w http.ResponseWriter, r *http.Request, _ *x402.SettleResponse) {
	state := stateFrom(r.Context())
	if state == nil {
		return
	}
	state.mu.Lock()
	handlerHeader := state.handlerHeader
	state.mu.Unlock()

	header := w.Header()
	for name, values := range handlerHeader {
		if canonical := http.CanonicalHeaderKey(name); canonical != http.CanonicalHeaderKey(headerPaymentResponse) && canonical != http.CanonicalHeaderKey(headerPaymentRequired) {
			header[canonical] = append([]string(nil), values...)
		}
	}
	handlerCacheControl := strings.Join(handlerHeader.Values(headerCacheControl), ", ")
	header.Set(headerCacheControl, x402http.WithPrivateCacheControl(handlerCacheControl))
}

// onSettleFailed runs inside the SDK when settle failed. It decides the status
// and body of every settle-failure answer; the handler's headers never reach it.
func (g *gate) onSettleFailed(w http.ResponseWriter, r *http.Request, _ error) {
	state := stateFrom(r.Context())
	scrubToGateHeaders(w.Header())
	if state == nil {
		writeGateError(w, http.StatusPaymentRequired, x402.ErrCodeSettlementFailed)
		return
	}
	snapshot := state.view()
	prepared := snapshot.identity.preparedTxHash
	if prepared == "" {
		prepared = snapshot.lastTx
	}

	switch {
	case snapshot.notCharged && snapshot.prior == nil:
		writeGateError(w, http.StatusServiceUnavailable, snapshot.notChargedMsg)
	case snapshot.notCharged:
		g.writeOutcomeUnknown(w, firstNonEmpty(snapshot.prior.UpdateID, prepared))
	case snapshot.settleUnknown:
		g.writeOutcomeUnknown(w, prepared)
	case snapshot.withheld:
		g.writeOutcomeUnknown(w, snapshot.withheldTx)
	case snapshot.redeemed:
		g.writeRedeemed(w, snapshot.redeemedTx)
	default:
		writeGateError(w, http.StatusPaymentRequired, x402.ErrCodeSettlementFailed)
	}
}

// writeOutcomeUnknown answers 503: money may have moved, so the payer is told
// not to pay again and to resend the same payment.
func (g *gate) writeOutcomeUnknown(w http.ResponseWriter, transaction string) {
	g.setPaymentResponse(w, x402.SettleResponse{
		Success: false, ErrorReason: cantonmech.ErrSettlementPending, Transaction: transaction, Network: cantonmech.CantonMainnetCAIP2,
	})
	writeGateError(w, http.StatusServiceUnavailable, msgOutcomeUnknown)
}

func (g *gate) writeRedeemed(w http.ResponseWriter, updateID string) {
	g.setPaymentResponse(w, x402.SettleResponse{
		Success: false, ErrorReason: errReasonAlreadyRedeemed, Transaction: updateID, Network: cantonmech.CantonMainnetCAIP2,
	})
	writeGateError(w, http.StatusPaymentRequired, errReasonAlreadyRedeemed)
}

func (g *gate) setPaymentResponse(w http.ResponseWriter, response x402.SettleResponse) {
	headers, err := g.httpServer.CreateSettlementHeaders(&response)
	if err != nil {
		g.logger.Error("cannot encode PAYMENT-RESPONSE", slog.String("error", err.Error()))
		return
	}
	for name, value := range headers {
		w.Header().Set(name, value)
	}
}

// looseRouteIndex remembers where paid routes live under a loose spelling: case
// folded, dot segments and repeated slashes resolved, trailing slash dropped.
// Each entry carries the route's payment lifetime in seconds.
type looseRouteIndex map[string]int

func looseKey(method, urlPath string) string {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	// A path net/http decoded once can still carry escapes that a second decode
	// (echo's EnablePathUnescaping, e.Static with EnablePathUnescapingStaticFiles)
	// turns into a paid path. Keys have no "%", so only lookups change.
	for i := 0; i < 3 && strings.Contains(urlPath, "%"); i++ {
		decoded, err := url.PathUnescape(urlPath)
		if err != nil || decoded == urlPath {
			break
		}
		urlPath = decoded
	}
	return method + " " + strings.ToLower(path.Clean("/"+urlPath))
}

// gatedMethods are the key methods the gate charges or refuses for a path:
// the loose key of the path, and of the path cut at "?" or "#" (the SDK cuts a
// decoded path there before it matches).
func (idx looseRouteIndex) gatedMethods(urlPath string) map[string]bool {
	cut := urlPath
	if i := strings.IndexAny(urlPath, "?#"); i >= 0 {
		cut = urlPath[:i]
	}
	var out map[string]bool
	for _, method := range paidpath.KeyMethods() {
		_, full := idx[looseKey(method, urlPath)]
		_, short := idx[looseKey(method, cut)]
		if full || short {
			if out == nil {
				out = map[string]bool{}
			}
			out[method] = true
		}
	}
	return out
}

func (idx looseRouteIndex) looksLikePaidRoute(r *http.Request) bool {
	_, found := idx[looseKey(r.Method, r.URL.Path)]
	return found
}

// executeBefore is the payment lifetime of the route the request matched. A
// path that does not map one-to-one (an odd escape) gets the largest lifetime.
func (idx looseRouteIndex) executeBefore(r *http.Request) time.Duration {
	if seconds, found := idx[looseKey(r.Method, r.URL.Path)]; found {
		return time.Duration(seconds) * time.Second
	}
	longest := 0
	for _, seconds := range idx {
		longest = max(longest, seconds)
	}
	return time.Duration(longest) * time.Second
}
