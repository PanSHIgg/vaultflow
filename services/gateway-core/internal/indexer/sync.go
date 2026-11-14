package indexer

import (
	"context"
	"fmt"
	"time"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 共享查询与规范化助手 (detect/finalize/rollback 复用)
// ---------------------------------------------------------------------------

// hashStr 哈希规范化: 0x 前缀小写 hex (§2.5.3).
func hashStr(h types.Hash) string {
	s := h.Hex()
	out := make([]byte, 0, len(s))
	out = append(out, s[:2]...)
	for i := 2; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// addrStr 地址规范化 (与 txmanager.addrStr 同规则).
func addrStr(a types.Address) string {
	s := a.Hex()
	out := make([]byte, 0, len(s))
	out = append(out, s[:2]...)
	for i := 2; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// orderInfo deposit_address → 订单关联快照 (分类与结算用).
type orderInfo struct {
	orderID        uuid.UUID
	depositAddrID  uuid.UUID
	merchantID     uuid.UUID
	assetSymbol    string
	tokenContract  types.Address // 订单应付币种合约 (supported_assets 反查)
	expectedAmount types.U256
	receivedAmount types.U256
	status         types.OrderStatus
	expiresAt      time.Time
	feeRateBps     int64
	quarantined    bool
}

// chainView 单 tick 的链配置快照: 白名单代币、watch 集合、vault→订单映射.
type chainView struct {
	tokenSymbols map[types.Address]string     // token_contract -> asset_symbol
	tokenList    []types.Address              // 白名单代币合约 (getLogs address filter)
	watch        []types.Address              // 监控地址全集 (vaults + 双热钱包)
	orders       map[types.Address]*orderInfo // deposit vault address -> order
}

// loadChainView 拉取本链 supported_assets + deposit_addresses+orders+merchant 视图.
func (ix *Indexer) loadChainView(ctx context.Context) (*chainView, error) {
	cv := &chainView{
		tokenSymbols: map[types.Address]string{},
		orders:       map[types.Address]*orderInfo{},
	}
	rows, err := ix.db.Query(ctx,
		`SELECT token_contract, asset_symbol FROM supported_assets WHERE chain_id=$1`,
		int64(ix.cfg.ChainID))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tc, sym string
		if err := rows.Scan(&tc, &sym); err != nil {
			rows.Close()
			return nil, err
		}
		a := types.HexToAddress(tc)
		cv.tokenSymbols[a] = sym
		cv.tokenList = append(cv.tokenList, a)
	}
	rows.Close()

	rows, err = ix.db.Query(ctx,
		`SELECT da.id, da.address, da.is_quarantined,
		        po.id, po.merchant_id, po.asset_symbol, po.expected_amount::text,
		        po.received_amount::text, po.status, po.expires_at, m.fee_rate_bps
		   FROM deposit_addresses da
		   JOIN payment_orders po ON po.deposit_address_id = da.id
		   JOIN merchants m ON m.id = po.merchant_id
		  WHERE da.chain_id=$1`, int64(ix.cfg.ChainID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[types.Address]bool{}
	for rows.Next() {
		var oi orderInfo
		var addr, exp, recv string
		if err := rows.Scan(&oi.depositAddrID, &addr, &oi.quarantined,
			&oi.orderID, &oi.merchantID, &oi.assetSymbol, &exp, &recv,
			&oi.status, &oi.expiresAt, &oi.feeRateBps); err != nil {
			return nil, err
		}
		va := types.HexToAddress(addr)
		oi.expectedAmount = types.MustParseU256(exp)
		oi.receivedAmount = types.MustParseU256(recv)
		cv.orders[va] = &oi
		if !seen[va] {
			seen[va] = true
			cv.watch = append(cv.watch, va)
		}
	}
	// §4.1: 过滤集合永含 payout_hot_wallet 与 relayer_hot_wallet
	// (自动识别冷热调拨与外部注资 — 分支 A).
	for _, w := range []types.Address{ix.cfg.PayoutHotWallet, ix.cfg.RelayerHotWallet} {
		if w != (types.Address{}) && !seen[w] {
			seen[w] = true
			cv.watch = append(cv.watch, w)
		}
	}
	return cv, rows.Err()
}

// orderToken 订单应付币种合约地址 (按 asset_symbol 反查 supported_assets).
func (cv *chainView) orderToken(oi *orderInfo) (types.Address, bool) {
	for tc, sym := range cv.tokenSymbols {
		if sym == oi.assetSymbol {
			return tc, true
		}
	}
	return types.Address{}, false
}

// ---------------------------------------------------------------------------
// 区块前推: chain_blocks + Transfer 捕获 + 游标 (单事务)
// ---------------------------------------------------------------------------

// processBlock 处理一个主链块 hdr (父哈希已由调用方校验连续).
// 单 PG 事务内: 块落 CANONICAL + 块内 Transfer 捕获 DETECTED + 游标推进.
func (ix *Indexer) processBlock(ctx context.Context, hdr *chain.BlockHeader) error {
	cv, err := ix.loadChainView(ctx)
	if err != nil {
		return err
	}
	// 拉取该块监控地址的 Transfer 日志 (A1: 按 canonical block hash 钉死,
	// 网络 I/O 在事务外).
	evs, err := ix.chain.TransferLogs(ctx, hdr, cv.tokenList, cv.watch, ix.cfg.MaxFilterAddrs)
	if err != nil {
		return fmt.Errorf("indexer: fetch logs block %d: %w", hdr.Number, err)
	}

	var after []func() // FIX-G BUG-2: 提交后 SSE 发布器 — 绝不先发后提
	err = pgx.BeginFunc(ctx, ix.db, func(tx pgx.Tx) error {
		// chain_blocks: 孤儿块哈希在新主链复现时复活为 CANONICAL (极端 re-reorg 边
		// 界; 同高度旧块已由 LIFO 先行 ORPHANED, 高度唯一索引不冲突).
		if _, err := tx.Exec(ctx,
			`INSERT INTO chain_blocks (chain_id, block_hash, block_number, parent_hash, status)
			 VALUES ($1,$2,$3,$4,'CANONICAL')
			 ON CONFLICT (chain_id, block_hash) DO UPDATE
			   SET status='CANONICAL', block_number=EXCLUDED.block_number,
			       parent_hash=EXCLUDED.parent_hash, indexed_at=now()`,
			int64(ix.cfg.ChainID), hashStr(hdr.Hash), int64(hdr.Number), hashStr(hdr.ParentHash)); err != nil {
			return err
		}

		for _, ev := range evs {
			if ev.BlockHash != hdr.Hash {
				// A1 双保险: 钉死语义由 Pool 保证, ChainQuerier 实现回归时
				// 异哈希日志在此兜底剔除, 绝不入 canonical 流水.
				ix.log.Warn("drop non-canonical blockHash log",
					zap.String("want", hashStr(hdr.Hash)), zap.String("got", hashStr(ev.BlockHash)),
					zap.String("tx", hashStr(ev.TxHash)))
				continue
			}
			cat, orderID := ix.classify(ev, cv)
			if cat == "" {
				continue // watch 集合外地址 (防御 — 正常不会发生)
			}
			oi := orderInfoAt(cv, ev.To)
			var matched *uuid.UUID
			if orderID != uuid.Nil {
				matched = &orderID
			} else if oi != nil && cat != types.DepositCategoryTopup && cat != types.DepositCategoryTreasuryRebalance {
				// WRONG_ASSET/POST_TERMINAL 仍关联订单供运营观测 (matched_order_id 可空列).
				matched = &oi.orderID
			}
			// 孤儿块哈希复活 (极端边界): 既有 ORPHANED_* 记录重捕为 DETECTED,
			// 结算侧按周期引用幂等防重复记账.
			if _, err := tx.Exec(ctx,
				`INSERT INTO onchain_deposits
				   (chain_id, block_hash, tx_hash, log_index, block_number,
				    token_contract, from_address, to_address, amount,
				    deposit_category, status, matched_order_id)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::numeric,$10,'DETECTED',$11)
				 ON CONFLICT (chain_id, block_hash, tx_hash, log_index) DO UPDATE
				   SET status='DETECTED'
				   WHERE onchain_deposits.status IN ('ORPHANED_SHALLOW','ORPHANED_DEEP')`,
				int64(ix.cfg.ChainID), hashStr(ev.BlockHash), hashStr(ev.TxHash),
				int64(ev.LogIndex), int64(ev.BlockNumber),
				addrStr(ev.TokenContract), addrStr(ev.From), addrStr(ev.To),
				ev.Amount.String(), string(cat), matched); err != nil {
				return err
			}
			if matched != nil {
				// FIX-G BUG-2: 订单关联充值捕获 — 提交后广播 block_detected
				// (TOPUP/REBALANCE 无订单不发; 孤儿复活重捕亦属真检测).
				oid := *matched
				after = append(after, func() {
					ix.publishOrderEvent(ctx, ports.OrderEvent{
						OrderID: oid, Type: ports.OrderEventDepositDetected,
						OccurredAt: ix.now()})
				})
			}
		}

		// 游标推进与块写入同事务 (崩溃后重放天然幂等).
		_, err := tx.Exec(ctx,
			`UPDATE chain_sync_cursors
			   SET latest_synced_height=$2, latest_synced_hash=$3, updated_at=now()
			 WHERE chain_id=$1`,
			int64(ix.cfg.ChainID), int64(hdr.Number), hashStr(hdr.Hash))
		return err
	})
	if err != nil {
		return err
	}
	for _, fn := range after {
		fn()
	}
	return nil
}

// classify §4.1 四分支入站分类 (detect 期快照; finalize 期对 ORDER_MATCH
// 边界做二次复核 — 订单状态可能在等待 safe_head 期间跃迁).
func (ix *Indexer) classify(ev chain.TransferEvent, cv *chainView) (types.DepositCategory, uuid.UUID) {
	// 分支 A: 热钱包入金 — 内部金库调拨或外部注资.
	if ev.To == ix.cfg.PayoutHotWallet || ev.To == ix.cfg.RelayerHotWallet {
		// A5: from ∈ 冷金库集合 (ColdWallet ∪ ColdWallets, 含轮换期新旧并存).
		if ix.isColdWallet(ev.From) {
			return types.DepositCategoryTreasuryRebalance, uuid.Nil
		}
		return types.DepositCategoryTopup, uuid.Nil
	}
	oi := cv.orders[ev.To]
	if oi == nil {
		// 不在视图内的收款地址 — 过滤集合外的转账, 跳过入库由调用方忽略.
		return "", uuid.Nil
	}
	// 分支 B: 异币误充 (token_contract != 订单应付币种合约).
	if want, ok := cv.orderToken(oi); ok && ev.TokenContract != want {
		return types.DepositCategoryWrongAsset, oi.orderID
	}
	// 分支 C: 终态订单二次误充 (业务完结语义终态).
	if oi.status.IsBusinessTerminal() {
		return types.DepositCategoryPostTerminal, oi.orderID
	}
	// 分支 D: 活跃/挂起订单正常入账.
	return types.DepositCategoryOrderMatch, oi.orderID
}

func orderInfoAt(cv *chainView, a types.Address) *orderInfo { return cv.orders[a] }

// ---------------------------------------------------------------------------
// 出站入块检测: BROADCASTED -> INCLUDED_UNSAFE (+ 兄弟 SUPERSEDED)
// ---------------------------------------------------------------------------

// detectOutboundInclusions 巡检在播候选回执; 入块且所在块在本地主链上时
// 标记 INCLUDED_UNSAFE + 意图 INCLUDED_PENDING_SAFE + 同 Nonce 兄弟 SUPERSEDED.
// 每 tick 调用一次 (块前推之后, 结算之前).
func (ix *Indexer) detectOutboundInclusions(ctx context.Context) error {
	rows, err := ix.db.Query(ctx,
		`SELECT a.id, a.intent_id, a.tx_hash
		   FROM outbound_tx_attempts a
		  WHERE a.chain_id=$1 AND a.status='BROADCASTED'
		  ORDER BY a.broadcasted_at LIMIT 500`, int64(ix.cfg.ChainID))
	if err != nil {
		return err
	}
	type cand struct {
		id       uuid.UUID
		intentID uuid.UUID
		txHash   string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.intentID, &c.txHash); err != nil {
			rows.Close()
			return err
		}
		cands = append(cands, c)
	}
	rows.Close()

	for _, c := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rcpt, err := ix.chain.ReceiptAt(ctx, types.HexToHash(c.txHash))
		if err != nil {
			ix.log.Warn("receipt query failed", zap.String("tx", c.txHash), zap.Error(err))
			continue
		}
		if rcpt == nil {
			continue
		}
		// 回执所在块必须已落本地主链, 否则等下一轮 (防节点分叉视图误导).
		var canon bool
		if err := ix.db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM chain_blocks
			    WHERE chain_id=$1 AND block_hash=$2 AND status='CANONICAL')`,
			int64(ix.cfg.ChainID), hashStr(rcpt.BlockHash)).Scan(&canon); err != nil {
			return err
		}
		if !canon {
			continue
		}
		if err := ix.markIncluded(ctx, c.intentID, c.id, rcpt); err != nil {
			return err
		}
	}
	return nil
}

// markIncluded 单事务: 兄弟 SUPERSEDED -> 本候选 INCLUDED_UNSAFE -> 意图
// INCLUDED_PENDING_SAFE (部分唯一索引 uq_attempts_one_included_per_intent
// 要求兄弟先行离场).
func (ix *Indexer) markIncluded(ctx context.Context, intentID, attemptID uuid.UUID,
	rcpt *ethtypes.Receipt) error {
	return pgx.BeginFunc(ctx, ix.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_tx_attempts SET status='SUPERSEDED'
			  WHERE intent_id=$1 AND status='BROADCASTED' AND id<>$2`, intentID, attemptID); err != nil {
			return err
		}
		var egp *string
		if rcpt.EffectiveGasPrice != nil {
			s := rcpt.EffectiveGasPrice.String()
			egp = &s
		}
		res, err := tx.Exec(ctx,
			`UPDATE outbound_tx_attempts
			   SET status='INCLUDED_UNSAFE', block_number=$2, block_hash=$3,
			       receipt_status=$4, gas_used=$5, effective_gas_price=$6::numeric
			 WHERE id=$1 AND status='BROADCASTED'`,
			attemptID, int64(rcpt.BlockNumber.Uint64()), hashStr(rcpt.BlockHash),
			int32(rcpt.Status), int64(rcpt.GasUsed), egp)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return nil // 已被并发处理
		}
		_, err = tx.Exec(ctx,
			`UPDATE outbound_tx_intents SET status='INCLUDED_PENDING_SAFE'
			  WHERE id=$1 AND status='IN_FLIGHT'`, intentID)
		return err
	})
}
