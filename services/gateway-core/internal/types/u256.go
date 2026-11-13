package types

import (
	"errors"
	"fmt"
	"math/big"
)

// U256 是 *big.Int 的薄封装, 对应 PostgreSQL NUMERIC(78,0) 与 EVM uint256 全值域.
// 系统内所有金额/Gas 字段一律以其最小原子单位的十进制整数流转, 杜绝浮点与
// 精度丢失; JSON 契约(P0-1)序列化为十进制字符串.
// 零值安全: var u U256 即等价于 0.
type U256 struct{ i *big.Int }

var (
	// ErrU256Malformed 十进制字符串格式非法.
	ErrU256Malformed = errors.New("types: malformed u256 decimal string")
	// ErrU256Negative 运算结果出现域内禁止的负值 (账本全科目非负不变量).
	ErrU256Negative = errors.New("types: u256 result would be negative")
)

// bpsDenominator BPS 分母: 手续费公式 fee = amount * fee_rate_bps / 10000.
var bpsDenominator = big.NewInt(10_000)

// bigint 返回底层只读视图 (nil 时返回临时零值, 不写回, 保证零值接收者安全).
func (u U256) bigint() *big.Int {
	if u.i == nil {
		return new(big.Int)
	}
	return u.i
}

// U256FromBig 由 *big.Int 构造 (拷贝语义, 不共享底层存储).
func U256FromBig(v *big.Int) U256 {
	if v == nil {
		return U256{}
	}
	return U256{i: new(big.Int).Set(v)}
}

// U256FromUint64 由 uint64 构造.
func U256FromUint64(v uint64) U256 {
	return U256{i: new(big.Int).SetUint64(v)}
}

// ParseU256 解析非负十进制字符串 ("0" 起, 仅允许 ASCII 数字).
func ParseU256(dec string) (U256, error) {
	if dec == "" {
		return U256{}, fmt.Errorf("%w: empty string", ErrU256Malformed)
	}
	for i := 0; i < len(dec); i++ {
		if dec[i] < '0' || dec[i] > '9' {
			return U256{}, fmt.Errorf("%w: %q", ErrU256Malformed, dec)
		}
	}
	v, ok := new(big.Int).SetString(dec, 10)
	if !ok {
		return U256{}, fmt.Errorf("%w: %q", ErrU256Malformed, dec)
	}
	return U256{i: v}, nil
}

// MustParseU256 同 ParseU256, 失败即 panic; 仅用于常量与测试.
func MustParseU256(dec string) U256 {
	u, err := ParseU256(dec)
	if err != nil {
		panic(err)
	}
	return u
}

// Big 返回底层 *big.Int 的拷贝 (供 pgx NUMERIC 编解码等使用).
func (u U256) Big() *big.Int {
	return new(big.Int).Set(u.bigint())
}

// Sign 返回 -1/0/+1.
func (u U256) Sign() int { return u.bigint().Sign() }

// IsZero 是否为零.
func (u U256) IsZero() bool { return u.bigint().Sign() == 0 }

// Cmp 比较: -1 (u<o) / 0 / +1.
func (u U256) Cmp(o U256) int { return u.bigint().Cmp(o.bigint()) }

// String 十进制表示 (JSON 契约的字符串形式).
func (u U256) String() string { return u.bigint().String() }

// Add 返回 u + o.
func (u U256) Add(o U256) U256 {
	return U256{i: new(big.Int).Add(u.bigint(), o.bigint())}
}

// Sub 返回 u - o; 结果 < 0 时返回 ErrU256Negative.
func (u U256) Sub(o U256) (U256, error) {
	d := new(big.Int).Sub(u.bigint(), o.bigint())
	if d.Sign() < 0 {
		return U256{}, ErrU256Negative
	}
	return U256{i: d}, nil
}

// Mul 返回 u * o.
func (u U256) Mul(o U256) U256 {
	return U256{i: new(big.Int).Mul(u.bigint(), o.bigint())}
}

// MulBpsFloor 返回 (u * bps) / 10000 向下取整 — 手续费公式
// fee = (amount * fee_rate_bps) / 10000 (ARCHITECTURE.md §6.1).
func (u U256) MulBpsFloor(bps uint64) U256 {
	m := new(big.Int).Mul(u.bigint(), new(big.Int).SetUint64(bps))
	return U256{i: m.Div(m, bpsDenominator)}
}

// MarshalText 序列化为十进制字符串 (encoding.TextMarshaler).
func (u U256) MarshalText() ([]byte, error) {
	return []byte(u.String()), nil
}

// UnmarshalText 解析十进制字符串 (encoding.TextUnmarshaler).
func (u *U256) UnmarshalText(b []byte) error {
	v, err := ParseU256(string(b))
	if err != nil {
		return err
	}
	u.i = v.i
	return nil
}
