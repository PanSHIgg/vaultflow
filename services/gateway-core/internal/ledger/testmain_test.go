package ledger

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// w3TestLockKey PG advisory lock 键: W3 三包集成测试共享同一数据库,
// go test 默认并行跑各包 -> 用会话级 advisory lock 串行化, 防止
// TRUNCATE/播种互相踩踏 (与业务无关, 纯测试隔离).
const w3TestLockKey int64 = 0x563300000001 // "W3" 测试域

// TestMain 在集成测试启用时先获取 advisory lock 再执行包测试.
func TestMain(m *testing.M) {
	dsn := os.Getenv("VF_TEST_PG_DSN")
	if dsn == "" {
		os.Exit(m.Run()) // 全部集成用例将 skip, 无需加锁
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger testmain connect: %v\n", err)
		os.Exit(1)
	}
	if _, err := conn.Exec(context.Background(),
		`SELECT pg_advisory_lock($1)`, w3TestLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "ledger testmain advisory lock: %v\n", err)
		os.Exit(1)
	}
	// 包级隔离：清空前序共享本测试库的其他包残留行
	if _, err := conn.Exec(context.Background(), `TRUNCATE chain_sync_cursors, supported_assets, merchants,
		merchant_withdrawal_addresses, deposit_addresses, payment_orders, chain_blocks,
		onchain_deposits, wallet_nonce_cursors, outbound_tx_intents, outbound_tx_attempts,
		sweep_batches, sweep_batch_items, merchant_payouts, webhook_outbox, signer_audit_anchor,
		ledger_accounts, ledger_transactions, ledger_entries RESTART IDENTITY CASCADE;
	INSERT INTO merchants (id, name, api_key_hash, fee_rate_bps)
	VALUES ('00000000-0000-0000-0000-000000000000', '__SYSTEM__', '__SYSTEM_SENTINEL_NO_API_KEY__', 0)
	ON CONFLICT (id) DO NOTHING`); err != nil {
		fmt.Fprintf(os.Stderr, "ledger testmain truncate: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, w3TestLockKey)
	_ = conn.Close(context.Background())
	os.Exit(code)
}
