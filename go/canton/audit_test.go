package canton

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the findings of the two v0.1 audits (H1 to H6, M1 to M6 and the lows).

// signatureWithPayload builds a header for the given route from the gate's own
// 402, with the payload JSON written exactly as given (so tests can craft
// duplicate and differently cased keys).
func signatureWithPayload(t *testing.T, h http.Handler, method, target, payloadJSON string) string {
	t.Helper()
	w := do(h, method, target, "")
	require.Equal(t, 402, w.Code, w.Body.String())
	var required map[string]any
	decodeHeader(t, w.Header().Get(headerPaymentRequired), &required)
	accepted, err := json.Marshal(required["accepts"].([]any)[0])
	require.NoError(t, err)
	resource, err := json.Marshal(required["resource"])
	require.NoError(t, err)
	envelope := fmt.Sprintf(`{"x402Version":2,"resource":%s,"accepted":%s,"payload":%s}`, resource, accepted, payloadJSON)
	return base64.StdEncoding.EncodeToString([]byte(envelope))
}

func payloadJSON(hash string) string {
	return fmt.Sprintf(`{"assetTransferMethod":"transfer-factory","preparedTransaction":"H4sIAAAA","preparedTxHash":%q,"signature":"c2lnbmF0dXJl"}`, hash)
}

// ---------------------------------------------------------------- H1

func TestUnknownOutcomeThenRetryPastTheDeadlineIsServed(t *testing.T) {
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n <= 4 {
			w.WriteHeader(502) // the first request's whole budget: unknown
			return
		}
		settleAlreadySettled(w, testUpdateID) // the facilitator's record answers the retry
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)
	first := do(h, "GET", "/paid", sig)
	require.Equal(t, 503, first.Code)

	// By the retry executeBefore has passed: /verify would say expired.
	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	verifies := f.verifyCalls.Load()
	retry := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, retry.Code, "charged, so served once")
	assert.Equal(t, testUpdateID, paymentResponse(t, retry)["transaction"])
	assert.Equal(t, verifies, f.verifyCalls.Load(), "the retry skips /verify and lets /settle decide")
	assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code, "and only once")
}

func TestAttemptIsRecordedBeforeTheFirstSettleCall(t *testing.T) {
	f := startMock(t)
	store := NewMemoryStore()
	var seen Record
	var seenFound bool
	f.settle = func(_ int32, w http.ResponseWriter) {
		seen, seenFound, _ = store.Record(context.Background(), testPreparedTx) // the process could die right here
		settleOK(w)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	require.True(t, seenFound, "the attempt is in the store while /settle is in flight")
	assert.Equal(t, StateAttempt, seen.State)
	assert.NotEmpty(t, seen.Digest)
	assert.Equal(t, testPayer, seen.Payer)
}

func TestRetryAfterACrashMidSettleIsServed(t *testing.T) {
	// The first process died after the facilitator committed. The second reads
	// the attempt from the shared store; /verify says expired.
	f := startMock(t)
	redis := newFakeRedis()
	crash := make(chan struct{})
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			dropConnection(w) // the process is gone; the lock stays until its TTL
			close(crash)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	shared := func(c *Config) { c.Store = NewRedisStore(redis, "x:"); c.SettleCalls = 1 }
	a := newGate(t, f, okHandler(new(atomic.Int32)), shared)
	sig := signatureFor(t, a, testPreparedTx)
	require.Equal(t, 503, do(a, "GET", "/paid", sig).Code)
	<-crash

	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	b := newGate(t, f, okHandler(new(atomic.Int32)), shared)
	assert.Equal(t, 200, do(b, "GET", "/paid", sig).Code)
}

func TestAttemptFallsBackToProcessMemoryWhenTheStoreWriteFails(t *testing.T) {
	f := startMock(t)
	store := newFaultyStore(NewMemoryStore())
	store.breakOp("put", true)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n <= 4 {
			w.WriteHeader(502)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 503, do(h, "GET", "/paid", sig).Code)

	store.breakOp("put", false)
	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "the in-process record let the retry skip /verify")
}

func TestStoreFailureAfterSettleThenRetryIsServed(t *testing.T) {
	// The settled write failed: 503. The retry, after the deadline, is still served.
	f := startMock(t)
	store := newFaultyStore(NewMemoryStore())
	store.breakOp("putSettled", true)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 503, do(h, "GET", "/paid", sig).Code)

	store.breakOp("putSettled", false)
	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
}

func TestDefiniteRefusalClearsTheAttempt(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) { settleRefused(w, "invalid_exact_canton_insufficient_balance") }
	store := NewMemoryStore()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	require.Equal(t, 402, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code)
	_, found, _ := store.Record(context.Background(), testPreparedTx)
	assert.False(t, found, "nothing moved: the next try goes through /verify")
}

// ---------------------------------------------------------------- M2, M3

func TestSkipVerifyOnlyForTheVeryBytesThatWereAttempted(t *testing.T) {
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n <= 4 {
			w.WriteHeader(502)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	good := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 503, do(h, "GET", "/paid", good).Code)
	hits.Store(0)

	// Junk bytes claiming the same hash: /verify runs and refuses them.
	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_signature_invalid") }
	junk := signatureWithPayload(t, h, "GET", "/paid", strings.Replace(payloadJSON(testPreparedTx), "c2lnbmF0dXJl", "Zm9yZ2Vk", 1))
	verifies := f.verifyCalls.Load()
	w := do(h, "GET", "/paid", junk)
	assert.Equal(t, 402, w.Code)
	assert.Equal(t, verifies+1, f.verifyCalls.Load(), "different bytes are verified, not skipped")
	assert.Equal(t, int32(0), hits.Load(), "the paid handler did not run for junk")

	// The real bytes still skip.
	assert.Equal(t, 200, do(h, "GET", "/paid", good).Code)
}

func TestDigestBindsPayloadAndRequirements(t *testing.T) {
	base := paymentDigest([]byte("payload"), []byte("requirements"))
	assert.NotEqual(t, base, paymentDigest([]byte("payload2"), []byte("requirements")))
	assert.NotEqual(t, base, paymentDigest([]byte("payload"), []byte("requirements2")))
	assert.NotEqual(t, paymentDigest([]byte("ab"), []byte("c")), paymentDigest([]byte("a"), []byte("bc")))
	assert.Equal(t, base, paymentDigest([]byte("payload"), []byte("requirements")))
}

func TestVerifyAndSettleSeeTheSameDigest(t *testing.T) {
	// If /verify and /settle bytes ever differed the skip would never match.
	f := startMock(t)
	store := NewMemoryStore()
	var attempt Record
	f.settle = func(_ int32, w http.ResponseWriter) {
		attempt, _, _ = store.Record(context.Background(), testPreparedTx)
		w.WriteHeader(502)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store; c.SettleCalls = 1 })
	sig := signatureFor(t, h, testPreparedTx)
	do(h, "GET", "/paid", sig)
	f.verify = func(_ int32, w http.ResponseWriter) { t.Error("the retry must skip /verify"); verifyOK(w) }
	f.settle = func(_ int32, w http.ResponseWriter) { settleAlreadySettled(w, testUpdateID) }
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
	assert.NotEmpty(t, attempt.Digest)
}

func TestSkipVerifyPathGivesTheHandlerTheRecordedPayer(t *testing.T) {
	f := startMock(t)
	ctx, leave := context.WithCancel(context.Background())
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			leave()
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	var seen []string
	h := newGate(t, f, func(w http.ResponseWriter, r *http.Request) {
		payer, _ := PayerFrom(r.Context())
		seen = append(seen, payer)
		_, _ = w.Write([]byte("ok"))
	})
	sig := signatureFor(t, h, testPreparedTx)
	doCtx(ctx, h, "GET", "/paid", sig)
	f.verify = func(_ int32, w http.ResponseWriter) { t.Error("must skip"); verifyOK(w) }
	require.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
	assert.Equal(t, []string{testPayer, testPayer}, seen, "the retry's handler sees the payer too")
}

// ---------------------------------------------------------------- H2

// deadlineRecorder is a ResponseWriter that supports write deadlines and can
// fail its flush, like a net/http connection.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
	flushErr  error
	writeErr  error
}

func newDeadlineRecorder() *deadlineRecorder {
	return &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	d.deadlines = append(d.deadlines, t)
	d.mu.Unlock()
	return nil
}

func (d *deadlineRecorder) FlushError() error { return d.flushErr }

func (d *deadlineRecorder) Write(p []byte) (int, error) {
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	return d.ResponseRecorder.Write(p)
}

func serveWith(h http.Handler, w http.ResponseWriter, sig string) {
	r := httptest.NewRequest("GET", "/paid", nil)
	if sig != "" {
		r.Header.Set(headerPaymentSig, sig)
	}
	h.ServeHTTP(w, r)
}

func TestWriteDeadlineIsLiftedOnlyAfterVerifyPasses(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	sig := signatureFor(t, h, testPreparedTx)

	unpaid := newDeadlineRecorder()
	serveWith(h, unpaid, "")
	assert.Empty(t, unpaid.deadlines, "an unpaid request cannot hold a connection longer")

	f.verify = func(_ int32, w http.ResponseWriter) { verifyInvalid(w, "invalid_exact_canton_expired") }
	refused := newDeadlineRecorder()
	serveWith(h, refused, sig)
	assert.Empty(t, refused.deadlines, "nor a payment /verify refused")

	f.verify = func(_ int32, w http.ResponseWriter) { verifyOK(w) }
	paid := newDeadlineRecorder()
	before := time.Now()
	serveWith(h, paid, sig)
	require.Len(t, paid.deadlines, 1)
	want := 120*time.Second + 4*65*time.Second + 3*retryAfterCap + postVerifyStoreCalls*storeCallTimeout
	assert.WithinDuration(t, before.Add(want), paid.deadlines[0], 5*time.Second,
		"the deadline covers the full request budget")
}

func TestWriteDeadlineIsLiftedOnTheSkipVerifyPathToo(t *testing.T) {
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n <= 4 {
			w.WriteHeader(502)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	sig := signatureFor(t, h, testPreparedTx)
	do(h, "GET", "/paid", sig)
	retry := newDeadlineRecorder()
	serveWith(h, retry, sig)
	assert.Len(t, retry.deadlines, 1)
}

func TestWriterWithoutDeadlineSupportIsFine(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	assert.Equal(t, 200, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code) // httptest.ResponseRecorder: ErrNotSupported
}

func TestFailedWriteReleasesTheClaimSoTheRetryIsServedOnce(t *testing.T) {
	for _, failure := range []string{"flush", "write"} {
		t.Run(failure, func(t *testing.T) {
			f := startMock(t)
			logger, logs := capturingLogger()
			var hits atomic.Int32
			store := NewMemoryStore()
			h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Logger = logger; c.Store = store })
			sig := signatureFor(t, h, testPreparedTx)

			broken := newDeadlineRecorder()
			if failure == "flush" {
				broken.flushErr = errors.New("i/o timeout")
			} else {
				broken.writeErr = errors.New("write: broken pipe")
			}
			serveWith(h, broken, sig)
			_, claimed, _ := store.Claimant(context.Background(), testUpdateID)
			assert.False(t, claimed, "the claim was released: nothing was delivered")
			assert.True(t, logsContain(logs, "level=ERROR", "claim released", "updateId="+testUpdateID))

			f.settle = func(_ int32, w http.ResponseWriter) { settleAlreadySettled(w, testUpdateID) }
			retry := do(h, "GET", "/paid", sig)
			assert.Equal(t, 200, retry.Code, "the retry is served once")
			assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
		})
	}
}

// ---------------------------------------------------------------- H3

func TestIdentityIsReadByExactKeyLikeTheSDK(t *testing.T) {
	bare := strings.Repeat("ab", 32)
	shadow := strings.Repeat("cd", 32)
	cases := map[string]string{
		"shadow key with another value":  `{"preparedTxHash":"` + bare + `","preparedtxhash":"` + shadow + `"}`,
		"shadow key with a number":       `{"preparedTxHash":"` + bare + `","preparedtxhash":0}`,
		"shadow key before the real one": `{"preparedtxhash":"` + shadow + `","preparedTxHash":"` + bare + `"}`,
		"upper-case shadow key":          `{"preparedTxHash":"` + bare + `","PREPAREDTXHASH":"` + shadow + `"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			envelope := signatureForPayload(nil)
			raw, _ := base64.StdEncoding.DecodeString(envelope)
			var parsed map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &parsed))
			parsed["payload"] = json.RawMessage(payload)
			rebuilt, _ := json.Marshal(parsed)
			identity, outcome := parsePaymentHeader(base64.StdEncoding.EncodeToString(rebuilt))
			require.Equal(t, headerReadable, outcome)
			assert.Equal(t, bare, identity.canonicalHash, "the key the facilitator uses")
		})
	}
}

func TestShadowKeyReplayIsStillRefusedAsRedeemed(t *testing.T) {
	f := startMock(t)
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	real := signatureFor(t, h, testPreparedTx)
	require.Equal(t, 200, do(h, "GET", "/paid", real).Code)
	verifies := f.verifyCalls.Load()

	shadowed := strings.Replace(payloadJSON(testPreparedTx), `"signature"`, `"preparedtxhash":"`+strings.Repeat("11", 32)+`","signature"`, 1)
	w := do(h, "GET", "/paid", signatureWithPayload(t, h, "GET", "/paid", shadowed))
	assert.Equal(t, 402, w.Code)
	assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, w.Body.String())
	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, verifies, f.verifyCalls.Load())
}

func TestHeaderWithoutAUsableHashIsA400BeforeAnyLockOrVerify(t *testing.T) {
	bad := map[string]string{
		"missing":         `{"assetTransferMethod":"transfer-factory"}`,
		"only wrong case": `{"preparedtxhash":"` + testPreparedTx + `"}`,
		"number":          `{"preparedTxHash":5}`,
		"null":            `{"preparedTxHash":null}`,
		"empty":           `{"preparedTxHash":""}`,
		"short":           `{"preparedTxHash":"abcd"}`,
		"not hex":         `{"preparedTxHash":"` + strings.Repeat("zz", 32) + `"}`,
		"too long":        `{"preparedTxHash":"` + strings.Repeat("ab", 40) + `"}`,
		"huge":            `{"preparedTxHash":"` + strings.Repeat("a", 40000) + `"}`,
	}
	for name, payload := range bad {
		t.Run(name, func(t *testing.T) {
			f := startMock(t)
			store := NewMemoryStore()
			var hits atomic.Int32
			h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
			w := do(h, "GET", "/paid", signatureWithPayload(t, h, "GET", "/paid", payload))
			assert.Equal(t, 400, w.Code)
			assert.JSONEq(t, `{"error":"payment header carries no valid preparedTxHash"}`, w.Body.String())
			assert.Equal(t, int32(0), f.verifyCalls.Load())
			assert.Equal(t, int32(0), hits.Load())
			assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
			assert.Equal(t, 0, len(store.(*memoryStore).entries), "no lock was taken")
		})
	}
}

func TestPrefixedAndUpperCaseHashesStillWork(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	assert.Equal(t, 200, do(h, "GET", "/paid", signatureFor(t, h, "1220"+strings.ToUpper(testPreparedTx))).Code)
}

func TestSettledRecordIsNeverOverwrittenByAnotherPayment(t *testing.T) {
	ctx := context.Background()
	led := newLedger(NewMemoryStore(), nopLogger())
	original := Record{State: StateSettled, UpdateID: "u1", Digest: "d1", Payer: "p"}
	require.NoError(t, led.saveSettled(ctx, "h", original))
	require.ErrorIs(t, led.saveSettled(ctx, "h", Record{State: StateSettled, UpdateID: "u2", Digest: "d1"}), errRecordConflict, "another updateId")
	require.NoError(t, led.saveSettled(ctx, "h", Record{State: StateSettled, UpdateID: "u1", Digest: "d2"}),
		"the same updateId under another digest (another /supported pin): confirmed by the facilitator, no conflict")
	got, _, _ := led.lookup(ctx, "h")
	assert.Equal(t, original, got, "and the existing record is kept as it is")

	require.NoError(t, led.saveAttempt(ctx, "h2", Record{State: StateAttempt, Digest: "d1"}))
	require.NoError(t, led.saveAttempt(ctx, "h2", Record{State: StateAttempt, Digest: "other"}))
	got, _, _ = led.lookup(ctx, "h2")
	assert.Equal(t, "d1", got.Digest, "an attempt never replaces a record")
	require.NoError(t, led.saveSettled(ctx, "h2", Record{State: StateSettled, UpdateID: "u", Digest: "other"}), "a confirmed settle replaces an attempt")
	got, _, _ = led.lookup(ctx, "h2")
	assert.Equal(t, StateSettled, got.State)
}

// ---------------------------------------------------------------- H4

func TestClaimWhoseReplyWasLostIsStillDelivered(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	f.settle = func(_ int32, w http.ResponseWriter) {
		redis.loseNextReplies(1) // the claim's SET NX applies, then the reply is lost
		settleOK(w)
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	sig := signatureFor(t, h, testPreparedTx)
	w := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, w.Code, "our token on the key means we hold the claim")
	assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
	assert.Equal(t, int32(1), hits.Load())
}

func TestLockWhoseReplyWasLostIsHeldAndReleased(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	redis.loseNextReplies(1) // the lock's SET NX applies, then the reply is lost
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	sig := signatureFor(t, h, testPreparedTx)
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
	_, found, _ := redis.Get(context.Background(), "x:lock:"+testPreparedTx)
	assert.False(t, found, "the lock did not stay behind")
}

func TestLostReplyOnAnotherHoldersKeyIsNotMistakenForOurs(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	redis.mu.Lock()
	redis.values["x:claimed:"+testUpdateID] = fakeRedisValue{value: "delivered:someone-else", expires: time.Now().Add(time.Hour)}
	redis.mu.Unlock()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 402, w.Code)
	assert.JSONEq(t, `{"error":"payment_already_redeemed"}`, w.Body.String())
}

func TestUnlockDoesNotDeleteAnotherHoldersLock(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	key := "x:lock:" + testPreparedTx
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		// The first request's lock expired and a second request holds it now.
		redis.mu.Lock()
		redis.values[key] = fakeRedisValue{value: "second-request", expires: time.Now().Add(time.Hour)}
		redis.mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}, func(c *Config) { c.Store = NewRedisStore(redis, "x:") })
	require.Equal(t, 200, do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx)).Code)
	holder, found, _ := redis.Get(context.Background(), key)
	assert.True(t, found, "the first request's unlock left the second holder's lock alone")
	assert.Equal(t, "second-request", holder)
}

func TestAmbiguousErrorWithAnUnreadableKeyStays503(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	store := newFaultyStore(NewRedisStore(redis, "x:"))
	store.breakOp("claim", true)
	store.breakOp("claimant", true)
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = store })
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 503, w.Code)
	assert.NotContains(t, w.Body.String(), "data")
}

// ---------------------------------------------------------------- H5

func TestFalsePayerLeftDoesNotDeliverWithoutAClaim(t *testing.T) {
	// A deadline set by middleware in front of the gate ends the request
	// context while the payer is still connected.
	f := startMock(t)
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			time.Sleep(80 * time.Millisecond)
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits))
	sig := signatureFor(t, h, testPreparedTx)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	first := doCtx(ctx, h, "GET", "/paid", sig)
	assert.Equal(t, 503, first.Code, "the body is withheld, never delivered unclaimed")
	assert.NotContains(t, first.Body.String(), "data")
	assert.Equal(t, testUpdateID, paymentResponse(t, first)["transaction"])
	assert.Contains(t, first.Body.String(), "do NOT pay again")

	retry := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, retry.Code, "the retry is served once")
	assert.Equal(t, 402, do(h, "GET", "/paid", sig).Code)
}

func TestFalsePayerLeftOnTheRecoveryPathIs503Not402(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) {
		time.Sleep(60 * time.Millisecond)
		settleAlreadySettled(w, testUpdateID)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	sig := signatureFor(t, h, testPreparedTx)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	w := doCtx(ctx, h, "GET", "/paid", sig)
	assert.Equal(t, 503, w.Code, "a paid payment is never answered 402")
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code)
}

// ---------------------------------------------------------------- H6

func TestUnreadRequestBodyDoesNotHideADisconnect(t *testing.T) {
	f := startMock(t)
	settling := make(chan struct{})
	var once sync.Once
	f.settle = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			once.Do(func() { close(settling) })
			time.Sleep(300 * time.Millisecond) // the payer disconnects in here
			settleOK(w)
			return
		}
		settleAlreadySettled(w, testUpdateID)
	}
	store := NewMemoryStore()
	var hits atomic.Int32
	gate := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1) // ignores its body, as a handler that reads the query only does
		_, _ = w.Write([]byte(`{"data":42}`))
	}, func(c *Config) {
		c.Store = store
		c.Routes = map[string]Route{"POST /paid": {PayTo: testPayTo, Price: CC("0.01")}}
	})
	sig := signatureWithPayload(t, gate, "POST", "/paid", payloadJSON(testPreparedTx))
	server := httptest.NewServer(gate)
	defer server.Close()

	ctx, leave := context.WithCancel(context.Background())
	go func() {
		<-settling
		time.Sleep(50 * time.Millisecond)
		leave()
	}()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/paid", strings.NewReader(`{"note":"unread"}`))
	req.Header.Set(headerPaymentSig, sig)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	require.Eventually(t, func() bool { return f.settleCalls.Load() == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(400 * time.Millisecond) // the gate finishes the request

	_, claimed, _ := store.Claimant(context.Background(), testUpdateID)
	assert.False(t, claimed, "the disconnect was seen before the claim")
	retry := do(gate, "POST", "/paid", sig)
	assert.Equal(t, 200, retry.Code, "so the retry is served")
}

func TestOversizedBodyIsDrainedOnlyUpToTheBound(t *testing.T) {
	assert.Equal(t, 256<<10, drainBound)
}

// ---------------------------------------------------------------- M1

func TestLockLifetimeFollowsTheMatchedRoute(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	ttls := map[string]time.Duration{}
	var mu sync.Mutex
	capture := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			redis.mu.Lock()
			ttl := redis.ttls["x:lock:"+testPreparedTx]
			redis.mu.Unlock()
			mu.Lock()
			ttls[path] = ttl
			mu.Unlock()
			_, _ = w.Write([]byte("ok"))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /short", capture("short"))
	mux.HandleFunc("GET /long", capture("long"))
	gate := newGate(t, f, mux.ServeHTTP, func(c *Config) {
		c.Store = NewRedisStore(redis, "x:")
		c.Timeout = 10 * time.Second
		c.SettleCalls, c.SettleTimeout = 2, 20*time.Second
		c.Routes = map[string]Route{
			"GET /short": {PayTo: testPayTo, Price: CC("1"), ExecuteBeforeSeconds: 30},
			"GET /long":  {PayTo: testPayTo, Price: CC("1"), ExecuteBeforeSeconds: 600},
		}
	})
	for _, route := range []string{"short", "long"} {
		w := do(gate, "GET", "/"+route, "")
		require.Equal(t, 402, w.Code)
		var required map[string]any
		decodeHeader(t, w.Header().Get(headerPaymentRequired), &required)
		accepted, _ := json.Marshal(required["accepts"].([]any)[0])
		envelope := fmt.Sprintf(`{"x402Version":2,"resource":%s,"accepted":%s,"payload":%s}`, mustJSON(required["resource"]), accepted, payloadJSON(testPreparedTx))
		require.Equal(t, 200, do(gate, "GET", "/"+route, base64.StdEncoding.EncodeToString([]byte(envelope))).Code)
		redis.mu.Lock()
		redis.values = map[string]fakeRedisValue{} // forget the delivery so the second route can pay the same hash
		redis.mu.Unlock()
	}
	extra := 2*20*time.Second + retryAfterCap + postVerifyStoreCalls*storeCallTimeout
	assert.Equal(t, 30*time.Second+extra, ttls["short"]) // Timeout 10 s < lifetime 30 s
	assert.Equal(t, 600*time.Second+extra, ttls["long"])
}

func mustJSON(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}

func TestRequestBudgetIsTheDocumentedFormula(t *testing.T) {
	// The values production runs with: 120 s payment, 65 s settle, 4 calls.
	cfg := Config{Routes: testRoutes()}
	want := 120*time.Second + 4*65*time.Second + 3*productionRetryAfterCap + (postVerifyStoreCalls+preLockStoreCalls)*storeCallTimeout
	assert.Equal(t, want, RequestBudget(cfg))
	assert.Equal(t, 505*time.Second, RequestBudget(cfg))
}

// ---------------------------------------------------------------- M4, M5

func TestStartupSurvivesAFailureOfTheSDKsOwnSupportedCall(t *testing.T) {
	f := startMock(t)
	f.supported = func(n int32, w http.ResponseWriter) {
		if n == 1 {
			w.WriteHeader(502)
			return
		}
		reply(w, 200, supportedBody(nil))
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	assert.Equal(t, 402, do(h, "GET", "/paid", "").Code)
	assert.GreaterOrEqual(t, int(f.supportedCalls.Load()), 2, "the glitch on the second read never reaches the SDK: one read per attempt")
}

func TestStartupDoesNotHangOnAFacilitatorThatNeverAnswers(t *testing.T) {
	old := supportedAttemptTimeout
	supportedAttemptTimeout = 40 * time.Millisecond
	defer func() { supportedAttemptTimeout = old }()
	f := startMock(t)
	release := make(chan struct{})
	defer close(release)
	f.supported = func(_ int32, _ http.ResponseWriter) { <-release }
	done := make(chan error, 1)
	go func() {
		_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 150 * time.Millisecond, Logger: nopLogger()})
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("NewMiddleware hung on a /supported that never answers")
	}
}

func TestMerchantHTTPClientTimeoutDoesNotCapSettle(t *testing.T) {
	f := startMock(t)
	f.settle = func(_ int32, w http.ResponseWriter) {
		time.Sleep(250 * time.Millisecond)
		settleOK(w)
	}
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) {
		c.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
		c.SettleTimeout = 2 * time.Second
	})
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, int32(1), f.settleCalls.Load(), "not cut and retried")
}

func TestMerchantHTTPClientIsNotMutated(t *testing.T) {
	f := startMock(t)
	client := &http.Client{Timeout: 7 * time.Second}
	newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.HTTPClient = client })
	assert.Equal(t, 7*time.Second, client.Timeout)
	assert.Nil(t, client.Transport)
}

// ---------------------------------------------------------------- lows

func TestInvalidHandlerStatusIsNotSettled(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(42)
		_, _ = w.Write([]byte("SECRET"))
	})
	w := do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	assert.Equal(t, 502, w.Code)
	assert.Equal(t, int32(0), f.settleCalls.Load())
	assert.NotContains(t, w.Body.String(), "SECRET")
}

func TestPayerGoneBeforeSettleIsNotChargedAndRetryWorks(t *testing.T) {
	f := startMock(t)
	ctx, leave := context.WithCancel(context.Background())
	var hits atomic.Int32
	h := newGate(t, f, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		leave() // the client gave up during a slow handler
		_, _ = w.Write([]byte("paid"))
	})
	sig := signatureFor(t, h, testPreparedTx)
	w := doCtx(ctx, h, "GET", "/paid", sig)
	assert.Equal(t, 503, w.Code)
	assert.Equal(t, int32(0), f.settleCalls.Load(), "nothing was submitted")
	assert.JSONEq(t, `{"error":"payer left before settle: nothing was charged; retry the same payment"}`, w.Body.String())

	hits.Store(-100) // the handler cancels its own request only the first time in this test
	retry := do(h, "GET", "/paid", sig)
	assert.Equal(t, 200, retry.Code)
	assert.Equal(t, int32(1), f.settleCalls.Load())
}

func TestEveryRedisKeyCarriesThePrefix(t *testing.T) {
	f := startMock(t)
	redis := newFakeRedis()
	h := newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Store = NewRedisStore(redis, "") })
	do(h, "GET", "/paid", signatureFor(t, h, testPreparedTx))
	require.NotEmpty(t, redis.values)
	for key := range redis.values {
		assert.True(t, strings.HasPrefix(key, defaultRedisPrefix), key)
	}
}
