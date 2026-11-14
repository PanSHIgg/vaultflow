package signerclient

// FIX-E/E7 回归 (PG 集成; 未设 DSN 自动 skip):
// signer_audit_anchor (005_audit_anchor.sql) 持久化锚点 — 巡检成功后
// 单调前进写锚; entries_checked 回退 => CRITICAL TAIL_TRUNCATED;
// 计数持平 tip 漂移 => CRITICAL TIP_DRIFT; 锚点在告警路径不回退.

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func anchorPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("VF_TEST_PG_DSN")
	if dsn == "" {
		dsn = os.Getenv("VF_TEST_DSN")
	}
	if dsn == "" {
		t.Skip("VF_TEST_PG_DSN/VF_TEST_DSN not set — skipping audit-anchor integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM signer_audit_anchor WHERE chain_id=31337 AND signer_id='t-signer'`); err != nil {
		t.Fatalf("signer_audit_anchor missing (005_audit_anchor.sql applied?): %v", err)
	}
	return pool
}

func TestAuditPatrolAnchorPersistsAndDetectsTruncation(t *testing.T) {
	pool := anchorPool(t)
	ctx := context.Background()

	var mu sync.Mutex
	var alerts []Alert
	push := func(a Alert) { mu.Lock(); alerts = append(alerts, a); mu.Unlock() }
	hasAlert := func(code string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range alerts {
			if a.Code == code && a.Severity == AlertCritical {
				return true
			}
		}
		return false
	}

	f := &fakeSigner{verifyValid: true, entriesSet: true,
		verifyEntries: 9, verifyTip: bytes.Repeat([]byte{0xab}, 32)}
	cfg := fastCfg()
	cfg.AnchorDB = pool
	cfg.AnchorSignerID = "t-signer"
	cfg.AnchorChainID = 31337
	c := newTestClient(t, f, cfg, push)

	readAnchor := func() (string, int64) {
		var tip string
		var cnt int64
		if err := pool.QueryRow(ctx,
			`SELECT last_tip_hash, last_entries_checked FROM signer_audit_anchor
			  WHERE chain_id=31337 AND signer_id='t-signer'`).Scan(&tip, &cnt); err != nil {
			t.Fatalf("read anchor: %v", err)
		}
		return tip, cnt
	}

	// Round 1: 首锚落库, 无告警.
	c.runAuditOnce(ctx)
	if tip, cnt := readAnchor(); cnt != 9 || tip != hexencode(bytes.Repeat([]byte{0xab}, 32)) {
		t.Fatalf("anchor = (%s,%d) want (0xabab...,9)", tip, cnt)
	}

	// Round 2: 单调前进 -> 锚点更新.
	f.verifyEntries = 12
	f.verifyTip = bytes.Repeat([]byte{0xcd}, 32)
	c.runAuditOnce(ctx)
	if tip, cnt := readAnchor(); cnt != 12 || tip != hexencode(bytes.Repeat([]byte{0xcd}, 32)) {
		t.Fatalf("anchor = (%s,%d) want (0xcdcd...,12)", tip, cnt)
	}
	if hasAlert(AlertCodeAuditTailTruncated) || hasAlert(AlertCodeAuditTipDrift) {
		t.Fatalf("false-positive alerts on monotonic progress: %+v", alerts)
	}

	// Round 3: 尾部截断 — entries 回退 => CRITICAL, 锚点保留供取证.
	f.verifyEntries = 5
	c.runAuditOnce(ctx)
	if !hasAlert(AlertCodeAuditTailTruncated) {
		t.Fatalf("missing SIGNER_AUDIT_TAIL_TRUNCATED alert: %+v", alerts)
	}
	if tip, cnt := readAnchor(); cnt != 12 || tip != hexencode(bytes.Repeat([]byte{0xcd}, 32)) {
		t.Fatalf("anchor regressed to (%s,%d) — prior anchor must be preserved for forensics", tip, cnt)
	}

	// Round 4: 计数持平但 tip 漂移 => CRITICAL TIP_DRIFT.
	f.verifyEntries = 12
	f.verifyTip = bytes.Repeat([]byte{0xef}, 32)
	c.runAuditOnce(ctx)
	if !hasAlert(AlertCodeAuditTipDrift) {
		t.Fatalf("missing SIGNER_AUDIT_TIP_DRIFT alert: %+v", alerts)
	}
}
