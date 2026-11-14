package api

import (
	"context"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// custodyCheckName 第二层四项核对 -> openapi CustodyCheckType 映射.
var custodyCheckName = map[types.LedgerAccountCode]string{
	types.AccountCustodyUnswept:   "UNSWEPT_DEPOSIT_VAULTS",
	types.AccountCustodyCold:      "COLD_WALLET_BALANCE",
	types.AccountCustodyHotPayout: "PAYOUT_HOT_WALLET_BALANCE",
	types.AccountHotWalletNative:  "HOT_WALLETS_NATIVE_BALANCE",
}

// reconReport 内部最近一次巡检存档 (run_id 追踪异步执行).
type reconReport struct {
	dto reconciliationReportDTO
}

// ReconManager 对账巡检编排: 单飞执行 + 最近一次报告缓存.
// openapi 语义: NEVER_RUN 初态; RUNNING 期再触发 -> 409 RECONCILIATION_RUNNING.
type ReconManager struct {
	deps *Deps

	mu      sync.Mutex
	running bool
	last    *reconReport
}

func newReconManager(deps *Deps) *ReconManager {
	return &ReconManager{deps: deps}
}

// status 返回最近一次巡检报告 (无记录返回 NEVER_RUN).
func (rm *ReconManager) status() reconciliationReportDTO {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.running {
		if rm.last != nil {
			d := rm.last.dto
			d.Status = "RUNNING"
			return d
		}
		return reconciliationReportDTO{Status: "RUNNING"}
	}
	if rm.last == nil {
		return reconciliationReportDTO{Status: "NEVER_RUN"}
	}
	return rm.last.dto
}

// trigger 启动一次异步巡检; 已在运行返回 false.
func (rm *ReconManager) trigger(chainID *types.ChainID) (uuid.UUID, bool) {
	rm.mu.Lock()
	if rm.running {
		rm.mu.Unlock()
		return uuid.Nil, false
	}
	rm.running = true
	rm.mu.Unlock()

	runID := uuid.New()
	started := rm.deps.now()
	rm.mu.Lock()
	rm.last = &reconReport{dto: reconciliationReportDTO{
		RunID: strPtr(runID.String()), Trigger: "MANUAL", Status: "RUNNING",
		StartedAt: &started,
	}}
	rm.mu.Unlock()

	go rm.execute(runID, chainID, started)
	return runID, true
}

// execute 遍历 (chain, asset) 逐对调用 LedgerService.RunInvariantCheck 聚合报告.
func (rm *ReconManager) execute(runID uuid.UUID, chainID *types.ChainID, started time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rep := reconciliationReportDTO{
		RunID: strPtr(runID.String()), Trigger: "MANUAL", StartedAt: &started,
	}
	failed := false
	execErr := ""

	defer func() {
		finished := rm.deps.now()
		rep.FinishedAt = &finished
		if execErr != "" {
			rep.Status = "FAILED"
			rep.Failures = append(rep.Failures, execErr)
		} else if failed {
			rep.Status = "FAILED"
		} else {
			rep.Status = "PASSED"
		}
		rm.mu.Lock()
		rm.last = &reconReport{dto: rep}
		rm.running = false
		rm.mu.Unlock()
	}()

	if rm.deps.Ledger == nil || rm.deps.Store == nil {
		execErr = "ledger service or read store not wired"
		return
	}

	assets, err := rm.deps.Store.ListSupportedAssets(ctx)
	if err != nil {
		execErr = "list supported assets: " + err.Error()
		return
	}
	for _, a := range assets {
		if chainID != nil && a.ChainID != *chainID {
			continue
		}
		var safeHead uint64
		if cur, err := rm.deps.Store.GetChainSyncCursor(ctx, a.ChainID); err == nil {
			safeHead = cur.SafeHeadHeight
		}
		inv, err := rm.deps.Ledger.RunInvariantCheck(ctx, ports.InvariantCheckRequest{
			ChainID:        a.ChainID,
			AssetSymbol:    a.AssetSymbol,
			SafeHeadHeight: safeHead,
			// Snapshot 链上快照由 W4 链查询器供给 (Wave-2 装配后注入);
			// 当前 nil = 仅第一层账本内试算平衡.
			Snapshot: nil,
		})
		if err != nil {
			rm.deps.Logger.Warn("invariant check failed for pair",
				zap.Uint64("chain_id", uint64(a.ChainID)), zap.String("asset", a.AssetSymbol),
				zap.Error(err))
			failed = true
			rep.Failures = append(rep.Failures, a.AssetSymbol+"@"+itoa64(uint64(a.ChainID))+": "+err.Error())
			continue
		}
		rep.TrialBalances = append(rep.TrialBalances, trialBalanceRowDTO{
			ChainID:                int64(a.ChainID),
			AssetSymbol:            a.AssetSymbol,
			TotalDebit:             inv.TotalDebits,
			TotalCredit:            inv.TotalCredits,
			Balanced:               inv.TrialBalanced,
			AllBalancesNonNegative: len(inv.NegativeAccounts) == 0,
		})
		for _, d := range inv.ChainDiffs {
			checkName, ok := custodyCheckName[d.Account]
			if !ok {
				checkName = d.Account.String()
			}
			rep.CustodyChecks = append(rep.CustodyChecks, custodyCheckDTO{
				Check:          checkName,
				ChainID:        int64(a.ChainID),
				AssetSymbol:    a.AssetSymbol,
				LedgerAccount:  d.Account.String(),
				LedgerBalance:  d.Ledger,
				OnChainBalance: d.OnChain,
				Delta:          deltaString(d.Delta),
				Matched:        d.Delta == nil || d.Delta.Sign() == 0,
				SafeHeadHeight: inv.SafeHeadHeight,
			})
		}
		if !inv.TrialBalanced || len(inv.NegativeAccounts) > 0 || len(inv.Violations) > 0 {
			failed = true
		}
		rep.Failures = append(rep.Failures, inv.Violations...)
	}
}

func strPtr(s string) *string { return &s }

func deltaString(d *big.Int) string {
	if d == nil {
		return "0"
	}
	return d.String()
}

func itoa64(v uint64) string { return strconv.FormatUint(v, 10) }
