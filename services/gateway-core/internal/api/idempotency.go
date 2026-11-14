package api

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// IdempotencyRecord 幂等存档: 同 (scope, key) 同 request_hash 回放首响应.
type IdempotencyRecord struct {
	RequestHash string `json:"request_hash"`
	StatusCode  int    `json:"status_code"`
	Body        []byte `json:"body"`
	StoredAt    int64  `json:"stored_at"`
}

// IdempotencyStore 幂等记录存储 (Redis 实现优先, 进程内降级兜底).
type IdempotencyStore interface {
	// Load 读取 (scope, key) 存档; found=false 表示不存在.
	Load(ctx context.Context, scope, key string) (rec IdempotencyRecord, found bool, err error)
	// SaveNX 仅当 (scope, key) 不存在时写入 (SET NX + TTL);
	// 返回 false 表示已有并发写入胜出.
	SaveNX(ctx context.Context, scope, key string, rec IdempotencyRecord, ttl time.Duration) (stored bool, err error)
}

// ---------------------------------------------------------------------------
// Redis 实现
// ---------------------------------------------------------------------------

type redisIdemStore struct{ rdb redis.UniversalClient }

// NewRedisIdempotencyStore Redis 幂等存储.
func NewRedisIdempotencyStore(rdb redis.UniversalClient) IdempotencyStore {
	return &redisIdemStore{rdb: rdb}
}

func (s *redisIdemStore) redisKey(scope, key string) string {
	return "vf:idem:" + scope + ":" + key
}

func (s *redisIdemStore) Load(ctx context.Context, scope, key string) (IdempotencyRecord, bool, error) {
	raw, err := s.rdb.Get(ctx, s.redisKey(scope, key)).Bytes()
	if err == redis.Nil {
		return IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("redis idem get: %w", err)
	}
	var rec IdempotencyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("redis idem decode: %w", err)
	}
	return rec, true, nil
}

func (s *redisIdemStore) SaveNX(ctx context.Context, scope, key string, rec IdempotencyRecord, ttl time.Duration) (bool, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	ok, err := s.rdb.SetNX(ctx, s.redisKey(scope, key), raw, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis idem setnx: %w", err)
	}
	return ok, nil
}

// ---------------------------------------------------------------------------
// 进程内实现 (Redis 未配置 / dev & test 兜底)
// ---------------------------------------------------------------------------

type memIdemStore struct {
	mu   sync.Mutex
	seen map[string]IdempotencyRecord
}

// NewMemoryIdempotencyStore 进程内幂等存储.
func NewMemoryIdempotencyStore() IdempotencyStore {
	return &memIdemStore{seen: make(map[string]IdempotencyRecord)}
}

func memKey(scope, key string) string { return scope + "\x00" + key }

func (s *memIdemStore) Load(_ context.Context, scope, key string) (IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.seen[memKey(scope, key)]
	return rec, ok, nil
}

func (s *memIdemStore) SaveNX(_ context.Context, scope, key string, rec IdempotencyRecord, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memKey(scope, key)
	if _, exists := s.seen[k]; exists {
		return false, nil
	}
	s.seen[k] = rec
	return true, nil
}

// ---------------------------------------------------------------------------
// degraded store — Redis 优先, 出错降级进程内 (fail-open 语义与频控一致:
// 降级期间以进程内 map 继续提供幂等保护并告警).
// ---------------------------------------------------------------------------

type degradedIdemStore struct {
	primary  IdempotencyStore
	fallback IdempotencyStore
	log      *zap.Logger
}

// NewDegradingIdempotencyStore 包装 Redis 主存: 出错自动走内存并告警.
func NewDegradingIdempotencyStore(primary IdempotencyStore, log *zap.Logger) IdempotencyStore {
	if log == nil {
		log = zap.NewNop()
	}
	return &degradedIdemStore{primary: primary, fallback: NewMemoryIdempotencyStore(), log: log}
}

func (d *degradedIdemStore) Load(ctx context.Context, scope, key string) (IdempotencyRecord, bool, error) {
	rec, found, err := d.primary.Load(ctx, scope, key)
	if err == nil {
		return rec, found, nil
	}
	d.log.Warn("idempotency store degraded on load; using in-process fallback",
		zap.String("scope", scope), zap.Error(err))
	return d.fallback.Load(ctx, scope, key)
}

func (d *degradedIdemStore) SaveNX(ctx context.Context, scope, key string, rec IdempotencyRecord, ttl time.Duration) (bool, error) {
	stored, err := d.primary.SaveNX(ctx, scope, key, rec, ttl)
	if err == nil {
		return stored, nil
	}
	d.log.Warn("idempotency store degraded on save; using in-process fallback",
		zap.String("scope", scope), zap.Error(err))
	return d.fallback.SaveNX(ctx, scope, key, rec, ttl)
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

var idemKeyPattern = regexp.MustCompile(`^[A-Za-z0-9\-_:.]{1,255}$`)

// responseCapture 捕获 handler 输出以便幂等存档.
type responseCapture struct {
	status int
	body   []byte
	header map[string]string
}

// idemStripedLocks (scope,key) 级并发互斥锁 (同实例内同键串行化,
// 防止并发重复写接口双执行; 跨实例由 store NX + 业务层 UNIQUE 双保险).
type idemStripedLocks struct {
	locks [256]sync.Mutex
}

func (l *idemStripedLocks) lockFor(scope, key string) *sync.Mutex {
	h := uint32(0)
	for i := 0; i < len(scope); i++ {
		h = h*31 + uint32(scope[i])
	}
	for i := 0; i < len(key); i++ {
		h = h*31 + uint32(key[i])
	}
	return &l.locks[h&255]
}
