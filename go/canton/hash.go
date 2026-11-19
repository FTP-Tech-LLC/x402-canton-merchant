package canton

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/x402-foundation/x402/go/v2/types"
)

const (
	// multihashPrefix is the Canton hash prefix (sha-256, 32 bytes) that some
	// clients keep on the prepared transaction hash and others strip.
	multihashPrefix = "1220"
	// prefixedHashLength is the length of a prefixed hash: 4 + 64 hex characters.
	prefixedHashLength = 68
	tokenBytes         = 16
)

// canonicalHashPattern is the only shape a canonical hash may have. It also
// bounds the length of the key the client's value becomes in a Store.
var canonicalHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// canonicalHash normalizes a preparedTxHash so every spelling of one payment
// maps to one key: trimmed, lower-case, and a leading "1220" removed only when
// the value is 68 characters long.
func canonicalHash(preparedTxHash string) string {
	hash := strings.ToLower(strings.TrimSpace(preparedTxHash))
	if len(hash) == prefixedHashLength && strings.HasPrefix(hash, multihashPrefix) {
		return hash[len(multihashPrefix):]
	}
	return hash
}

// paymentIdentity is what the gate reads from a PAYMENT-SIGNATURE header.
type paymentIdentity struct {
	preparedTxHash string // trimmed, lower-case, as the facilitator reports it
	canonicalHash  string
}

// headerOutcome says what the gate does with a PAYMENT-SIGNATURE header.
type headerOutcome int

const (
	// headerUnreadable: the SDK cannot read it either, so the SDK answers.
	headerUnreadable headerOutcome = iota
	// headerReadable: the SDK reads it and it carries a usable hash.
	headerReadable
	// headerNoHash: the SDK reads it but payload.preparedTxHash is missing, not
	// a string, or not a 64-character hash. The facilitator would refuse it too.
	headerNoHash
)

// parsePaymentHeader derives the payment identity exactly the way the SDK
// reads the header and forwards the payload: standard base64, version 2, the
// SDK's own payload type, and Payload["preparedTxHash"] by that exact key. A
// parser that matched keys loosely would give the gate a different key than
// the facilitator uses, and the lock and the delivered check would be bypassed.
func parsePaymentHeader(header string) (paymentIdentity, headerOutcome) {
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return paymentIdentity{}, headerUnreadable
	}
	if version, err := types.DetectVersion(raw); err != nil || version != supportedVersion {
		return paymentIdentity{}, headerUnreadable
	}
	payload, err := types.ToPaymentPayload(raw)
	if err != nil {
		return paymentIdentity{}, headerUnreadable
	}
	prepared, ok := payload.Payload["preparedTxHash"].(string)
	if !ok {
		return paymentIdentity{}, headerNoHash
	}
	identity := paymentIdentity{preparedTxHash: strings.ToLower(strings.TrimSpace(prepared)), canonicalHash: canonicalHash(prepared)}
	if !canonicalHashPattern.MatchString(identity.canonicalHash) {
		return paymentIdentity{}, headerNoHash
	}
	return identity, headerReadable
}

// paymentDigest binds a record to the exact payload bytes and the requirements
// the SDK forwards to the facilitator (the same in /verify and /settle). The
// requirements are taken without extra.feePayer and extra.synchronizerId, the
// fields the facilitator injects and the SDK's own match leaves out: a
// /supported pinned after a fee-payer rotation must not make a withheld payment
// look like a different one.
func paymentDigest(payload, requirements []byte) string {
	return digestOf(payload, withoutDynamicExtra(requirements))
}

// legacyPaymentDigest is the digest over the requirements as sent, accepted for
// records written before the dynamic fields were left out.
func legacyPaymentDigest(payload, requirements []byte) string {
	return digestOf(payload, requirements)
}

func digestOf(payload, requirements []byte) string {
	sum := sha256.New()
	sum.Write(payload)
	sum.Write([]byte{0})
	sum.Write(requirements)
	return hex.EncodeToString(sum.Sum(nil))
}

// digestMatches reports whether a record was written for these bytes.
func digestMatches(recorded string, payload, requirements []byte) bool {
	return recorded != "" && (recorded == paymentDigest(payload, requirements) || recorded == legacyPaymentDigest(payload, requirements))
}

func withoutDynamicExtra(requirements []byte) []byte {
	var parsed map[string]interface{}
	if json.Unmarshal(requirements, &parsed) != nil {
		return requirements
	}
	if extra, ok := parsed["extra"].(map[string]interface{}); ok {
		delete(extra, "feePayer")
		delete(extra, "synchronizerId")
	}
	normalized, err := json.Marshal(parsed)
	if err != nil {
		return requirements
	}
	return normalized
}

// newToken is a per-request random value: the owner of a lock or a claim.
func newToken() string {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		panic("canton: no system randomness: " + err.Error()) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(buf)
}
