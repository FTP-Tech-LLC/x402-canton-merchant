package canton

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// defaultMemoryMaxEntries bounds the in-memory store. A delivered payment holds
// two entries (record and claim) for 26 hours, so the default admits about
// 100,000 paid calls per 26 hours on one instance. An entry costs about 350
// bytes, so the default bound is around 70 MB.
const defaultMemoryMaxEntries = 200_000

// minMemoryMaxEntries is the smallest bound that admits any payment: a lock, a
// record and a claim.
const minMemoryMaxEntries = 3

const (
	// lockHeadroom is what a payment that holds a lock may still add: its
	// record and its claim. Lock admits a payment only with room for both, so a
	// payment that was admitted can always be recorded and claimed.
	lockHeadroom = 2
	// purgeInterval limits the O(n) scan for expired entries on a full store.
	purgeInterval = time.Second
)

const (
	keyLock    = "lock:"
	keyRecord  = "record:"
	keyClaimed = "claimed:"
)

// errMemoryStoreFull is returned by Lock instead of evicting a live entry:
// dropping a lock, a record or a claim could let one payment be delivered twice.
var errMemoryStoreFull = errors.New("memory store is full of live entries")

// MemoryOption configures NewMemoryStore.
type MemoryOption func(*memoryStore)

// WithMaxEntries changes the bound of the in-memory store (default 200,000;
// values below 3 are raised to 3). Admission of new payments stops at the bound;
// payments already in flight may take the store up to two entries each above it.
func WithMaxEntries(entries int) MemoryOption {
	return func(s *memoryStore) {
		s.maxEntries = max(entries, minMemoryMaxEntries)
	}
}

func withClock(now func() time.Time) MemoryOption {
	return func(s *memoryStore) { s.now = now }
}

type memoryEntry struct {
	value   string
	expires time.Time
}

// memoryStore is the default Store: one process only, and it forgets
// everything on restart. It never evicts a live entry. The bound is applied when
// a payment is admitted (Lock), which keeps room for its record and claim;
// PutRecord and Claim are never refused, so a payment that was admitted and
// settled can always be recorded and delivered. A full store refuses new
// payments with an error, and the gate answers 503 before anything is settled.
type memoryStore struct {
	mu         sync.Mutex
	entries    map[string]memoryEntry
	maxEntries int
	now        func() time.Time
	lastPurge  time.Time
}

// NewMemoryStore returns an atomic, TTL-bound, size-bound in-memory Store for a
// single process. It forgets everything on restart: use goredis.NewStore (or
// NewRedisStore) when more than one instance serves traffic, and to keep
// deliveries across a restart.
func NewMemoryStore(opts ...MemoryOption) Store {
	s := &memoryStore{entries: make(map[string]memoryEntry), maxEntries: defaultMemoryMaxEntries, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *memoryStore) Lock(_ context.Context, hash, token string, ttl time.Duration) (bool, error) {
	return s.setIfAbsent(keyLock+hash, token, ttl, true)
}

func (s *memoryStore) LockHolder(_ context.Context, hash string) (string, bool, error) {
	value, found := s.get(keyLock + hash)
	return value, found, nil
}

func (s *memoryStore) Unlock(_ context.Context, hash, token string) error {
	s.deleteIfValue(keyLock+hash, token)
	return nil
}

func (s *memoryStore) Record(_ context.Context, hash string) (Record, bool, error) {
	value, found := s.get(keyRecord + hash)
	if !found {
		return Record{}, false, nil
	}
	var record Record
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

func (s *memoryStore) PutRecord(_ context.Context, hash string, record Record, ttl time.Duration) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertLocked(keyRecord+hash, string(encoded), ttl)
	return nil
}

func (s *memoryStore) DeleteRecord(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, keyRecord+hash)
	return nil
}

func (s *memoryStore) Claim(_ context.Context, updateID, token string, ttl time.Duration) (bool, error) {
	return s.setIfAbsent(keyClaimed+updateID, token, ttl, false)
}

func (s *memoryStore) Claimant(_ context.Context, updateID string) (string, bool, error) {
	value, found := s.get(keyClaimed + updateID)
	return value, found, nil
}

func (s *memoryStore) Unclaim(_ context.Context, updateID, token string) error {
	s.deleteIfValue(keyClaimed+updateID, token)
	return nil
}

func (s *memoryStore) MarkDelivered(_ context.Context, updateID, token string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertLocked(keyClaimed+updateID, deliveredMark(token), ttl)
	return nil
}

func (s *memoryStore) get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.entries[key]
	if !found || !s.now().Before(entry.expires) {
		delete(s.entries, key)
		return "", false
	}
	return entry.value, true
}

func (s *memoryStore) deleteIfValue(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, found := s.entries[key]; found && entry.value == value {
		delete(s.entries, key)
	}
}

func (s *memoryStore) setIfAbsent(key, value string, ttl time.Duration, admission bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, found := s.entries[key]; found && s.now().Before(entry.expires) {
		return false, nil
	}
	if admission && !s.hasLiveRecordLocked(key) && !s.hasRoomLocked(1+lockHeadroom) {
		return false, errMemoryStoreFull
	}
	s.insertLocked(key, value, ttl)
	return true, nil
}

// hasLiveRecordLocked reports whether the payment behind a lock key already has a
// live record: it was admitted before, and its retry adds at most a lock and one
// claim, so it needs no admission room.
func (s *memoryStore) hasLiveRecordLocked(lockKey string) bool {
	hash := lockKey[len(keyLock):]
	entry, found := s.entries[keyRecord+hash]
	return found && s.now().Before(entry.expires)
}

func (s *memoryStore) insertLocked(key, value string, ttl time.Duration) {
	s.entries[key] = memoryEntry{value: value, expires: s.now().Add(ttl)}
}

// hasRoomLocked reports whether n more entries fit. When they do not, expired
// entries are purged (one O(n) scan, at most once per purgeInterval) and the
// check is repeated.
func (s *memoryStore) hasRoomLocked(n int) bool {
	if len(s.entries)+n <= s.maxEntries {
		return true
	}
	if now := s.now(); now.Sub(s.lastPurge) >= purgeInterval {
		s.lastPurge = now
		for key, entry := range s.entries {
			if !now.Before(entry.expires) {
				delete(s.entries, key)
			}
		}
	}
	return len(s.entries)+n <= s.maxEntries
}
