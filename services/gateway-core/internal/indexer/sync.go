package indexer

import (
	"context"
	"fmt"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/chain"
	"vaultflow/gateway-core/internal/store"
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

// chainView 单 tick 的链配置快照: 白名单代币、watch 集合、
// vault→用户地址行映射与 ACTIVE 平台钱包角色映射.
type chainView struct {
	tokenSymbols map[types.Address]string                   // token_contract -> asset_symbol
	tokenList    []types.Address                            // 白名单代币合约 (getLogs address filter)
	watch        []types.Address                            // 监控地址全集 (用户 vaults + ACTIVE 平台钱包)
	users        map[types.Address]store.UserDepositAddress // vault address -> 归属用户行
	platformRole map[types.Address]string                   // ACTIVE platform wallet address -> role
}

// loadChainView 拉取本链 supported_assets + user_deposit_addresses +
// ACTIVE platform_wallets 视图 — 每块刷新, admin 平台钱包 CRUD 无重启生效.
func (ix *Indexer) loadChainView(ctx context.Context) (*chainView, error) {
	cv := &chainView{
		tokenSymbols: map[types.Address]string{},
		users:        map[types.Address]store.UserDepositAddress{},
		platformRole: map[types.Address]string{},
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

	// 用户充值地址全集 — watch 过滤 + USER_DEPOSIT 归属反查
	// (语义等价 store.LookupUserByAddress, 批量预载免逐事件往返).
	rows, err = ix.db.Query(ctx,
		`SELECT id, user_id, address, is_deployed, is_quarantined
		   FROM user_deposit_addresses WHERE chain_id=$1`, int64(ix.cfg.ChainID))
	if err != nil {
		return nil, err
	}
	seen := map[types.Address]bool{}
	for rows.Next() {
		var uda store.UserDepositAddress
		var addr string
		if err := rows.Scan(&uda.ID, &uda.UserID, &addr,
			&uda.IsDeployed, &uda.IsQuarantined); err != nil {
			rows.Close()
			return nil, err
		}
		uda.ChainID = ix.cfg.ChainID
		uda.Address = types.HexToAddress(addr)
		cv.users[uda.Address] = uda
		if !seen[uda.Address] {
			seen[uda.Address] = true
			cv.watch = append(cv.watch, uda.Address)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// ACTIVE 平台钱包 — COLD=TREASURY_REBALANCE / RELAYER,PAYOUT=TOPUP 分类 +
	// watch 常驻 (热钱包注资与冷金库调拨识别).
	wallets, err := ix.st.ListPlatformWallets(ctx, &ix.cfg.ChainID)
	if err != nil {
		return nil, err
	}
	for _, w := range wallets {
		if w.Status != store.WalletStatusActive {
			continue
		}
		cv.platformRole[w.Address] = w.Role
		if !seen[w.Address] {
			seen[w.Address] = true
			cv.watch = append(cv.watch, w.Address)
		}
	}
	return cv, nil
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
	// 拉取该块监控地址的 Transfer 日志 (按 canonical block hash 钉死,
	// 网络 I/O 在事务外).
	evs, err := ix.chain.TransferLogs(ctx, hdr, cv.tokenList, cv.watch, ix.cfg.MaxFilterAddrs)
	if err != nil {
		return fmt.Errorf("indexer: fetch logs block %d: %w", hdr.Number, err)
	}

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
				// 双保险: 钉死语义由 Pool 保证, ChainQuerier 实现回归时
				// 异哈希日志在此兜底剔除, 绝不入 canonical 流水.
				ix.log.Warn("drop non-canonical blockHash log",
					zap.String("want", hashStr(hdr.Hash)), zap.String("got", hashStr(ev.BlockHash)),
					zap.String("tx", hashStr(ev.TxHash)))
				continue
			}
			cat, userID := ix.classify(ev, cv)
			var uid *uuid.UUID
			if userID != uuid.Nil {
				uid = &userID
			}
			// 孤儿块哈希复活 (极端边界): 既有 ORPHANED_* 记录重捕为 DETECTED.
			if _, err := tx.Exec(ctx,
				`INSERT INTO onchain_deposits
				   (chain_id, block_hash, tx_hash, log_index, block_number,
				    token_contract, from_address, to_address, amount,
				    deposit_category, status, user_id)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::numeric,$10,'DETECTED',$11)
				 ON CONFLICT (chain_id, block_hash, tx_hash, log_index) DO UPDATE
				   SET status='DETECTED'
				   WHERE onchain_deposits.status IN ('ORPHANED_SHALLOW','ORPHANED_DEEP')`,
				int64(ix.cfg.ChainID), hashStr(ev.BlockHash), hashStr(ev.TxHash),
				int64(ev.LogIndex), int64(ev.BlockNumber),
				addrStr(ev.TokenContract), addrStr(ev.From), addrStr(ev.To),
				ev.Amount.String(), cat, uid); err != nil {
				return err
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
	return err
}

// classify 入站分类 (交易所托管模型):
//
//  1. to_address ∈ user_deposit_addresses -> USER_DEPOSIT + user_id;
//  2. to_address ∈ ACTIVE platform_wallets -> COLD=TREASURY_REBALANCE,
//     RELAYER/PAYOUT=TOPUP;
//  3. 否则 UNMATCHED (watch 集合外来宾转账, 供运营对账观测).
//
// 返回 (deposit_category, user_id); 非 USER_DEPOSIT 时 user_id == uuid.Nil.
func (ix *Indexer) classify(ev chain.TransferEvent, cv *chainView) (string, uuid.UUID) {
	if uda, ok := cv.users[ev.To]; ok {
		return store.DepositCategoryUserDeposit, uda.UserID
	}
	if role, ok := cv.platformRole[ev.To]; ok {
		if role == store.WalletRoleCold {
			return store.DepositCategoryTreasuryRebalance, uuid.Nil
		}
		return store.DepositCategoryTopup, uuid.Nil // RELAYER / PAYOUT
	}
	return store.DepositCategoryUnmatched, uuid.Nil
}

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
