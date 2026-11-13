// gateway-core 主程序装配入口.
//
// Phase 0 契约冻结桩: 仅引用 internal/types 与 internal/ports 使其进入构建图,
// 真实模块装配 (PG 连接池 / Redis 可降级客户端 / mTLS Signer 客户端 /
// initCodeHash 启动强断言 / 单写者 Indexer / TxManager / Sweeper /
// Outbox Worker / HTTP+SSE Server) 在 Wave 2 窗口 I1 完成.
package main

import (
	"fmt"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// 编译期契约锚点: 冻结接口签名变化将在此处直接暴露.
var (
	_ ports.LedgerService
	_ ports.OrderService
	_ ports.TxManager
	_ ports.SignerClient
	_ ports.SSEBroker
	_ ports.SimulatorService
)

func main() {
	// 契约自检: 状态机白名单与封闭科目表已加载.
	if !types.OrderAwaitingPayment.CanTransition(types.OrderPaid) ||
		!types.TemplateOrderSettle.Allows(types.AccountOrderEscrowLiab, types.AccountMerchantAvailable) {
		panic("frozen contract tables corrupted")
	}
	_ = types.SystemMerchantID

	fmt.Println("vaultflow gateway-core — assembly in Wave 2 (window I1)")
}
