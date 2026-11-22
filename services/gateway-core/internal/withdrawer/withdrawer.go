// Package withdrawer 用户提现 worker (交易所托管模型).
//
// 每链一个实例, 3s 轮询推进 withdrawals 状态机:
//
//	QUEUED      —> 构造 69B ADDR_V1 白名单租约 (admin Ed25519 签名),
//	              signerclient.RegisterWhitelist -> WHITELISTED (冷却锚点).
//	WHITELISTED —> 冷却到期后 ABI 编码 transfer(to,amount) 经 txmanager
//	              QueueIntent (MERCHANT_PAYOUT, payout 热钱包出账) -> IN_FLIGHT.
//	IN_FLIGHT   —> 对账 outbound_tx_intents 终态:
//	              CONFIRMED -> 核销 pending; 失败/取消 -> 退回 available.
//
// signer 侧硬约束 (engine.check_whitelist_lease): PAYOUT 必须有
// (chain, recipient) 的 ACTIVE 白名单租约且过冷却 — 本 worker 只做
// 注册与排队, 签名裁决完全在 signer 进程边界内.
package withdrawer

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/api"
	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/store"
	"vaultflow/gateway-core/internal/types"
)

// erc20TransferSel ERC-20 transfer(address,uint256) 函数选择器.
var erc20TransferSel = []byte{0xa9, 0x05, 0x9c, 0xbb}

// whitelistLeaseTTL 白名单租约有效期 (注册后 valid_until).
const whitelistLeaseTTL = 30 * 24 * time.Hour

// Config 单链装配参数.
type Config struct {
	ChainID           types.ChainID
	PayoutHot         types.Address // 出账热钱包 (nonce/签名归属)
	PayoutGasLimit    uint64        // transfer gas 上限 (缺省 150_000)
	CooldownSecs      int64         // 白名单冷却 (须与 signer-config cooldown_secs 一致)
	MaxFeePerGas      types.U256
	MaxPriorityFee    types.U256
	AdminEd25519Priv  ed25519.PrivateKey // 白名单租约签名私钥 (空 -> worker 不启动)
}

// Withdrawer 单链提现 worker.
type Withdrawer struct {
	cfg    Config
	pool   *pgxpool.Pool
	st     *store.Store
	txm    ports.TxManager
	signer ports.SignerClient
	log    *zap.Logger
}

// New 装配 worker (不启动).
func New(pool *pgxpool.Pool, txm ports.TxManager, signer ports.SignerClient,
	st *store.Store, cfg Config, log *zap.Logger) *Withdrawer {
	if cfg.PayoutGasLimit == 0 {
		cfg.PayoutGasLimit = 150_000
	}
	if cfg.CooldownSecs <= 0 {
		cfg.CooldownSecs = 60
	}
	return &Withdrawer{cfg: cfg, pool: pool, st: st, txm: txm, signer: signer, log: log}
}

// Enabled 无 admin 私钥无法签租约 — 该链提现功能关闭.
func (w *Withdrawer) Enabled() bool { return len(w.cfg.AdminEd25519Priv) == ed25519.PrivateKeySize }

// Start 后台推进循环 (ctx 取消即退出).
func (w *Withdrawer) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := w.tick(ctx); err != nil {
				w.log.Warn("withdrawer tick failed",
					zap.Uint64("chain_id", uint64(w.cfg.ChainID)), zap.Error(err))
			}
		}
	}()
}

func (w *Withdrawer) tick(ctx context.Context) error {
	if err := w.registerWhitelists(ctx); err != nil {
		return err
	}
	if err := w.dispatchQueued(ctx); err != nil {
		return err
	}
	return w.settleInFlight(ctx)
}

// registerWhitelists QUEUED -> WHITELISTED: 每单注册一次白名单租约.
// merchant_uuid 槽位复用 user_id (uuid raw16) — 租约归属语义.
func (w *Withdrawer) registerWhitelists(ctx context.Context) error {
	wds, err := w.st.ListWithdrawalsByStatus(ctx, w.cfg.ChainID, store.WithdrawalStatusQueued, 20)
	if err != nil {
		return err
	}
	for _, wd := range wds {
		payload := api.BuildWhitelistPayload(uint64(w.cfg.ChainID), wd.UserID,
			wd.ToAddress, uint64(time.Now().Add(whitelistLeaseTTL).Unix()))
		sig := ed25519.Sign(w.cfg.AdminEd25519Priv, payload)
		resp, err := w.signer.RegisterWhitelist(ctx, ports.RegisterWhitelistRequest{
			SignedPayload:  payload,
			AdminSignature: sig,
		})
		if err != nil {
			return fmt.Errorf("register whitelist rpc: %w", err)
		}
		if !resp.Accepted {
			if err := w.st.SettleWithdrawalFailed(ctx, wd.ID,
				fmt.Sprintf("whitelist rejected: %s", resp.Reason), true); err != nil {
				return err
			}
			w.log.Warn("withdrawal whitelist rejected",
				zap.String("withdrawal", wd.ID.String()), zap.String("reason", resp.Reason))
			continue
		}
		if err := w.st.MarkWithdrawalWhitelisted(ctx, wd.ID); err != nil {
			return err
		}
		w.log.Info("withdrawal whitelisted",
			zap.String("withdrawal", wd.ID.String()),
			zap.String("to", wd.ToAddress.Hex()),
			zap.String("amount", wd.Amount.String()))
	}
	return nil
}

// dispatchQueued WHITELISTED + 冷却到期 -> QueueIntent MERCHANT_PAYOUT.
// transfer(to, amount) calldata: sel(4) || to(32B left-pad) || amount(32B BE).
func (w *Withdrawer) dispatchQueued(ctx context.Context) error {
	wds, err := w.st.ListWithdrawalsByStatus(ctx, w.cfg.ChainID, store.WithdrawalStatusWhitelisted, 20)
	if err != nil {
		return err
	}
	cooldownReady := time.Now().Add(-time.Duration(w.cfg.CooldownSecs) * time.Second)
	for _, wd := range wds {
		if wd.WhitelistedAt == nil || wd.WhitelistedAt.After(cooldownReady) {
			continue // 冷却中
		}
		data := make([]byte, 0, 68)
		data = append(data, erc20TransferSel...)
		var pad [32]byte
		copy(pad[12:], wd.ToAddress.Bytes())
		data = append(data, pad[:]...)
		amt := wd.Amount.Big()
		var b32 [32]byte
		amt.FillBytes(b32[:])
		data = append(data, b32[:]...)

		intent, err := w.txm.QueueIntent(ctx, ports.QueueIntentRequest{
			IntentType:           types.IntentTypeMerchantPayout,
			ReferenceID:          wd.ID, // payout_uuid 幂等槽 = withdrawal id
			ChainID:              w.cfg.ChainID,
			WalletAddress:        w.cfg.PayoutHot,
			To:                   wd.TokenContract,
			Value:                types.U256FromBig(new(big.Int)),
			Calldata:             data,
			GasLimit:             w.cfg.PayoutGasLimit,
			MaxFeePerGas:         w.cfg.MaxFeePerGas,
			MaxPriorityFeePerGas: w.cfg.MaxPriorityFee,
			RetrySeq:             0,
		})
		if err != nil {
			if errors.Is(err, ports.ErrNonceHalted) {
				w.log.Warn("payout wallet nonce halted — holding withdrawal",
					zap.String("withdrawal", wd.ID.String()))
				continue // 熔断, 下 tick 再试 (保持 WHITELISTED)
			}
			return fmt.Errorf("queue payout intent: %w", err)
		}
		if err := w.st.AttachWithdrawalIntent(ctx, wd.ID, intent.ID); err != nil {
			return err
		}
		w.log.Info("withdrawal intent queued",
			zap.String("withdrawal", wd.ID.String()),
			zap.String("intent", intent.ID.String()))
	}
	return nil
}

// settleInFlight IN_FLIGHT -> CONFIRMED / FAILED 对账.
func (w *Withdrawer) settleInFlight(ctx context.Context) error {
	wds, err := w.st.ListInFlightWithdrawals(ctx, w.cfg.ChainID)
	if err != nil {
		return err
	}
	for _, wd := range wds {
		if wd.IntentID == nil {
			continue
		}
		status, txHash, err := w.st.IntentTerminalStatus(ctx, *wd.IntentID)
		if err != nil {
			return err
		}
		switch status {
		case "CONFIRMED":
			if err := w.st.SettleWithdrawalConfirmed(ctx, wd.ID, txHash); err != nil {
				return err
			}
			w.log.Info("withdrawal confirmed",
				zap.String("withdrawal", wd.ID.String()), zap.String("tx", txHash))
		case "FAILED_REVERTED", "CANCELLED_BY_FILLER", "NONCE_STOLEN_HALT":
			if err := w.st.SettleWithdrawalFailed(ctx, wd.ID, "intent "+status, false); err != nil {
				return err
			}
			w.log.Warn("withdrawal failed — refunded",
				zap.String("withdrawal", wd.ID.String()), zap.String("intent_status", status))
		}
	}
	return nil
}
