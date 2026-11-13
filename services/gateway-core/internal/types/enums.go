package types

// Direction 复式记账方向 (ledger_entries.direction / ledger_accounts.normal_side).
type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

// String 返回 DB 存储字符串.
func (d Direction) String() string { return string(d) }

// IntentType 出站交易意图类型 (outbound_tx_intents.intent_type, §6.3).
type IntentType string

const (
	// IntentTypeSweepBatch 定额批量归集 (deployAndSweepBatch).
	IntentTypeSweepBatch IntentType = "SWEEP_BATCH"
	// IntentTypeMerchantPayout 商户提现/客户退款 (稳定币 transfer).
	IntentTypeMerchantPayout IntentType = "MERCHANT_PAYOUT"
	// IntentTypeEIP3009Relay USDC EIP-3009 免 Gas 中继 (payOrderWithAuthorization).
	IntentTypeEIP3009Relay IntentType = "EIP3009_RELAY"
	// IntentTypeNonceGapFiller 0 ETH 自转账 (取消在途/填 Nonce 洞).
	IntentTypeNonceGapFiller IntentType = "NONCE_GAP_FILLER"
)

// String 返回 DB 存储字符串.
func (t IntentType) String() string { return string(t) }

// AttemptKind 同 Nonce 候选交易类型 (outbound_tx_attempts.attempt_kind).
type AttemptKind string

const (
	// AttemptKindPrimary 业务主交易候选.
	AttemptKindPrimary AttemptKind = "PRIMARY_INTENT"
	// AttemptKindGapFiller GAP_FILLER_POLICY 自转账取消候选.
	AttemptKindGapFiller AttemptKind = "GAP_FILLER"
)

// String 返回 DB 存储字符串.
func (k AttemptKind) String() string { return string(k) }

// PayoutKind 提现/退款单类型 (merchant_payouts.payout_kind).
type PayoutKind string

const (
	// PayoutKindWithdrawal 商户常规提现.
	PayoutKindWithdrawal PayoutKind = "WITHDRAWAL"
	// PayoutKindCustomerRefund 客户异常退款 (超付/迟付/少付/异币误充/终态二次误充).
	PayoutKindCustomerRefund PayoutKind = "CUSTOMER_REFUND"
)

// String 返回 DB 存储字符串.
func (k PayoutKind) String() string { return string(k) }

// DepositCategory 入站四分支分类 (onchain_deposits.deposit_category, §4.1).
type DepositCategory string

const (
	// DepositCategoryOrderMatch 分支 D: 活跃/过期挂起订单的正常同币种入账.
	DepositCategoryOrderMatch DepositCategory = "ORDER_MATCH"
	// DepositCategoryWrongAsset 分支 B: 异币误充 (token_contract 与订单币种不符),
	// 不累加 received_amount、不改订单状态, 挂 SUSPENSE_UNMATCHED_LIAB.
	DepositCategoryWrongAsset DepositCategory = "WRONG_ASSET"
	// DepositCategoryPostTerminal 分支 C: 终态订单二次误充,
	// 不改原订单状态与结算额, 挂 SUSPENSE_REFUNDABLE_LIAB.
	DepositCategoryPostTerminal DepositCategory = "POST_TERMINAL"
	// DepositCategoryTreasuryRebalance 分支 A: 冷金库 -> payout 热钱包内部调拨
	// (模板 2 INTERNAL_TREASURY_REBALANCE).
	DepositCategoryTreasuryRebalance DepositCategory = "TREASURY_REBALANCE"
	// DepositCategoryTopup 分支 A: 外部备付金/热钱包 Gas 注资 (模板 1 TREASURY_TOPUP).
	DepositCategoryTopup DepositCategory = "TOPUP"
)

// String 返回 DB 存储字符串.
func (c DepositCategory) String() string { return string(c) }
