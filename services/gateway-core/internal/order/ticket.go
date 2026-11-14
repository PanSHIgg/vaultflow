package order

// FIX-D: CUSTOMER_REFUND 票据字节级校验与提现白名单哨兵错误.
// 127B RefundTicket 载荷布局为 AGENTS.md §2.3-B 冻结契约 (只读断言,
// 不重新实现拼装 — 载荷由 api 层 BuildRefundTicketPayload 构造).

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// refundTicketMagic REFUND_V1 载荷魔数 (19B ASCII, §2.3-B).
const refundTicketMagic = "VAULTFLOW_REFUND_V1"

const (
	// RefundTicketPayloadLen 127B 票据载荷定长.
	RefundTicketPayloadLen = 19 + 8 + 20 + 20 + 32 + 16 + 4 + 8
	// ed25519SigLen 64B Ed25519 签名.
	ed25519SigLen = 64
	// refundTicketPackedLen 入库打包定长 = 127B payload || 64B sig.
	refundTicketPackedLen = RefundTicketPayloadLen + ed25519SigLen

	// 载荷内字段偏移 (§2.3-B 冻结字节序).
	ticketOffChainID  = 19
	ticketOffToken    = 27
	ticketOffTo       = 47
	ticketOffAmount   = 67
	ticketOffPayoutID = 99
	ticketOffRetrySeq = 115
	ticketOffExpiry   = 119
)

var (
	// ErrInvalidRefundTicket 退款票据未通过本端校验 (长度/魔数/retry_seq/
	// payout_uuid 绑定不一致). API 映射 422 REFUND_TICKET_INVALID.
	ErrInvalidRefundTicket = errors.New("order: invalid refund ticket")
	// ErrAddressNotWhitelisted WITHDRAWAL 收款地址不是该商户该链上
	// status=ACTIVE 且 now<valid_until 的白名单记录. API 映射
	// 422 ADDRESS_NOT_WHITELISTED.
	ErrAddressNotWhitelisted = errors.New("order: withdrawal address not whitelisted")
)

// validateNewRefundTicket 校验新建 CUSTOMER_REFUND 的票据:
//   - SignedPayload 恰 127B 且 magic 前缀匹配;
//   - AdminSignature 恰 64B;
//   - 内嵌 retry_seq([115:119] u32BE) == 0 — 新建单 retry_seq 列恒 0,
//     票据 seq 与之逐字节对齐否则 signer 必拒;
//   - 返回票据内嵌 payout_uuid([99:115] raw16) — 即 merchant_payouts.id
//     必须取用的值, 保证广播时 intent.reference_id == 票据 uuid.
func validateNewRefundTicket(t *ports.RefundTicket) (uuid.UUID, error) {
	if t == nil {
		return uuid.Nil, fmt.Errorf("%w: CUSTOMER_REFUND requires RefundTicket", ErrInvalidRefundTicket)
	}
	if len(t.SignedPayload) != RefundTicketPayloadLen {
		return uuid.Nil, fmt.Errorf("%w: payload %dB != %dB", ErrInvalidRefundTicket, len(t.SignedPayload), RefundTicketPayloadLen)
	}
	if len(t.AdminSignature) != ed25519SigLen {
		return uuid.Nil, fmt.Errorf("%w: admin signature %dB != %dB", ErrInvalidRefundTicket, len(t.AdminSignature), ed25519SigLen)
	}
	if !bytes.Equal(t.SignedPayload[:len(refundTicketMagic)], []byte(refundTicketMagic)) {
		return uuid.Nil, fmt.Errorf("%w: bad payload magic", ErrInvalidRefundTicket)
	}
	if rs := binary.BigEndian.Uint32(t.SignedPayload[ticketOffRetrySeq:ticketOffExpiry]); rs != 0 {
		return uuid.Nil, fmt.Errorf("%w: embedded retry_seq %d != 0 for a fresh payout", ErrInvalidRefundTicket, rs)
	}
	var id uuid.UUID
	copy(id[:], t.SignedPayload[ticketOffPayoutID:ticketOffRetrySeq])
	return id, nil
}

// validateTicketFields 交叉校验票据载荷字段与业务请求字段 — 建单前即拒绝
// 错票, 而非留待广播时 signer 逐字节比对拒签 (更早失败面, 同一冻结口径):
//   - chain_id([19:27] u64BE) == 请求链;
//   - token_address([27:47] raw20) == supported_assets 该链该币种合约;
//   - to_address([47:67] raw20) == 收款地址 (审核退款取请求 refund_to);
//   - exact_amount([67:99] u256BE) == 退款金额 (审核=订单 received_amount);
//   - retry_seq == 0 已由 validateNewRefundTicket 保证;
//   - expiry([119:127] u64BE) 必须仍在未来.
func validateTicketFields(t *ports.RefundTicket, chainID types.ChainID,
	token, to types.Address, amount types.U256, now time.Time) error {
	pl := t.SignedPayload
	if got := binary.BigEndian.Uint64(pl[ticketOffChainID:ticketOffToken]); types.ChainID(got) != chainID {
		return fmt.Errorf("%w: ticket chain_id %d != %d", ErrInvalidRefundTicket, got, chainID)
	}
	if got := types.Address(pl[ticketOffToken:ticketOffTo]); got != token {
		return fmt.Errorf("%w: ticket token %s != %s", ErrInvalidRefundTicket,
			"0x"+hex.EncodeToString(got.Bytes()), "0x"+hex.EncodeToString(token.Bytes()))
	}
	if got := types.Address(pl[ticketOffTo:ticketOffAmount]); got != to {
		return fmt.Errorf("%w: ticket to %s != %s", ErrInvalidRefundTicket,
			"0x"+hex.EncodeToString(got.Bytes()), "0x"+hex.EncodeToString(to.Bytes()))
	}
	if got := types.U256FromBig(new(big.Int).SetBytes(pl[ticketOffAmount:ticketOffPayoutID])); got.Cmp(amount) != 0 {
		return fmt.Errorf("%w: ticket amount %s != %s", ErrInvalidRefundTicket, got, amount)
	}
	if got := binary.BigEndian.Uint64(pl[ticketOffExpiry:RefundTicketPayloadLen]); got <= uint64(now.Unix()) {
		return fmt.Errorf("%w: ticket expiry %d already passed", ErrInvalidRefundTicket, got)
	}
	return nil
}

// splitStoredRefundTicket 还原入库打包字节 (127B payload || 64B sig)
// 为 ports.RefundTicket — 与 txmanager.rebuildPayout 切片口径逐字节一致.
func splitStoredRefundTicket(packed []byte) *ports.RefundTicket {
	if len(packed) < RefundTicketPayloadLen {
		return nil
	}
	t := &ports.RefundTicket{SignedPayload: packed[:RefundTicketPayloadLen]}
	if len(packed) >= refundTicketPackedLen {
		t.AdminSignature = packed[RefundTicketPayloadLen:refundTicketPackedLen]
	}
	return t
}
