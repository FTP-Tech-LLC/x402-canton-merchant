package canton

import (
	"context"
	"errors"
	"sync"
	"time"
)

// fakeRedis is an in-memory RedisClient with real SET NX and expiry semantics.
type fakeRedis struct {
	mu      sync.Mutex
	now     func() time.Time
	values  map[string]fakeRedisValue
	ttls    map[string]time.Duration // the ttl last written, for assertions
	setNX   map[string]int
	failing bool
	// lostReplies makes the next n SetNX calls apply and then report an error,
	// as a timeout after the server processed the command does.
	lostReplies int
	// errorOnTaken makes SetNX report an error (not false) when the key exists.
	errorOnTaken bool
	// delayNext makes the next SetNX return an error WITHOUT applying; the write
	// lands at the start of the following SetNX, like a command still in flight.
	delayNext bool
	pending   *fakeRedisPending
}

type fakeRedisPending struct {
	key, value string
	ttl        time.Duration
}

type fakeRedisValue struct {
	value   string
	expires time.Time
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{now: time.Now, values: map[string]fakeRedisValue{}, ttls: map[string]time.Duration{}, setNX: map[string]int{}}
}

var errRedisDown = errors.New("redis down")

func (r *fakeRedis) live(key string) (fakeRedisValue, bool) {
	entry, found := r.values[key]
	if !found || !r.now().Before(entry.expires) {
		delete(r.values, key)
		return fakeRedisValue{}, false
	}
	return entry, true
}

func (r *fakeRedis) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.failing {
		return false, errRedisDown
	}
	if r.pending != nil {
		r.values[r.pending.key] = fakeRedisValue{value: r.pending.value, expires: r.now().Add(r.pending.ttl)}
		r.pending = nil
	}
	if r.delayNext {
		r.delayNext = false
		r.pending = &fakeRedisPending{key: key, value: value, ttl: ttl}
		return false, errRedisDown
	}
	r.setNX[key]++
	if _, found := r.live(key); found {
		if r.errorOnTaken {
			return false, errRedisDown
		}
		return false, nil
	}
	r.values[key] = fakeRedisValue{value: value, expires: r.now().Add(ttl)}
	r.ttls[key] = ttl
	if r.lostReplies > 0 {
		r.lostReplies--
		return false, errRedisDown
	}
	return true, nil
}

func (r *fakeRedis) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.failing {
		return errRedisDown
	}
	r.values[key] = fakeRedisValue{value: value, expires: r.now().Add(ttl)}
	r.ttls[key] = ttl
	return nil
}

func (r *fakeRedis) Get(ctx context.Context, key string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if r.failing {
		return "", false, errRedisDown
	}
	entry, found := r.live(key)
	return entry.value, found, nil
}

func (r *fakeRedis) Del(ctx context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.failing {
		return errRedisDown
	}
	delete(r.values, key)
	return nil
}

func (r *fakeRedis) DelIfValue(ctx context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.failing {
		return errRedisDown
	}
	if entry, found := r.live(key); found && entry.value == value {
		delete(r.values, key)
	}
	return nil
}

func (r *fakeRedis) fail(on bool) {
	r.mu.Lock()
	r.failing = on
	r.mu.Unlock()
}

func (r *fakeRedis) set(fn func(*fakeRedis)) {
	r.mu.Lock()
	fn(r)
	r.mu.Unlock()
}

func (r *fakeRedis) loseNextReplies(n int) {
	r.mu.Lock()
	r.lostReplies = n
	r.mu.Unlock()
}

// faultyStore wraps a Store and fails the named operations: "lock", "lockholder",
// "record" (read), "put" (write), "putSettled", "lockFull" (Lock reports
// a full memory store), "claim", "claimant", "unclaim". failNext(op, n) fails
// only the next n calls.
type faultyStore struct {
	Store
	mu          sync.Mutex
	fail        map[string]bool
	times       map[string]int
	unlockDelay time.Duration
}

var errStoreDown = errors.New("store down")

func newFaultyStore(inner Store) *faultyStore {
	return &faultyStore{Store: inner, fail: map[string]bool{}, times: map[string]int{}}
}

func (s *faultyStore) failing(operation string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.times[operation] > 0 {
		s.times[operation]--
		return true
	}
	return s.fail[operation]
}

func (s *faultyStore) failNext(operation string, calls int) {
	s.mu.Lock()
	s.times[operation] = calls
	s.mu.Unlock()
}

func (s *faultyStore) LockHolder(ctx context.Context, hash string) (string, bool, error) {
	if s.failing("lockholder") {
		return "", false, errStoreDown
	}
	return s.Store.LockHolder(ctx, hash)
}

func (s *faultyStore) MarkDelivered(ctx context.Context, updateID, token string, ttl time.Duration) error {
	if s.failing("mark") {
		return errStoreDown
	}
	return s.Store.MarkDelivered(ctx, updateID, token, ttl)
}

func (s *faultyStore) DeleteRecord(ctx context.Context, hash string) error {
	if s.failing("delete") {
		return errStoreDown
	}
	return s.Store.DeleteRecord(ctx, hash)
}

func (s *faultyStore) Unclaim(ctx context.Context, updateID, token string) error {
	if s.failing("panicUnclaim") {
		panic("store bug")
	}
	if s.failing("unclaim") {
		return errStoreDown
	}
	return s.Store.Unclaim(ctx, updateID, token)
}

func (s *faultyStore) breakOp(operation string, broken bool) {
	s.mu.Lock()
	s.fail[operation] = broken
	s.mu.Unlock()
}

func (s *faultyStore) Unlock(ctx context.Context, hash, token string) error {
	s.mu.Lock()
	delay := s.unlockDelay
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if s.failing("unlock") {
		return errStoreDown
	}
	return s.Store.Unlock(ctx, hash, token)
}

func (s *faultyStore) Lock(ctx context.Context, hash, token string, ttl time.Duration) (bool, error) {
	if s.failing("lockFull") {
		return false, errMemoryStoreFull
	}
	if s.failing("lock") {
		return false, errStoreDown
	}
	return s.Store.Lock(ctx, hash, token, ttl)
}

func (s *faultyStore) Record(ctx context.Context, hash string) (Record, bool, error) {
	if s.failing("record") {
		return Record{}, false, errStoreDown
	}
	return s.Store.Record(ctx, hash)
}

func (s *faultyStore) PutRecord(ctx context.Context, hash string, record Record, ttl time.Duration) error {
	if s.failing("put") || (s.failing("putSettled") && record.State == StateSettled) {
		return errStoreDown
	}
	return s.Store.PutRecord(ctx, hash, record, ttl)
}

func (s *faultyStore) Claim(ctx context.Context, updateID, token string, ttl time.Duration) (bool, error) {
	if s.failing("claim") {
		return false, errStoreDown
	}
	return s.Store.Claim(ctx, updateID, token, ttl)
}

func (s *faultyStore) Claimant(ctx context.Context, updateID string) (string, bool, error) {
	if s.failing("claimant") {
		return "", false, errStoreDown
	}
	return s.Store.Claimant(ctx, updateID)
}
