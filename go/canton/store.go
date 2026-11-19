package canton

import (
	"context"
	"strings"
	"time"
)

const (
	// recordTTL is how long settled, attempt and delivered records live. The
	// facilitator keeps its own record for 24 hours; 26 hours leaves a margin.
	recordTTL = 26 * time.Hour
	// storeCallTimeout bounds one Store call made after the request context may
	// already be gone (a payer who left, or the SDK deadline).
	storeCallTimeout = 10 * time.Second
)

// RecordState says how far a payment got.
type RecordState string

const (
	// StateAttempt: /settle was about to be called, or its outcome is unknown.
	// The payment may or may not have moved.
	StateAttempt RecordState = "attempt"
	// StateSettled: the facilitator reported the payment settled.
	StateSettled RecordState = "settled"
)

// Record is what the gate remembers about a payment, keyed by its canonical
// preparedTxHash.
type Record struct {
	State RecordState `json:"state"`
	// UpdateID is the ledger update of a settled payment; empty for an attempt.
	UpdateID string `json:"updateId,omitempty"`
	// Digest binds the record to the exact signed payload and route terms the
	// facilitator was given. A retry skips /verify only when it matches.
	Digest string `json:"digest"`
	// Payer is the payer the facilitator proved, for the handler on a retry.
	Payer string `json:"payer,omitempty"`
	// At is when an attempt was recorded (unix seconds). An attempt older than the
	// facilitator's own record can no longer be settled by a refusal.
	At int64 `json:"at,omitempty"`
}

// Store keeps the state that makes one payment produce one delivery:
//
//   - a lock per canonical hash while a request works on the payment, owned by
//     a per-request token;
//   - a Record per canonical hash: attempt, then settled with the updateId;
//   - a claim on the updateId, owned by a token, taken before the response is
//     written, and marked delivered after it was: the claim is the delivery.
//
// Implementations must be safe for concurrent use. Lock and Claim must be
// atomic: of two concurrent calls with the same key and different tokens exactly
// one returns true. A call whose token already holds the key may return true
// (a client library that re-sends a write after a lost reply would otherwise
// make the owner wait out the TTL); the gate also reads the holder back when a
// lock is refused. Unlock and Unclaim must delete only when the stored token equals the
// one given. Records and claims must live for the ttl given, at least.
//
// A Store never evicts a live lock, record or claim to make room: when it is
// full it returns an error, and the gate answers 503.
type Store interface {
	// Lock takes the in-flight lock for hash with the owner token.
	Lock(ctx context.Context, hash, token string, ttl time.Duration) (bool, error)
	// LockHolder returns the token that holds the lock, if any.
	LockHolder(ctx context.Context, hash string) (token string, found bool, err error)
	// Unlock releases the lock when token still owns it.
	Unlock(ctx context.Context, hash, token string) error
	// Record returns the record of a payment.
	Record(ctx context.Context, hash string) (Record, bool, error)
	// PutRecord stores or overwrites the record of a payment.
	PutRecord(ctx context.Context, hash string, record Record, ttl time.Duration) error
	// DeleteRecord removes the record of a payment.
	DeleteRecord(ctx context.Context, hash string) error
	// Claim marks updateId as delivered to token. It returns false when it was already claimed.
	Claim(ctx context.Context, updateID, token string, ttl time.Duration) (bool, error)
	// Claimant returns the token that claimed updateId, if any.
	Claimant(ctx context.Context, updateID string) (token string, found bool, err error)
	// Unclaim releases a claim when token still owns it.
	Unclaim(ctx context.Context, updateID, token string) error
	// MarkDelivered rewrites the claim of token to the delivered mark once the
	// response reached the client. Only a marked claim is a confirmed delivery.
	MarkDelivered(ctx context.Context, updateID, token string, ttl time.Duration) error
}

// deliveredPrefix starts the value of a claim whose response was delivered.
const deliveredPrefix = "delivered:"

func deliveredMark(token string) string { return deliveredPrefix + token }

func isDeliveredMark(value string) bool { return strings.HasPrefix(value, deliveredPrefix) }

// storeContext detaches a Store call from request cancellation: recording a
// settled payment must succeed even when the payer has left.
func storeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), storeCallTimeout)
}
