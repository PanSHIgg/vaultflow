package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// 本包测试仅使用标准库 testing (testify 传递依赖 go.yaml.in/yaml/v3
// 未收录 go.sum, go.mod 铁律禁止增补 — 见交付报告).
//
// 集成测试需要真实 PostgreSQL: 通过 VF_TEST_PG_DSN 提供 DSN,
// 未设置时自动 skip (契约: migrations/001_init.sql 已导入).

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("VF_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("VF_TEST_PG_DSN not set; skipping PostgreSQL integration test")
	}
	return dsn
}

var testChain = types.ChainID(31337)
var testMerchant = uuid.MustParse("11111111-2222-3333-4444-555555555555")

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// resetLedger 清空账本相关表并播种测试商户 (merchants FK).
func resetLedger(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// TRUNCATE 不触发行级 append-only 触发器 (其仅拦截 UPDATE/DELETE 行操作).
	for _, q := range []string{
		`TRUNCATE ledger_entries, ledger_transactions, ledger_accounts, webhook_outbox RESTART IDENTITY CASCADE`,
		// merchants 被 mwa/payment_orders/payouts 等十余张表 FK 引用;
		// 同一 DSN 下跨套件遗留行会 23503 — CASCADE 全清, 再回补哨兵商户.
		`TRUNCATE merchants RESTART IDENTITY CASCADE`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("reset %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO merchants (id, name, api_key_hash, fee_rate_bps)
		VALUES ('00000000-0000-0000-0000-000000000000',
		        '__SYSTEM__', '__SYSTEM_SENTINEL_NO_API_KEY__', 0)`); err != nil {
		t.Fatalf("seed system merchant: %v", err)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO merchants (id, name, api_key_hash, fee_rate_bps)
		VALUES ($1, 'w3-test-merchant', $2, 100)`,
		testMerchant, "hash-"+testMerchant.String())
	if err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
}

func mkLeg(owner uuid.UUID, chain types.ChainID, asset string, acc types.LedgerAccountCode,
	dir types.Direction, amount string) ports.PostEntryLeg {
	return ports.PostEntryLeg{
		OwnerMerchantID: owner, ChainID: chain, AssetSymbol: asset,
		Account: acc, Direction: dir, Amount: types.MustParseU256(amount),
	}
}

func sysLeg(acc types.LedgerAccountCode, dir types.Direction, amount string) ports.PostEntryLeg {
	return mkLeg(types.SystemMerchantID, testChain, "ETH", acc, dir, amount)
}

func usdcLeg(owner uuid.UUID, acc types.LedgerAccountCode, dir types.Direction, amount string) ports.PostEntryLeg {
	return mkLeg(owner, testChain, "USDC", acc, dir, amount)
}

func balanceOf(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, chain types.ChainID,
	asset string, acc types.LedgerAccountCode) string {
	t.Helper()
	var bal string
	err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(balance),0)::text FROM ledger_accounts
		WHERE owner_merchant_id=$1 AND chain_id=$2 AND asset_symbol=$3 AND account_type=$4`,
		owner, int64(chain), asset, acc.String()).Scan(&bal)
	if err != nil {
		t.Fatalf("balanceOf %s: %v", acc, err)
	}
	return bal
}

func mustPost(t *testing.T, s *Service, req ports.PostEntryRequest) ports.PostedEntry {
	t.Helper()
	p, err := s.PostEntry(context.Background(), req)
	if err != nil {
		t.Fatalf("PostEntry(%s): %v", req.ReferenceID, err)
	}
	return p
}

// ---------------------------------------------------------------------------
// 无 PG 纯逻辑测试
// ---------------------------------------------------------------------------

func TestConsolidateLegs_MergeAndNet(t *testing.T) {
	owner := testMerchant
	legs := []ports.PostEntryLeg{
		usdcLeg(owner, types.AccountMerchantAvailable, types.DirectionCredit, "100"),
		usdcLeg(owner, types.AccountMerchantAvailable, types.DirectionCredit, "50"),
		usdcLeg(owner, types.AccountMerchantAvailable, types.DirectionDebit, "30"),
		usdcLeg(owner, types.AccountOrderEscrowLiab, types.DirectionDebit, "120"),
	}
	out, err := consolidateLegs(legs)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	// 同账户 CREDIT 合并 + DEBIT 对消 -> net CREDIT 120.
	if len(out) != 2 {
		t.Fatalf("consolidated legs = %d, want 2", len(out))
	}
	for _, l := range out {
		if l.Scope.Account == types.AccountMerchantAvailable {
			if l.Direction != types.DirectionCredit || l.Amount.String() != "120" {
				t.Fatalf("MERCHANT_AVAILABLE net = %s %s, want CREDIT 120", l.Direction, l.Amount)
			}
		}
	}
}

func TestConsolidateLegs_FullNetDropsLeg(t *testing.T) {
	legs := []ports.PostEntryLeg{
		usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionCredit, "100"),
		usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionDebit, "100"),
	}
	if _, err := consolidateLegs(legs); !errors.Is(err, ports.ErrUnbalancedEntry) {
		t.Fatalf("full-net must be rejected as empty posting, got %v", err)
	}
}

func TestValidateLegs_TemplateWhitelist(t *testing.T) {
	// ORDER_SETTLE 合法腿: DR SUSPENSE_REFUNDABLE_LIAB / CR MERCHANT_AVAILABLE+FEE_REVENUE.
	ok := []leg{
		{Scope: accountScope{Account: types.AccountSuspenseRefundableLiab}, Direction: types.DirectionDebit, Amount: big.NewInt(100)},
		{Scope: accountScope{Account: types.AccountMerchantAvailable}, Direction: types.DirectionCredit, Amount: big.NewInt(90)},
		{Scope: accountScope{Account: types.AccountFeeRevenue}, Direction: types.DirectionCredit, Amount: big.NewInt(10)},
	}
	if err := validateLegs(types.TemplateOrderSettle, ok); err != nil {
		t.Fatalf("valid legs rejected: %v", err)
	}
	// 非法组合: DR MERCHANT_AVAILABLE / CR FEE_REVENUE 不在白名单.
	bad := []leg{
		{Scope: accountScope{Account: types.AccountMerchantAvailable}, Direction: types.DirectionDebit, Amount: big.NewInt(10)},
		{Scope: accountScope{Account: types.AccountFeeRevenue}, Direction: types.DirectionCredit, Amount: big.NewInt(10)},
	}
	if err := validateLegs(types.TemplateOrderSettle, bad); !errors.Is(err, ports.ErrInvalidTemplateLeg) {
		t.Fatalf("invalid pair must fail with ErrInvalidTemplateLeg, got %v", err)
	}
	// 不平: SUM 不等.
	unbal := []leg{
		{Scope: accountScope{Account: types.AccountSuspenseRefundableLiab}, Direction: types.DirectionDebit, Amount: big.NewInt(100)},
		{Scope: accountScope{Account: types.AccountMerchantAvailable}, Direction: types.DirectionCredit, Amount: big.NewInt(90)},
	}
	if err := validateLegs(types.TemplateOrderSettle, unbal); !errors.Is(err, ports.ErrUnbalancedEntry) {
		t.Fatalf("unbalanced legs must fail, got %v", err)
	}
	// REORG_REVERSAL 复合冲回: DR {HOT_WALLET_NATIVE, ORDER_ESCROW_LIAB}
	// CR {GAS_EXPENSE, CUSTODY_UNSWEPT} — HWN->CUSTODY_UNSWEPT 命中非法组合被拒
	// (全对笛卡尔校验: 复合冲回必须拆事务).
	mixed := []leg{
		{Scope: accountScope{Account: types.AccountHotWalletNative}, Direction: types.DirectionDebit, Amount: big.NewInt(5)},
		{Scope: accountScope{Account: types.AccountOrderEscrowLiab}, Direction: types.DirectionDebit, Amount: big.NewInt(10)},
		{Scope: accountScope{Account: types.AccountGasExpense}, Direction: types.DirectionCredit, Amount: big.NewInt(5)},
		{Scope: accountScope{Account: types.AccountCustodyUnswept}, Direction: types.DirectionCredit, Amount: big.NewInt(10)},
	}
	if err := validateLegs(types.TemplateReorgReversal, mixed); !errors.Is(err, ports.ErrInvalidTemplateLeg) {
		t.Fatalf("cross-rule composite must fail cross-product check, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 集成测试 (真实 PG)
// ---------------------------------------------------------------------------

func TestPostEntry_TreasuryTopupAndBalances(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	s := NewService(pool)

	posted := mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateTreasuryTopup,
		ReferenceID: "topup-1",
		Description: "initial gas injection",
		Legs: []ports.PostEntryLeg{
			sysLeg(types.AccountHotWalletNative, types.DirectionDebit, "1000000"),
			sysLeg(types.AccountTreasuryEquityCapital, types.DirectionCredit, "1000000"),
		},
	})
	if posted.AlreadyExisted {
		t.Fatal("first post must not be idempotent")
	}
	if len(posted.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(posted.Entries))
	}
	if got := balanceOf(t, pool, types.SystemMerchantID, testChain, "ETH", types.AccountHotWalletNative); got != "1000000" {
		t.Fatalf("HOT_WALLET_NATIVE = %s, want 1000000", got)
	}
	if got := balanceOf(t, pool, types.SystemMerchantID, testChain, "ETH", types.AccountTreasuryEquityCapital); got != "1000000" {
		t.Fatalf("TREASURY_EQUITY_CAPITAL = %s, want 1000000", got)
	}
	// balance_after 快照断言.
	for _, e := range posted.Entries {
		if e.BalanceAfter.String() != "1000000" {
			t.Errorf("entry %s balance_after = %s, want 1000000", e.Direction, e.BalanceAfter)
		}
	}
}

func TestPostEntry_Idempotent(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	s := NewService(pool)
	req := ports.PostEntryRequest{
		Template:    types.TemplateTreasuryTopup,
		ReferenceID: "topup-idem",
		Legs: []ports.PostEntryLeg{
			sysLeg(types.AccountHotWalletNative, types.DirectionDebit, "500"),
			sysLeg(types.AccountTreasuryEquityCapital, types.DirectionCredit, "500"),
		},
	}
	first := mustPost(t, s, req)
	second, err := s.PostEntry(context.Background(), req)
	if err != nil {
		t.Fatalf("idempotent repost: %v", err)
	}
	if !second.AlreadyExisted {
		t.Fatal("repost must hit AlreadyExisted")
	}
	if second.Transaction.ID != first.Transaction.ID {
		t.Errorf("idempotent txn id mismatch")
	}
	if got := balanceOf(t, pool, types.SystemMerchantID, testChain, "ETH", types.AccountHotWalletNative); got != "500" {
		t.Fatalf("balance = %s after repost, want 500", got)
	}
	// 同 ref 异内容 (仍平衡, 进入幂等比对路径) -> ErrDuplicateReference.
	req.Legs[0].Amount = types.MustParseU256("999")
	req.Legs[1].Amount = types.MustParseU256("999")
	if _, err := s.PostEntry(context.Background(), req); !errors.Is(err, ports.ErrDuplicateReference) {
		t.Fatalf("mismatched repost must fail ErrDuplicateReference, got %v", err)
	}
}

func TestPostEntry_NegativeBalanceRejected(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	s := NewService(pool)
	_, err := s.PostEntry(context.Background(), ports.PostEntryRequest{
		Template:    types.TemplateInternalTreasuryRebalance,
		ReferenceID: "rebal-neg",
		Legs: []ports.PostEntryLeg{
			usdcLeg(types.SystemMerchantID, types.AccountCustodyHotPayout, types.DirectionDebit, "100"),
			usdcLeg(types.SystemMerchantID, types.AccountCustodyCold, types.DirectionCredit, "100"),
		},
	})
	if !errors.Is(err, ports.ErrNegativeBalance) {
		t.Fatalf("debit beyond funds must fail ErrNegativeBalance, got %v", err)
	}
}

func TestPostEntry_OrderLifecycleFlow(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	s := NewService(pool)
	ctx := context.Background()

	// 1) 入站确认: DR CUSTODY_UNSWEPT / CR ORDER_ESCROW_LIAB (100 USDC).
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateDepositFinalized,
		ReferenceID: "dep-1",
		Legs: []ports.PostEntryLeg{
			usdcLeg(types.SystemMerchantID, types.AccountCustodyUnswept, types.DirectionDebit, "100"),
			usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionCredit, "100"),
		},
	})
	// 2) 订单结算: DR ORDER_ESCROW_LIAB 100 / CR MERCHANT_AVAILABLE 99 + FEE_REVENUE 1.
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateOrderSettle,
		ReferenceID: "settle-1",
		Legs: []ports.PostEntryLeg{
			usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionDebit, "100"),
			usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionCredit, "99"),
			usdcLeg(types.SystemMerchantID, types.AccountFeeRevenue, types.DirectionCredit, "1"),
		},
	})
	// 3) 提现冻结: DR MERCHANT_AVAILABLE 50 / CR PAYOUT_LOCKED_LIAB 50.
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplatePayoutFlow,
		ReferenceID: "payout_freeze:x",
		Legs: []ports.PostEntryLeg{
			usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionDebit, "50"),
			usdcLeg(testMerchant, types.AccountPayoutLockedLiab, types.DirectionCredit, "50"),
		},
	})
	// 3.5) 平台备付金注资热钱包: DR CUSTODY_HOT_PAYOUT 50 / CR TREASURY_EQUITY_CAPITAL.
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateTreasuryTopup,
		ReferenceID: "topup-payout",
		Legs: []ports.PostEntryLeg{
			usdcLeg(types.SystemMerchantID, types.AccountCustodyHotPayout, types.DirectionDebit, "50"),
			usdcLeg(types.SystemMerchantID, types.AccountTreasuryEquityCapital, types.DirectionCredit, "50"),
		},
	})
	// 4) 提现成功: DR PAYOUT_LOCKED_LIAB 50 / CR CUSTODY_HOT_PAYOUT 50.
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplatePayoutFlow,
		ReferenceID: "payout_settle:x",
		Legs: []ports.PostEntryLeg{
			usdcLeg(testMerchant, types.AccountPayoutLockedLiab, types.DirectionDebit, "50"),
			usdcLeg(types.SystemMerchantID, types.AccountCustodyHotPayout, types.DirectionCredit, "50"),
		},
	})

	checks := map[string]struct {
		owner uuid.UUID
		acc   types.LedgerAccountCode
		want  string
	}{
		"unswept":   {types.SystemMerchantID, types.AccountCustodyUnswept, "100"},
		"escrow":    {testMerchant, types.AccountOrderEscrowLiab, "0"},
		"available": {testMerchant, types.AccountMerchantAvailable, "49"},
		"locked":    {testMerchant, types.AccountPayoutLockedLiab, "0"},
		"fee":       {types.SystemMerchantID, types.AccountFeeRevenue, "1"},
	}
	for name, c := range checks {
		if got := balanceOf(t, pool, c.owner, testChain, "USDC", c.acc); got != c.want {
			t.Errorf("%s balance = %s, want %s", name, got, c.want)
		}
	}
	// outbox: 商户相关记账写入 webhook_outbox (merchant 为接收方).
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_outbox WHERE merchant_id=$1`, testMerchant).Scan(&cnt); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if cnt < 3 {
		t.Errorf("merchant outbox rows = %d, want >= 3", cnt)
	}
}

func TestReorgReversal_SpillAndFreeze(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	s := NewService(pool)
	ctx := context.Background()

	// 前置: 商户 MERCHANT_AVAILABLE = 60 (USDC), FEE_REVENUE = 10, CUSTODY_UNSWEPT = 110.
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateDepositFinalized,
		ReferenceID: "dep-seed",
		Legs: []ports.PostEntryLeg{
			usdcLeg(types.SystemMerchantID, types.AccountCustodyUnswept, types.DirectionDebit, "110"),
			usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionCredit, "110"),
		},
	})
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateOrderSettle,
		ReferenceID: "settle-seed",
		Legs: []ports.PostEntryLeg{
			usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionDebit, "110"),
			usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionCredit, "100"),
			usdcLeg(types.SystemMerchantID, types.AccountFeeRevenue, types.DirectionCredit, "10"),
		},
	})
	// 商户提走 40 -> 可用剩 60.
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplatePayoutFlow,
		ReferenceID: "pf-seed",
		Legs: []ports.PostEntryLeg{
			usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionDebit, "40"),
			usdcLeg(testMerchant, types.AccountPayoutLockedLiab, types.DirectionCredit, "40"),
		},
	})

	// 深回滚冲回已结单 110: 需 DR MA 100 + FR 10 -> 但 MA 只剩 60,
	// 差额 40 应自动转 MERCHANT_REORG_RECEIVABLE 且 payout_frozen = true.
	posted, err := s.PostEntry(ctx, ports.PostEntryRequest{
		Template:    types.TemplateReorgReversal,
		ReferenceID: "reorg-inbound-1",
		Legs: []ports.PostEntryLeg{
			usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionDebit, "100"),
			usdcLeg(types.SystemMerchantID, types.AccountFeeRevenue, types.DirectionDebit, "10"),
			usdcLeg(types.SystemMerchantID, types.AccountCustodyUnswept, types.DirectionCredit, "110"),
		},
	})
	if err != nil {
		t.Fatalf("reorg reversal: %v", err)
	}
	// 期望腿: DR MRR 40 + DR MA 60 + DR FR 10 -> CR CU 110.
	var mrr, ma *types.LedgerEntry
	// 从 entries 反查 scope 需要 account_id -> 直接查库断言.
	if got := balanceOf(t, pool, testMerchant, testChain, "USDC", types.AccountMerchantAvailable); got != "0" {
		t.Fatalf("MERCHANT_AVAILABLE = %s, want 0", got)
	}
	if got := balanceOf(t, pool, testMerchant, testChain, "USDC", types.AccountMerchantReorgReceivable); got != "40" {
		t.Fatalf("MERCHANT_REORG_RECEIVABLE = %s, want 40", got)
	}
	if got := balanceOf(t, pool, types.SystemMerchantID, testChain, "USDC", types.AccountCustodyUnswept); got != "0" {
		t.Fatalf("CUSTODY_UNSWEPT = %s, want 0", got)
	}
	_ = mrr
	_ = ma
	_ = posted
	var frozen bool
	if err := pool.QueryRow(ctx, `SELECT payout_frozen FROM merchants WHERE id=$1`, testMerchant).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if !frozen {
		t.Fatal("payout_frozen must be true after reorg shortfall")
	}
}

// seedAccount 直接播种 ledger_accounts 余额 (绕过模板白名单的建户路径).
func seedAccount(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, acc types.LedgerAccountCode, bal string) {
	t.Helper()
	side := "DEBIT"
	if acc.NormalSide() == types.DirectionCredit {
		side = "CREDIT"
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
		 VALUES ($1,$2,'USDC',$3,$4,$5::numeric)`,
		owner, int64(testChain), string(acc), side, bal); err != nil {
		t.Fatalf("seed account %s: %v", acc, err)
	}
}

// TestReorgReversal_LiabilitySpillAllScopes (FIX-A/A2):
// 模板 9 透支拆分对全部商户自有负债科目生效 — ORDER_ESCROW_LIAB /
// 三个 SUSPENSE_*_LIAB / MERCHANT_AVAILABLE 均走「锁内钳制 + 差额转
// MERCHANT_REORG_RECEIVABLE + 同事务 payout_frozen」路径.
func TestReorgReversal_LiabilitySpillAllScopes(t *testing.T) {
	ctx := context.Background()
	for _, liab := range []types.LedgerAccountCode{
		types.AccountOrderEscrowLiab,
		types.AccountSuspenseOverpayLiab,
		types.AccountSuspenseRefundableLiab,
		types.AccountSuspenseUnmatchedLiab,
		types.AccountMerchantAvailable,
	} {
		t.Run(string(liab), func(t *testing.T) {
			pool := newPool(t)
			resetLedger(t, pool)
			s := NewService(pool)

			// 负债科目余额 70, 冲回借方 90 -> 差额 20 转应收.
			seedAccount(t, pool, testMerchant, liab, "70")
			seedAccount(t, pool, types.SystemMerchantID, types.AccountCustodyUnswept, "200")
			if _, err := s.PostEntry(ctx, ports.PostEntryRequest{
				Template:    types.TemplateReorgReversal,
				ReferenceID: "spill-" + string(liab),
				Legs: []ports.PostEntryLeg{
					usdcLeg(testMerchant, liab, types.DirectionDebit, "90"),
					usdcLeg(types.SystemMerchantID, types.AccountCustodyUnswept, types.DirectionCredit, "90"),
				},
			}); err != nil {
				t.Fatalf("reorg reversal on %s: %v", liab, err)
			}
			if got := balanceOf(t, pool, testMerchant, testChain, "USDC", liab); got != "0" {
				t.Fatalf("%s = %s, want 0 (clamped at locked balance 70)", liab, got)
			}
			if got := balanceOf(t, pool, testMerchant, testChain, "USDC", types.AccountMerchantReorgReceivable); got != "20" {
				t.Fatalf("MERCHANT_REORG_RECEIVABLE = %s, want 20", got)
			}
			var frozen bool
			if err := pool.QueryRow(ctx, `SELECT payout_frozen FROM merchants WHERE id=$1`, testMerchant).Scan(&frozen); err != nil {
				t.Fatal(err)
			}
			if !frozen {
				t.Fatalf("payout_frozen must be set for %s overdraft", liab)
			}
		})
	}
}

// TestReorgReversal_PlatformCustodyNoSpill (FIX-A/A2):
// 平台托管科目 (借方科目 + 系统哨兵持有) 透支绝不转商户应收 — 直接拒账.
func TestReorgReversal_PlatformCustodyNoSpill(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	s := NewService(pool)

	seedAccount(t, pool, types.SystemMerchantID, types.AccountCustodyUnswept, "50")
	_, err := s.PostEntry(context.Background(), ports.PostEntryRequest{
		Template:    types.TemplateReorgReversal,
		ReferenceID: "plat-nospill",
		Legs: []ports.PostEntryLeg{
			usdcLeg(types.SystemMerchantID, types.AccountCustodyUnswept, types.DirectionDebit, "80"),
			usdcLeg(types.SystemMerchantID, types.AccountCustodyCold, types.DirectionCredit, "80"),
		},
	})
	if !errors.Is(err, ports.ErrNegativeBalance) {
		t.Fatalf("platform custody overdraft must fail ErrNegativeBalance, got %v", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ledger_accounts WHERE account_type='MERCHANT_REORG_RECEIVABLE'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("platform overdraft must not create receivable accounts, got %d", n)
	}
}

// TestPostEntry_ConcurrentNoDeadlock 并发对同一组账户记账:
// account_id ASC 单条 SELECT FOR UPDATE 协议下无死锁且余额一致.
func TestPostEntry_ConcurrentNoDeadlock(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	s := NewService(pool)
	ctx := context.Background()

	// 播种: CU=1000, escrow=1000; 预结 100 使 MA=100 > 冻结峰值 40
	// (冻结与结算并发交错时不会真实透支).
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateDepositFinalized,
		ReferenceID: "dep-conc",
		Legs: []ports.PostEntryLeg{
			usdcLeg(types.SystemMerchantID, types.AccountCustodyUnswept, types.DirectionDebit, "1000"),
			usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionCredit, "1000"),
		},
	})
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateOrderSettle,
		ReferenceID: "settle-conc-seed",
		Legs: []ports.PostEntryLeg{
			usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionDebit, "100"),
			usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionCredit, "100"),
		},
	})

	const workers = 8
	errs := make(chan error, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(2)
		// 方向 1: escrow -> available (结算方向).
		go func() {
			defer wg.Done()
			_, err := s.PostEntry(ctx, ports.PostEntryRequest{
				Template:    types.TemplateOrderSettle,
				ReferenceID: fmt.Sprintf("settle-c-%d", i),
				Legs: []ports.PostEntryLeg{
					usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionDebit, "10"),
					usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionCredit, "10"),
				},
			})
			errs <- err
		}()
		// 方向 2: available -> locked (冻结方向), 交叉账户顺序制造死锁面.
		go func() {
			defer wg.Done()
			_, err := s.PostEntry(ctx, ports.PostEntryRequest{
				Template:    types.TemplatePayoutFlow,
				ReferenceID: fmt.Sprintf("freeze-c-%d", i),
				Legs: []ports.PostEntryLeg{
					usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionDebit, "5"),
					usdcLeg(testMerchant, types.AccountPayoutLockedLiab, types.DirectionCredit, "5"),
				},
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent posting failed (deadlock?): %v", err)
		}
	}
	// escrow 1000-100-80=820, MA 100+80-40=140, locked 40.
	if got := balanceOf(t, pool, testMerchant, testChain, "USDC", types.AccountOrderEscrowLiab); got != "820" {
		t.Fatalf("escrow = %s, want 820", got)
	}
	if got := balanceOf(t, pool, testMerchant, testChain, "USDC", types.AccountMerchantAvailable); got != "140" {
		t.Fatalf("MA = %s, want 140 (100+80-40)", got)
	}
	if got := balanceOf(t, pool, testMerchant, testChain, "USDC", types.AccountPayoutLockedLiab); got != "40" {
		t.Fatalf("locked = %s, want 40", got)
	}
}

func TestGetBalances(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	s := NewService(pool)
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateTreasuryTopup,
		ReferenceID: "topup-bal",
		Legs: []ports.PostEntryLeg{
			sysLeg(types.AccountHotWalletNative, types.DirectionDebit, "7"),
			sysLeg(types.AccountTreasuryEquityCapital, types.DirectionCredit, "7"),
		},
	})
	bals, err := s.GetBalances(context.Background(), ports.BalanceQuery{
		OwnerMerchantID: types.SystemMerchantID, ChainID: testChain, AssetSymbol: "ETH",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bals) != 2 {
		t.Fatalf("balances = %d, want 2", len(bals))
	}
	// 过滤单科目.
	bals, err = s.GetBalances(context.Background(), ports.BalanceQuery{
		OwnerMerchantID: types.SystemMerchantID, ChainID: testChain, AssetSymbol: "ETH",
		Accounts: []types.LedgerAccountCode{types.AccountHotWalletNative},
	})
	if err != nil || len(bals) != 1 || bals[0].Balance.String() != "7" {
		t.Fatalf("filtered balance = %+v, err=%v", bals, err)
	}
}

func TestInvariantCheck_TwoLayers(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	s := NewService(pool)
	ctx := context.Background()

	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateDepositFinalized,
		ReferenceID: "dep-inv",
		Legs: []ports.PostEntryLeg{
			usdcLeg(types.SystemMerchantID, types.AccountCustodyUnswept, types.DirectionDebit, "100"),
			usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionCredit, "100"),
		},
	})
	mustPost(t, s, ports.PostEntryRequest{
		Template:    types.TemplateTreasuryTopup,
		ReferenceID: "topup-inv",
		Legs: []ports.PostEntryLeg{
			sysLeg(types.AccountHotWalletNative, types.DirectionDebit, "3"),
			sysLeg(types.AccountTreasuryEquityCapital, types.DirectionCredit, "3"),
		},
	})

	// 第一层: USDC 维度试算平衡 (UNSWEPT 100 == ESCROW 100).
	rep, err := s.RunInvariantCheck(ctx, ports.InvariantCheckRequest{
		ChainID: testChain, AssetSymbol: "USDC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.TrialBalanced {
		t.Fatalf("trial balance must hold, got %v", rep.Violations)
	}
	if rep.TotalDebits.String() != "100" || rep.TotalCredits.String() != "100" {
		t.Fatalf("totals = %s/%s", rep.TotalDebits, rep.TotalCredits)
	}
	if len(rep.ChainDiffs) != 0 {
		t.Error("snapshot nil must skip layer 2")
	}

	// 第二层: 正确快照 -> 无违例.
	rep2, err := s.RunInvariantCheck(ctx, ports.InvariantCheckRequest{
		ChainID: testChain, AssetSymbol: "USDC", SafeHeadHeight: 42,
		Snapshot: &ports.ChainSnapshot{
			UnsweptVaultTotal:     types.MustParseU256("100"),
			ColdWalletBalance:     types.MustParseU256("0"),
			HotPayoutTokenBalance: types.MustParseU256("0"),
			HotWalletNativeTotal:  types.MustParseU256("3"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Violations) != 0 {
		t.Fatalf("clean snapshot must produce no violations, got %v", rep2.Violations)
	}
	if len(rep2.ChainDiffs) != 4 {
		t.Fatalf("diffs = %d, want 4", len(rep2.ChainDiffs))
	}

	// 第二层: 偏移快照 -> 差异违例.
	rep3, err := s.RunInvariantCheck(ctx, ports.InvariantCheckRequest{
		ChainID: testChain, AssetSymbol: "USDC",
		Snapshot: &ports.ChainSnapshot{
			UnsweptVaultTotal:    types.MustParseU256("99"),
			HotWalletNativeTotal: types.MustParseU256("3"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep3.Violations) == 0 {
		t.Fatal("skewed snapshot must produce violations")
	}
}
