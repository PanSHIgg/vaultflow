package create2

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// 本包测试仅使用标准库 testing: testify v1.12.1 的传递依赖
// go.yaml.in/yaml/v3 未收录进 go.sum 且 go.mod 铁律禁止增补 (见交付报告).

func fatalf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatalf(format, args...)
}

func mustNoErr(t *testing.T, err error, ctx string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", ctx, err)
	}
}

// goldenVectors docs/GOLDEN_VECTORS.json 的 JSON 投影 (只取 create2 相关字段).
type goldenVectors struct {
	EIP1167 struct {
		InitCodeTemplate string `json:"init_code_template"`
		RuntimeBytes     int    `json:"runtime_bytes"`
		CreationBytes    int    `json:"creation_bytes"`
	} `json:"eip1167"`
	CanonicalAddresses struct {
		Implementation string `json:"implementation"`
		Factory        string `json:"factory"`
	} `json:"canonical_addresses"`
	Vectors []struct {
		Name         string `json:"name"`
		ChainID      uint64 `json:"chain_id"`
		MerchantID   string `json:"merchant_id"`
		OrderID      string `json:"order_id"`
		Salt         string `json:"salt"`
		AuthNonce    string `json:"auth_nonce"`
		InitCode     string `json:"init_code"`
		InitCodeHash string `json:"init_code_hash"`
		VaultAddress string `json:"vault_address"`
	} `json:"vectors"`
}

func loadGoldenVectors(t *testing.T) goldenVectors {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "docs", "GOLDEN_VECTORS.json")
	raw, err := os.ReadFile(path)
	mustNoErr(t, err, "read "+path)
	var gv goldenVectors
	mustNoErr(t, json.Unmarshal(raw, &gv), "unmarshal golden vectors")
	if len(gv.Vectors) != 2 {
		fatalf(t, "expected exactly 2 golden vectors, got %d", len(gv.Vectors))
	}
	return gv
}

// mustRaw16 解析 "0x<32hex>" bytes16 为 UUID (bytes16 原始字节与 UUID raw16
// 一一对应, 去连字符即 hex).
func mustRaw16(t *testing.T, s string) uuid.UUID {
	t.Helper()
	raw, err := hex.DecodeString(s[2:])
	mustNoErr(t, err, "decode bytes16 "+s)
	if len(raw) != 16 {
		fatalf(t, "bytes16 %s must be 16 bytes, got %d", s, len(raw))
	}
	var u uuid.UUID
	copy(u[:], raw)
	return u
}

func TestGoldenVectors_ByteExact(t *testing.T) {
	gv := loadGoldenVectors(t)

	if gv.EIP1167.RuntimeBytes != 45 || gv.EIP1167.CreationBytes != 55 {
		fatalf(t, "eip1167 byte counts = %d/%d, want 45/55",
			gv.EIP1167.RuntimeBytes, gv.EIP1167.CreationBytes)
	}

	impl := types.HexToAddress(gv.CanonicalAddresses.Implementation)
	factory := types.HexToAddress(gv.CanonicalAddresses.Factory)
	d := NewDeriver(factory, impl)

	for _, v := range gv.Vectors {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			chainID := types.ChainID(v.ChainID)
			mid := mustRaw16(t, v.MerchantID)
			oid := mustRaw16(t, v.OrderID)

			// salt = keccak256(abi.encode(chainId, merchantId, orderId))
			salt := d.Salt(chainID, mid, oid)
			if got := salt.Hex(); got != v.Salt {
				t.Errorf("salt = %s, want %s", got, v.Salt)
			}

			// auth_nonce = keccak256(abi.encode(chainId, salt))
			nonce := d.AuthNonce(chainID, salt)
			if got := nonce.Hex(); got != v.AuthNonce {
				t.Errorf("auth_nonce = %s, want %s", got, v.AuthNonce)
			}

			// init_code: 55B template byte-exact
			if got := "0x" + hex.EncodeToString(d.InitCode()); got != v.InitCode {
				t.Errorf("init_code = %s, want %s", got, v.InitCode)
			}
			if len(d.InitCode()) != 55 {
				t.Errorf("init_code len = %d, want 55", len(d.InitCode()))
			}

			// init_code_hash
			if got := d.InitCodeHash().Hex(); got != v.InitCodeHash {
				t.Errorf("init_code_hash = %s, want %s", got, v.InitCodeHash)
			}

			// vault_address = last20(keccak256(0xff||factory||salt||init_code_hash))
			addr := d.VaultAddress(salt)
			if got := "0x" + hex.EncodeToString(addr.Bytes()); got != v.VaultAddress {
				t.Errorf("vault_address = %s, want %s", got, v.VaultAddress)
			}

			// 便捷方法与逐步推导一致
			if got := d.VaultAddressForOrder(chainID, mid, oid); got != addr {
				t.Errorf("VaultAddressForOrder = %s, want %s", got.Hex(), addr.Hex())
			}
		})
	}
}

func TestDeriveSalt_ABIEncodeLayout(t *testing.T) {
	// 手工重建 96B blob 断言 bytes16 右填充布局:
	// chainId(32B BE) || merchantId(16B) || 0x00*16 || orderId(16B) || 0x00*16
	chainID := types.ChainID(31337)
	mid := uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff")
	oid := uuid.MustParse("ffeeddcc-bbaa-9988-7766-554433221100")

	var blob [96]byte
	blob[30] = 0x7a // chain_id 31337 = 0x7a69 -> 32B BE 末尾两字节
	blob[31] = 0x69
	copy(blob[32:48], mid[:])
	// [48:64] 必须全零 — bytes16 右填充
	copy(blob[64:80], oid[:])
	// [80:96] 必须全零

	expected := common.BytesToHash(crypto.Keccak256(blob[:]))
	if got := DeriveSalt(chainID, mid, oid); got != expected {
		t.Fatalf("DeriveSalt = %s, want %s (96B abi.encode layout)", got.Hex(), expected.Hex())
	}

	// 与黄金向量第一组一致
	if got := DeriveSalt(chainID, mid, oid).Hex(); got != "0x55b28547100539cc5b92b67e6265adcc0b81e207dcb758222285a14b58e16834" {
		t.Fatalf("salt = %s, want golden vector", got)
	}
}

func TestBuildInitCode_Template(t *testing.T) {
	impl := types.HexToAddress("0x1111111111111111111111111111111111111111")
	code := BuildInitCode(impl)
	if len(code) != InitCodeLen {
		t.Fatalf("init_code len = %d, want %d", len(code), InitCodeLen)
	}

	// 模板前后缀逐字节钉死
	if got := "0x" + hex.EncodeToString(code[:10]); got != "0x3d602d80600a3d3981f3" {
		t.Errorf("creation prefix = %s", got)
	}
	if got := "0x" + hex.EncodeToString(code[10:20]); got != "0x363d3d373d3d3d363d73" {
		t.Errorf("runtime prefix = %s", got)
	}
	if got := code[20:40]; string(got) != string(impl.Bytes()) {
		t.Errorf("implementation bytes mismatch")
	}
	if got := "0x" + hex.EncodeToString(code[40:55]); got != "0x5af43d82803e903d91602b57fd5bf3" {
		t.Errorf("runtime suffix = %s", got)
	}
	if len(code[10:]) != RuntimeCodeLen {
		t.Errorf("runtime len = %d, want %d", len(code[10:]), RuntimeCodeLen)
	}
}

// staticQuerier mock 链上 initCodeHash() 查询.
type staticQuerier struct {
	hash types.Hash
	err  error
	got  types.Address
}

func (q *staticQuerier) InitCodeHash(_ context.Context, factory types.Address) (types.Hash, error) {
	q.got = factory
	return q.hash, q.err
}

func TestVerifyChainFactoryHash(t *testing.T) {
	impl := types.HexToAddress("0x1111111111111111111111111111111111111111")
	factory := types.HexToAddress("0x2222222222222222222222222222222222222222")
	d := NewDeriver(factory, impl)

	// 一致: 通过
	q := &staticQuerier{hash: d.InitCodeHash()}
	if err := d.VerifyChainFactoryHash(context.Background(), q); err != nil {
		t.Fatalf("matching hash must pass, got %v", err)
	}
	if q.got != factory {
		t.Errorf("queried factory = %s, want bound %s", q.got.Hex(), factory.Hex())
	}

	// 不一致: ErrFactoryHashMismatch (拒绝启动)
	bad := &staticQuerier{hash: types.HexToHash("0xdeadbeef")}
	if err := d.VerifyChainFactoryHash(context.Background(), bad); !errors.Is(err, ErrFactoryHashMismatch) {
		t.Fatalf("mismatched hash must return ErrFactoryHashMismatch, got %v", err)
	}

	// 查询失败: 包装透传
	qErr := &staticQuerier{err: errors.New("rpc timeout")}
	if err := d.VerifyChainFactoryHash(context.Background(), qErr); err == nil {
		t.Fatal("querier error must propagate")
	}
}
