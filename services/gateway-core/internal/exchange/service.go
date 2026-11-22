// Package exchange — 交易所托管钱包业务层, 被 API (gateway) 与 backend
// worker 消费: 用户注册/登录、每用户每链 CREATE2 充值地址惰性分配、
// 链上状态 -> API 状态映射.
package exchange

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/auth"
	"vaultflow/gateway-core/internal/create2"
	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/types"
)

// 业务错误哨兵 (API 层映射 4xx).
var (
	// ErrInvalidInput 输入校验失败 (email 缺 '@' / password < 8 字符).
	ErrInvalidInput = errors.New("exchange: invalid input")
	// ErrEmailTaken 邮箱已注册 (users lower(email) 唯一冲突).
	ErrEmailTaken = errors.New("exchange: email already registered")
	// ErrBadCredentials 邮箱不存在或口令错误 (对外不区分).
	ErrBadCredentials = errors.New("exchange: invalid email or password")
)

// API 侧充值状态 (MapDepositStatus 输出, 对应前端充值记录渲染).
const (
	DepositStatusPending   = "PENDING"   // DETECTED / ORPHANED_SHALLOW / ORPHANED_DEEP
	DepositStatusConfirmed = "CONFIRMED" // FINALIZED (确认数达标, 入账中)
	DepositStatusCredited  = "CREDITED"  // 已贷记 user_balances
	DepositStatusSwept     = "SWEPT"     // 已归集至平台金库
)

// Service 业务门面: 持有 *store.Store 与逐链 *create2.Deriver map.
type Service struct {
	store    *store.Store
	derivers map[types.ChainID]*create2.Deriver
}

// NewService 构造 Service. derivers 为逐链 CREATE2 推导器 (公链部署地址可与
// 全局 deploy 面不同 — chains.<id>.factory/implementation 覆盖装配).
func NewService(st *store.Store, derivers map[types.ChainID]*create2.Deriver) *Service {
	return &Service{store: st, derivers: derivers}
}

// Store 暴露底层 store (余额/充值/平台钱包查询等直通场景).
func (s *Service) Store() *store.Store { return s.store }

// Deriver 暴露指定链的 CREATE2 推导器 (未配置链返回 nil).
func (s *Service) Deriver(chainID types.ChainID) *create2.Deriver {
	return s.derivers[chainID]
}

// normalizeEmail trim + 小写 (与 uq_users_email_lower 一致).
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// RegisterUser 注册: 规范化 email, 边缘校验 (含 '@', password >= 8 字符),
// bcrypt 散列后落库. 邮箱冲突返回 ErrEmailTaken.
func (s *Service) RegisterUser(ctx context.Context, email, password string) (store.User, error) {
	email = normalizeEmail(email)
	if !strings.Contains(email, "@") {
		return store.User{}, fmt.Errorf("%w: malformed email", ErrInvalidInput)
	}
	if utf8.RuneCountInString(password) < 8 {
		return store.User{}, fmt.Errorf("%w: password must be >= 8 characters", ErrInvalidInput)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return store.User{}, err
	}
	u, err := s.store.CreateUser(ctx, email, hash)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return store.User{}, ErrEmailTaken
		}
		return store.User{}, err
	}
	return u, nil
}

// AuthenticateUser 登录: 邮箱查找 + bcrypt 校验. 用户不存在或口令错误
// 统一返回 ErrBadCredentials (不泄露账号存在性).
func (s *Service) AuthenticateUser(ctx context.Context, email, password string) (store.User, error) {
	u, err := s.store.GetUserByEmail(ctx, normalizeEmail(email))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrBadCredentials
		}
		return store.User{}, err
	}
	if err := auth.CheckPassword(u.PasswordHash, password); err != nil {
		return store.User{}, ErrBadCredentials
	}
	return u, nil
}

// GetOrCreateDepositAddress 惰性分配: 查 (user_id, chain_id) 常驻地址; 未分配
// 则 DeriveUserSalt + CREATE2 推导并插入. 并发重复请求撞
// UNIQUE(user_id,chain_id) (23505) 时重选返回竞争胜出者.
func (s *Service) GetOrCreateDepositAddress(ctx context.Context, userID uuid.UUID,
	chainID types.ChainID) (store.UserDepositAddress, error) {
	uda, err := s.store.GetUserDepositAddress(ctx, userID, chainID)
	if err == nil {
		return uda, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.UserDepositAddress{}, err
	}
	d := s.derivers[chainID]
	if d == nil {
		return store.UserDepositAddress{}, fmt.Errorf("no CREATE2 deriver for chain %d", chainID)
	}
	salt := create2.DeriveUserSalt(chainID, userID)
	addr := d.VaultAddress(salt)
	uda, err = s.store.InsertUserDepositAddress(ctx, userID, chainID, addr, salt)
	if err == nil {
		return uda, nil
	}
	if store.IsUniqueViolation(err) {
		// 竞态: 另一请求抢先插入, 重选返回胜者.
		uda, err2 := s.store.GetUserDepositAddress(ctx, userID, chainID)
		if err2 == nil {
			return uda, nil
		}
	}
	return store.UserDepositAddress{}, err
}

// MapDepositStatus onchain_deposits.status -> API 充值状态:
//
//	DETECTED -> PENDING, ORPHANED_SHALLOW -> PENDING, FINALIZED -> CONFIRMED,
//	CREDITED -> CREDITED, SWEPT -> SWEPT, ORPHANED_DEEP -> PENDING.
//
// 未知输入按 PENDING 兜底 (最保守展示).
func MapDepositStatus(status string) string {
	switch status {
	case store.DepositStatusFinalized:
		return DepositStatusConfirmed
	case store.DepositStatusCredited:
		return DepositStatusCredited
	case store.DepositStatusSwept:
		return DepositStatusSwept
	default: // DETECTED, ORPHANED_SHALLOW, ORPHANED_DEEP, unknown
		return DepositStatusPending
	}
}
