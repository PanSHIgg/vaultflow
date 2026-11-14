package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testDSN() string {
	if d := os.Getenv("VF_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://postgres:postgres@localhost:55432/vaultflow?sslmode=disable"
}

func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db, err := pgxpool.New(context.Background(), testDSN())
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(context.Background(), `TRUNCATE
		chain_sync_cursors, supported_assets, merchants,
		merchant_withdrawal_addresses, deposit_addresses, payment_orders,
		chain_blocks, onchain_deposits, wallet_nonce_cursors,
		outbound_tx_intents, outbound_tx_attempts, sweep_batches,
		sweep_batch_items, merchant_payouts, webhook_outbox, signer_audit_anchor,
		ledger_accounts, ledger_transactions, ledger_entries
		RESTART IDENTITY CASCADE`); err != nil {
		t.Skipf("truncate: %v", err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO merchants (id, name, api_key_hash, fee_rate_bps)
		 VALUES ('00000000-0000-0000-0000-000000000000','__SYSTEM__','__SYSTEM_SENTINEL_NO_API_KEY__',0) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("sentinel: %v", err)
	}
	return db
}

func seedMerchant(t *testing.T, db *pgxpool.Pool, url, secret string) uuid.UUID {
	id := uuid.New()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO merchants (id, name, api_key_hash, webhook_url, webhook_secret_enc)
		 VALUES ($1,'m',$2,$3,$4)`, id, "h"+id.String()[:8], url, secret); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedOutbox(t *testing.T, db *pgxpool.Pool, m uuid.UUID, payload string) uuid.UUID {
	id := uuid.New()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO webhook_outbox (id, merchant_id, event_type, payload_json)
		 VALUES ($1,$2,'ORDER_SETTLED',$3)`, id, m, payload); err != nil {
		t.Fatal(err)
	}
	return id
}

// capture 录制请求头/体.
type capture struct {
	mu   sync.Mutex
	hits []hit
}

type hit struct {
	sig, ts, ev, body string
}

func TestDeliverWithHMACSignature(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.hits = append(cap.hits, hit{
			sig:  r.Header.Get("X-VaultFlow-Signature"),
			ts:   r.Header.Get("X-VaultFlow-Timestamp"),
			ev:   r.Header.Get("X-VaultFlow-Event"),
			body: string(body)})
		cap.mu.Unlock()
		// 验签: sha256=hex(hmac(secret, ts+"."+body)).
		mac := hmac.New(sha256.New, []byte("s3cr3t"))
		mac.Write([]byte(r.Header.Get("X-VaultFlow-Timestamp")))
		mac.Write([]byte("."))
		mac.Write(body)
		if "sha256="+hex.EncodeToString(mac.Sum(nil)) != r.Header.Get("X-VaultFlow-Signature") {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	m := seedMerchant(t, db, srv.URL, "s3cr3t")
	payload := `{"order_id":"o1","amount":"1000"}`
	msgID := seedOutbox(t, db, m, payload)

	w := New(db, srv.Client(), Config{AllowInsecureHTTP: true}, nil, nil)
	n, err := w.DrainOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	var st string
	if err := db.QueryRow(ctx, `SELECT status FROM webhook_outbox WHERE id=$1`, msgID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "DELIVERED" {
		t.Fatalf("status %s", st)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.hits) != 1 || cap.hits[0].body != payload || cap.hits[0].ev != "ORDER_SETTLED" {
		t.Fatalf("hits %+v", cap.hits)
	}
	if _, err := strconv.ParseInt(cap.hits[0].ts, 10, 64); err != nil {
		t.Fatal("timestamp header missing")
	}
}

func TestRetryBackoffAndDeadLetter(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
	}))
	defer srv.Close()

	m := seedMerchant(t, db, srv.URL, "k")
	msgID := seedOutbox(t, db, m, "{}")

	var alerts []Alert
	w := New(db, srv.Client(), Config{
		AllowInsecureHTTP: true, MaxRetries: 1,
		BaseBackoff: time.Minute}, nil, func(a Alert) { alerts = append(alerts, a) })

	n, _ := w.DrainOnce(ctx)
	if n != 1 || calls != 1 {
		t.Fatalf("first drain n=%d calls=%d", n, calls)
	}
	var st string
	var rc int
	var nr time.Time
	if err := db.QueryRow(ctx,
		`SELECT status, retry_count, next_retry_at FROM webhook_outbox WHERE id=$1`,
		msgID).Scan(&st, &rc, &nr); err != nil {
		t.Fatal(err)
	}
	if st != "PENDING" || rc != 1 || !nr.After(time.Now().Add(-time.Hour)) {
		t.Fatalf("retry state %s rc=%d next=%v", st, rc, nr)
	}
	// 推到到期再投 -> 第二次失败 retry_count=2 > MaxRetries=1 -> DEAD_LETTER.
	if _, err := db.Exec(ctx,
		`UPDATE webhook_outbox SET next_retry_at=now()-interval '1s' WHERE id=$1`, msgID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT status FROM webhook_outbox WHERE id=$1`, msgID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "DEAD_LETTER" {
		t.Fatalf("status %s want DEAD_LETTER", st)
	}
	found := false
	for _, a := range alerts {
		if a.Code == "WEBHOOK_DEAD_LETTER" && a.Severity == "CRITICAL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing dead-letter alert %+v", alerts)
	}
}

func TestSkipLockedConcurrentDrain(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen[string(b)]++
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	m := seedMerchant(t, db, srv.URL, "k")
	for i := 0; i < 5; i++ {
		seedOutbox(t, db, m, fmt.Sprintf(`{"i":%d}`, i))
	}
	w1 := New(db, srv.Client(), Config{AllowInsecureHTTP: true}, nil, nil)
	w2 := New(db, srv.Client(), Config{AllowInsecureHTTP: true}, nil, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = w1.DrainOnce(ctx) }()
	go func() { defer wg.Done(); _, _ = w2.DrainOnce(ctx) }()
	wg.Wait()
	// 余量补排.
	_, _ = w1.DrainOnce(ctx)

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for body, c := range seen {
		if c != 1 {
			t.Fatalf("payload %s delivered %d times (SKIP LOCKED breach)", body, c)
		}
		total += c
	}
	if total != 5 {
		t.Fatalf("total deliveries %d want 5", total)
	}
}
