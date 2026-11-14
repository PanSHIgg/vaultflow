package sim

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// forge: EIP-2718 未签名交易黄金向量 (GOLDEN_VECTORS.eip2718_unsigned_tx_example)
// ---------------------------------------------------------------------------

func TestBuildUnsignedEIP1559_Golden(t *testing.T) {
	got, err := BuildUnsignedEIP1559(
		31337, 0,
		big.NewInt(1_000_000_000), big.NewInt(2_000_000_000),
		21_000, types.HexToAddress("0x3333333333333333333333333333333333333333"),
		new(big.Int), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "0x02e9827a6980843b9aca0084773594008252089433333333333333333333333333333333333333338080c0"
	if "0x"+hex.EncodeToString(got) != want {
		t.Fatalf("unsigned tx mismatch:\n got 0x%x\nwant %s", got, want)
	}
}

func TestBuildSweepCalldata_Selector(t *testing.T) {
	cd, err := BuildSweepCalldata(
		[]SweepItem{{Salt: [32]byte{1}, ExactAmount: big.NewInt(5)}},
		types.HexToAddress("0x7777777777777777777777777777777777777777"),
		types.HexToAddress("0x6666666666666666666666666666666666666666"))
	if err != nil {
		t.Fatal(err)
	}
	wantSel := crypto.Keccak256(
		[]byte("deployAndSweepBatch((bytes32,uint256)[],address,address)"))[:4]
	if hex.EncodeToString(cd[:4]) != hex.EncodeToString(wantSel) {
		t.Fatalf("selector = 0x%x want 0x%x (BatchSweeper.sol:86)", cd[:4], wantSel)
	}
}

func TestBuildTransferCalldata(t *testing.T) {
	cd, err := BuildTransferCalldata(
		types.HexToAddress("0x3333333333333333333333333333333333333333"), big.NewInt(1500000))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(cd[:4]) != "a9059cbb" {
		t.Fatalf("transfer selector = 0x%x", cd[:4])
	}
	if len(cd) != 4+64 {
		t.Fatalf("len=%d want 68", len(cd))
	}
}

// ---------------------------------------------------------------------------
// AnvilClient JSON-RPC (httptest fake)
// ---------------------------------------------------------------------------

type fakeAnvil struct {
	head   atomic.Uint64
	calls  []string
	snapID string
}

func (f *fakeAnvil) handler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.calls = append(f.calls, req.Method)
	var res any
	switch req.Method {
	case "evm_snapshot":
		res = f.snapID
	case "eth_blockNumber":
		res = "0x" + big.NewInt(int64(f.head.Load())).Text(16)
	case "anvil_mine":
		var n uint64 = 1
		if len(req.Params) > 0 {
			if s, ok := req.Params[0].(string); ok && len(s) > 2 {
				v, _ := new(big.Int).SetString(s[2:], 16)
				n = v.Uint64()
			}
		}
		f.head.Add(n)
		res = "0x0"
	case "evm_revert":
		f.head.Store(0)
		res = true
	case "anvil_dropTransaction":
		res = true
	case "anvil_setAutomine":
		res = nil
	default:
		res = nil
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "result": res,
	})
}

func newFakeAnvilServer(t *testing.T) (*AnvilClient, *fakeAnvil, func()) {
	f := &fakeAnvil{snapID: "0x1"}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return NewAnvilClient(srv.URL, srv.Client()), f, srv.Close
}

func hasCall(f *fakeAnvil, m string) bool {
	for _, c := range f.calls {
		if c == m {
			return true
		}
	}
	return false
}

func TestAnvilClient_Controls(t *testing.T) {
	cli, f, _ := newFakeAnvilServer(t)
	ctx := context.Background()

	id, err := cli.Snapshot(ctx)
	if err != nil || id != "0x1" {
		t.Fatalf("snapshot: id=%q err=%v", id, err)
	}

	if err := cli.Mine(ctx, 5); err != nil {
		t.Fatal(err)
	}
	head, err := cli.BlockNumber(ctx)
	if err != nil || head != 5 {
		t.Fatalf("head=%d err=%v", head, err)
	}

	ok, err := cli.Revert(ctx, "0x1")
	if err != nil || !ok {
		t.Fatalf("revert: ok=%v err=%v", ok, err)
	}
	if head, _ := cli.BlockNumber(ctx); head != 0 {
		t.Fatalf("head after revert=%d", head)
	}

	ok, err = cli.DropTransaction(ctx, types.HexToHash("0xabc"))
	if err != nil || !ok {
		t.Fatalf("dropTransaction: ok=%v err=%v", ok, err)
	}
	if err := cli.SetAutomine(ctx, false); err != nil {
		t.Fatal(err)
	}

	for _, m := range []string{"evm_snapshot", "anvil_mine", "evm_revert", "anvil_dropTransaction", "anvil_setAutomine"} {
		if !hasCall(f, m) {
			t.Fatalf("missing rpc call %s", m)
		}
	}
}

func TestAnvilRouter_Invoke(t *testing.T) {
	cli, _, _ := newFakeAnvilServer(t)
	router := NewAnvilRouter(map[types.ChainID]*AnvilClient{31337: cli}, 31337)
	ctx := context.Background()

	v, err := router.Invoke(ctx, 31337, "snapshot", nil)
	if err != nil || v != "0x1" {
		t.Fatalf("snapshot: v=%v err=%v", v, err)
	}
	v, err = router.Invoke(ctx, 31337, "mine", map[string]any{"blocks": float64(3)})
	if err != nil || v != "0x0" {
		t.Fatalf("mine: v=%v err=%v", v, err)
	}
	v, err = router.Invoke(ctx, 31337, "revert", map[string]any{"snapshot_id": "0x1"})
	if err != nil || v != true {
		t.Fatalf("revert: v=%v err=%v", v, err)
	}
	if _, err := router.Invoke(ctx, 31337, "revert", map[string]any{}); err == nil {
		t.Fatal("revert without snapshot_id should fail")
	}
	v, err = router.Invoke(ctx, 31337, "drop-transaction",
		map[string]any{"tx_hash": "0x" + hex.EncodeToString(make([]byte, 32))})
	if err != nil || v != true {
		t.Fatalf("drop-transaction: v=%v err=%v", v, err)
	}
	if _, err := router.Invoke(ctx, 31337, "bogus", nil); err == nil {
		t.Fatal("unknown action should fail")
	}
}

// ---------------------------------------------------------------------------
// Service: 场景触发 / 步骤日志 / 伪造拒签
// ---------------------------------------------------------------------------

type fakeSigner struct {
	calls []ports.SignTxRequest
	codes map[ports.TxPolicy]ports.RejectCode
}

func (f *fakeSigner) SignTx(_ context.Context, req ports.SignTxRequest) (ports.SignTxResponse, error) {
	f.calls = append(f.calls, req)
	if req.PolicyHint == ports.TxPolicyGapFiller {
		return ports.SignTxResponse{Signed: &ports.SignedTx{
			RawTx: req.UnsignedTx, TxHash: types.HexToHash("0xfeed"), AuditSeq: 99,
		}}, nil
	}
	code := f.codes[req.PolicyHint]
	if code == 0 {
		code = ports.RejectCodePolicyViolation
	}
	return ports.SignTxResponse{Rejected: &ports.Rejection{
		Code: code, Reason: "forged", AuditSeq: uint64(len(f.calls)),
	}}, nil
}
func (f *fakeSigner) RegisterWhitelist(context.Context, ports.RegisterWhitelistRequest) (ports.WhitelistOpResponse, error) {
	return ports.WhitelistOpResponse{}, nil
}
func (f *fakeSigner) RevokeWhitelist(context.Context, ports.RevokeWhitelistRequest) (ports.WhitelistOpResponse, error) {
	return ports.WhitelistOpResponse{}, nil
}
func (f *fakeSigner) VerifyAuditChain(context.Context, ports.VerifyAuditChainRequest) (ports.VerifyAuditChainResponse, error) {
	return ports.VerifyAuditChainResponse{Valid: true, EntriesChecked: 7, TipHash: types.HexToHash("0xab")}, nil
}
func (f *fakeSigner) Health(context.Context) (ports.SignerHealth, error) {
	return ports.SignerHealth{Version: "test"}, nil
}

type breachSigner struct{ fakeSigner }

func (b *breachSigner) SignTx(_ context.Context, req ports.SignTxRequest) (ports.SignTxResponse, error) {
	return ports.SignTxResponse{Signed: &ports.SignedTx{
		RawTx: req.UnsignedTx, TxHash: types.HexToHash("0xdead"), AuditSeq: 1,
	}}, nil
}

func waitRun(t *testing.T, svc *Service, id uuid.UUID) ports.SimRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := svc.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != ports.SimRunRunning {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not finish in time")
	return ports.SimRun{}
}

type runDetail struct {
	Steps []struct {
		Seq    int    `json:"seq"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"steps"`
	Summary map[string]any `json:"summary"`
	Error   string         `json:"error"`
}

func decodeDetail(t *testing.T, run ports.SimRun) runDetail {
	t.Helper()
	var d runDetail
	if err := json.Unmarshal([]byte(run.Detail), &d); err != nil {
		t.Fatalf("detail not JSON: %v\n%s", err, run.Detail)
	}
	return d
}

func TestService_ReorgShallow(t *testing.T) {
	cli, _, _ := newFakeAnvilServer(t)
	svc := NewService(Deps{Router: NewAnvilRouter(
		map[types.ChainID]*AnvilClient{31337: cli}, 31337)})

	run, err := svc.Trigger(context.Background(), ports.SimTriggerRequest{
		Scenario: ports.SimScenarioReorgShallow, ChainID: 31337,
		Params: map[string]string{"depth": "3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != ports.SimRunRunning {
		t.Fatalf("status=%s want RUNNING", run.Status)
	}

	fin := waitRun(t, svc, run.ID)
	if fin.Status != ports.SimRunSucceeded {
		d := decodeDetail(t, fin)
		t.Fatalf("status=%s err=%s", fin.Status, d.Error)
	}
	d := decodeDetail(t, fin)
	if len(d.Steps) != 4 {
		t.Fatalf("steps=%d want 4", len(d.Steps))
	}
	if d.Steps[0].Name != "evm_snapshot" || d.Steps[2].Name != "evm_revert" {
		t.Fatalf("unexpected step order: %+v", d.Steps)
	}
	for _, st := range d.Steps {
		if st.Status != "OK" {
			t.Fatalf("step %s status=%s", st.Name, st.Status)
		}
	}
	if d.Summary["depth"] != float64(3) {
		t.Fatalf("summary depth=%v", d.Summary["depth"])
	}
}

func TestService_ReorgUnknownParam(t *testing.T) {
	cli, _, _ := newFakeAnvilServer(t)
	svc := NewService(Deps{Router: NewAnvilRouter(
		map[types.ChainID]*AnvilClient{31337: cli}, 31337)})
	_, err := svc.Trigger(context.Background(), ports.SimTriggerRequest{
		Scenario: ports.SimScenarioReorgShallow, ChainID: 31337,
		Params: map[string]string{"bogus_key": "1"},
	})
	if err == nil {
		t.Fatal("unknown option accepted")
	}
}

func TestService_UnknownScenario(t *testing.T) {
	cli, _, _ := newFakeAnvilServer(t)
	svc := NewService(Deps{Router: NewAnvilRouter(
		map[types.ChainID]*AnvilClient{31337: cli}, 31337)})
	if _, err := svc.Trigger(context.Background(), ports.SimTriggerRequest{
		Scenario: "BOGUS", ChainID: 31337}); err == nil {
		t.Fatal("unknown scenario accepted")
	}
}

func TestService_SignerPenetration(t *testing.T) {
	cli, _, _ := newFakeAnvilServer(t)
	signer := &fakeSigner{codes: map[ports.TxPolicy]ports.RejectCode{
		ports.TxPolicySweep:        ports.RejectCodeColdWalletNotAllowed,
		ports.TxPolicyEIP3009Relay: ports.RejectCodeAuthNonceMismatch,
		ports.TxPolicyPayout:       ports.RejectCodeWhitelistNotFound,
	}}
	svc := NewService(Deps{
		Router: NewAnvilRouter(map[types.ChainID]*AnvilClient{31337: cli}, 31337),
		Signer: signer,
	})

	run, err := svc.Trigger(context.Background(), ports.SimTriggerRequest{
		Scenario: ports.SimScenarioSignerPenetration, ChainID: 31337,
	})
	if err != nil {
		t.Fatal(err)
	}
	fin := waitRun(t, svc, run.ID)
	d := decodeDetail(t, fin)
	if fin.Status != ports.SimRunSucceeded {
		t.Fatalf("status=%s err=%s", fin.Status, d.Error)
	}
	if len(d.Steps) != 6 {
		t.Fatalf("steps=%d want 6", len(d.Steps))
	}
	for i := 0; i < 4; i++ {
		if d.Steps[i].Status != "OK" {
			t.Fatalf("forgery step %d status=%s (%s)", i, d.Steps[i].Status, d.Steps[i].Name)
		}
	}
	if d.Steps[5].Name != "verify_audit_chain" || d.Steps[5].Status != "OK" {
		t.Fatalf("audit step: %+v", d.Steps[5])
	}
	if d.Summary["all_rejected"] != true {
		t.Fatalf("all_rejected=%v", d.Summary["all_rejected"])
	}
	// 4 伪造 + 1 合法 GAP_FILLER = 5 次签名调用.
	if len(signer.calls) != 5 {
		t.Fatalf("signer calls=%d want 5", len(signer.calls))
	}
	if signer.calls[4].PolicyHint != ports.TxPolicyGapFiller {
		t.Fatalf("last call policy=%v", signer.calls[4].PolicyHint)
	}
	if signer.calls[3].RefundTicket == nil || signer.calls[3].PayoutUUID == uuid.Nil {
		t.Fatal("forgery #4 missing RefundTicket/PayoutUUID")
	}
}

func TestService_SignerPenetration_Breach(t *testing.T) {
	cli, _, _ := newFakeAnvilServer(t)
	svc := NewService(Deps{
		Router: NewAnvilRouter(map[types.ChainID]*AnvilClient{31337: cli}, 31337),
		Signer: &breachSigner{},
	})
	run, err := svc.Trigger(context.Background(), ports.SimTriggerRequest{
		Scenario: ports.SimScenarioSignerPenetration, ChainID: 31337})
	if err != nil {
		t.Fatal(err)
	}
	fin := waitRun(t, svc, run.ID)
	if fin.Status != ports.SimRunFailed {
		t.Fatalf("status=%s want FAILED (signer signed a forgery)", fin.Status)
	}
	d := decodeDetail(t, fin)
	if d.Error == "" {
		t.Fatal("expected error detail on policy breach")
	}
}

func TestService_GetRun_NotFound(t *testing.T) {
	cli, _, _ := newFakeAnvilServer(t)
	svc := NewService(Deps{Router: NewAnvilRouter(
		map[types.ChainID]*AnvilClient{31337: cli}, 31337)})
	if _, err := svc.GetRun(context.Background(), uuid.New()); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}
