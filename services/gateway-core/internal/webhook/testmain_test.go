package webhook

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// testTruncateAll wipes every application table so this PG-integrated suite is
// isolated from rows left behind by other packages sharing the same test
// database (`go test -p 1` serializes packages but does not reset state).
const testTruncateAll = `TRUNCATE chain_sync_cursors, supported_assets, merchants,
	merchant_withdrawal_addresses, deposit_addresses, payment_orders, chain_blocks,
	onchain_deposits, wallet_nonce_cursors, outbound_tx_intents, outbound_tx_attempts,
	sweep_batches, sweep_batch_items, merchant_payouts, webhook_outbox, signer_audit_anchor,
	ledger_accounts, ledger_transactions, ledger_entries RESTART IDENTITY CASCADE;
INSERT INTO merchants (id, name, api_key_hash, fee_rate_bps)
VALUES ('00000000-0000-0000-0000-000000000000', '__SYSTEM__', '__SYSTEM_SENTINEL_NO_API_KEY__', 0)
ON CONFLICT (id) DO NOTHING`

func testEnvDSN() string {
	if d := os.Getenv("VF_TEST_PG_DSN"); d != "" {
		return d
	}
	return os.Getenv("VF_TEST_DSN")
}

func TestMain(m *testing.M) {
	dsn := testEnvDSN()
	if dsn == "" {
		os.Exit(m.Run())
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "webhook testmain connect: %v\n", err)
		os.Exit(1)
	}
	if _, err := conn.Exec(context.Background(), testTruncateAll); err != nil {
		fmt.Fprintf(os.Stderr, "webhook testmain truncate: %v\n", err)
		os.Exit(1)
	}
	_ = conn.Close(context.Background())
	os.Exit(m.Run())
}
