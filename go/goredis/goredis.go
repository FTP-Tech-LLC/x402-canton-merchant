// Package goredis is a canton.Store on github.com/redis/go-redis/v9 (the
// package requires go-redis v9; github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton
// does not).
//
//	rdb := redis.NewClient(&redis.Options{Addr: "redis:6379", ContextTimeoutEnabled: true})
//	cfg.Store = goredis.NewStore(rdb, "x402:myapp:")
//
// Use one Redis, a Cluster (every operation touches one key) or a Sentinel
// failover client, with a dedicated database and the noeviction policy. Not a
// Ring: it moves keys to another shard when one is marked down, and a delivered
// payment can then be delivered again. No replica reads (ReadOnly, RouteRandomly,
// RouteByLatency): a replica may not have the claim yet.
//
// Set ContextTimeoutEnabled: true. Without it go-redis ignores the gate's
// per-call deadline and a stalled Redis holds a paid request past its budget.
// See OPERATIONS.md for persistence and failover.
package goredis

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
)

// compareAndDeleteScript deletes the key only when it holds the given value.
// It is sent with EVAL, not through redis.NewScript, whose package-level SHA-1
// panics at init under GODEBUG=fips140=only.
const compareAndDeleteScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`

// NewStore returns a canton.Store for several processes. Every key starts with
// prefix, for example "x402:myapp:" (an empty prefix becomes "x402-canton:").
//
// A nil client makes canton.NewGate return an error. A *redis.Ring, or a
// cluster client with ReadOnly, RouteRandomly or RouteByLatency, panics: those
// can serve one payment twice, and the wiring bug should stop startup.
func NewStore(client redis.UniversalClient, prefix string) canton.Store {
	if isNil(client) {
		return canton.NewRedisStore(nil, prefix)
	}
	checkTopology(client)
	return canton.NewRedisStore(adapter{client: client}, prefix)
}

func isNil(client redis.UniversalClient) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func checkTopology(client redis.UniversalClient) {
	switch c := client.(type) {
	case *redis.Ring:
		panic("goredis: a Ring moves keys to another shard when one is down, so a delivered payment can be delivered again: use one Redis, a Cluster or a Sentinel failover client")
	case *redis.ClusterClient:
		options := c.Options()
		if options.ReadOnly || options.RouteRandomly || options.RouteByLatency {
			panic("goredis: ReadOnly, RouteRandomly and RouteByLatency read from replicas, which may not have a claim yet: turn them off")
		}
		warnIfContextTimeoutOff(options.ContextTimeoutEnabled)
	case *redis.Client:
		warnIfContextTimeoutOff(c.Options().ContextTimeoutEnabled)
	}
}

func warnIfContextTimeoutOff(enabled bool) {
	if !enabled {
		slog.Warn("goredis: ContextTimeoutEnabled is off, so go-redis ignores the gate's per-call deadlines; set ContextTimeoutEnabled: true in redis.Options")
	}
}

// adapter maps go-redis onto canton.RedisClient.
type adapter struct{ client redis.UniversalClient }

func (a adapter) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return a.client.SetNX(ctx, key, value, ttl).Result()
}

func (a adapter) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return a.client.Set(ctx, key, value, ttl).Err()
}

func (a adapter) Get(ctx context.Context, key string) (string, bool, error) {
	value, err := a.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return value, err == nil, err
}

func (a adapter) Del(ctx context.Context, key string) error {
	return a.client.Del(ctx, key).Err()
}

func (a adapter) DelIfValue(ctx context.Context, key, value string) error {
	return a.client.Eval(ctx, compareAndDeleteScript, []string{key}, value).Err()
}
