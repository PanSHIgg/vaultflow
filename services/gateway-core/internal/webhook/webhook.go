// Package webhook implements W4-4: webhook_outbox 事务性外发消费器.
//
//   - FOR UPDATE SKIP LOCKED 行级抢占, 多实例安全并发;
//   - HMAC-SHA256 签名: hex(hmac_sha256(secret, timestamp + "." + payload)),
//     每商户独立 webhook_secret;
//   - HTTPS POST (默认强制 https, 测试可开 AllowInsecureHTTP);
//   - 指数退避 next_retry_at = now + Base * 2^retry (封顶 MaxBackoff),
//     超过 MaxRetries 置 DEAD_LETTER 并告警.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Alert 外发告警.
type Alert struct {
	Severity string
	Code     string
	Message  string
	Detail   string
	At       time.Time
}

// AlertFunc 告警回调.
type AlertFunc func(Alert)

// Config 消费器参数.
type Config struct {
	PollInterval      time.Duration // 拉取 tick (默认 1s)
	BatchPerTick      int           // 单 tick 最多投递条数 (默认 32)
	MaxRetries        int           // 最大重试次数, 超过置 DEAD_LETTER (默认 12)
	BaseBackoff       time.Duration // 指数底 (默认 2s)
	MaxBackoff        time.Duration // 退避上限 (默认 15min)
	HTTPTimeout       time.Duration // 单次 POST 超时 (默认 10s)
	AllowInsecureHTTP bool          // 测试用: 允许 http:// 目标
}

func (c *Config) withDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.BatchPerTick <= 0 {
		c.BatchPerTick = 32
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 12
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 2 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 15 * time.Minute
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 10 * time.Second
	}
}

// Worker webhook 外发协程.
type Worker struct {
	db     *pgxpool.Pool
	http   *http.Client
	cfg    Config
	log    *zap.Logger
	alertf AlertFunc

	cancel context.CancelFunc
	wg     sync.WaitGroup
	now    func() time.Time
}

// New 构造 Worker (http 为 nil 时用默认 client).
func New(db *pgxpool.Pool, hc *http.Client, cfg Config, log *zap.Logger, alertf AlertFunc) *Worker {
	cfg.withDefaults()
	if log == nil {
		log = zap.NewNop()
	}
	if hc == nil {
		hc = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	return &Worker{db: db, http: hc, cfg: cfg, log: log, alertf: alertf, now: time.Now}
}

func (w *Worker) raise(a Alert) {
	a.At = w.now()
	if w.alertf != nil {
		w.alertf(a)
	}
	lvl := w.log.Warn
	if a.Severity == "CRITICAL" {
		lvl = w.log.Error
	}
	lvl("webhook alert", zap.String("severity", a.Severity),
		zap.String("code", a.Code), zap.String("message", a.Message),
		zap.String("detail", a.Detail))
}

// Start 后台消费循环.
func (w *Worker) Start(ctx context.Context) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(w.cfg.PollInterval)
		defer t.Stop()
		for {
			n, err := w.DrainOnce(ctx)
			if err != nil {
				w.log.Warn("webhook drain failed", zap.Error(err))
			}
			_ = n
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Stop 停止.
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

// ---------------------------------------------------------------------------
// 投递流水线
// ---------------------------------------------------------------------------

// DrainOnce 抢占并投递至多 BatchPerTick 条到期消息 (导出供测试驱动).
func (w *Worker) DrainOnce(ctx context.Context) (int, error) {
	n := 0
	for n < w.cfg.BatchPerTick {
		done, err := w.deliverOne(ctx)
		if err != nil {
			return n, err
		}
		if !done {
			break
		}
		n++
	}
	return n, nil
}

// deliverOne 抢占一条到期消息并完成一次投递尝试.
// 三段: ① SKIP LOCKED 抢占(PENDING 标记不提前, 锁定仅覆盖查询瞬间不可取 —
//
//	采用短事务内 SELECT FOR UPDATE SKIP LOCKED 后立即在同事务内更新终态或
//	释放后由 next_retry_at 自然让位): 为保证投递期间不被重复抢占,
//	在事务内先把 retry_count+1 与 next_retry_at 推进 (占位), 投递后按结果
//	二次修正 — 宕机时该行以下次退避时间自动复活重投.
func (w *Worker) deliverOne(ctx context.Context) (bool, error) {
	var id, merchantID uuid.UUID
	var eventType, payload, whURL, secret string
	var retry int

	err := pgx.BeginFunc(ctx, w.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT o.id, o.merchant_id, o.event_type, o.payload_json, o.retry_count,
			        COALESCE(m.webhook_url,''), COALESCE(m.webhook_secret_enc,'')
			   FROM webhook_outbox o JOIN merchants m ON m.id=o.merchant_id
			  WHERE o.status='PENDING' AND o.next_retry_at<=now()
			  ORDER BY o.next_retry_at, o.id
			  LIMIT 1 FOR UPDATE OF o SKIP LOCKED`).
			Scan(&id, &merchantID, &eventType, &payload, &retry, &whURL, &secret)
		if err != nil {
			return err
		}
		// 占位推进: 本行在下个退避窗口前对其他 worker 不可见.
		_, err = tx.Exec(ctx,
			`UPDATE webhook_outbox SET retry_count=retry_count+1, next_retry_at=$2
			 WHERE id=$1`, id, w.nextRetry(retry+1))
		return err
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, err
	}

	// 投递 (锁外网络 I/O).
	derr := w.post(ctx, whURL, secret, eventType, payload)
	var final string
	var finalRetry = retry + 1
	switch {
	case derr == nil:
		final = "DELIVERED"
	case finalRetry > w.cfg.MaxRetries:
		final = "DEAD_LETTER"
	default:
		final = "PENDING"
	}

	_, uerr := w.db.Exec(ctx,
		`UPDATE webhook_outbox SET status=$2 WHERE id=$1`, id, final)
	if uerr != nil {
		return true, uerr
	}
	if final == "DEAD_LETTER" {
		w.raise(Alert{Severity: "CRITICAL", Code: "WEBHOOK_DEAD_LETTER",
			Message: "webhook exhausted retries -> DEAD_LETTER",
			Detail:  fmt.Sprintf("id=%s merchant=%s event=%s err=%v", id, merchantID, eventType, derr)})
	}
	return true, nil
}

// nextRetry retry_count -> next_retry_at.
func (w *Worker) nextRetry(retry int) time.Time {
	d := w.cfg.BaseBackoff
	for i := 1; i < retry && d < w.cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > w.cfg.MaxBackoff {
		d = w.cfg.MaxBackoff
	}
	return w.now().Add(d)
}

// post 执行签名 POST; 返回 nil 表示 2xx.
func (w *Worker) post(ctx context.Context, whURL, secret, eventType, payload string) error {
	if whURL == "" {
		return fmt.Errorf("merchant has no webhook_url")
	}
	u, err := url.Parse(whURL)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && !(w.cfg.AllowInsecureHTTP && u.Scheme == "http") {
		return fmt.Errorf("webhook url must be https (got %s)", u.Scheme)
	}
	ts := strconv.FormatInt(w.now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, whURL,
		bytes.NewReader([]byte(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-VaultFlow-Event", eventType)
	req.Header.Set("X-VaultFlow-Timestamp", ts)
	req.Header.Set("X-VaultFlow-Signature", "sha256="+sig)

	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook http %d", resp.StatusCode)
	}
	return nil
}
