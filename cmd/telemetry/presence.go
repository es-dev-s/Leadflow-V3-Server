package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const presenceKeyPrefix = "lf:support:presence:"
const presenceTTL = 45 * time.Second

type Presence struct {
	rdb *redis.Client
	mu  sync.Mutex
	mem map[string]time.Time // fallback when redis unavailable
}

func NewPresence(redisURL string) *Presence {
	p := &Presence{mem: make(map[string]time.Time)}
	if strings.TrimSpace(redisURL) == "" {
		log.Println("telemetry presence: in-memory (no REDIS_URL)")
		return p
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Printf("telemetry presence: bad REDIS_URL, using memory: %v", err)
		return p
	}
	rdb := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("telemetry presence: redis ping failed, using memory: %v", err)
		_ = rdb.Close()
		return p
	}
	p.rdb = rdb
	log.Println("telemetry presence: redis enabled")
	return p
}

func (p *Presence) Close() {
	if p.rdb != nil {
		_ = p.rdb.Close()
	}
}

func (p *Presence) Touch(ctx context.Context, userID, sessionID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}
	key := presenceKeyPrefix + userID
	if p.rdb != nil {
		val := sessionID
		if val == "" {
			val = "1"
		}
		if err := p.rdb.Set(ctx, key, val, presenceTTL).Err(); err != nil {
			log.Printf("telemetry presence redis set failed, using memory: %v", err)
			// Fall through to memory so presence never breaks Support KPIs.
		} else {
			return nil
		}
	}
	p.mu.Lock()
	p.mem[userID] = nowUTC().Add(presenceTTL)
	p.mu.Unlock()
	return nil
}

func (p *Presence) Count(ctx context.Context) (active int64, concurrent int64, err error) {
	if p.rdb != nil {
		var cursor uint64
		seen := make(map[string]struct{})
		scanFailed := false
		for {
			keys, next, scanErr := p.rdb.Scan(ctx, cursor, presenceKeyPrefix+"*", 200).Result()
			if scanErr != nil {
				log.Printf("telemetry presence redis scan failed, using memory: %v", scanErr)
				scanFailed = true
				break
			}
			for _, k := range keys {
				seen[k] = struct{}{}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		if !scanFailed {
			n := int64(len(seen))
			// Also include in-memory fallbacks.
			now := nowUTC()
			p.mu.Lock()
			for id, exp := range p.mem {
				if exp.Before(now) {
					delete(p.mem, id)
					continue
				}
				seen[presenceKeyPrefix+id] = struct{}{}
			}
			p.mu.Unlock()
			n = int64(len(seen))
			return n, n, nil
		}
	}

	now := nowUTC()
	p.mu.Lock()
	defer p.mu.Unlock()
	alive := int64(0)
	for id, exp := range p.mem {
		if exp.Before(now) {
			delete(p.mem, id)
			continue
		}
		alive++
	}
	return alive, alive, nil
}
