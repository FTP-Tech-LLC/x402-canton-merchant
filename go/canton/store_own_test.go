package canton

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedRedis answers SetNX and Get as the test says; the rest is not used.
type scriptedRedis struct {
	RedisClient
	setNX  func() (bool, error)
	get    func() (string, bool, error)
	getHit int
}

func (s *scriptedRedis) SetNX(context.Context, string, string, time.Duration) (bool, error) {
	return s.setNX()
}

func (s *scriptedRedis) Get(context.Context, string) (string, bool, error) {
	s.getHit++
	return s.get()
}

// SET NX that answers "exists" may be the re-send of our own write: only a key that
// still holds our token counts as ours.
func TestRedisStoreSetNXOwnBranches(t *testing.T) {
	errRead := errors.New("read failed")
	for name, tc := range map[string]struct {
		setNX    func() (bool, error)
		get      func() (string, bool, error)
		want     bool
		wantErr  error
		wantRead bool
	}{
		"set succeeds: no read":                     {setNX: func() (bool, error) { return true, nil }, want: true},
		"set fails: the error is returned":          {setNX: func() (bool, error) { return false, errRead }, wantErr: errRead},
		"exists and the holder is us":               {setNX: func() (bool, error) { return false, nil }, get: func() (string, bool, error) { return "tok", true, nil }, want: true, wantRead: true},
		"exists and the holder is someone else":     {setNX: func() (bool, error) { return false, nil }, get: func() (string, bool, error) { return "other", true, nil }, wantRead: true},
		"exists and the key is gone by the read":    {setNX: func() (bool, error) { return false, nil }, get: func() (string, bool, error) { return "", false, nil }, wantRead: true},
		"exists and the read fails: error returned": {setNX: func() (bool, error) { return false, nil }, get: func() (string, bool, error) { return "", false, errRead }, wantErr: errRead, wantRead: true},
	} {
		t.Run(name, func(t *testing.T) {
			for opName, take := range map[string]func(Store) (bool, error){
				"lock":  func(s Store) (bool, error) { return s.Lock(context.Background(), "h", "tok", time.Minute) },
				"claim": func(s Store) (bool, error) { return s.Claim(context.Background(), "u", "tok", time.Minute) },
			} {
				client := &scriptedRedis{setNX: tc.setNX, get: tc.get}
				got, err := take(NewRedisStore(client, "p:"))
				assert.Equal(t, tc.want, got, opName)
				if tc.wantErr != nil {
					assert.ErrorIs(t, err, tc.wantErr, opName)
				} else {
					assert.NoError(t, err, opName)
				}
				assert.Equal(t, tc.wantRead, client.getHit > 0, opName+": read back only after an \"exists\"")
			}
		})
	}
}

// readOnlyStore refuses writes as a replica does.
type readOnlyStore struct{ Store }

func (readOnlyStore) Lock(context.Context, string, string, time.Duration) (bool, error) {
	return false, errors.New("READONLY You can't write against a read only replica")
}

// The startup probe writes: a read-only Redis, or an ACL without SET NX or EVAL, is a
// startup error that names the store, not a surprise for the first payer.
func TestStartupProbeWritesAndRefusesAReadOnlyStore(t *testing.T) {
	f := startMock(t)
	_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 2 * time.Second,
		Logger: nopLogger(), Store: readOnlyStore{NewMemoryStore()}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "payment store unusable after")
	assert.Contains(t, err.Error(), "READONLY")
	assert.NotContains(t, err.Error(), "facilitator")

	redis := newFakeRedis()
	_, err = NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 2 * time.Second,
		Logger: nopLogger(), Store: NewRedisStore(redis, "x:")})
	require.NoError(t, err)
	assert.Empty(t, redis.values, "the probe key is removed again")
}

func TestPaidRequestWithNoBodyDoesNotPanic(t *testing.T) {
	f := startMock(t)
	h := newGate(t, f, okHandler(new(atomic.Int32)))
	sig := signatureFor(t, h, testPreparedTx)
	r := httptest.NewRequest("GET", "/paid", nil)
	r.Body = nil // as http.NewRequest(method, url, nil) in a unit test
	r.Header.Set("PAYMENT-SIGNATURE", sig)
	assert.NotPanics(t, func() { h.ServeHTTP(httptest.NewRecorder(), r) })
}

// A refused Lock whose holder cannot be read may be our own applied write: the gate
// takes it back (compare-and-delete) and reports the error, so the lock does not
// hold the payer's retry for its TTL.
func TestRefusedLockWithUnreadableHolderIsTakenBack(t *testing.T) {
	f := startMock(t)
	inner := NewMemoryStore()
	store := newFaultyStore(&lostReplyLockStore{Store: inner, answer: func() (bool, error) { return false, nil }})
	store.breakOp("lockholder", true)
	var hits atomic.Int32
	h := newGate(t, f, okHandler(&hits), func(c *Config) { c.Store = store })
	sig := signatureFor(t, h, testPreparedTx)
	assert.Equal(t, 503, do(h, "GET", "/paid", sig).Code)
	_, held, _ := inner.LockHolder(context.Background(), testPreparedTx)
	assert.False(t, held, "our applied lock is released, not left for its TTL")
	assert.Zero(t, hits.Load())
	store.breakOp("lockholder", false)
	assert.Equal(t, 200, do(h, "GET", "/paid", sig).Code, "the retry is not made to wait")
}

// probeRecorder remembers the hashes the startup probe used.
type probeRecorder struct {
	Store
	hashes    []string
	recordErr error
}

func (s *probeRecorder) Lock(ctx context.Context, hash, token string, ttl time.Duration) (bool, error) {
	s.hashes = append(s.hashes, hash)
	return s.Store.Lock(ctx, hash, token, ttl)
}

func (s *probeRecorder) Record(ctx context.Context, hash string) (Record, bool, error) {
	s.hashes = append(s.hashes, hash)
	if s.recordErr != nil {
		return Record{}, false, s.recordErr
	}
	return s.Store.Record(ctx, hash)
}

// The startup probe uses canonical 64-hex keys, like real payments, and reads a
// record too: a Store whose Record fails must fail startup.
func TestStartupProbeUsesCanonicalKeysAndReadsARecord(t *testing.T) {
	f := startMock(t)
	store := &probeRecorder{Store: NewMemoryStore()}
	_, err := NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 2 * time.Second,
		Logger: nopLogger(), Store: store})
	require.NoError(t, err)
	require.Len(t, store.hashes, 2, "one Record read and one Lock")
	for _, hash := range store.hashes {
		assert.Regexp(t, `^[0-9a-f]{64}$`, hash)
	}

	broken := &probeRecorder{Store: NewMemoryStore(), recordErr: errors.New("record read refused")}
	_, err = NewMiddleware(context.Background(), Config{FacilitatorURL: f.URL, Routes: testRoutes(), SupportedWait: 2 * time.Second,
		Logger: nopLogger(), Store: broken})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "payment store unusable after")
	assert.Contains(t, err.Error(), "record read refused")
}
