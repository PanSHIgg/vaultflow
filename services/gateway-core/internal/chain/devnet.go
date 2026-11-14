package chain

import (
	"context"

	"github.com/ethereum/go-ethereum"
)

// devnetTagAdapter 开发链 safe/finalized 标签适配器 (FIX-G BUG-4).
//
// 背景: pool.Heights 的"确认数回退"(safe = latest-requiredConfs) 只在节点
// 对 safe 标签 *报错/返回空* 时触发. 本环境 Anvil 并不报错, 而是返回一个
// 共识链风格的安全头 — safe ≈ latest-32 / finalized ≈ latest-64; 早期版本
// (I2) 仅处理 safe=0 边界, 对 latest-safe > requiredConfs 的陈旧标签
// 毫无作为, 导致结算安全线被拖在 ~32 块之后. 适配器在 chain 层显式把
// "不可信标签"转译为 ethereum.NotFound, 让 Pool 走既定回退路径.
//
// 生产链不得使用 — 真实共识 safe/finalized 标签是权威安全源, 必须直通.
type devnetTagAdapter struct {
	NodeClient
	requiredConfs uint64
}

// NewDevnetTagAdapter 显式装配选项: 包装一个节点客户端, 当其报告的
// safe/finalized 头相对 latest 滞后超过 requiredConfs 块、或链已有高度
// 而标签仍答 0 号块时, 将该标签响应转译为 ethereum.NotFound (触发 Pool 的
// latest-requiredConfs 回退); 其余响应逐字节透传. requiredConfs 与
// WithRequiredConfirmations 必须同值, 否则判定阈值与回退深度脱节.
func NewDevnetTagAdapter(inner NodeClient, requiredConfs uint64) NodeClient {
	return devnetTagAdapter{NodeClient: inner, requiredConfs: requiredConfs}
}

func (a devnetTagAdapter) HeaderByTag(ctx context.Context, tag BlockTag) (*BlockHeader, error) {
	h, err := a.NodeClient.HeaderByTag(ctx, tag)
	if err != nil || h == nil {
		return h, err
	}
	if tag != TagSafe && tag != TagFinalized {
		return h, nil
	}
	latest, lerr := a.NodeClient.BlockNumber(ctx)
	if lerr != nil || latest == 0 {
		// 读不到 latest 或链尚在 genesis: 无法判定滞后 — 透传原始标签,
		// 宁可保守放行也不误伤真实安全源.
		return h, nil
	}
	switch {
	case h.Number == 0:
		// 链已有高度而 safe/fin 仍答 0 — 该标签在此链上无意义.
		return nil, ethereum.NotFound
	case h.Number < latest && latest-h.Number > a.requiredConfs:
		// 标签陈旧 (滞后超阈值) — 转译为缺失, 走 latest-requiredConfs 回退;
		// latest-requiredConfs > h.Number 恒真 (滞后定义), 安全线前移.
		return nil, ethereum.NotFound
	}
	return h, nil
}
