package signerclient

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"vaultflow/gateway-core/internal/ports"
)

// 巡检告警码 (机器可读, 监控台据此分级).
const (
	// AlertCodeAuditChainBroken 审计哈希链校验失败 — CRITICAL, 触发最高优先级
	// 安全响应 (signer 审计日志可能被篡改/损坏).
	AlertCodeAuditChainBroken = "SIGNER_AUDIT_CHAIN_BROKEN"
	// AlertCodeSignerUnreachable 巡检期 RPC 连续失败.
	AlertCodeSignerUnreachable = "SIGNER_UNREACHABLE"
	// AlertCodeAuditTailTruncated E7 — entries_checked 低于锚点记录:
	// 审计日志尾部条目被物理删除/截断, CRITICAL 安全事件.
	AlertCodeAuditTailTruncated = "SIGNER_AUDIT_TAIL_TRUNCATED"
	// AlertCodeAuditTipDrift E7 — entries_checked 与锚点持平但 tip_hash 不同:
	// 同高度审计内容被改写, CRITICAL 安全事件.
	AlertCodeAuditTipDrift = "SIGNER_AUDIT_TIP_DRIFT"
	// AlertCodeAuditAnchorStore E7 — 锚点存储不可用 (DSN/表缺失):
	// WARNING 降级 — 截断/漂移检测失效但巡检本体不受影响.
	AlertCodeAuditAnchorStore = "SIGNER_AUDIT_ANCHOR_STORE"
)

// startPatrol 启动每 AuditInterval (默认 5min) 一次的 VerifyAuditChain 巡检协程.
// 审计链校验失败 → CRITICAL 告警; 传输层失败 → WARNING (连续失败同样上报).
func (c *Client) startPatrol() {
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		t := time.NewTicker(c.cfg.AuditInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.runAuditOnce(ctx)
			}
		}
	}()
}

// runAuditOnce 执行一轮审计链校验; 提取为方法供测试直接驱动.
func (c *Client) runAuditOnce(ctx context.Context) {
	resp, err := c.VerifyAuditChain(ctx, ports.VerifyAuditChainRequest{FromSeq: 0, ToSeq: 0})
	switch {
	case err != nil:
		c.raise(Alert{
			Severity: AlertWarning,
			Code:     AlertCodeSignerUnreachable,
			Message:  "vault-signer VerifyAuditChain RPC failed during patrol",
			Detail:   err.Error(),
		})
	case !resp.Valid:
		c.raise(Alert{
			Severity: AlertCritical,
			Code:     AlertCodeAuditChainBroken,
			Message:  "vault-signer audit hash chain verification FAILED — audit log may be tampered",
			Detail:   "first_bad_seq=" + itoa(resp.FirstBadSeq) + " entries_checked=" + itoa(resp.EntriesChecked),
		})
	default:
		c.persistAuditAnchor(ctx, &resp)
		c.log.Debug("signer audit chain verified",
			zap.Uint64("entries", resp.EntriesChecked),
			zap.String("tip", resp.TipHash.Hex()))
	}
}

// persistAuditAnchor E7 — 校验成功后与 signer_audit_anchor 持久化锚点比对
// 并写回本轮进度:
//   - entries_checked < 锚点 → CRITICAL SIGNER_AUDIT_TAIL_TRUNCATED
//     (审计日志尾部被物理截断 — 篡改强信号, 锚点保留旧值供取证);
//   - entries_checked == 锚点 但 tip_hash 不同 → CRITICAL SIGNER_AUDIT_TIP_DRIFT
//     (同高度链内容被改写);
//   - entries_checked 前进/持平和 tip 一致 → upsert 新锚点;
//   - 未配置 AnchorDB 或表不可用 → WARNING 降级告警, 不阻断巡检.
func (c *Client) persistAuditAnchor(ctx context.Context, resp *ports.VerifyAuditChainResponse) {
	db := c.cfg.AnchorDB
	if db == nil {
		return // 装配未接锚点库 — 锚定检测静默关闭
	}
	signerID := c.cfg.AnchorSignerID
	if signerID == "" {
		signerID = c.cfg.Endpoint
	}
	chainID := c.cfg.AnchorChainID
	tip := resp.TipHash.Hex()

	var lastTip string
	var lastChecked int64
	err := db.QueryRow(ctx,
		`SELECT last_tip_hash, last_entries_checked FROM signer_audit_anchor
		  WHERE chain_id=$1 AND signer_id=$2`, chainID, signerID).
		Scan(&lastTip, &lastChecked)
	switch {
	case err == nil && resp.EntriesChecked < uint64(lastChecked):
		c.raise(Alert{
			Severity: AlertCritical,
			Code:     AlertCodeAuditTailTruncated,
			Message: "signer audit chain SHRANK since last verified patrol — " +
				"log tail deleted; keep prior anchor for forensics",
			Detail: "signer=" + signerID + " anchor_entries=" + itoa(uint64(lastChecked)) +
				" anchor_tip=" + lastTip + " now_entries=" + itoa(resp.EntriesChecked) +
				" now_tip=" + tip,
		})
		return // 不写回 — 保留旧锚点作为被截断前的证据
	case err == nil && resp.EntriesChecked == uint64(lastChecked) && lastTip != tip:
		c.raise(Alert{
			Severity: AlertCritical,
			Code:     AlertCodeAuditTipDrift,
			Message: "signer audit tip hash changed at unchanged entry count — " +
				"chain content rewritten",
			Detail: "signer=" + signerID + " entries=" + itoa(resp.EntriesChecked) +
				" anchor_tip=" + lastTip + " now_tip=" + tip,
		})
		return
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		// 表缺失 (005 未导入) / 连接抖动 — 降级告警, 巡检本体不受阻.
		c.raise(Alert{
			Severity: AlertWarning,
			Code:     AlertCodeAuditAnchorStore,
			Message:  "signer audit anchor store unavailable — tail-truncation detection degraded",
			Detail:   err.Error(),
		})
		return
	}
	// 首锚写入或单调前进 — upsert 锚点.
	if _, err := db.Exec(ctx,
		`INSERT INTO signer_audit_anchor
		   (chain_id, signer_id, last_tip_hash, last_entries_checked, verified_at)
		 VALUES ($1,$2,$3,$4,now())
		 ON CONFLICT (chain_id, signer_id) DO UPDATE SET
		   last_tip_hash=EXCLUDED.last_tip_hash,
		   last_entries_checked=EXCLUDED.last_entries_checked,
		   verified_at=EXCLUDED.verified_at`,
		chainID, signerID, tip, int64(resp.EntriesChecked)); err != nil {
		c.raise(Alert{
			Severity: AlertWarning,
			Code:     AlertCodeAuditAnchorStore,
			Message:  "persist signer audit anchor failed — tail-truncation detection degraded",
			Detail:   err.Error(),
		})
	}
}

func (c *Client) raise(a Alert) {
	a.At = time.Now()
	if c.alertf != nil {
		c.alertf(a)
	}
	lvl := c.log.Warn
	if a.Severity == AlertCritical {
		lvl = c.log.Error
	}
	lvl("signerclient alert",
		zap.String("severity", string(a.Severity)),
		zap.String("code", a.Code),
		zap.String("message", a.Message),
		zap.String("detail", a.Detail))
}

func itoa(v uint64) string {
	// 避免为格式化引入额外依赖; 十进制无符号转换.
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
