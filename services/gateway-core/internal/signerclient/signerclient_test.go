package signerclient

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"vaultflow/gateway-core/internal/genproto/signerv1"
	"vaultflow/gateway-core/internal/ports"
)

// fakeSigner 可编程 VaultSignerServiceServer.
type fakeSigner struct {
	signerv1.UnimplementedVaultSignerServiceServer
	signCalls     atomic.Int32
	failFirstN    int32 // 前 N 次 SignTransaction 返 Unavailable
	lastSignReq   *signerv1.SignTransactionRequest
	verifyValid   bool
	verifyCalls   atomic.Int32
	verifyEntries uint64 // FIX-E/E7: 可编程 entries_checked (默认 9 保持旧行为)
	verifyTip     []byte // FIX-E/E7: 可编程 tip_hash (nil -> 32B 零)
	entriesSet    bool
	health        *signerv1.GetSignerHealthResponse
}

func (f *fakeSigner) SignTransaction(ctx context.Context, req *signerv1.SignTransactionRequest) (*signerv1.SignTransactionResponse, error) {
	n := f.signCalls.Add(1)
	f.lastSignReq = req
	if n <= int32(f.failFirstN) {
		return nil, status.Error(codes.Unavailable, "transient transport failure")
	}
	return &signerv1.SignTransactionResponse{
		Result: &signerv1.SignTransactionResponse_Signed{
			Signed: &signerv1.SignedTx{
				RawTx:    req.UnsignedTx,
				TxHash:   make([]byte, 32),
				AuditSeq: 77,
			}}}, nil
}

func (f *fakeSigner) VerifyAuditChain(ctx context.Context, req *signerv1.VerifyAuditChainRequest) (*signerv1.VerifyAuditChainResponse, error) {
	f.verifyCalls.Add(1)
	entries := f.verifyEntries
	if !f.entriesSet {
		entries = 9 // 旧测试默认
	}
	tip := f.verifyTip
	if tip == nil {
		tip = make([]byte, 32)
	}
	return &signerv1.VerifyAuditChainResponse{
		Valid: f.verifyValid, EntriesChecked: entries,
		FirstBadSeq: 4, TipHash: tip}, nil
}

func (f *fakeSigner) GetSignerHealth(ctx context.Context, _ *signerv1.GetSignerHealthRequest) (*signerv1.GetSignerHealthResponse, error) {
	if f.health != nil {
		return f.health, nil
	}
	return &signerv1.GetSignerHealthResponse{Version: "test", SignerClockUnix: 1700000000}, nil
}

// newTestClient bufconn + insecure 直连.
func newTestClient(t *testing.T, srv signerv1.VaultSignerServiceServer,
	cfg Config, alertf AlertFunc) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	signerv1.RegisterVaultSignerServiceServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := NewInsecureForTesting(conn, cfg, nil, alertf)
	t.Cleanup(func() { c.Close() })
	return c
}

func fastCfg() Config {
	return Config{Endpoint: "bufnet", DialTimeout: 2 * time.Second,
		MaxAttempts: 3, BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		AuditInterval: time.Hour}
}

func TestSignTxConversionAndRetry(t *testing.T) {
	f := &fakeSigner{failFirstN: 2, verifyValid: true}
	c := newTestClient(t, f, fastCfg(), nil)

	unsigned := []byte{0x02, 0xe9}
	resp, err := c.SignTx(context.Background(), ports.SignTxRequest{
		ChainID: 31337, UnsignedTx: unsigned, PolicyHint: ports.TxPolicyPayout,
		PayoutUUID: uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff"), RetrySeq: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Signed == nil || resp.Signed.AuditSeq != 77 {
		t.Fatalf("signed resp %+v", resp.Signed)
	}
	if got := f.signCalls.Load(); got != 3 {
		t.Fatalf("retry: signCalls=%d want 3", got)
	}
	if len(f.lastSignReq.PayoutUuid) != 16 || f.lastSignReq.RetrySeq != 3 {
		t.Fatal("payout uuid / retry_seq not propagated")
	}
}

func TestSignTxUnavailableExhaustsRetries(t *testing.T) {
	f := &fakeSigner{failFirstN: 99}
	c := newTestClient(t, f, fastCfg(), nil)
	_, err := c.SignTx(context.Background(), ports.SignTxRequest{
		ChainID: 1, UnsignedTx: []byte{0x02}})
	if err == nil || !errors.Is(err, ports.ErrSignerUnavailable) {
		t.Fatalf("want ErrSignerUnavailable, got %v", err)
	}
	if got := f.signCalls.Load(); got != 3 {
		t.Fatalf("attempts=%d want 3 (MaxAttempts)", got)
	}
}

func TestAuditPatrolCriticalAlert(t *testing.T) {
	f := &fakeSigner{verifyValid: false}
	var got []Alert
	c := newTestClient(t, f, fastCfg(), func(a Alert) { got = append(got, a) })

	c.runAuditOnce(context.Background())
	if len(got) != 1 || got[0].Severity != AlertCritical ||
		got[0].Code != AlertCodeAuditChainBroken {
		t.Fatalf("alerts %+v", got)
	}
}

func TestAuditPatrolValidSilent(t *testing.T) {
	f := &fakeSigner{verifyValid: true}
	var got []Alert
	c := newTestClient(t, f, fastCfg(), func(a Alert) { got = append(got, a) })
	c.runAuditOnce(context.Background())
	if len(got) != 0 {
		t.Fatalf("valid audit must not alert, got %+v", got)
	}
}

func TestHealthMapping(t *testing.T) {
	f := &fakeSigner{verifyValid: true,
		health: &signerv1.GetSignerHealthResponse{
			Version: "v9", UptimeSeconds: 12, SignerClockUnix: 1700000123,
			WhitelistEntries: 5, AuditEntries: 42,
			LoadedWallets: []string{"0x1111111111111111111111111111111111111111"}}}
	c := newTestClient(t, f, fastCfg(), nil)
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != "v9" || h.WhitelistEntries != 5 || h.AuditEntries != 42 ||
		len(h.LoadedWallets) != 1 || h.SignerClock.Unix() != 1700000123 {
		t.Fatalf("health %+v", h)
	}
}
