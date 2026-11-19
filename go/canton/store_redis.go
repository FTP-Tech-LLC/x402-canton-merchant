package canton

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// defaultRedisPrefix is used when NewRedisStore gets an empty prefix, so the
// plugin's keys never land bare in a shared Redis database.
const defaultRedisPrefix = "x402-canton:"

// RedisClient is the small surface the Redis store needs. It has no go-redis
// dependency: for github.com/redis/go-redis/v9 use the goredis package.
type RedisClient interface {
	// SetNX is SET key value NX EX ttl. It returns false when the key exists.
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// Set is a plain SET key value EX ttl: it overwrites.
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	// Get returns the value, with found false when the key does not exist.
	Get(ctx context.Context, key string) (value string, found bool, err error)
	// Del deletes the key.
	Del(ctx context.Context, key string) error
	// DelIfValue deletes the key only when its value equals value, atomically
	// (a Lua compare-and-delete).
	DelIfValue(ctx context.Context, key, value string) error
}

type redisStore struct {
	client RedisClient
	prefix string
}

// NewRedisStore returns a Store that several instances can share. Every key
// starts with prefix, for example "x402:myapp:" (an empty prefix becomes
// "x402-canton:"). Locks and claims use SET NX EX with the owner's token,
// records a plain SET EX. Use a dedicated Redis database with the noeviction
// policy, and read from the primary: see OPERATIONS.md.
func NewRedisStore(client RedisClient, prefix string) Store {
	if prefix == "" {
		prefix = defaultRedisPrefix
	}
	return &redisStore{client: client, prefix: prefix}
}

func (s *redisStore) lockKey(hash string) string      { return s.prefix + keyLock + hash }
func (s *redisStore) recordKey(hash string) string    { return s.prefix + keyRecord + hash }
func (s *redisStore) claimKey(updateID string) string { return s.prefix + keyClaimed + updateID }

func (s *redisStore) Lock(ctx context.Context, hash, token string, ttl time.Duration) (bool, error) {
	return s.setNXOwn(ctx, s.lockKey(hash), token, ttl)
}

// setNXOwn is SET NX that recognises its own write. A client library may re-send a
// command after a network error that lost the reply (go-redis does by default): the
// first send set the key, the re-send answers "exists", and the owner would wait for
// the TTL on its own lock. A token is unique per request, so a key that holds ours
// was set by us.
func (s *redisStore) setNXOwn(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	acquired, err := s.client.SetNX(ctx, key, token, ttl)
	if err != nil || acquired {
		return acquired, err
	}
	holder, found, err := s.client.Get(ctx, key)
	if err != nil {
		return false, err
	}
	return found && holder == token, nil
}

func (s *redisStore) LockHolder(ctx context.Context, hash string) (string, bool, error) {
	return s.client.Get(ctx, s.lockKey(hash))
}

func (s *redisStore) Unlock(ctx context.Context, hash, token string) error {
	return s.client.DelIfValue(ctx, s.lockKey(hash), token)
}

func (s *redisStore) Record(ctx context.Context, hash string) (Record, bool, error) {
	value, found, err := s.client.Get(ctx, s.recordKey(hash))
	if err != nil || !found {
		return Record{}, false, err
	}
	var record Record
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

func (s *redisStore) PutRecord(ctx context.Context, hash string, record Record, ttl time.Duration) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.recordKey(hash), string(encoded), ttl)
}

func (s *redisStore) DeleteRecord(ctx context.Context, hash string) error {
	return s.client.Del(ctx, s.recordKey(hash))
}

func (s *redisStore) Claim(ctx context.Context, updateID, token string, ttl time.Duration) (bool, error) {
	return s.setNXOwn(ctx, s.claimKey(updateID), token, ttl)
}

func (s *redisStore) Claimant(ctx context.Context, updateID string) (string, bool, error) {
	return s.client.Get(ctx, s.claimKey(updateID))
}

func (s *redisStore) MarkDelivered(ctx context.Context, updateID, token string, ttl time.Duration) error {
	return s.client.Set(ctx, s.claimKey(updateID), deliveredMark(token), ttl)
}

func (s *redisStore) Unclaim(ctx context.Context, updateID, token string) error {
	return s.client.DelIfValue(ctx, s.claimKey(updateID), token)
}

// clusterProbeTags are hash tags whose CRC16 slot is 0, 1024, ... 15360: sixteen
// keys that spread over the whole slot space of a Redis Cluster.
var clusterProbeTags = [...]string{
	"3560", "22069", "48576", "11467", "4129", "130", "1296", "41342",
	"8209", "3706", "2821", "48710", "356", "45971", "16248", "4782",
}

// errClusterEndpoint is a configuration error: the client points at a Redis
// Cluster endpoint but does not follow redirects.
type errClusterEndpoint struct{ reply string }

func (e errClusterEndpoint) Error() string {
	return "the Redis endpoint is a cluster (" + e.reply + ") but the client does not follow redirects: " +
		"use redis.NewClusterClient (goredis.NewStore takes it) or a single Redis"
}

// startupCheck reads one key under the prefix in each of sixteen slots. A plain
// client on a cluster endpoint gets MOVED or ASK for part of them, and the
// lock, record and claim of one payment would land on different nodes. It uses
// plain GETs only: no PING or INFO, so a restricted ACL still starts.
func (s *redisStore) startupCheck(ctx context.Context) error {
	for _, tag := range clusterProbeTags {
		_, _, err := s.client.Get(ctx, s.prefix+"probe:{"+tag+"}")
		if err == nil {
			continue
		}
		if reply := err.Error(); strings.HasPrefix(reply, "MOVED ") || strings.HasPrefix(reply, "ASK ") {
			return errClusterEndpoint{reply: reply}
		}
		return err
	}
	return nil
}
