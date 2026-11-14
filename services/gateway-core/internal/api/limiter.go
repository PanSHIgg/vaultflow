package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RateLimiter 频控器: Redis 固定窗口计数为主, Redis 断连自动降级放行并打
// 结构化告警 (不阻断支付主流程, ARCHITECTURE §1.1 Redis 仅承担无状态频控);
// Redis 未配置时退化为进程内滑动窗口 (dev/test 闭环).
type RateLimiter struct {
	redis redis.UniversalClient
	log   *zap.Logger
	local *localWindowLimiter // 仅 redis == nil 时使用
}

// NewRateLimiter 装配: rdb 为 nil 时使用进程内窗口限流.
func NewRateLimiter(rdb redis.UniversalClient, log *zap.Logger) *RateLimiter {
	if log == nil {
		log = zap.NewNop()
	}
	return &RateLimiter{redis: rdb, log: log, local: newLocalWindowLimiter()}
}

// Allow 判定 key 在 rule 窗口内是否放行.
// allowed=false 时 retryAfter 为建议重试间隔;
// Redis 错误一律 fail-open (allowed=true) + Warn 日志.
func (l *RateLimiter) Allow(ctx context.Context, key string, rule RateLimitRule) (allowed bool, retryAfter time.Duration) {
	if rule.Limit <= 0 || rule.Window <= 0 {
		return true, 0
	}
	if l.redis != nil {
		ok, after, err := l.redisAllow(ctx, key, rule)
		if err == nil {
			return ok, after
		}
		l.log.Warn("rate limiter redis degraded; failing open",
			zap.String("key", key), zap.Error(err))
		return true, 0
	}
	return l.local.allow(key, rule)
}

// redisAllow Redis 固定窗口计数: INCR key 首次命中 PEXPIRE window,
// count > limit 则拒绝, retryAfter = 窗口剩余毫秒.
func (l *RateLimiter) redisAllow(ctx context.Context, key string, rule RateLimitRule) (bool, time.Duration, error) {
	rkey := "vf:rl:" + key
	count, err := l.redis.Incr(ctx, rkey).Result()
	if err != nil {
		return false, 0, fmt.Errorf("redis incr: %w", err)
	}
	if count == 1 {
		if err := l.redis.PExpire(ctx, rkey, rule.Window).Err(); err != nil {
			return false, 0, fmt.Errorf("redis pexpire: %w", err)
		}
	}
	if count <= rule.Limit {
		return true, 0, nil
	}
	ttl, err := l.redis.PTTL(ctx, rkey).Result()
	if err != nil || ttl < 0 {
		ttl = rule.Window
	}
	return false, ttl, nil
}

// localWindowLimiter 进程内固定窗口计数器 (无 Redis 部署的 dev/test 降级实现).
type localWindowLimiter struct {
	mu   sync.Mutex
	seen map[string]*windowCell
}

type windowCell struct {
	windowStart time.Time
	count       int64
}

func newLocalWindowLimiter() *localWindowLimiter {
	return &localWindowLimiter{seen: make(map[string]*windowCell)}
}

func (l *localWindowLimiter) allow(key string, rule RateLimitRule) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	cell, ok := l.seen[key]
	if !ok || now.Sub(cell.windowStart) >= rule.Window {
		l.seen[key] = &windowCell{windowStart: now, count: 1}
		return true, 0
	}
	if cell.count < rule.Limit {
		cell.count++
		return true, 0
	}
	return false, rule.Window - now.Sub(cell.windowStart)
}
