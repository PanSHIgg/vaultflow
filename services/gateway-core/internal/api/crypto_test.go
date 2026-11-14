package api

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// 黄金向量 (docs/GOLDEN_VECTORS.json) 逐字节断言 — AGENTS.md §2.3/§2.1 冻结契约.
// 注: go.sum 未含 testify 传递依赖 (go.yaml.in/yaml/v3), 测试一律标准库.

func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid.Parse(%q): %v", s, err)
	}
	return id
}

func TestBuildWhitelistPayload_Golden(t *testing.T) {
	// 69B: "VAULTFLOW_ADDR_V1" || chain_id u64BE || merchant raw16 || recipient 20B || valid_until u64BE
	merchant := mustParseUUID(t, "00112233-4455-6677-8899-aabbccddeeff")
	recipient := types.HexToAddress("0x3333333333333333333333333333333333333333")
	payload := BuildWhitelistPayload(31337, merchant, recipient, 0x70dbd880)
	if len(payload) != 69 {
		t.Fatalf("payload len = %d, want 69", len(payload))
	}
	want := "0x5641554c54464c4f575f414444525f56310000000000007a6900112233445566778899aabbccddeeff33333333333333333333333333333333333333330000000070dbd880"
	if got := "0x" + hex.EncodeToString(payload); got != want {
		t.Fatalf("payload mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestBuildRefundTicketPayload_Golden(t *testing.T) {
	// 127B: "VAULTFLOW_REFUND_V1" || chain_id u64BE || token 20B || to 20B
	// || amount u256BE || payout_uuid raw16 || retry_seq u32BE || expiry u64BE
	token := types.HexToAddress("0x4444444444444444444444444444444444444444")
	to := types.HexToAddress("0x5555555555555555555555555555555555555555")
	payoutID := mustParseUUID(t, "ffeeddcc-bbaa-9988-7766-554433221100")
	amount := types.U256FromBig(big.NewInt(0x16e360)) // 1500000
	payload := BuildRefundTicketPayload(31337, token, to, amount, payoutID, 0, 0x70dbd880)
	if len(payload) != 127 {
		t.Fatalf("payload len = %d, want 127", len(payload))
	}
	want := "0x5641554c54464c4f575f524546554e445f56310000000000007a6944444444444444444444444444444444444444445555555555555555555555555555555555555555000000000000000000000000000000000000000000000000000000000016e360ffeeddccbbaa99887766554433221100000000000000000070dbd880"
	if got := "0x" + hex.EncodeToString(payload); got != want {
		t.Fatalf("payload mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestAuthNonce_Golden(t *testing.T) {
	cases := []struct {
		chainID uint64
		salt    string
		want    string
	}{
		{31337,
			"0x55b28547100539cc5b92b67e6265adcc0b81e207dcb758222285a14b58e16834",
			"0x3337d78303e32c3481fbe13adfa72942f14970c28e974855c2023a9cee9ac225"},
		{1,
			"0xdc1ab224505a934e3d9842da897a6b7ecccba22c1f4cb202ad5bba11ddac8ffc",
			"0xcbfd98f6edc05b6afe4ee597131f9a7c61c114b8aee79e0bee1008b93ef4616f"},
	}
	for _, tc := range cases {
		got := AuthNonce(tc.chainID, types.HexToHash(tc.salt))
		if hexs := "0x" + hex.EncodeToString(got.Bytes()); hexs != tc.want {
			t.Fatalf("chain_id=%d: auth_nonce = %s, want %s", tc.chainID, hexs, tc.want)
		}
	}
}

func TestBuildEIP3009RelayCalldata(t *testing.T) {
	salt := types.HexToHash("0x55b28547100539cc5b92b67e6265adcc0b81e207dcb758222285a14b58e16834")
	payer := types.HexToAddress("0x3333333333333333333333333333333333333333")
	nonce := AuthNonce(31337, salt)
	cd := BuildEIP3009RelayCalldata(salt, payer, big.NewInt(1500000),
		big.NewInt(0), big.NewInt(1893456000), nonce, 27, types.Hash{0xaa}, types.Hash{0xbb})
	// 4B selector + 9*32B 定长参数 = 292B
	if len(cd) != 292 {
		t.Fatalf("calldata len = %d, want 292", len(cd))
	}
	if bytes.Equal(cd[:4], []byte{0, 0, 0, 0}) {
		t.Fatal("selector is zero")
	}
	if !bytes.Equal(salt.Bytes(), cd[4:36]) {
		t.Fatal("salt slot mismatch")
	}
	if !bytes.Equal(make([]byte, 12), cd[36:48]) || !bytes.Equal(payer.Bytes(), cd[48:68]) {
		t.Fatal("payer slot not left-padded address")
	}
	if got := new(big.Int).SetBytes(cd[68:100]); got.Cmp(big.NewInt(1500000)) != 0 {
		t.Fatalf("amount slot = %s, want 1500000", got)
	}
	if !bytes.Equal(nonce.Bytes(), cd[164:196]) {
		t.Fatal("authNonce slot mismatch")
	}
	if cd[196+31] != 27 {
		t.Fatalf("v byte = %d, want 27", cd[196+31])
	}
	if !bytes.Equal(types.Hash{0xaa}.Bytes(), cd[228:260]) {
		t.Fatal("r slot mismatch")
	}
	if !bytes.Equal(types.Hash{0xbb}.Bytes(), cd[260:292]) {
		t.Fatal("s slot mismatch")
	}
}

func TestParseEVMAddress(t *testing.T) {
	a, err := parseEVMAddress("0x3333333333333333333333333333333333333333")
	if err != nil {
		t.Fatalf("valid address rejected: %v", err)
	}
	if a != types.HexToAddress("0x3333333333333333333333333333333333333333") {
		t.Fatalf("address = %s", a.Hex())
	}
	for _, bad := range []string{"0xzz", "0x1234", "3333333333333333333333333333333333333333", ""} {
		if _, err := parseEVMAddress(bad); err == nil {
			t.Fatalf("bad address %q accepted", bad)
		}
	}
}

func TestParseEd25519Signature(t *testing.T) {
	sig, err := parseEd25519Signature("0x" + hex.EncodeToString(make([]byte, 64)))
	if err != nil || len(sig) != 64 {
		t.Fatalf("valid sig rejected: %v len=%d", err, len(sig))
	}
	if _, err := parseEd25519Signature("0x" + hex.EncodeToString(make([]byte, 63))); err == nil {
		t.Fatal("63-byte sig accepted")
	}
}
