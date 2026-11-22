// Package store — 交易所托管模型的 PostgreSQL 持久化层 (pgx/v5 + pgxpool).
//
// 对应 migrations/008_exchange.sql 的用户域表 (users / user_deposit_addresses /
// user_balances / platform_wallets / supported_assets / onchain_deposits).
// 风格沿用 cmd/server/store.go: NUMERIC(78,0) 一律 ::text 取回经
// types.MustParseU256, 地址落库为 '0x' 小写 hex, 未命中返回 ports.ErrNotFound.
package store

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ErrNotFound 目标行不存在 (复用共享哨兵, errors.Is 兼容).
var ErrNotFound = ports.ErrNotFound

// Store 用户域持久化门面, 包装 *pgxpool.Pool. 不可变, 并发安全.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore 构造 Store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Pool 暴露底层连接池 (调用方自建事务/巡检查询用).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// WithinTx 在单个 pgx 事务内执行 fn; fn 返回 error 即回滚, nil 即提交.
// 需要把多个 Store 写操作合成一个 ACID 单元的调用方使用 (底层 tx 直接透传).
func (s *Store) WithinTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// 共享小工具
// ---------------------------------------------------------------------------

// addrHex types.Address -> '0x' 小写 hex (跨栈 hex 规范 §2.5.3).
func addrHex(a types.Address) string { return "0x" + hex.EncodeToString(a.Bytes()) }

// parseU256Text NUMERIC ::text -> types.U256. 数据库已约束非负, panic 即数据损坏.
func parseU256Text(s string) types.U256 { return types.MustParseU256(s) }

// IsUniqueViolation 判断 PostgreSQL 23505 unique_violation
// (GetOrCreateDepositAddress 竞态重选 / RegisterUser email 占用判定).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// mapNoRows pgx.ErrNoRows -> ErrNotFound.
func mapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
