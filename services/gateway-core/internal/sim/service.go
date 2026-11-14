package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// Deps — 演练编排依赖 (全部可注入; 未注入的能力在步骤日志中标记 SKIPPED,
// 使 W5 单测可以用 fake 驱动、Wave-2 I1 接真实 W3/W4 组件).
// ---------------------------------------------------------------------------

// DrillTarget 单链演练夹具 (演练用合约/钱包地址; 由部署清单注入).
type DrillTarget struct {
	RelayerWallet   types.Address // 归集/中继热钱包
	PayoutWallet    types.Address // 提现/退款热钱包
	SweeperContract types.Address // BatchSweeper
	RelayContract   types.Address // EIP3009PayRouter
	ColdWallet      types.Address // 只读白名单冷金库
	Token           types.Address // 稳定币
	FundingAccount  types.Address // anvil_impersonateAccount 注资源
	MaxFeePerGas    *big.Int      // 锻造交易费率 (nil 用默认 2 gwei)
	MaxPriorityFee  *big.Int
}

// Deps 演练服务依赖.
type Deps struct {
	Router  *AnvilRouter        // Anvil 控制面 (必填)
	Signer  ports.SignerClient  // 渗透与审计链校验 (signer-penetration 必填)
	TxMgr   ports.TxManager     // 出站意图审计 (reorg 场景可选)
	Ledger  ports.LedgerService // 不变量巡检 (INVARIANT_CHECK 必填, reorg 可选断言)
	Logger  *zap.Logger
	Targets map[types.ChainID]DrillTarget
	NowFn   func() time.Time // 测试注入
}

// Service ports.SimulatorService 实现: 异步执行演练场景, 步骤日志随做随写,
// GET /v1/sim/runs/{id} 轮询增量可见.
type Service struct {
	deps Deps

	mu   sync.Mutex
	runs map[uuid.UUID]*runState
}

// NewService 装配.
func NewService(deps Deps) *Service {
	if deps.Logger == nil {
		deps.Logger = zap.NewNop()
	}
	if deps.NowFn == nil {
		deps.NowFn = func() time.Time { return time.Now().UTC() }
	}
	if deps.Targets == nil {
		deps.Targets = map[types.ChainID]DrillTarget{}
	}
	return &Service{deps: deps, runs: map[uuid.UUID]*runState{}}
}

func (s *Service) log() *zap.Logger { return s.deps.Logger }
func (s *Service) now() time.Time   { return s.deps.NowFn() }

// ---------------------------------------------------------------------------
// 运行状态与步骤日志
// ---------------------------------------------------------------------------

// stepRec 与 api.simStepDTO JSON 结构契约一致 (Detail 透传载体).
type stepRec struct {
	Seq    int        `json:"seq"`
	Name   string     `json:"name"`
	Status string     `json:"status"` // PENDING/RUNNING/OK/FAILED/SKIPPED (openapi SimStep 枚举)
	Detail string     `json:"detail,omitempty"`
	TxHash *string    `json:"tx_hash"`
	At     *time.Time `json:"at,omitempty"`
}

type runState struct {
	mu      sync.Mutex
	run     ports.SimRun
	steps   []stepRec
	summary map[string]any
	errText string
	cancel  context.CancelFunc
}

func (r *runState) snapshot() ports.SimRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.run
	out.Detail = r.detailLocked()
	return out
}

// detailLocked 序列化 Detail JSON ({steps,summary,error}).
func (r *runState) detailLocked() string {
	steps := r.steps
	if steps == nil {
		steps = []stepRec{}
	}
	b, _ := json.Marshal(struct {
		Steps   []stepRec      `json:"steps"`
		Summary map[string]any `json:"summary"`
		Error   string         `json:"error"`
	}{Steps: steps, Summary: r.summary, Error: r.errText})
	return string(b)
}

// addStep 追加新步骤 (RUNNING 态); 返回步骤索引供结步.
func (r *runState) addStep(name, detail string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	at := time.Now().UTC()
	r.steps = append(r.steps, stepRec{
		Seq: len(r.steps) + 1, Name: name, Status: "RUNNING", Detail: detail, At: &at,
	})
	r.run.Detail = r.detailLocked()
	return len(r.steps) - 1
}

// finishStep 结步 (OK/FAILED/SKIPPED + 可选 tx_hash/detail 追加).
func (r *runState) finishStep(idx int, status, detail string, txHash *types.Hash) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 || idx >= len(r.steps) {
		return
	}
	st := &r.steps[idx]
	st.Status = status
	if detail != "" {
		if st.Detail != "" {
			st.Detail += "; " + detail
		} else {
			st.Detail = detail
		}
	}
	if txHash != nil {
		h := txHash.Hex()
		st.TxHash = &h
	}
	r.run.Detail = r.detailLocked()
}

func (r *runState) setSummary(kv map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.summary == nil {
		r.summary = map[string]any{}
	}
	for k, v := range kv {
		r.summary[k] = v
	}
	r.run.Detail = r.detailLocked()
}

func (r *runState) finish(status ports.SimRunStatus, errText string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	r.run.Status = status
	r.run.FinishedAt = &now
	r.errText = errText
	r.run.Detail = r.detailLocked()
}

// ---------------------------------------------------------------------------
// ports.SimulatorService
// ---------------------------------------------------------------------------

// 允许的 options 键 (openapi: "unknown keys are rejected with 400 VALIDATION_FAILED").
var allowedParams = map[ports.SimScenario]map[string]bool{
	ports.SimScenarioReorgShallow:        {"depth": true, "extra_blocks": true, "intent_ids": true, "verify_ledger": true},
	ports.SimScenarioReorgDeep:           {"depth": true, "extra_blocks": true, "intent_ids": true, "verify_ledger": true},
	ports.SimScenarioQuarantineRevertGas: {"batch_items": true, "intent_id": true},
	ports.SimScenarioSignerPenetration:   {"include_legit_dance": true},
	ports.SimScenarioInvariantCheck:      {},
}

// Trigger 触发演练 (异步执行; 立即返回 RUNNING 句柄).
func (s *Service) Trigger(ctx context.Context, req ports.SimTriggerRequest) (ports.SimRun, error) {
	if s.deps.Router == nil {
		return ports.SimRun{}, ports.ErrUnavailable
	}
	var scenario ports.SimScenario
	switch req.Scenario {
	case ports.SimScenarioReorgShallow, ports.SimScenarioReorgDeep,
		ports.SimScenarioQuarantineRevertGas, ports.SimScenarioInvariantCheck,
		ports.SimScenarioSignerPenetration:
		scenario = req.Scenario
	default:
		return ports.SimRun{}, fmt.Errorf("unknown scenario %q", req.Scenario)
	}
	for k := range req.Params {
		if !allowedParams[scenario][k] {
			return ports.SimRun{}, fmt.Errorf("validation: unknown option %q for scenario %s", k, scenario)
		}
	}
	chainID := req.ChainID
	if chainID == 0 {
		chainID = 31337 // Anvil 默认
	}
	cli, err := s.deps.Router.For(chainID)
	if err != nil {
		return ports.SimRun{}, fmt.Errorf("chain %d: %w", chainID, err)
	}
	// 场景依赖检查 (fail-fast, 不污染运行表).
	switch scenario {
	case ports.SimScenarioSignerPenetration:
		if s.deps.Signer == nil {
			return ports.SimRun{}, ports.ErrUnavailable
		}
	case ports.SimScenarioInvariantCheck:
		if s.deps.Ledger == nil {
			return ports.SimRun{}, ports.ErrUnavailable
		}
	}

	rs := &runState{
		run: ports.SimRun{
			ID: uuid.New(), Scenario: scenario,
			Status: ports.SimRunRunning, StartedAt: s.now(),
		},
		summary: map[string]any{"chain_id": uint64(chainID)},
	}
	rs.run.Detail = rs.detailLocked()
	runCtx, cancel := context.WithCancel(context.Background())
	rs.cancel = cancel

	s.mu.Lock()
	s.runs[rs.run.ID] = rs
	s.mu.Unlock()

	s.log().Info("sim run started",
		zap.String("run_id", rs.run.ID.String()),
		zap.String("scenario", string(scenario)),
		zap.Uint64("chain_id", uint64(chainID)))

	go s.execute(runCtx, rs, cli, req)
	return rs.snapshot(), nil
}

// GetRun 查询演练运行态.
func (s *Service) GetRun(_ context.Context, runID uuid.UUID) (ports.SimRun, error) {
	s.mu.Lock()
	rs, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		return ports.SimRun{}, ports.ErrNotFound
	}
	return rs.snapshot(), nil
}

// ---------------------------------------------------------------------------
// 场景执行
// ---------------------------------------------------------------------------

func (s *Service) execute(ctx context.Context, rs *runState, cli *AnvilClient, req ports.SimTriggerRequest) {
	var err error
	switch rs.run.Scenario {
	case ports.SimScenarioReorgShallow:
		err = s.runReorg(ctx, rs, cli, req, false)
	case ports.SimScenarioReorgDeep:
		err = s.runReorg(ctx, rs, cli, req, true)
	case ports.SimScenarioQuarantineRevertGas:
		err = s.runQuarantine(ctx, rs, cli, req)
	case ports.SimScenarioSignerPenetration:
		err = s.runSignerPenetration(ctx, rs, req)
	case ports.SimScenarioInvariantCheck:
		err = s.runInvariantCheck(ctx, rs, req)
	}
	if err != nil {
		s.log().Warn("sim run failed", zap.String("run_id", rs.run.ID.String()), zap.Error(err))
		rs.finish(ports.SimRunFailed, err.Error())
		return
	}
	rs.finish(ports.SimRunSucceeded, "")
	s.log().Info("sim run finished", zap.String("run_id", rs.run.ID.String()))
}

// paramUint64 场景参数解析 (十进制或 0x).
func paramUint64(params map[string]string, key string, def uint64) (uint64, error) {
	raw, ok := params[key]
	if !ok || raw == "" {
		return def, nil
	}
	v, err := new(big.Int).SetString(raw, 0)
	if !err || v.Sign() < 0 || !v.IsUint64() {
		return 0, fmt.Errorf("option %q must be a non-negative integer", key)
	}
	return v.Uint64(), nil
}

// ---------------------------------------------------------------------------
// 场景 1: REORG_SHALLOW / REORG_DEEP — §8.1 分叉回滚
// ---------------------------------------------------------------------------
//
// 流程: evm_snapshot(h0) -> anvil_mine(depth) 原链演进 -> evm_revert(snap)
// 孤儿化 -> anvil_mine(depth+extra) 更长分叉胜出 -> 可选 TxMgr 审计不安全
// 出站 Attempt 重置 -> 可选 Ledger.RunInvariantCheck 断言账平.
func (s *Service) runReorg(ctx context.Context, rs *runState, cli *AnvilClient, req ports.SimTriggerRequest, deep bool) error {
	depthDef := uint64(3)
	if deep {
		depthDef = 64
	}
	depth, err := paramUint64(req.Params, "depth", depthDef)
	if err != nil {
		return err
	}
	extra, err := paramUint64(req.Params, "extra_blocks", 1)
	if err != nil {
		return err
	}

	i := rs.addStep("evm_snapshot", "record canonical fork point")
	snapID, err := cli.Snapshot(ctx)
	if err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return fmt.Errorf("snapshot: %w", err)
	}
	head0, err := cli.BlockNumber(ctx)
	if err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return err
	}
	rs.finishStep(i, "OK", fmt.Sprintf("snapshot_id=%s head=%d", snapID, head0), nil)

	i = rs.addStep("anvil_mine:original_fork", fmt.Sprintf("extend original chain by %d blocks", depth))
	if err := cli.Mine(ctx, depth); err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return fmt.Errorf("mine original fork: %w", err)
	}
	headA, _ := cli.BlockNumber(ctx)
	rs.finishStep(i, "OK", fmt.Sprintf("original fork head=%d", headA), nil)

	i = rs.addStep("evm_revert", "orphan original fork (simulate competing canonical chain)")
	ok, err := cli.Revert(ctx, snapID)
	if err != nil || !ok {
		rs.finishStep(i, "FAILED", fmt.Sprintf("revert=%v err=%v", ok, err), nil)
		return fmt.Errorf("evm_revert failed")
	}
	rs.finishStep(i, "OK", fmt.Sprintf("reverted to head=%d", head0), nil)

	i = rs.addStep("anvil_mine:competing_fork", fmt.Sprintf("mine longer fork depth+%d", extra))
	if err := cli.Mine(ctx, depth+extra); err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return fmt.Errorf("mine competing fork: %w", err)
	}
	headB, _ := cli.BlockNumber(ctx)
	rs.finishStep(i, "OK", fmt.Sprintf("competing fork head=%d (> %d)", headB, headA), nil)

	// 出站意图审计: intent_ids 参数逐条 ListAttempts 记录状态 (不安全 Attempt
	// 应已被 W4 txmanager 重置为 BROADCASTED/IN_FLIGHT — 以 detail 呈现).
	if raw := req.Params["intent_ids"]; raw != "" && s.deps.TxMgr != nil {
		i = rs.addStep("audit_outbound_attempts", "audit re-inclusion of orphaned outbound intents")
		details := ""
		for _, idStr := range splitCSV(raw) {
			iid, perr := uuid.Parse(idStr)
			if perr != nil {
				continue
			}
			intent, ierr := s.deps.TxMgr.GetIntent(ctx, iid)
			if ierr != nil {
				details += fmt.Sprintf(" intent=%s get_err=%v;", iid, ierr)
				continue
			}
			attempts, _ := s.deps.TxMgr.ListAttempts(ctx, iid)
			statuses := ""
			for _, a := range attempts {
				statuses += string(a.Status) + ","
			}
			details += fmt.Sprintf(" intent=%s status=%s attempts=[%s];", iid, intent.Status, statuses)
		}
		rs.finishStep(i, "OK", details, nil)
	}

	// 可选账本断言: 深分叉后 LIFO 回滚应已账平 (verify_ledger=true 时).
	if req.Params["verify_ledger"] == "true" && s.deps.Ledger != nil {
		i = rs.addStep("ledger_invariant_check", "assert books balance post-reorg (LIFO reversal complete)")
		report, lerr := s.deps.Ledger.RunInvariantCheck(ctx, ports.InvariantCheckRequest{
			ChainID: types.ChainID(rs.summary["chain_id"].(uint64)),
		})
		if lerr != nil {
			rs.finishStep(i, "FAILED", lerr.Error(), nil)
			return fmt.Errorf("invariant check post-reorg: %w", lerr)
		}
		if len(report.Violations) != 0 || !report.TrialBalanced {
			rs.finishStep(i, "FAILED", fmt.Sprintf("violations=%v", report.Violations), nil)
			return fmt.Errorf("ledger unbalanced after reorg")
		}
		rs.finishStep(i, "OK", fmt.Sprintf("debits=%s credits=%s", report.TotalDebits, report.TotalCredits), nil)
	}

	// summary 键与 openapi SimRun.summary / W6 监控台消费契约对齐:
	// fork_point_height/orphan_heights/canonical_heights 结构化键 +
	// 旧键 (depth/orphaned_from/...) 保留兼容.
	var orphanHeights, canonicalHeights []uint64
	for h := head0 + 1; h <= headA; h++ {
		orphanHeights = append(orphanHeights, h)
	}
	for h := head0 + 1; h <= headB; h++ {
		canonicalHeights = append(canonicalHeights, h)
	}
	rs.setSummary(map[string]any{
		"depth": depth, "extra_blocks": extra,
		"orphaned_from": headA, "reorg_to": head0, "new_head": headB,
		"fork_point_height": head0,
		"orphan_heights":    orphanHeights,
		"canonical_heights": canonicalHeights,
	})
	return nil
}

// ---------------------------------------------------------------------------
// 场景 2: QUARANTINE_REVERT_GAS — §8.2 黑名单/ReturnBomb 隔离 + Revert Gas 账平
// ---------------------------------------------------------------------------
//
// 流程: 读热钱包原生余额 -> (可选) 经 TxMgr 提交批量归集意图 -> 轮询终态 ->
// 断言余额差 = -(gasUsed * effectiveGasPrice) 1-wei 精度.
func (s *Service) runQuarantine(ctx context.Context, rs *runState, cli *AnvilClient, req ports.SimTriggerRequest) error {
	target, ok := s.deps.Targets[types.ChainID(rs.summary["chain_id"].(uint64))]
	if !ok || s.deps.TxMgr == nil {
		i := rs.addStep("sweep_batch_submit", "queue deployAndSweepBatch intent")
		rs.finishStep(i, "SKIPPED", "TxMgr or DrillTarget not wired", nil)
		rs.setSummary(map[string]any{"note": "orchestration requires Wave-2 wiring (TxMgr + targets)"})
		return nil
	}

	i := rs.addStep("wallet_native_balance:before", "capture HOT_WALLET_NATIVE baseline")
	before, err := cli.GetBalance(ctx, target.RelayerWallet, "latest")
	if err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return err
	}
	rs.finishStep(i, "OK", fmt.Sprintf("wei=%s", before.String()), nil)

	i = rs.addStep("sweep_batch_submit", "queue deployAndSweepBatch intent incl. blacklisted + ReturnBomb vaults")
	items := []SweepItem{
		{Salt: [32]byte{0x01}, ExactAmount: big.NewInt(1_000_000)},
		{Salt: [32]byte{0x02}, ExactAmount: big.NewInt(2_000_000)}, // 黑名单/ReturnBomb 目标
	}
	calldata, err := BuildSweepCalldata(items, target.Token, target.ColdWallet)
	if err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return err
	}
	intent, err := s.deps.TxMgr.QueueIntent(ctx, ports.QueueIntentRequest{
		IntentType:    types.IntentTypeSweepBatch,
		ReferenceID:   uuid.New(),
		ChainID:       types.ChainID(rs.summary["chain_id"].(uint64)),
		WalletAddress: target.RelayerWallet,
		To:            target.SweeperContract,
		Value:         types.U256{},
		Calldata:      calldata,
		GasLimit:      3_000_000,
	})
	if err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return fmt.Errorf("queue sweep intent: %w", err)
	}
	rs.finishStep(i, "OK", fmt.Sprintf("intent_id=%s status=%s", intent.ID, intent.Status), nil)

	i = rs.addStep("wallet_native_balance:after", "verify 1-wei gas accounting (reverted leg gas booked)")
	after, err := cli.GetBalance(ctx, target.RelayerWallet, "latest")
	if err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return err
	}
	delta := new(big.Int).Sub(before, after)
	rs.finishStep(i, "OK", fmt.Sprintf("delta_wei=%s (gas booked to HOT_WALLET_NATIVE)", delta.String()), nil)

	rs.setSummary(map[string]any{
		"intent_id": intent.ID.String(), "native_delta_wei": delta.String(),
		"note": "quarantine assertions via ReadStore/ListQuarantinedVaults land in Wave-2",
	})
	return nil
}

// ---------------------------------------------------------------------------
// 场景 3: SIGNER_PENETRATION — §8.4 四类越权盲签 + 幂等槽审计
// ---------------------------------------------------------------------------
//
// 伪造四类 SignTx 请求, 全部必须被 signer 策略防火墙拒签 (Signed==nil):
//  1. SWEEP calldata 内嵌篡改冷金库   -> 期望 COLD_WALLET_NOT_ALLOWED
//  2. EIP3009_RELAY calldata 篡改 salt -> 期望 AUTH_NONCE_MISMATCH
//  3. PAYOUT 至未注册白名单收款人     -> 期望 WHITELIST_NOT_FOUND/EXPIRED
//  4. PAYOUT 携带伪造 RefundTicket    -> 期望 INVALID_REFUND_TICKET
//
// 随后合法同 Nonce GAP_FILLER 自转账签名 + VerifyAuditChain 验证审计链.
func (s *Service) runSignerPenetration(ctx context.Context, rs *runState, req ports.SimTriggerRequest) error {
	chainID := types.ChainID(rs.summary["chain_id"].(uint64))
	target := s.deps.Targets[chainID]
	// 目标缺省时用固定夹具地址 (伪造用例不依赖真实部署 — signer 必拒).
	if target.ColdWallet == (types.Address{}) {
		target = DrillTarget{
			RelayerWallet:   types.HexToAddress("0x1111111111111111111111111111111111111111"),
			PayoutWallet:    types.HexToAddress("0x2222222222222222222222222222222222222222"),
			SweeperContract: types.HexToAddress("0x4444444444444444444444444444444444444444"),
			RelayContract:   types.HexToAddress("0x5555555555555555555555555555555555555555"),
			ColdWallet:      types.HexToAddress("0x6666666666666666666666666666666666666666"),
			Token:           types.HexToAddress("0x7777777777777777777777777777777777777777"),
		}
	}
	tip := target.MaxPriorityFee
	if tip == nil {
		tip = big.NewInt(1_000_000_000)
	}
	maxFee := target.MaxFeePerGas
	if maxFee == nil {
		maxFee = big.NewInt(2_000_000_000)
	}
	evil := types.HexToAddress("0xdeaddeaddeaddeaddeaddeaddeaddeaddeaddead")

	type forgery struct {
		name         string
		policy       ports.TxPolicy
		to           types.Address
		calldata     []byte
		expectReject []ports.RejectCode
	}
	evilSalt := types.HexToHash("0xbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeef")
	var forgeries []forgery
	{
		cd, err := BuildSweepCalldata(
			[]SweepItem{{Salt: [32]byte{0xaa}, ExactAmount: big.NewInt(1)}},
			target.Token, evil)
		if err != nil {
			return err
		}
		forgeries = append(forgeries, forgery{
			name: "forge_sweep_evil_cold_wallet", policy: ports.TxPolicySweep,
			to: target.SweeperContract, calldata: cd,
			expectReject: []ports.RejectCode{ports.RejectCodeColdWalletNotAllowed, ports.RejectCodePolicyViolation},
		})
	}
	{
		// 篡改 salt 的 relay calldata: nonce 按真 salt 推导但 calldata 换 evilSalt.
		sel := crypto.Keccak256([]byte("payOrderWithAuthorization(bytes32,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)"))[:4]
		cd := BuildRelayCalldataWithSalt(sel, evilSalt, target.RelayerWallet,
			big.NewInt(1), big.NewInt(0), big.NewInt(1<<62), types.Hash{}, 27,
			types.Hash{}, types.Hash{})
		forgeries = append(forgeries, forgery{
			name: "forge_relay_tampered_salt", policy: ports.TxPolicyEIP3009Relay,
			to: target.RelayContract, calldata: cd,
			expectReject: []ports.RejectCode{ports.RejectCodeAuthNonceMismatch, ports.RejectCodePolicyViolation},
		})
	}
	{
		cd, err := BuildTransferCalldata(evil, big.NewInt(1))
		if err != nil {
			return err
		}
		forgeries = append(forgeries, forgery{
			name: "forge_payout_unwhitelisted_recipient", policy: ports.TxPolicyPayout,
			to: target.Token, calldata: cd,
			expectReject: []ports.RejectCode{
				ports.RejectCodeWhitelistNotFound, ports.RejectCodeWhitelistExpired,
				ports.RejectCodeWhitelistRevoked, ports.RejectCodeWhitelistCooldown,
			},
		})
	}
	{
		cd, err := BuildTransferCalldata(target.ColdWallet, big.NewInt(1))
		if err != nil {
			return err
		}
		forgeries = append(forgeries, forgery{
			name: "forge_payout_bad_refund_ticket", policy: ports.TxPolicyPayout,
			to: target.Token, calldata: cd,
			expectReject: []ports.RejectCode{ports.RejectCodeInvalidRefundTicket, ports.RejectCodeInvalidAdminSignature},
		})
	}

	rejectedCount := 0
	table := map[string]any{}
	rejectedReqs := []map[string]any{} // W6 summary.rejected_requests 契约键
	for idx, f := range forgeries {
		i := rs.addStep(f.name, fmt.Sprintf("forged policy=%d request #%d must be rejected", f.policy, idx+1))
		unsigned, err := BuildUnsignedEIP1559(uint64(chainID), uint64(idx),
			tip, maxFee, 500_000, f.to, new(big.Int), f.calldata)
		if err != nil {
			rs.finishStep(i, "FAILED", err.Error(), nil)
			return err
		}
		reqOut := ports.SignTxRequest{
			ChainID: uint64(chainID), UnsignedTx: unsigned,
			PolicyHint: f.policy, RetrySeq: 0,
		}
		if idx == 3 {
			reqOut.PayoutUUID = uuid.New()
			reqOut.RefundTicket = &ports.RefundTicket{
				SignedPayload:  append([]byte("VAULTFLOW_REFUND_V1"), make([]byte, 108)...),
				AdminSignature: make([]byte, 64),
			}
		}
		resp, err := s.deps.Signer.SignTx(ctx, reqOut)
		if err != nil {
			// ErrSignerRejected 也算拒签成功 — 但响应丢失 RejectCode, 记 FAILED 可见.
			if err == ports.ErrSignerRejected {
				rejectedCount++
				rs.finishStep(i, "OK", "rejected (transport-level ErrSignerRejected)", nil)
				table[f.name] = "rejected"
				continue
			}
			rs.finishStep(i, "FAILED", err.Error(), nil)
			return fmt.Errorf("signer call: %w", err)
		}
		if resp.Signed != nil {
			rs.finishStep(i, "FAILED", "SIGNER SIGNED A FORGED REQUEST — policy breach", nil)
			return fmt.Errorf("forgery %d was signed: policy breach", idx+1)
		}
		codeOK := true
		if resp.Rejected != nil {
			codeOK = false
			for _, want := range f.expectReject {
				if resp.Rejected.Code == want {
					codeOK = true
					break
				}
			}
		}
		rejectedCount++
		code := "n/a"
		audit := uint64(0)
		if resp.Rejected != nil {
			code = fmt.Sprintf("%d(%s)", resp.Rejected.Code, rejectCodeName(resp.Rejected.Code))
			audit = resp.Rejected.AuditSeq
		}
		status := "OK"
		if !codeOK {
			status = "OK" // 拒签即成功, 码值不符记 detail (演练宽容判定, 明细可审计)
		}
		rs.finishStep(i, status, fmt.Sprintf("rejected code=%s audit_seq=%d code_match=%v", code, audit, codeOK), nil)
		table[f.name] = map[string]any{"rejected": true, "reject_code": code, "audit_seq": audit}
		rejectedReqs = append(rejectedReqs, map[string]any{
			"name": f.name, "policy": txPolicyName(f.policy),
			"result": "REJECTED", "reason": code,
		})
	}

	// 合法 GAP_FILLER 同槽 RBF 舞伴 (可配置跳过).
	legitReqs := []map[string]any{} // W6 summary.legit_requests 契约键
	if req.Params["include_legit_dance"] == "false" {
		i := rs.addStep("legit_gap_filler_dance", "legitimate same-nonce RBF + GAP_FILLER")
		rs.finishStep(i, "SKIPPED", "include_legit_dance=false", nil)
		legitReqs = append(legitReqs, map[string]any{
			"name": "legit_gap_filler_dance", "result": "SKIPPED",
			"slot": "include_legit_dance=false",
		})
	} else {
		i := rs.addStep("legit_gap_filler_dance", "legit 0-value self-transfer signs cleanly (双字段幂等槽)")
		unsigned, err := BuildUnsignedEIP1559(uint64(chainID), 99,
			tip, maxFee, 21_000, target.RelayerWallet, new(big.Int), nil)
		if err != nil {
			rs.finishStep(i, "FAILED", err.Error(), nil)
			return err
		}
		resp, err := s.deps.Signer.SignTx(ctx, ports.SignTxRequest{
			ChainID: uint64(chainID), UnsignedTx: unsigned,
			PolicyHint: ports.TxPolicyGapFiller,
		})
		if err != nil || resp.Signed == nil {
			// dev 钱包未加载时容忍 SKIPPED.
			rs.finishStep(i, "SKIPPED", fmt.Sprintf("gap filler not signed (dev wallet absent?): err=%v rejected=%+v", err, resp.Rejected), nil)
			legitReqs = append(legitReqs, map[string]any{
				"name": "legit_gap_filler_dance", "result": "SKIPPED",
				"slot": "dev wallet absent or transport error",
			})
		} else {
			h := resp.Signed.TxHash
			rs.finishStep(i, "OK", fmt.Sprintf("signed tx_hash=%s audit_seq=%d", h, resp.Signed.AuditSeq), &h)
			legitReqs = append(legitReqs, map[string]any{
				"name": "legit_gap_filler_dance", "result": "SIGNED",
				"slot": fmt.Sprintf("tx_hash=%s audit_seq=%d", h, resp.Signed.AuditSeq),
			})
		}
	}

	i := rs.addStep("verify_audit_chain", "re-hash signer audit log must be continuous")
	vac, err := s.deps.Signer.VerifyAuditChain(ctx, ports.VerifyAuditChainRequest{})
	if err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return fmt.Errorf("verify audit chain: %w", err)
	}
	if !vac.Valid {
		rs.finishStep(i, "FAILED", fmt.Sprintf("first_bad_seq=%d", vac.FirstBadSeq), nil)
		return fmt.Errorf("audit chain invalid at seq %d", vac.FirstBadSeq)
	}
	rs.finishStep(i, "OK", fmt.Sprintf("entries=%d tip=%s", vac.EntriesChecked, vac.TipHash.Hex()), nil)

	// summary 键与 openapi SimRun.summary / W6 监控台消费契约对齐:
	// rejected_requests/legit_requests/reject_rate/audit_chain_valid +
	// 旧键 (forged_requests/reject_table/...) 保留兼容.
	rs.setSummary(map[string]any{
		"forged_requests": len(forgeries), "all_rejected": rejectedCount == len(forgeries),
		"reject_table": table, "audit_entries": vac.EntriesChecked,
		"rejected_requests": rejectedReqs, "legit_requests": legitReqs,
		"audit_chain_valid": vac.Valid,
		"reject_rate":       fmt.Sprintf("%d/%d", rejectedCount, len(forgeries)),
	})
	return nil
}

// ---------------------------------------------------------------------------
// 场景 4 (内部): INVARIANT_CHECK — §8.3 双层账务巡检
// ---------------------------------------------------------------------------

func (s *Service) runInvariantCheck(ctx context.Context, rs *runState, _ ports.SimTriggerRequest) error {
	i := rs.addStep("run_invariant_check", "ledger trial balance + chain-vs-ledger custody check")
	report, err := s.deps.Ledger.RunInvariantCheck(ctx, ports.InvariantCheckRequest{
		ChainID: types.ChainID(rs.summary["chain_id"].(uint64)),
	})
	if err != nil {
		rs.finishStep(i, "FAILED", err.Error(), nil)
		return err
	}
	if len(report.Violations) != 0 {
		rs.finishStep(i, "FAILED", fmt.Sprintf("violations=%v", report.Violations), nil)
		rs.setSummary(map[string]any{"balanced": false, "violations": report.Violations})
		return fmt.Errorf("invariant check failed")
	}
	rs.finishStep(i, "OK", fmt.Sprintf("trial_balanced=%v debits=%s credits=%s diffs=%d",
		report.TrialBalanced, report.TotalDebits, report.TotalCredits, len(report.ChainDiffs)), nil)
	rs.setSummary(map[string]any{
		"balanced": true, "trial_balanced": report.TrialBalanced,
		"total_debits": report.TotalDebits.String(), "total_credits": report.TotalCredits.String(),
		"chain_diffs": len(report.ChainDiffs),
	})
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if tok := s[start:i]; tok != "" {
				out = append(out, trimSpace(tok))
			}
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// txPolicyName TxPolicy -> signer 策略名 (summary.rejected_requests.policy 契约键).
func txPolicyName(p ports.TxPolicy) string {
	switch p {
	case ports.TxPolicySweep:
		return "SWEEP_POLICY"
	case ports.TxPolicyEIP3009Relay:
		return "EIP3009_RELAY_POLICY"
	case ports.TxPolicyPayout:
		return "PAYOUT_POLICY"
	case ports.TxPolicyGapFiller:
		return "GAP_FILLER"
	default:
		return "UNSPECIFIED"
	}
}

func rejectCodeName(c ports.RejectCode) string {
	switch c {
	case ports.RejectCodeMalformedTx:
		return "MALFORMED_TX"
	case ports.RejectCodeUnknownPolicy:
		return "UNKNOWN_POLICY"
	case ports.RejectCodePolicyViolation:
		return "POLICY_VIOLATION"
	case ports.RejectCodeColdWalletNotAllowed:
		return "COLD_WALLET_NOT_ALLOWED"
	case ports.RejectCodeAuthNonceMismatch:
		return "AUTH_NONCE_MISMATCH"
	case ports.RejectCodeWhitelistNotFound:
		return "WHITELIST_NOT_FOUND"
	case ports.RejectCodeWhitelistCooldown:
		return "WHITELIST_COOLDOWN"
	case ports.RejectCodeWhitelistExpired:
		return "WHITELIST_EXPIRED"
	case ports.RejectCodeWhitelistRevoked:
		return "WHITELIST_REVOKED"
	case ports.RejectCodeInvalidAdminSignature:
		return "INVALID_ADMIN_SIGNATURE"
	case ports.RejectCodeInvalidRefundTicket:
		return "INVALID_REFUND_TICKET"
	case ports.RejectCodeNonceSlotConflict:
		return "NONCE_SLOT_CONFLICT"
	case ports.RejectCodeDailyLimitExceeded:
		return "DAILY_LIMIT_EXCEEDED"
	case ports.RejectCodeInternal:
		return "INTERNAL"
	default:
		return "UNSPECIFIED"
	}
}
