package txmanager

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// rebuildTxParams — 出站载荷的确定性重建.
//
// 002_intent_payload.sql 起 outbound_tx_intents 持久化 payload_to/payload_data/
// payload_value/payload_gas/payload_max_fee/payload_max_tip — QUEUED 首播 (重启
// 后) 与重发路径 (RBF 提价 / DROPPED 补发 / §4.3 回滚重广播) 均可完全重建
// EIP-1559 载荷; 确定性签名下重放同参数字节复现同 tx_hash / 命中 Signer 同
// Nonce 幂等槽. 优先级:
//   ① pendingParams 内存 fast-path (broadcastIntent 侧);
//   ② payload_* 持久化列 (本函数, 全类型 — 含 EIP3009_RELAY);
//   ③ 迁移前旧行的业务表兼容重建:
//       SWEEP_BATCH       -> deployAndSweepBatch(sweep_batch_items ⋈ deposit_addresses.salt)
//       MERCHANT_PAYOUT   -> ERC20 transfer(merchant_payouts.to_address, amount)
//       EIP3009_RELAY     -> ErrRebuildUnsupported (仅可能出现在 002 之前的旧行)
//   NONCE_GAP_FILLER / attempt_kind=GAP_FILLER -> 0 ETH 自转账 (不读载荷).
// ---------------------------------------------------------------------------

// ErrRebuildUnsupported 载荷无法由持久化列或业务表确定性重建.
var ErrRebuildUnsupported = errors.New("txmanager: tx payload not rebuildable from schema")

// rebuildTxParams 重建某意图指定候选类型 (PRIMARY_INTENT / GAP_FILLER) 的
// 签名参数; 费率由调用方另行决定 (沿用/提价) — payload 列为首播费率快照.
func (m *Manager) rebuildTxParams(ctx context.Context, it types.OutboundTxIntent,
	kind types.AttemptKind) (txParams, error) {

	if kind == types.AttemptKindGapFiller || it.IntentType == types.IntentTypeNonceGapFiller {
		return fillerParams(it.WalletAddress, big.NewInt(0), big.NewInt(0)), nil
	}

	// ② 持久化载荷 (B1) — EIP3009_RELAY 唯一可用路径, 其余类型优先采用.
	if p, ok, err := m.payloadParams(ctx, it); err != nil {
		return txParams{}, err
	} else if ok {
		if it.IntentType == types.IntentTypeMerchantPayout {
			if err := m.enrichPayoutParams(ctx, it.ReferenceID, &p); err != nil {
				return txParams{}, err
			}
		}
		return p, nil
	}

	// ③ 兼容路径: 无持久化载荷的迁移前旧行.
	switch it.IntentType {
	case types.IntentTypeMerchantPayout:
		return m.rebuildPayout(ctx, it)
	case types.IntentTypeSweepBatch:
		return m.rebuildSweep(ctx, it)
	case types.IntentTypeEIP3009Relay:
		return txParams{}, fmt.Errorf("%w: EIP3009_RELAY payload not persisted", ErrRebuildUnsupported)
	}
	return txParams{}, fmt.Errorf("%w: intent_type=%s", ErrRebuildUnsupported, it.IntentType)
}

// payloadParams 从 outbound_tx_intents.payload_* 列重建基础签名参数
// (to/value/calldata/gas/首播费率 + 类型默认 policy). 返回 ok=false 表示
// 该行无持久化载荷 (迁移前旧行), 调用方走业务表兼容重建.
// MERCHANT_PAYOUT 的幂等槽字段 (payoutUUID/retrySeq/refundTicket) 由
// enrichPayoutParams 另行补齐.
func (m *Manager) payloadParams(ctx context.Context, it types.OutboundTxIntent) (txParams, bool, error) {
	var to *string
	var data []byte
	var value, fee, tip *string
	var gas *int64
	if err := m.db.QueryRow(ctx,
		`SELECT payload_to, payload_data, payload_value::text, payload_gas,
		        payload_max_fee::text, payload_max_tip::text
		   FROM outbound_tx_intents WHERE id=$1`, it.ID).
		Scan(&to, &data, &value, &gas, &fee, &tip); err != nil {
		return txParams{}, false, fmt.Errorf("load payload for intent %s: %w", it.ID, err)
	}
	if to == nil {
		return txParams{}, false, nil
	}
	p := txParams{
		to:         types.HexToAddress(*to),
		value:      new(big.Int),
		calldata:   data,
		policy:     policyFor(it.IntentType),
		payoutUUID: uuid.Nil,
	}
	if value != nil {
		p.value = types.MustParseU256(*value).Big()
	}
	if gas != nil && *gas > 0 {
		p.gasLimit = uint64(*gas)
	} else {
		p.gasLimit = m.defaultGasForRebuild(ctx, it)
	}
	// 首播费率; 缺失 (旧行/异常) 时回退最近一次 attempt 费率 — 重发路径
	// bumpParams 会再覆盖, QUEUED 首播路径则必须非空.
	if fee != nil && tip != nil {
		p.maxFee = types.MustParseU256(*fee).Big()
		p.maxTip = types.MustParseU256(*tip).Big()
	} else if f, t, err := m.topFees(ctx, it.ID); err == nil {
		p.maxFee, p.maxTip = f, t
	} else {
		return txParams{}, false,
			fmt.Errorf("%w: intent %s missing persisted fee fields", ErrRebuildUnsupported, it.ID)
	}
	return p, true, nil
}

// defaultGasForRebuild 载荷缺 gas 时按类型默认值补 (SWEEP_BATCH 以实际条目数计).
func (m *Manager) defaultGasForRebuild(ctx context.Context, it types.OutboundTxIntent) uint64 {
	n := 1
	if it.IntentType == types.IntentTypeSweepBatch {
		var c int
		if err := m.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM sweep_batch_items WHERE batch_id=$1`,
			it.ReferenceID).Scan(&c); err == nil && c > 0 {
			n = c
		}
	}
	return m.defaultGas(it.IntentType, n)
}

// enrichPayoutParams MERCHANT_PAYOUT 幂等槽字段补齐: payoutUUID=reference_id,
// retry_seq/admin_refund_ticket 取 merchant_payouts 当前值 (与 rebuildPayout
// 同口径 — 同 Nonce 槽绑定的业务授权字段必须一致).
func (m *Manager) enrichPayoutParams(ctx context.Context, payoutID uuid.UUID, p *txParams) error {
	var retrySeq int32
	var kind string
	var ticket []byte
	if err := m.db.QueryRow(ctx,
		`SELECT retry_seq, payout_kind, COALESCE(admin_refund_ticket, ''::bytea)
		   FROM merchant_payouts WHERE id=$1`, payoutID).
		Scan(&retrySeq, &kind, &ticket); err != nil {
		return fmt.Errorf("enrich payout %s: %w", payoutID, err)
	}
	p.policy = ports.TxPolicyPayout
	p.payoutUUID = payoutID
	p.retrySeq = uint32(retrySeq)
	if types.PayoutKind(kind) == types.PayoutKindCustomerRefund && len(ticket) >= 127 {
		t := &ports.RefundTicket{SignedPayload: ticket[:127]}
		if len(ticket) >= 191 {
			t.AdminSignature = ticket[127:191]
		}
		p.refundTicket = t
	}
	return nil
}

// rebuildPayout MERCHANT_PAYOUT: to=白名单稳定币合约, data=transfer(to,amount),
// gas=GasLimitPayout, policy=PAYOUT, 幂等槽绑定 (payout_uuid, retry_seq),
// CUSTOMER_REFUND 透传 RefundTicket.
func (m *Manager) rebuildPayout(ctx context.Context, it types.OutboundTxIntent) (txParams, error) {
	var p types.MerchantPayout
	var amount, toAddr string
	err := m.db.QueryRow(ctx,
		`SELECT id, merchant_id, payout_kind, source_account_type, source_order_id,
		        chain_id, asset_symbol, to_address, amount::text, retry_seq,
		        COALESCE(admin_refund_ticket, ''::bytea), status
		   FROM merchant_payouts WHERE id=$1`, it.ReferenceID).
		Scan(&p.ID, &p.MerchantID, &p.PayoutKind, &p.SourceAccount, &p.SourceOrderID,
			&p.ChainID, &p.AssetSymbol, &toAddr, &amount, &p.RetrySeq,
			&p.AdminRefundTicket, &p.Status)
	if err != nil {
		return txParams{}, fmt.Errorf("load payout %s: %w", it.ReferenceID, err)
	}

	var tokenContract string
	if err := m.db.QueryRow(ctx,
		`SELECT token_contract FROM supported_assets
		  WHERE chain_id=$1 AND asset_symbol=$2`,
		int64(p.ChainID), p.AssetSymbol).Scan(&tokenContract); err != nil {
		return txParams{}, fmt.Errorf("resolve token contract %s@%d: %w", p.AssetSymbol, p.ChainID, err)
	}

	p.ToAddress = types.HexToAddress(toAddr)
	amt := types.MustParseU256(amount).Big()
	to := types.HexToAddress(toAddr)
	data, err := chain.EncodeERC20Transfer(to, amt)
	if err != nil {
		return txParams{}, err
	}
	out := txParams{
		to:         types.HexToAddress(tokenContract),
		value:      new(big.Int),
		calldata:   data,
		gasLimit:   m.cfg.GasLimitPayout,
		policy:     ports.TxPolicyPayout,
		payoutUUID: p.ID,
		retrySeq:   uint32(p.RetrySeq),
	}
	// CUSTOMER_REFUND 透传票据: admin_refund_ticket = payload(127B) || sig(64B).
	if p.PayoutKind == types.PayoutKindCustomerRefund && len(p.AdminRefundTicket) >= 127 {
		ticket := &ports.RefundTicket{SignedPayload: p.AdminRefundTicket[:127]}
		if len(p.AdminRefundTicket) >= 191 {
			ticket.AdminSignature = p.AdminRefundTicket[127:191]
		}
		out.refundTicket = ticket
	}
	return out, nil
}

// rebuildSweep SWEEP_BATCH: to=BatchSweeper 合约 (由 items 所在批次目标决定? ——
// 合约地址本身不随业务行变化, 需要 chain 级配置; 此处从意图原请求无从恢复时
// 依赖 chainConfig 注入; 见 Manager.chainContracts).
func (m *Manager) rebuildSweep(ctx context.Context, it types.OutboundTxIntent) (txParams, error) {
	var batch types.SweepBatch
	var total, cold string
	err := m.db.QueryRow(ctx,
		`SELECT id, chain_id, asset_symbol, target_cold_wallet,
		        total_swept_amount::text, status
		   FROM sweep_batches WHERE id=$1`, it.ReferenceID).
		Scan(&batch.ID, &batch.ChainID, &batch.AssetSymbol, &cold,
			&total, &batch.Status)
	if err != nil {
		return txParams{}, fmt.Errorf("load sweep batch %s: %w", it.ReferenceID, err)
	}
	var tokenContract string
	if err := m.db.QueryRow(ctx,
		`SELECT token_contract FROM supported_assets
		  WHERE chain_id=$1 AND asset_symbol=$2`,
		int64(batch.ChainID), batch.AssetSymbol).Scan(&tokenContract); err != nil {
		return txParams{}, fmt.Errorf("resolve token %s@%d: %w", batch.AssetSymbol, batch.ChainID, err)
	}
	rows, err := m.db.Query(ctx,
		`SELECT da.create2_salt, i.exact_sweep_amount::text
		   FROM sweep_batch_items i
		   JOIN deposit_addresses da ON da.id = i.deposit_address_id
		  WHERE i.batch_id=$1 ORDER BY i.id`, batch.ID)
	if err != nil {
		return txParams{}, err
	}
	defer rows.Close()
	var items []chain.SweepItem
	for rows.Next() {
		var salt []byte
		var amt string
		if err := rows.Scan(&salt, &amt); err != nil {
			return txParams{}, err
		}
		var s types.Hash
		copy(s[:], salt)
		items = append(items, chain.SweepItem{Salt: s, ExactAmount: types.MustParseU256(amt).Big()})
	}
	if err := rows.Err(); err != nil {
		return txParams{}, err
	}
	if len(items) == 0 {
		return txParams{}, fmt.Errorf("sweep batch %s has no items", batch.ID)
	}

	batch.TargetColdWallet = types.HexToAddress(cold)
	data, err := chain.EncodeDeployAndSweepBatch(items,
		types.HexToAddress(tokenContract), batch.TargetColdWallet)
	if err != nil {
		return txParams{}, err
	}
	return txParams{
		to:         m.batchSweeperAddr(it.ChainID),
		value:      new(big.Int),
		calldata:   data,
		gasLimit:   m.cfg.GasLimitSweepBase + uint64(len(items))*m.cfg.GasLimitSweepItem,
		policy:     ports.TxPolicySweep,
		payoutUUID: uuid.Nil,
	}, nil
}

// batchSweeperAddr 每链 BatchSweeper 合约地址 (链级部署配置; 装配时注入).
// 未注入时返回零地址 — 生产装配必须提供, 否则 Signer 策略校验必拒签.
func (m *Manager) batchSweeperAddr(chainID types.ChainID) types.Address {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chainContracts == nil {
		return types.Address{}
	}
	if c, ok := m.chainContracts[chainID]; ok {
		return c.BatchSweeper
	}
	return types.Address{}
}

// ChainContracts 每链合约部署配置 (rebuild 路径所需).
type ChainContracts struct {
	BatchSweeper  types.Address
	EIP3009Router types.Address
	Factory       types.Address
}

// SetChainContracts 注入链级合约配置 (装配窗口 I1 调用; 测试同样经此注入).
func (m *Manager) SetChainContracts(m2 map[types.ChainID]ChainContracts) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chainContracts = m2
}
