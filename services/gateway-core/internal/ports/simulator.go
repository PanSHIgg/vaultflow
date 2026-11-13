package ports

import (
	"context"

	"github.com/google/uuid"
)

// SimulatorService 交互式对抗演练编排器 (W5 sim 实现, 仅 ENABLE_DEV_SIMULATOR=true
// 启用). 封装 Anvil evm_snapshot/evm_revert/anvil_mine/anvil_dropTransaction
// 等底层控制, 提供 ARCHITECTURE.md §8 四大演示场景一键触发.
// 消费方: W5 api 演练控制台路由, W6 演练舱.
type SimulatorService interface {
	// Trigger 触发一个演练场景, 返回运行句柄 (异步执行).
	Trigger(ctx context.Context, req SimTriggerRequest) (SimRun, error)

	// GetRun 查询演练运行状态与明细 (分叉树对比 / LIFO 红冲流水 /
	// 对账报告 / Signer 拦截审计卡片 JSON).
	GetRun(ctx context.Context, runID uuid.UUID) (SimRun, error)
}
