package canton

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rule 10, stores: both implementations pass the same contract.

type storeFactory func(t *testing.T, clock *fakeClock) Store

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func storeImplementations() map[string]storeFactory {
	return map[string]storeFactory{
		"memory": func(_ *testing.T, clock *fakeClock) Store { return NewMemoryStore(withClock(clock.Now)) },
		"redis": func(_ *testing.T, clock *fakeClock) Store {
			redis := newFakeRedis()
			redis.now = clock.Now
			return NewRedisStore(redis, "x402:")
		},
	}
}

func TestStoreContract(t *testing.T) {
	ctx := context.Background()
	for name, factory := range storeImplementations() {
		t.Run(name+"/lock is exclusive, owned and releasable only by its owner", func(t *testing.T) {
			store := factory(t, newClock())
			got, err := store.Lock(ctx, "h1", "tokA", time.Minute)
			require.NoError(t, err)
			assert.True(t, got)
			got, _ = store.Lock(ctx, "h1", "tokB", time.Minute)
			assert.False(t, got)
			holder, found, _ := store.LockHolder(ctx, "h1")
			assert.True(t, found)
			assert.Equal(t, "tokA", holder)
			other, _ := store.Lock(ctx, "h2", "tokB", time.Minute)
			assert.True(t, other, "locks are per hash")

			require.NoError(t, store.Unlock(ctx, "h1", "tokB"))
			got, _ = store.Lock(ctx, "h1", "tokC", time.Minute)
			assert.False(t, got, "a stranger's Unlock does not release the lock")
			require.NoError(t, store.Unlock(ctx, "h1", "tokA"))
			got, _ = store.Lock(ctx, "h1", "tokC", time.Minute)
			assert.True(t, got)
		})
		t.Run(name+"/lock expires", func(t *testing.T) {
			clock := newClock()
			store := factory(t, clock)
			_, _ = store.Lock(ctx, "h1", "a", time.Minute)
			clock.Advance(59 * time.Second)
			got, _ := store.Lock(ctx, "h1", "b", time.Minute)
			assert.False(t, got)
			clock.Advance(2 * time.Second)
			got, _ = store.Lock(ctx, "h1", "b", time.Minute)
			assert.True(t, got, "an abandoned lock does not stick forever")
		})
		t.Run(name+"/record overwrites, deletes and expires", func(t *testing.T) {
			clock := newClock()
			store := factory(t, clock)
			_, found, err := store.Record(ctx, "h1")
			require.NoError(t, err)
			assert.False(t, found)
			attempt := Record{State: StateAttempt, Digest: "d1", Payer: "p"}
			settled := Record{State: StateSettled, UpdateID: "u2", Digest: "d1", Payer: "p"}
			require.NoError(t, store.PutRecord(ctx, "h1", attempt, recordTTL))
			got, found, _ := store.Record(ctx, "h1")
			assert.True(t, found)
			assert.Equal(t, attempt, got)
			require.NoError(t, store.PutRecord(ctx, "h1", settled, recordTTL))
			got, _, _ = store.Record(ctx, "h1")
			assert.Equal(t, settled, got)
			clock.Advance(recordTTL - time.Minute)
			_, found, _ = store.Record(ctx, "h1")
			assert.True(t, found, "still there after 25 h")
			clock.Advance(2 * time.Minute)
			_, found, _ = store.Record(ctx, "h1")
			assert.False(t, found, "gone after 26 h")

			require.NoError(t, store.PutRecord(ctx, "h2", attempt, recordTTL))
			require.NoError(t, store.DeleteRecord(ctx, "h2"))
			_, found, _ = store.Record(ctx, "h2")
			assert.False(t, found)
		})
		t.Run(name+"/record survives unlock", func(t *testing.T) {
			store := factory(t, newClock())
			_, _ = store.Lock(ctx, "h1", "a", time.Minute)
			_ = store.PutRecord(ctx, "h1", Record{State: StateSettled, UpdateID: "u1"}, recordTTL)
			_ = store.Unlock(ctx, "h1", "a")
			got, found, _ := store.Record(ctx, "h1")
			assert.True(t, found)
			assert.Equal(t, "u1", got.UpdateID)
		})
		t.Run(name+"/claim is once, owned and releasable only by its owner", func(t *testing.T) {
			clock := newClock()
			store := factory(t, clock)
			_, found, _ := store.Claimant(ctx, "u1")
			assert.False(t, found)
			first, err := store.Claim(ctx, "u1", "tokA", recordTTL)
			require.NoError(t, err)
			second, _ := store.Claim(ctx, "u1", "tokB", recordTTL)
			assert.True(t, first)
			assert.False(t, second)
			holder, found, _ := store.Claimant(ctx, "u1")
			assert.True(t, found)
			assert.Equal(t, "tokA", holder)
			require.NoError(t, store.Unclaim(ctx, "u1", "tokB"))
			_, found, _ = store.Claimant(ctx, "u1")
			assert.True(t, found, "a stranger's Unclaim does nothing")
			require.NoError(t, store.Unclaim(ctx, "u1", "tokA"))
			_, found, _ = store.Claimant(ctx, "u1")
			assert.False(t, found)
			again, _ := store.Claim(ctx, "u1", "tokB", recordTTL)
			assert.True(t, again, "an unclaimed payment can be claimed again")
			clock.Advance(recordTTL + time.Second)
			_, found, _ = store.Claimant(ctx, "u1")
			assert.False(t, found, "the claim lives 26 h")
		})
		for _, kind := range []string{"claim", "lock"} {
			t.Run(name+"/"+kind+" is atomic under concurrency", func(t *testing.T) {
				store := factory(t, newClock())
				var winners atomic.Int32
				var wg sync.WaitGroup
				for i := 0; i < 64; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						token := fmt.Sprintf("tok%d", i)
						var won bool
						if kind == "claim" {
							won, _ = store.Claim(ctx, "u-race", token, recordTTL)
						} else {
							won, _ = store.Lock(ctx, "h-race", token, time.Minute)
						}
						if won {
							winners.Add(1)
						}
					}()
				}
				wg.Wait()
				assert.Equal(t, int32(1), winners.Load())
			})
		}
	}
}

func TestMemoryStoreAdmitsAPaymentOnlyWithRoomForItsRecordAndClaim(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(WithMaxEntries(6), withClock(newClock().Now))
	// Two admitted payments take lock + record + claim each: exactly 6 entries.
	for i := 0; i < 2; i++ {
		hash, update := fmt.Sprintf("h%d", i), fmt.Sprintf("u%d", i)
		ok, err := store.Lock(ctx, hash, "tok", time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, store.PutRecord(ctx, hash, Record{State: StateSettled, UpdateID: update}, recordTTL))
		claimed, err := store.Claim(ctx, update, "tok", recordTTL)
		require.NoError(t, err)
		require.True(t, claimed)
	}
	_, err := store.Lock(ctx, "h-new", "tok", time.Minute)
	require.ErrorIs(t, err, errMemoryStoreFull, "a full store refuses a new payment at admission")
	for i := 0; i < 2; i++ {
		_, found, _ := store.Claimant(ctx, fmt.Sprintf("u%d", i))
		assert.True(t, found, "no live entry was evicted")
	}
}

func TestMemoryStoreNeverRefusesTheRecordOrClaimOfAnAdmittedPayment(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(WithMaxEntries(3), withClock(newClock().Now))
	ok, err := store.Lock(ctx, "h", "tok", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "room for lock, record and claim")
	// Other payments crowd the store while this one works.
	for i := 0; i < 10; i++ {
		_, _ = store.Claim(ctx, fmt.Sprintf("other%d", i), "t", recordTTL)
	}
	require.NoError(t, store.PutRecord(ctx, "h", Record{State: StateSettled, UpdateID: "u"}, recordTTL), "an attempt or settled write is never refused")
	claimed, err := store.Claim(ctx, "u", "tok", recordTTL)
	require.NoError(t, err)
	assert.True(t, claimed, "a claim is never refused")
}

func TestMemoryStorePurgeOnAFullStoreIsRateLimited(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	store := NewMemoryStore(WithMaxEntries(30), withClock(clock.Now)).(*memoryStore)
	for i := 0; i < 28; i++ {
		_, _ = store.Claim(ctx, fmt.Sprintf("u%d", i), "t", recordTTL)
	}
	_, err := store.Lock(ctx, "h1", "t", time.Minute)
	require.ErrorIs(t, err, errMemoryStoreFull)
	first := store.lastPurge
	_, _ = store.Lock(ctx, "h2", "t", time.Minute)
	assert.Equal(t, first, store.lastPurge, "a second refusal inside the interval does not scan again")
	clock.Advance(2 * purgeInterval)
	_, _ = store.Lock(ctx, "h3", "t", time.Minute)
	assert.True(t, store.lastPurge.After(first))
}

func TestMemoryStoreReplacingARecordNeedsNoRoom(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(WithMaxEntries(1), withClock(newClock().Now))
	require.NoError(t, store.PutRecord(ctx, "h", Record{State: StateAttempt}, recordTTL))
	require.NoError(t, store.PutRecord(ctx, "h", Record{State: StateSettled, UpdateID: "u"}, recordTTL), "attempt to settled on a full store")
}

func TestMemoryStoreMakesRoomByDroppingExpiredEntries(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	store := NewMemoryStore(WithMaxEntries(6), withClock(clock.Now))
	_, _ = store.Claim(ctx, "keep", "tok", recordTTL)
	_, _ = store.Claim(ctx, "short1", "tok", time.Second)
	_, _ = store.Claim(ctx, "short2", "tok", time.Second)
	_, _ = store.Claim(ctx, "short3", "tok", time.Second)
	clock.Advance(2 * time.Second)
	ok, err := store.Lock(ctx, "new", "tok", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
	_, found, _ := store.Claimant(ctx, "keep")
	assert.True(t, found, "expired entries made room; the live claim stayed")
}

func TestMemoryStoreDefaultBound(t *testing.T) {
	assert.Equal(t, 200_000, NewMemoryStore().(*memoryStore).maxEntries)
	assert.Equal(t, 26*time.Hour, recordTTL)
}

func TestRedisStoreUsesTheDocumentedKeysAndTTLs(t *testing.T) {
	ctx := context.Background()
	redis := newFakeRedis()
	store := NewRedisStore(redis, "app:")
	_, _ = store.Lock(ctx, "hash1", "tokA", 3*time.Minute)
	_ = store.PutRecord(ctx, "hash1", Record{State: StateSettled, UpdateID: "update1"}, recordTTL)
	_, _ = store.Claim(ctx, "update1", "tokA", recordTTL)

	assert.Equal(t, 3*time.Minute, redis.ttls["app:lock:hash1"])
	assert.Equal(t, recordTTL, redis.ttls["app:record:hash1"])
	assert.Equal(t, recordTTL, redis.ttls["app:claimed:update1"])
	assert.Equal(t, "tokA", redis.values["app:lock:hash1"].value, "the lock value is the owner's token")
	assert.Equal(t, "tokA", redis.values["app:claimed:update1"].value)
	assert.Equal(t, 1, redis.setNX["app:lock:hash1"], "locks use SET NX")
	assert.Equal(t, 1, redis.setNX["app:claimed:update1"], "claims use SET NX")
	assert.Zero(t, redis.setNX["app:record:hash1"], "records use a plain SET")
}

func TestRedisStoreNeverUsesABareKey(t *testing.T) {
	redis := newFakeRedis()
	store := NewRedisStore(redis, "")
	_, _ = store.Lock(context.Background(), "hash1", "t", time.Minute)
	for key := range redis.values {
		assert.True(t, strings.HasPrefix(key, defaultRedisPrefix), key)
	}
}

func TestRedisStorePropagatesErrors(t *testing.T) {
	ctx := context.Background()
	redis := newFakeRedis()
	store := NewRedisStore(redis, "")
	redis.fail(true)
	_, err := store.Lock(ctx, "h", "t", time.Minute)
	assert.ErrorIs(t, err, errRedisDown)
	_, _, err = store.Record(ctx, "h")
	assert.ErrorIs(t, err, errRedisDown)
	assert.ErrorIs(t, store.PutRecord(ctx, "h", Record{}, time.Minute), errRedisDown)
	_, err = store.Claim(ctx, "u", "t", time.Minute)
	assert.ErrorIs(t, err, errRedisDown)
	_, _, err = store.Claimant(ctx, "u")
	assert.ErrorIs(t, err, errRedisDown)
	assert.ErrorIs(t, store.Unlock(ctx, "h", "t"), errRedisDown)
	assert.ErrorIs(t, store.Unclaim(ctx, "u", "t"), errRedisDown)
}

func TestMemoryStoreWarnsAtStartup(t *testing.T) {
	f := startMock(t)
	logger, logs := capturingLogger()
	newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) { c.Logger = logger })
	assert.True(t, logsContain(logs, "level=WARN", "memory store: one process only; use goredis.NewStore (or NewRedisStore) for more than one instance and to keep deliveries across a restart"))

	quiet, quietLogs := capturingLogger()
	newGate(t, f, okHandler(new(atomic.Int32)), func(c *Config) {
		c.Logger = quiet
		c.Store = NewRedisStore(newFakeRedis(), "p:")
	})
	assert.False(t, strings.Contains(quietLogs.String(), "memory store"))
}
