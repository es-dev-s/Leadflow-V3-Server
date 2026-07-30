package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ResponseCacher is the response-cache contract handlers rely on. Production
// uses the Redis implementation (shared across instances, realtime
// invalidation via a generation counter); the in-memory ResponseCache is both
// the standalone default and the automatic fallback when Redis is unreachable.
type ResponseCacher interface {
	Get(key string) ([]byte, bool)
	Set(key string, data []byte, ttl time.Duration)
	Clear()
}

const (
	redisOpTimeout = 250 * time.Millisecond
	// Local generation cache: bounds cross-instance invalidation lag while
	// avoiding an extra Redis round trip on every request. Mutations made
	// through this process invalidate locally in the same call.
	redisGenCacheFor = 300 * time.Millisecond
	redisMaxValue    = 1 << 20 // don't cache pathological payloads
)

type RedisResponseCache struct {
	rdb      *redis.Client
	fallback *ResponseCache
	genKey   string
	prefix   string

	mu         sync.Mutex
	gen        int64
	genFetched time.Time

	degraded atomic.Bool // log state transitions once, not per request
}

func NewRedisResponseCache(url string, fallback *ResponseCache) (*RedisResponseCache, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	opts.DialTimeout = 500 * time.Millisecond
	opts.ReadTimeout = 300 * time.Millisecond
	opts.WriteTimeout = 300 * time.Millisecond
	opts.PoolSize = 64
	opts.MinIdleConns = 4
	// Managed Redis ACLs commonly restrict keys to a "<username>:" prefix, so
	// scope every key under it (harmless on unrestricted servers).
	scope := "lf:"
	if opts.Username != "" {
		scope = opts.Username + ":lf:"
	}
	c := &RedisResponseCache{
		rdb:      redis.NewClient(opts),
		fallback: fallback,
		genKey:   scope + "leads:gen",
		prefix:   scope + "resp:",
	}
	// Warm the connection pool so the first request's Get doesn't pay the
	// TCP+AUTH dial cost inside its tight per-op timeout.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.rdb.Ping(ctx).Err()
	}()
	return c, nil
}

func (c *RedisResponseCache) noteDegraded(err error) {
	if c.degraded.CompareAndSwap(false, true) {
		log.Printf("redis cache degraded, serving from in-memory fallback: %v", err)
	}
}

func (c *RedisResponseCache) noteHealthy() {
	if c.degraded.CompareAndSwap(true, false) {
		log.Printf("redis cache recovered")
	}
}

// generation returns the current invalidation generation. ok=false means
// Redis is unreachable and callers should use the in-memory fallback.
func (c *RedisResponseCache) generation() (int64, bool) {
	c.mu.Lock()
	if !c.genFetched.IsZero() && time.Since(c.genFetched) < redisGenCacheFor {
		g := c.gen
		c.mu.Unlock()
		return g, true
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	val, err := c.rdb.Get(ctx, c.genKey).Int64()
	if err == redis.Nil {
		val, err = 0, nil
	}
	if err != nil {
		c.noteDegraded(err)
		return 0, false
	}
	c.noteHealthy()
	c.mu.Lock()
	c.gen = val
	c.genFetched = time.Now()
	c.mu.Unlock()
	return val, true
}

// fullKey embeds the generation, so bumping it makes every older entry
// unreachable instantly; the stale keys then age out via their TTLs.
func (c *RedisResponseCache) fullKey(gen int64, key string) string {
	sum := sha256.Sum256([]byte(key))
	return c.prefix + strconv.FormatInt(gen, 10) + ":" + hex.EncodeToString(sum[:16])
}

func (c *RedisResponseCache) Get(key string) ([]byte, bool) {
	gen, ok := c.generation()
	if !ok {
		return c.fallback.Get(key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	data, err := c.rdb.Get(ctx, c.fullKey(gen, key)).Bytes()
	if err == redis.Nil {
		return nil, false
	}
	if err != nil {
		c.noteDegraded(err)
		return c.fallback.Get(key)
	}
	return data, true
}

func (c *RedisResponseCache) Set(key string, data []byte, ttl time.Duration) {
	if ttl <= 0 || len(data) == 0 || len(data) > redisMaxValue {
		return
	}
	gen, ok := c.generation()
	if !ok {
		c.fallback.Set(key, data, ttl)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	if err := c.rdb.Set(ctx, c.fullKey(gen, key), data, ttl).Err(); err != nil {
		c.noteDegraded(err)
		c.fallback.Set(key, data, ttl)
	}
}

// Clear bumps the generation: every cached response (on every instance)
// becomes unreachable immediately. Called on any lead/user mutation.
func (c *RedisResponseCache) Clear() {
	c.fallback.Clear()
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	val, err := c.rdb.Incr(ctx, c.genKey).Result()
	if err != nil {
		c.noteDegraded(err)
		// Force a refetch so we don't serve entries from the stale generation
		// once Redis comes back.
		c.mu.Lock()
		c.genFetched = time.Time{}
		c.mu.Unlock()
		return
	}
	c.noteHealthy()
	c.mu.Lock()
	c.gen = val
	c.genFetched = time.Now()
	c.mu.Unlock()
}
