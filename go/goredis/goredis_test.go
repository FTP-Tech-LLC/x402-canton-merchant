package goredis_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/goredis"
)

func newStore(t *testing.T) (canton.Store, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = client.Close() })
	return goredis.NewStore(client, "x402:test:"), server
}

func TestLockIsExclusiveOwnedAndExpires(t *testing.T) {
	ctx := context.Background()
	store, server := newStore(t)
	got, err := store.Lock(ctx, "h1", "tokA", time.Minute)
	require.NoError(t, err)
	assert.True(t, got)
	got, _ = store.Lock(ctx, "h1", "tokB", time.Minute)
	assert.False(t, got, "a second owner does not get the lock")
	holder, found, _ := store.LockHolder(ctx, "h1")
	assert.True(t, found)
	assert.Equal(t, "tokA", holder)

	require.NoError(t, store.Unlock(ctx, "h1", "tokB"))
	got, _ = store.Lock(ctx, "h1", "tokC", time.Minute)
	assert.False(t, got, "a stranger's Unlock does not release the lock")
	require.NoError(t, store.Unlock(ctx, "h1", "tokA"))
	got, _ = store.Lock(ctx, "h1", "tokC", time.Minute)
	assert.True(t, got)

	server.FastForward(2 * time.Minute)
	got, _ = store.Lock(ctx, "h1", "tokD", time.Minute)
	assert.True(t, got, "an abandoned lock expires")
}

func TestRecordRoundTripDeleteAndTTL(t *testing.T) {
	ctx := context.Background()
	store, server := newStore(t)
	_, found, err := store.Record(ctx, "h1")
	require.NoError(t, err)
	assert.False(t, found)
	settled := canton.Record{State: canton.StateSettled, UpdateID: "u1", Digest: "d", Payer: "p"}
	require.NoError(t, store.PutRecord(ctx, "h1", settled, 26*time.Hour))
	got, found, _ := store.Record(ctx, "h1")
	assert.True(t, found)
	assert.Equal(t, settled, got)
	assert.Equal(t, 26*time.Hour, server.TTL("x402:test:record:h1"))
	require.NoError(t, store.DeleteRecord(ctx, "h1"))
	_, found, _ = store.Record(ctx, "h1")
	assert.False(t, found)
}

func TestClaimIsOnceOwnedAndMarkedDelivered(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	got, _ := store.Claim(ctx, "u1", "tokA", time.Hour)
	assert.True(t, got)
	got, _ = store.Claim(ctx, "u1", "tokB", time.Hour)
	assert.False(t, got)
	require.NoError(t, store.Unclaim(ctx, "u1", "tokB"))
	holder, found, _ := store.Claimant(ctx, "u1")
	assert.True(t, found)
	assert.Equal(t, "tokA", holder, "a stranger's Unclaim keeps the claim")

	require.NoError(t, store.MarkDelivered(ctx, "u1", "tokA", time.Hour))
	holder, _, _ = store.Claimant(ctx, "u1")
	assert.NotEqual(t, "tokA", holder, "the claim carries the delivered mark")
	got, _ = store.Claim(ctx, "u1", "tokA", time.Hour)
	assert.False(t, got, "a delivered claim is not taken again, even by its former owner")
}

func TestEveryKeyCarriesThePrefix(t *testing.T) {
	ctx := context.Background()
	store, server := newStore(t)
	_, _ = store.Lock(ctx, "h", "t", time.Minute)
	_ = store.PutRecord(ctx, "h", canton.Record{State: canton.StateAttempt}, time.Hour)
	_, _ = store.Claim(ctx, "u", "t", time.Hour)
	require.NotEmpty(t, server.Keys())
	for _, key := range server.Keys() {
		assert.Contains(t, key, "x402:test:", key)
	}
	empty := goredis.NewStore(redis.NewClient(&redis.Options{Addr: server.Addr(), ContextTimeoutEnabled: true}), "")
	_, _ = empty.Lock(ctx, "h2", "t", time.Minute)
	assert.True(t, server.Exists("x402-canton:lock:h2"), "an empty prefix becomes the default one")
}

func TestNilClientIsLeftForNewGateToReport(t *testing.T) {
	var typedNil *redis.Client
	assert.NotPanics(t, func() { goredis.NewStore(nil, "p:") })
	assert.NotPanics(t, func() { goredis.NewStore(typedNil, "p:") }, "a nil *redis.Client in a variable is a nil client too")
}

// A Ring moves keys to another shard when one is marked down, and a cluster client
// that reads from replicas may not see a claim yet: both can serve a payment twice.
func TestTopologiesThatCanServeAPaymentTwiceAreRefused(t *testing.T) {
	ring := redis.NewRing(&redis.RingOptions{Addrs: map[string]string{"a": "127.0.0.1:1"}})
	t.Cleanup(func() { _ = ring.Close() })
	assert.PanicsWithValue(t, "goredis: a Ring moves keys to another shard when one is down, so a delivered payment can be delivered again: use one Redis, a Cluster or a Sentinel failover client",
		func() { goredis.NewStore(ring, "p:") })
	for name, options := range map[string]*redis.ClusterOptions{
		"ReadOnly":       {Addrs: []string{"127.0.0.1:1"}, ReadOnly: true},
		"RouteRandomly":  {Addrs: []string{"127.0.0.1:1"}, RouteRandomly: true},
		"RouteByLatency": {Addrs: []string{"127.0.0.1:1"}, RouteByLatency: true},
	} {
		cluster := redis.NewClusterClient(options)
		t.Cleanup(func() { _ = cluster.Close() })
		assert.Panics(t, func() { goredis.NewStore(cluster, "p:") }, name)
	}
	plain := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{"127.0.0.1:1"}, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = plain.Close() })
	assert.NotPanics(t, func() { goredis.NewStore(plain, "p:") }, "a plain cluster client is fine")
}

func TestWarnsWhenContextTimeoutIsOff(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server := miniredis.RunT(t)
	off := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = off.Close() })
	goredis.NewStore(off, "p:")
	assert.Contains(t, logs.String(), "ContextTimeoutEnabled")
	logs.Reset()
	on := redis.NewClient(&redis.Options{Addr: server.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = on.Close() })
	goredis.NewStore(on, "p:")
	assert.Empty(t, logs.String())
}

// lostReplyProxy forwards to Redis, but the first SET ... NX is applied by the
// server and its reply is dropped with the connection: what a flaky network does.
// go-redis then re-sends the command (MaxRetries 3 by default).
type lostReplyProxy struct {
	listener net.Listener
	target   string
	armed    atomic.Bool
	sends    atomic.Int32 // SET NX commands that reached the server
}

func newLostReplyProxy(t *testing.T, target string) *lostReplyProxy {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &lostReplyProxy{listener: listener, target: target}
	proxy.armed.Store(true)
	t.Cleanup(func() { _ = listener.Close() })
	go proxy.serve()
	return proxy
}

func (p *lostReplyProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(client)
	}
}

func (p *lostReplyProxy) handle(client net.Conn) {
	defer client.Close()
	server, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.target)
	if err != nil {
		return
	}
	defer server.Close()
	var dropReplies atomic.Bool
	go func() { // replies, until one is to be lost
		reply := make([]byte, 64<<10)
		for {
			n, err := server.Read(reply)
			if err != nil || dropReplies.Load() {
				return
			}
			if _, err := client.Write(reply[:n]); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 64<<10)
	for {
		n, err := client.Read(buf)
		if err != nil {
			return
		}
		command := bytes.ToLower(buf[:n])
		isSetNX := bytes.Contains(command, []byte("\r\nset\r\n")) && bytes.Contains(command, []byte("\r\nnx\r\n"))
		if isSetNX {
			p.sends.Add(1)
		}
		if isSetNX && p.armed.CompareAndSwap(true, false) {
			dropReplies.Store(true)
			_, _ = server.Write(buf[:n])
			time.Sleep(100 * time.Millisecond) // the server applies it and answers; the answer is lost
			return                             // both connections close: the client sees EOF and re-sends
		}
		if _, err := server.Write(buf[:n]); err != nil {
			return
		}
	}
}

func TestLockSurvivesALostReplyAndResend(t *testing.T) {
	for name, take := range map[string]func(store canton.Store) (bool, error){
		"lock":  func(s canton.Store) (bool, error) { return s.Lock(context.Background(), "h1", "tokA", time.Minute) },
		"claim": func(s canton.Store) (bool, error) { return s.Claim(context.Background(), "u1", "tokA", time.Minute) },
	} {
		t.Run(name, func(t *testing.T) {
			server := miniredis.RunT(t)
			proxy := newLostReplyProxy(t, server.Addr())
			client := redis.NewClient(&redis.Options{ContextTimeoutEnabled: true, Addr: proxy.listener.Addr().String(), MaxRetries: 3, MinRetryBackoff: time.Millisecond, MaxRetryBackoff: 5 * time.Millisecond})
			t.Cleanup(func() { _ = client.Close() })
			store := goredis.NewStore(client, "x402:test:")

			got, err := take(store)
			require.NoError(t, err)
			assert.True(t, got, "the re-sent SET NX answers exists, but the key holds our own token")
			assert.GreaterOrEqual(t, proxy.sends.Load(), int32(2), "the command really was re-sent")

			// Another owner still does not get it.
			var other bool
			if name == "lock" {
				other, err = store.Lock(context.Background(), "h1", "tokB", time.Minute)
			} else {
				other, err = store.Claim(context.Background(), "u1", "tokB", time.Minute)
			}
			require.NoError(t, err)
			assert.False(t, other, "another token does not get a %s that was taken", name)
		})
	}
}

func TestUnlockAndUnclaimAreSafeToResend(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	_, _ = store.Lock(ctx, "h1", "tokA", time.Minute)
	require.NoError(t, store.Unlock(ctx, "h1", "tokA"))
	require.NoError(t, store.Unlock(ctx, "h1", "tokA"), "a re-sent compare-and-delete finds nothing and is not an error")
	got, _ := store.Lock(ctx, "h1", "tokB", time.Minute)
	assert.True(t, got)
	require.NoError(t, store.Unlock(ctx, "h1", "tokA"), "an old owner's re-send does not release the new owner's lock")
	holder, _, _ := store.LockHolder(ctx, "h1")
	assert.Equal(t, "tokB", holder)
	require.NoError(t, store.DeleteRecord(ctx, "none"), "deleting a missing record is not an error")
}
