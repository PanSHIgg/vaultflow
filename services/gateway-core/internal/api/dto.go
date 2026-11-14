package api

// openapi.yaml components/schemas 的逐字段 DTO 投影.
// 金额字段一律 types.U256 (MarshalText -> 十进制字符串);
// 可空金额用 *types.U256; 地址/哈希一律 0x 前缀小写.

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// 通用
// ---------------------------------------------------------------------------

type pageInfo struct {
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

func pageOf(hasMore bool, next string) pageInfo {
	if !hasMore || next == "" {
		return pageInfo{NextCursor: nil, HasMore: hasMore}
	}
	return pageInfo{NextCursor: &next, HasMore: true}
}

// ---------------------------------------------------------------------------
// 订单 (Order / CreateOrderRequest / ReviewOrderRequest)
// ---------------------------------------------------------------------------

type createOrderRequest struct {
	MerchantOrderRef *string    `json:"merchant_order_ref"`
	ChainID          int64      `json:"chain_id"`
	AssetSymbol      string     `json:"asset_symbol"`
	Amount           types.U256 `json:"amount"`
	ExpiresInSeconds *int64     `json:"expires_in_seconds"`
}

type eip3009HintDTO struct {
	Supported     bool   `json:"supported"`
	RouterAddress string `json:"router_address"`
	AuthNonce     string `json:"auth_nonce"`
	TokenContract string `json:"token_contract"`
	// ValidBefore EIP-3009 授权上界 = 订单 expires_at (RFC3339);
	// 付款方须以 Unix 秒原样回填 Eip3009Submission.valid_before.
	ValidBefore *time.Time `json:"valid_before,omitempty"`
}

type orderDTO struct {
	ID                     string          `json:"id"`
	MerchantID             string          `json:"merchant_id"`
	MerchantOrderRef       *string         `json:"merchant_order_ref"`
	ChainID                int64           `json:"chain_id"`
	AssetSymbol            string          `json:"asset_symbol"`
	TokenContract          string          `json:"token_contract,omitempty"`
	ExpectedAmount         types.U256      `json:"expected_amount"`
	ReceivedAmount         types.U256      `json:"received_amount"`
	Status                 string          `json:"status"`
	DepositAddressID       string          `json:"deposit_address_id"`
	DepositAddress         string          `json:"deposit_address"`
	DepositAddressDeployed bool            `json:"deposit_address_deployed"`
	Create2Salt            string          `json:"create2_salt"`
	CashierURL             string          `json:"cashier_url"`
	EIP3009                *eip3009HintDTO `json:"eip3009"`
	ExpiresAt              time.Time       `json:"expires_at"`
	SettledAt              *time.Time      `json:"settled_at"`
	// ⚠ payment_orders DDL 无 created_at 列 (契约缺陷已上报) — 指针可空输出.
	CreatedAt *time.Time `json:"created_at"`
}

// toOrderDTO types.PaymentOrder -> Order 投影 (附带地址/资产/链配置上下文).
// merchantRef 为请求时透传的商户单号 (DDL 无列, 查询回显恒为 null — 契约缺陷已上报).
func (s *Server) toOrderDTO(o types.PaymentOrder, dep *types.DepositAddress, asset *types.SupportedAsset, merchantRef *string) orderDTO {
	dto := orderDTO{
		ID:               o.ID.String(),
		MerchantID:       o.MerchantID.String(),
		MerchantOrderRef: merchantRef,
		ChainID:          int64(o.ChainID),
		AssetSymbol:      o.AssetSymbol,
		ExpectedAmount:   o.ExpectedAmount,
		ReceivedAmount:   o.ReceivedAmount,
		Status:           o.Status.String(),
		DepositAddressID: o.DepositAddressID.String(),
		ExpiresAt:        o.ExpiresAt.UTC(),
		SettledAt:        nil,
		CashierURL:       fmt.Sprintf(s.deps.Config.CashierURLFormat, o.ID.String()),
	}
	if o.SettledAt != nil {
		t := o.SettledAt.UTC()
		dto.SettledAt = &t
	}
	if dep != nil {
		dto.DepositAddress = lowerHexAddr(dep.Address)
		dto.DepositAddressDeployed = dep.IsDeployed
		dto.Create2Salt = lowerHexHash(dep.Create2Salt)
		if asset != nil && asset.SupportsEIP3009 {
			cc := s.deps.Config.Chains[o.ChainID]
			vb := o.ExpiresAt.UTC()
			dto.EIP3009 = &eip3009HintDTO{
				Supported:     true,
				RouterAddress: lowerHexAddr(cc.EIP3009Router),
				AuthNonce:     lowerHexHash(AuthNonce(uint64(o.ChainID), dep.Create2Salt)),
				TokenContract: lowerHexAddr(asset.TokenContract),
				ValidBefore:   &vb,
			}
		}
	}
	if asset != nil {
		dto.TokenContract = lowerHexAddr(asset.TokenContract)
	}
	return dto
}

type orderPageDTO struct {
	Data []orderDTO `json:"data"`
	Page pageInfo   `json:"page"`
}

// reviewOrderRequest ReviewOrderRequest 契约.
type reviewOrderRequest struct {
	Action string              `json:"action"`
	Note   string              `json:"note"`
	Refund *refundTicketFields `json:"refund"`
}

// refundTicketFields openapi RefundTicketFields (127B 票据字段分解).
type refundTicketFields struct {
	ToAddress string `json:"to_address"`
	// PayoutUUID 票据载荷内嵌 payout_uuid (raw16, offset 99) — 即所建
	// merchant_payouts.id: 审核退款 (refund) 必填; CreateRefundRequest 下
	// (refund_ticket) 可省略, 提供时必须等于顶层 payout_uuid.
	PayoutUUID     *string `json:"payout_uuid,omitempty"`
	RetrySeq       int32   `json:"retry_seq"`
	ExpiryUnix     int64   `json:"expiry_unix"`
	AdminSignature string  `json:"admin_signature"`
}

type reviewOrderResponse struct {
	Order  orderDTO   `json:"order"`
	Payout *payoutDTO `json:"payout,omitempty"`
}

// ---------------------------------------------------------------------------
// 提现 / 退款 (Payout / CreatePayoutRequest / CreateRefundRequest)
// ---------------------------------------------------------------------------

type createPayoutRequest struct {
	PayoutUUID    string     `json:"payout_uuid"`
	SourceAccount string     `json:"source_account"`
	SourceOrderID *string    `json:"source_order_id"`
	ChainID       int64      `json:"chain_id"`
	AssetSymbol   string     `json:"asset_symbol"`
	Amount        types.U256 `json:"amount"`
	ToAddress     string     `json:"to_address"`
}

type createRefundRequest struct {
	PayoutUUID    string              `json:"payout_uuid"`
	SourceAccount string              `json:"source_account"`
	SourceOrderID *string             `json:"source_order_id"`
	ChainID       int64               `json:"chain_id"`
	AssetSymbol   string              `json:"asset_symbol"`
	Amount        types.U256          `json:"amount"`
	RefundTicket  *refundTicketFields `json:"refund_ticket"`
}

type payoutDTO struct {
	ID                string     `json:"id"`
	MerchantID        string     `json:"merchant_id"`
	PayoutKind        string     `json:"payout_kind"`
	SourceAccountType string     `json:"source_account_type"`
	SourceOrderID     *string    `json:"source_order_id"`
	ChainID           int64      `json:"chain_id"`
	AssetSymbol       string     `json:"asset_symbol"`
	ToAddress         string     `json:"to_address"`
	Amount            types.U256 `json:"amount"`
	RetrySeq          int32      `json:"retry_seq"`
	Status            string     `json:"status"`
	OutboundIntentID  *string    `json:"outbound_intent_id"`
	TxHash            *string    `json:"tx_hash"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func toPayoutDTO(p types.MerchantPayout, intentID *uuid.UUID, txHash *types.Hash) payoutDTO {
	dto := payoutDTO{
		ID:                p.ID.String(),
		MerchantID:        p.MerchantID.String(),
		PayoutKind:        p.PayoutKind.String(),
		SourceAccountType: p.SourceAccount.String(),
		ChainID:           int64(p.ChainID),
		AssetSymbol:       p.AssetSymbol,
		ToAddress:         lowerHexAddr(p.ToAddress),
		Amount:            p.Amount,
		RetrySeq:          p.RetrySeq,
		Status:            p.Status.String(),
		// ⚠ merchant_payouts 无 updated_at 列 — 以 created_at 填充 (契约缺陷已上报).
		CreatedAt: p.CreatedAt.UTC(),
		UpdatedAt: p.CreatedAt.UTC(),
	}
	if p.SourceOrderID != nil {
		s := p.SourceOrderID.String()
		dto.SourceOrderID = &s
	}
	if intentID != nil {
		s := intentID.String()
		dto.OutboundIntentID = &s
	}
	if txHash != nil {
		s := lowerHexHash(*txHash)
		dto.TxHash = &s
	}
	return dto
}

type payoutPageDTO struct {
	Data []payoutDTO `json:"data"`
	Page pageInfo    `json:"page"`
}

// ---------------------------------------------------------------------------
// 提现白名单
// ---------------------------------------------------------------------------

type registerWithdrawalAddressRequest struct {
	ChainID        int64  `json:"chain_id"`
	Address        string `json:"address"`
	ValidUntilUnix int64  `json:"valid_until_unix"`
	AdminSignature string `json:"admin_signature"`
}

type withdrawalAddressDTO struct {
	ID            string     `json:"id"`
	MerchantID    string     `json:"merchant_id"`
	ChainID       int64      `json:"chain_id"`
	Address       string     `json:"address"`
	Status        string     `json:"status"`
	ValidUntil    time.Time  `json:"valid_until"`
	FirstSeenAt   *time.Time `json:"first_seen_at"`
	SignerRevoked bool       `json:"signer_revoked"`
	CreatedAt     time.Time  `json:"created_at"`
}

type withdrawalAddressPageDTO struct {
	Data []withdrawalAddressDTO `json:"data"`
	Page pageInfo               `json:"page"`
}

// ---------------------------------------------------------------------------
// 账本
// ---------------------------------------------------------------------------

type ledgerAccountDTO struct {
	ID              string     `json:"id"`
	OwnerMerchantID string     `json:"owner_merchant_id"`
	ChainID         int64      `json:"chain_id"`
	AssetSymbol     string     `json:"asset_symbol"`
	AccountType     string     `json:"account_type"`
	NormalSide      string     `json:"normal_side"`
	Balance         types.U256 `json:"balance"`
}

type ledgerAccountListDTO struct {
	Data []ledgerAccountDTO `json:"data"`
}

type ledgerTransactionDTO struct {
	ID            string    `json:"id"`
	ReferenceType string    `json:"reference_type"`
	ReferenceID   string    `json:"reference_id"`
	Description   *string   `json:"description"`
	CreatedAt     time.Time `json:"created_at"`
}

type ledgerTransactionPageDTO struct {
	Data []ledgerTransactionDTO `json:"data"`
	Page pageInfo               `json:"page"`
}

type ledgerEntryDTO struct {
	ID              string     `json:"id"`
	TransactionID   string     `json:"transaction_id"`
	AccountID       string     `json:"account_id"`
	AccountType     string     `json:"account_type"`
	OwnerMerchantID string     `json:"owner_merchant_id"`
	Direction       string     `json:"direction"`
	Amount          types.U256 `json:"amount"`
	BalanceAfter    types.U256 `json:"balance_after"`
	CreatedAt       time.Time  `json:"created_at"`
}

type ledgerEntryPageDTO struct {
	Data []ledgerEntryDTO `json:"data"`
	Page pageInfo         `json:"page"`
}

// ---------------------------------------------------------------------------
// 公开收银台
// ---------------------------------------------------------------------------

type publicPaymentDTO struct {
	OrderID                string          `json:"order_id"`
	Status                 string          `json:"status"`
	MerchantName           string          `json:"merchant_name"`
	MerchantOrderRef       *string         `json:"merchant_order_ref"`
	ChainID                int64           `json:"chain_id"`
	AssetSymbol            string          `json:"asset_symbol"`
	TokenContract          string          `json:"token_contract"`
	Decimals               int32           `json:"decimals"`
	ExpectedAmount         types.U256      `json:"expected_amount"`
	ReceivedAmount         types.U256      `json:"received_amount"`
	DepositAddress         string          `json:"deposit_address"`
	DepositAddressDeployed bool            `json:"deposit_address_deployed"`
	ExpiresAt              time.Time       `json:"expires_at"`
	SecondsRemaining       int64           `json:"seconds_remaining"`
	SettledAt              *time.Time      `json:"settled_at"`
	SafeHeadHeight         uint64          `json:"safe_head_height"`
	RequiredConfirmations  uint64          `json:"required_confirmations"`
	EIP3009                *eip3009HintDTO `json:"eip3009"`
}

type eip3009Submission struct {
	From        string     `json:"from"`
	To          string     `json:"to"`
	Value       types.U256 `json:"value"`
	ValidAfter  int64      `json:"valid_after"`
	ValidBefore int64      `json:"valid_before"`
	Nonce       string     `json:"nonce"`
	V           int        `json:"v"`
	R           string     `json:"r"`
	S           string     `json:"s"`
}

type eip3009AcceptedDTO struct {
	OrderID      string `json:"order_id"`
	IntentID     string `json:"intent_id"`
	IntentStatus string `json:"intent_status"`
}

// ---------------------------------------------------------------------------
// SSE data payloads (openapi Sse*Data schemas)
// ---------------------------------------------------------------------------

type sseDepositData struct {
	DepositID       string     `json:"deposit_id"`
	ChainID         int64      `json:"chain_id"`
	TxHash          string     `json:"tx_hash"`
	LogIndex        int32      `json:"log_index"`
	BlockNumber     uint64     `json:"block_number"`
	FromAddress     string     `json:"from_address"`
	Amount          types.U256 `json:"amount"`
	AssetSymbol     string     `json:"asset_symbol"`
	DepositCategory string     `json:"deposit_category"`
	Status          string     `json:"status"`
	DetectedAt      *time.Time `json:"detected_at,omitempty"`
}

type sseConfirmationData struct {
	DepositID             string `json:"deposit_id,omitempty"`
	TxHash                string `json:"tx_hash,omitempty"`
	BlockNumber           uint64 `json:"block_number,omitempty"`
	CurrentConfirmations  uint64 `json:"current_confirmations"`
	RequiredConfirmations uint64 `json:"required_confirmations"`
	SafeHeadHeight        uint64 `json:"safe_head_height"`
}

type sseOrderStatusData struct {
	OrderID        string     `json:"order_id"`
	Status         string     `json:"status"`
	ExpectedAmount types.U256 `json:"expected_amount"`
	ReceivedAmount types.U256 `json:"received_amount"`
	TxHash         *string    `json:"tx_hash"`
	ExpiresAt      time.Time  `json:"expires_at"`
	SettledAt      *time.Time `json:"settled_at"`
	OccurredAt     time.Time  `json:"occurred_at"`
}

// ---------------------------------------------------------------------------
// Admin / Ops
// ---------------------------------------------------------------------------

type trialBalanceRowDTO struct {
	ChainID                int64      `json:"chain_id"`
	AssetSymbol            string     `json:"asset_symbol"`
	TotalDebit             types.U256 `json:"total_debit"`
	TotalCredit            types.U256 `json:"total_credit"`
	Balanced               bool       `json:"balanced"`
	AllBalancesNonNegative bool       `json:"all_balances_non_negative"`
}

type custodyCheckDTO struct {
	Check          string     `json:"check"`
	ChainID        int64      `json:"chain_id"`
	AssetSymbol    string     `json:"asset_symbol"`
	LedgerAccount  string     `json:"ledger_account"`
	LedgerBalance  types.U256 `json:"ledger_balance"`
	OnChainBalance types.U256 `json:"onchain_balance"`
	Delta          string     `json:"delta"`
	Matched        bool       `json:"matched"`
	SafeHeadHeight uint64     `json:"safe_head_height"`
}

type reconciliationReportDTO struct {
	RunID         *string              `json:"run_id"`
	Trigger       string               `json:"trigger,omitempty"`
	Status        string               `json:"status"`
	StartedAt     *time.Time           `json:"started_at"`
	FinishedAt    *time.Time           `json:"finished_at"`
	TrialBalances []trialBalanceRowDTO `json:"trial_balances,omitempty"`
	CustodyChecks []custodyCheckDTO    `json:"custody_checks,omitempty"`
	Failures      []string             `json:"failures,omitempty"`
}

type reconciliationRunAcceptedDTO struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

type outboundAttemptDTO struct {
	ID          string `json:"id"`
	AttemptNo   int32  `json:"attempt_no"`
	AttemptKind string `json:"attempt_kind"`
	// TxHash openapi anyOf(nullable): 排队未广播投影无哈希 — 零值
	// Hash 须输出 null 而非 "0x0000…" (FIX-D5).
	TxHash               *string     `json:"tx_hash"`
	Status               string      `json:"status"`
	BlockNumber          *uint64     `json:"block_number"`
	BlockHash            *string     `json:"block_hash"`
	ReceiptStatus        *int32      `json:"receipt_status"`
	MaxFeePerGas         types.U256  `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas types.U256  `json:"max_priority_fee_per_gas"`
	GasUsed              *uint64     `json:"gas_used"`
	EffectiveGasPrice    *types.U256 `json:"effective_gas_price"`
	BroadcastedAt        time.Time   `json:"broadcasted_at"`
}

type outboundIntentDTO struct {
	ID            string               `json:"id"`
	IntentType    string               `json:"intent_type"`
	ReferenceID   *string              `json:"reference_id"`
	ChainID       int64                `json:"chain_id"`
	WalletAddress string               `json:"wallet_address"`
	Nonce         uint64               `json:"nonce"`
	Status        string               `json:"status"`
	CreatedAt     time.Time            `json:"created_at"`
	Attempts      []outboundAttemptDTO `json:"attempts"`
}

type nonceCursorDTO struct {
	ChainID       int64      `json:"chain_id"`
	WalletAddress string     `json:"wallet_address"`
	WalletRole    string     `json:"wallet_role"`
	NextNonce     uint64     `json:"next_nonce"`
	Halted        bool       `json:"halted"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

type haltedWalletDTO struct {
	ChainID       int64  `json:"chain_id"`
	WalletAddress string `json:"wallet_address"`
	IntentID      string `json:"intent_id"`
}

type outboundQueueViewDTO struct {
	NonceCursors  []nonceCursorDTO    `json:"nonce_cursors"`
	HaltedWallets []haltedWalletDTO   `json:"halted_wallets"`
	Data          []outboundIntentDTO `json:"data"`
	Page          pageInfo            `json:"page"`
}

type signerWalletDTO struct {
	Role     string  `json:"role"`
	Address  string  `json:"address"`
	ChainIDs []int64 `json:"chain_ids"`
	Loaded   bool    `json:"loaded"`
}

type signerHealthDTO struct {
	Reachable      bool              `json:"reachable"`
	MTLSOK         bool              `json:"mtls_ok"`
	SignerVersion  string            `json:"signer_version,omitempty"`
	UptimeSeconds  uint64            `json:"uptime_seconds,omitempty"`
	Wallets        []signerWalletDTO `json:"wallets,omitempty"`
	CircuitBreaker *struct {
		Tripped         bool       `json:"tripped"`
		Rolling24hUsed  types.U256 `json:"rolling_24h_used"`
		Rolling24hLimit types.U256 `json:"rolling_24h_limit"`
	} `json:"circuit_breaker,omitempty"`
	Whitelist *struct {
		ActiveCount          int `json:"active_count"`
		PendingCooldownCount int `json:"pending_cooldown_count"`
		RevokedCount         int `json:"revoked_count"`
	} `json:"whitelist,omitempty"`
	IntentSlots *struct {
		OpenSlots         int `json:"open_slots"`
		FillerSignedSlots int `json:"filler_signed_slots"`
	} `json:"intent_slots,omitempty"`
	AuditChain *struct {
		Valid                       bool      `json:"valid"`
		LastSeq                     uint64    `json:"last_seq"`
		LastEntryHash               string    `json:"last_entry_hash"`
		VerifiedAt                  time.Time `json:"verified_at"`
		VerificationIntervalSeconds int       `json:"verification_interval_seconds"`
	} `json:"audit_chain,omitempty"`
}

type quarantinedVaultDTO struct {
	DepositAddressID string     `json:"deposit_address_id"`
	ChainID          int64      `json:"chain_id"`
	Address          string     `json:"address"`
	IsQuarantined    bool       `json:"is_quarantined"`
	AssetSymbol      string     `json:"asset_symbol,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	SweepBatchID     *string    `json:"sweep_batch_id"`
	SweepBatchItemID *string    `json:"sweep_batch_item_id"`
	ItemStatus       string     `json:"item_status,omitempty"`
	RelatedOrderID   *string    `json:"related_order_id"`
	QuarantinedAt    *time.Time `json:"quarantined_at"`
}

type quarantinedVaultPageDTO struct {
	Data []quarantinedVaultDTO `json:"data"`
	Page pageInfo              `json:"page"`
}

type merchantFreezeStateDTO struct {
	MerchantID             string     `json:"merchant_id"`
	PayoutFrozen           bool       `json:"payout_frozen"`
	ReorgReceivableBalance types.U256 `json:"reorg_receivable_balance"`
	UnfrozenAt             *time.Time `json:"unfrozen_at"`
}

// ---------------------------------------------------------------------------
// Dev simulator
// ---------------------------------------------------------------------------

type simStepDTO struct {
	Seq    int        `json:"seq"`
	Name   string     `json:"name"`
	Status string     `json:"status"`
	Detail string     `json:"detail,omitempty"`
	TxHash *string    `json:"tx_hash"`
	At     *time.Time `json:"at,omitempty"`
}

type simRunDTO struct {
	RunID      string         `json:"run_id"`
	Scenario   string         `json:"scenario"`
	Status     string         `json:"status"`
	Steps      []simStepDTO   `json:"steps"`
	Summary    map[string]any `json:"summary,omitempty"`
	Error      *string        `json:"error"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at"`
}

// runDetailJSON sim 包编码进 ports.SimRun.Detail 的 JSON 载体
// (openapi SimRun.steps/summary/error 在冻结端口上无独立字段 — 契约缺陷已上报,
// 故经 Detail 透明 JSON 透传; sim 包与本 handler 共享同一编码契约).
type runDetailJSON struct {
	Steps   []simStepDTO   `json:"steps"`
	Summary map[string]any `json:"summary"`
	Error   string         `json:"error"`
}

type simAnvilResultDTO struct {
	Action string `json:"action"`
	Result any    `json:"result"`
}
