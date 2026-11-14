package ledger

// FIX-E/E3 回归: 全局锁序 merchants → ledger_accounts.
//
// createPayoutLocked (order) 持 merchants 行锁后进账本锁账户;
// 模板 9 (REORG_REVERSAL) 原实现先锁账户再在 freezeMerchants 回写
// merchants — 两路交叉即 SQLSTATE 40P01. 修复后模板 9 在 lockAccounts
// 之前对可能被冻结的商户按 id ASC FOR UPDATE 预锁, 两条取锁序列
// 归并为单向全序, 本测试以并发压力循环证明 40P01 不再出现.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

func isDeadlockErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// 并发【商户提现冻结: merchants FOR UPDATE -> 账本冻结腿】×
// 【深回滚红冲: 账户锁 -> 透支拆分 -> merchants 回写】压力循环.
// T2 以大额透支腿保证每轮都走 receivable 拆分 + payout_frozen 回写,
// 即每轮都真实穿越 merchants→accounts 锁序边界; 修复前该交错面
// 在锁持有窗口内产生 40P01, 修复后单向全序杜绝.
func TestReorgReversalNoMerchantAccountDeadlock(t *testing.T) {
	pool := newPool(t)
	resetLedger(t, pool)
	svc := NewService(pool)
	ctx := context.Background()

	// 期初: DR UNSWEPT/CR ESCROW (入金) + DR ESCROW/CR MA (结算) =>
	// 商户 MA 5000, 系统 UNSWEPT 5000 (模板白名单合法腿序).
	mustPost(t, svc, ports.PostEntryRequest{
		Template:    types.TemplateDepositFinalized,
		ReferenceID: "e3:seed:dep",
		Legs: []ports.PostEntryLeg{
			usdcLeg(types.SystemMerchantID, types.AccountCustodyUnswept, types.DirectionDebit, "5000"),
			usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionCredit, "5000"),
		},
	})
	mustPost(t, svc, ports.PostEntryRequest{
		Template:    types.TemplateOrderSettle,
		ReferenceID: "e3:seed:settle",
		Legs: []ports.PostEntryLeg{
			usdcLeg(testMerchant, types.AccountOrderEscrowLiab, types.DirectionDebit, "5000"),
			usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionCredit, "5000"),
		},
	})

	const rounds = 30
	errCh := make(chan error, rounds*2)
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		i := i
		// T1: 模拟 createPayoutLocked — 先 merchants 行锁, 再账本冻结腿.
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx,
					`SELECT id FROM merchants WHERE id=$1 FOR UPDATE`, testMerchant); err != nil {
					return err
				}
				time.Sleep(2 * time.Millisecond) // 拉开锁持有窗口, 放大交错面
				_, err := svc.PostEntryInTx(ctx, tx, ports.PostEntryRequest{
					Template:    types.TemplatePayoutFlow,
					ReferenceID: fmt.Sprintf("e3:freeze:%d", i),
					Legs: []ports.PostEntryLeg{
						usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionDebit, "1"),
						usdcLeg(testMerchant, types.AccountPayoutLockedLiab, types.DirectionCredit, "1"),
					},
				})
				return err
			})
			if err != nil {
				errCh <- err
			}
		}()
		// T2: 深回滚红冲 — 大额透支强制 receivable 拆分 + merchants 回写.
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.PostEntry(ctx, ports.PostEntryRequest{
				Template:    types.TemplateReorgReversal,
				ReferenceID: fmt.Sprintf("e3:reorg:%d", i),
				Legs: []ports.PostEntryLeg{
					usdcLeg(testMerchant, types.AccountMerchantAvailable, types.DirectionDebit, "10000"),
					usdcLeg(types.SystemMerchantID, types.AccountCustodyUnswept, types.DirectionCredit, "10000"),
				},
			})
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if isDeadlockErr(err) {
			t.Fatalf("40P01 deadlock — merchants→accounts lock order violated: %v", err)
		}
		var perr *pgconn.PgError
		if errors.As(err, &perr) {
			t.Fatalf("unexpected pg error %s: %v", perr.Code, err)
		}
		// 业务级错误 (透支拒账/负余额) 属时序合法的失败面, 不视为缺陷.
	}
}
