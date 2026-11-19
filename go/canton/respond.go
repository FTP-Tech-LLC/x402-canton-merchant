package canton

import (
	"encoding/json"
	"net/http"
	"strconv"
)

const (
	// retryAfterSeconds is the Retry-After the gate sends with its 503 answers.
	retryAfterSeconds = "3"

	headerPaymentResponse = "PAYMENT-RESPONSE"
	headerPaymentRequired = "PAYMENT-REQUIRED"
	headerPaymentSig      = "PAYMENT-SIGNATURE"
	headerRetryAfter      = "Retry-After"
	headerCacheControl    = "Cache-Control"
	headerContentType     = "Content-Type"
	cacheControlNoStore   = "no-store"
	contentTypeJSON       = "application/json"
)

// Gate answers. The wording of the unknown-outcome and verify answers is the
// contract with clients: "do NOT pay again" tells a client not to start a new
// payment, and "retry the same payment" tells it to resend the same signature.
const (
	msgOutcomeUnknown     = "payment outcome unknown: do NOT pay again; retry the same payment"
	msgVerifyUnavailable  = "verification unavailable: nothing was settled; retry the same payment"
	msgPaymentInProgress  = "payment in progress: retry the same payment"
	msgStoreUnavailable   = "payment state unavailable: do NOT pay again; retry the same payment"
	msgStoreFull          = "payment state unavailable: nothing was charged; retry the same payment"
	msgNotVerified        = "payment not verified; nothing was charged"
	msgHeadWithPayment    = "send the payment with GET: a HEAD cannot receive the paid response"
	msgRedirectNotCharged = "paid route answered a redirect; nothing was charged"
	msgNotFound           = "not found"
	msgNoHash             = "payment header carries no valid preparedTxHash"
	msgPayerLeft          = "payer left before settle: nothing was charged; retry the same payment"
	msgInvalidStatus      = "paid handler wrote an invalid status; nothing was charged"
)

// gateAllowedHeaders are the only headers a gate-made error response keeps.
var gateAllowedHeaders = map[string]bool{
	http.CanonicalHeaderKey(headerPaymentResponse): true,
	http.CanonicalHeaderKey(headerPaymentRequired): true,
	http.CanonicalHeaderKey(headerRetryAfter):      true,
	http.CanonicalHeaderKey(headerCacheControl):    true,
	http.CanonicalHeaderKey(headerContentType):     true,
	"Content-Length": true, // the gate frames the body it holds
}

// scrubToGateHeaders drops every header that is not the gate's own. A paid
// handler may have set Set-Cookie or Content-Length; a gate error must never
// carry them.
func scrubToGateHeaders(header http.Header) {
	for name := range header {
		if !gateAllowedHeaders[http.CanonicalHeaderKey(name)] {
			delete(header, name)
		}
	}
}

// writeGateError writes a JSON gate answer: no-store, and Retry-After for 503.
// It leaves headers already on w alone; callers that hold a writer a paid
// handler touched scrub it first.
func writeGateError(w http.ResponseWriter, status int, message string) {
	header := w.Header()
	header.Set(headerContentType, contentTypeJSON)
	header.Set(headerCacheControl, cacheControlNoStore)
	if status == http.StatusServiceUnavailable {
		header.Set(headerRetryAfter, retryAfterSeconds)
	}
	body, _ := json.Marshal(map[string]string{"error": message})
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
