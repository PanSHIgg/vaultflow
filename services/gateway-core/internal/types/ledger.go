package types

// ARCHITECTURE.md §6.1 — 14 个封闭科目与 normal_side; §6.2 — 9 大封闭
// 标准复式分录模板. 常量字符串即 DB CHECK 枚举值, 各窗口严禁自行扩展.

// LedgerAccountCode 账本科目码 (ledger_accounts.account_type).
type LedgerAccountCode string

const (
	// ---- 借方余额科目 normal_side = DEBIT (6 个) ----

	// AccountCustodyUnswept 已过 safe_head 停留在各 DepositVault 的待归集资产.
	AccountCustodyUnswept LedgerAccountCode = "CUSTODY_UNSWEPT"
	// AccountCustodyCold 已归集沉淀在 ColdWallet 的资产.
	AccountCustodyCold LedgerAccountCode = "CUSTODY_COLD"
	// AccountCustodyHotPayout 停留在 payout_hot_wallet 供提现/退款的备付稳定币.
	AccountCustodyHotPayout LedgerAccountCode = "CUSTODY_HOT_PAYOUT"
	// AccountHotWalletNative 两个热钱包持有的原生 ETH Gas 资产 (按钱包/链维度核算).
	AccountHotWalletNative LedgerAccountCode = "HOT_WALLET_NATIVE"
	// AccountGasExpense 累计已确认消耗的链上原生 Gas 费用.
	AccountGasExpense LedgerAccountCode = "GAS_EXPENSE"
	// AccountMerchantReorgReceivable 极端深回滚时商户可用余额不足抵扣产生的追偿应收债权.
	AccountMerchantReorgReceivable LedgerAccountCode = "MERCHANT_REORG_RECEIVABLE"

	// ---- 贷方余额科目 normal_side = CREDIT (8 个) ----

	// AccountOrderEscrowLiab 未完结活跃订单收到的在途托管负债.
	AccountOrderEscrowLiab LedgerAccountCode = "ORDER_ESCROW_LIAB"
	// AccountMerchantAvailable 已清分结算可供商户提现的负债余额.
	AccountMerchantAvailable LedgerAccountCode = "MERCHANT_AVAILABLE"
	// AccountPayoutLockedLiab 提现或客户退款执行期间从来源负债科目转入的冻结负债.
	AccountPayoutLockedLiab LedgerAccountCode = "PAYOUT_LOCKED_LIAB"
	// AccountSuspenseOverpayLiab 仅限活跃订单在有效期内结算为 OVERPAID 时超出的溢缴部分.
	AccountSuspenseOverpayLiab LedgerAccountCode = "SUSPENSE_OVERPAY_LIAB"
	// AccountSuspenseRefundableLiab 过期迟付/少付超时挂起款 (LATE_PAID_REVIEW /
	// UNDERPAID_EXPIRED_REVIEW) 以及已完结终态订单收到的二次误转账款.
	AccountSuspenseRefundableLiab LedgerAccountCode = "SUSPENSE_REFUNDABLE_LIAB"
	// AccountSuspenseUnmatchedLiab 转入 DepositAddr 但币种合约与订单应付币种不符的异币误充款.
	AccountSuspenseUnmatchedLiab LedgerAccountCode = "SUSPENSE_UNMATCHED_LIAB"
	// AccountTreasuryEquityCapital 平台外部注入热钱包的初始 Gas 或自有备付金权益资本.
	AccountTreasuryEquityCapital LedgerAccountCode = "TREASURY_EQUITY_CAPITAL"
	// AccountFeeRevenue 平台手续费收入.
	AccountFeeRevenue LedgerAccountCode = "FEE_REVENUE"
)

// accountNormalSide §6.1 科目 -> 正常余额方向 (写死映射, 兼作合法性枚举表).
var accountNormalSide = map[LedgerAccountCode]Direction{
	AccountCustodyUnswept:          DirectionDebit,
	AccountCustodyCold:             DirectionDebit,
	AccountCustodyHotPayout:        DirectionDebit,
	AccountHotWalletNative:         DirectionDebit,
	AccountGasExpense:              DirectionDebit,
	AccountMerchantReorgReceivable: DirectionDebit,
	AccountOrderEscrowLiab:         DirectionCredit,
	AccountMerchantAvailable:       DirectionCredit,
	AccountPayoutLockedLiab:        DirectionCredit,
	AccountSuspenseOverpayLiab:     DirectionCredit,
	AccountSuspenseRefundableLiab:  DirectionCredit,
	AccountSuspenseUnmatchedLiab:   DirectionCredit,
	AccountTreasuryEquityCapital:   DirectionCredit,
	AccountFeeRevenue:              DirectionCredit,
}

// IsValid 是否为 14 个封闭科目之一.
func (a LedgerAccountCode) IsValid() bool {
	_, ok := accountNormalSide[a]
	return ok
}

// NormalSide 返回科目正常余额方向; 非法科目返回空串.
func (a LedgerAccountCode) NormalSide() Direction {
	return accountNormalSide[a]
}

// String 返回 DB 存储字符串.
func (a LedgerAccountCode) String() string { return string(a) }

// AllLedgerAccountCodes 返回全部 14 个科目 (供 CHECK 约束与测试遍历).
func AllLedgerAccountCodes() []LedgerAccountCode {
	return []LedgerAccountCode{
		AccountCustodyUnswept, AccountCustodyCold, AccountCustodyHotPayout,
		AccountHotWalletNative, AccountGasExpense, AccountMerchantReorgReceivable,
		AccountOrderEscrowLiab, AccountMerchantAvailable, AccountPayoutLockedLiab,
		AccountSuspenseOverpayLiab, AccountSuspenseRefundableLiab, AccountSuspenseUnmatchedLiab,
		AccountTreasuryEquityCapital, AccountFeeRevenue,
	}
}

// SuspenseLiabilityAccounts 三类挂账待退负债科目.
var SuspenseLiabilityAccounts = []LedgerAccountCode{
	AccountSuspenseOverpayLiab, AccountSuspenseRefundableLiab, AccountSuspenseUnmatchedLiab,
}

// PayoutSourceAccounts §5.4 PAYOUT_FLOW 申请冻结腿的合法来源负债科目
// (merchant_payouts.source_account_type: MERCHANT_AVAILABLE + 三类 SUSPENSE_*_LIAB).
var PayoutSourceAccounts = []LedgerAccountCode{
	AccountMerchantAvailable,
	AccountSuspenseOverpayLiab,
	AccountSuspenseRefundableLiab,
	AccountSuspenseUnmatchedLiab,
}

// IsPayoutSource 是否为合法的提现/退款冻结来源负债科目.
func (a LedgerAccountCode) IsPayoutSource() bool {
	for _, s := range PayoutSourceAccounts {
		if a == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// §6.2 九大封闭标准复式分录模板 (ledger_transactions.reference_type)
// ---------------------------------------------------------------------------

// EntryTemplateCode 分录模板码.
type EntryTemplateCode string

const (
	// TemplateTreasuryTopup 1. 外部备付金/Gas 注资.
	TemplateTreasuryTopup EntryTemplateCode = "TREASURY_TOPUP"
	// TemplateInternalTreasuryRebalance 2. 冷金库向提现热钱包调拨 (纯内部重分类).
	TemplateInternalTreasuryRebalance EntryTemplateCode = "INTERNAL_TREASURY_REBALANCE"
	// TemplateDepositFinalized 3. 入站充值安全确认.
	TemplateDepositFinalized EntryTemplateCode = "DEPOSIT_FINALIZED"
	// TemplateOrderSettle 4. 订单清分结算.
	TemplateOrderSettle EntryTemplateCode = "ORDER_SETTLE"
	// TemplateOrderSuspense 5. 订单超时转挂待退.
	TemplateOrderSuspense EntryTemplateCode = "ORDER_SUSPENSE"
	// TemplateSweepSettle 6. 定额批量归集确认.
	TemplateSweepSettle EntryTemplateCode = "SWEEP_SETTLE"
	// TemplateGasSettle 7. 出站回执确认扣 Gas (成功与 Revert 均必记).
	TemplateGasSettle EntryTemplateCode = "GAS_SETTLE"
	// TemplatePayoutFlow 8. 提现与四类退款流转.
	TemplatePayoutFlow EntryTemplateCode = "PAYOUT_FLOW"
	// TemplateReorgReversal 9. 双向深回滚 LIFO 红冲.
	TemplateReorgReversal EntryTemplateCode = "REORG_REVERSAL"
)

// IsValid 是否为 9 个封闭模板之一.
func (t EntryTemplateCode) IsValid() bool {
	_, ok := entryTemplateRules[t]
	return ok
}

// String 返回 DB 存储字符串.
func (t EntryTemplateCode) String() string { return string(t) }

// AllEntryTemplateCodes 返回全部 9 个模板码.
func AllEntryTemplateCodes() []EntryTemplateCode {
	return []EntryTemplateCode{
		TemplateTreasuryTopup, TemplateInternalTreasuryRebalance, TemplateDepositFinalized,
		TemplateOrderSettle, TemplateOrderSuspense, TemplateSweepSettle,
		TemplateGasSettle, TemplatePayoutFlow, TemplateReorgReversal,
	}
}

// TemplateLegRule 模板内一条允许的 (借方集合 -> 贷方集合) 分录腿规则.
// 一条记账腿 (debit -> credit) 合法当且仅当它命中某一规则的
// debit ∈ Debits 且 credit ∈ Credits.
type TemplateLegRule struct {
	Debits  []LedgerAccountCode
	Credits []LedgerAccountCode
}

// entryTemplateRules §6.2 模板 -> 分录腿白名单分组.
var entryTemplateRules = map[EntryTemplateCode][]TemplateLegRule{
	// 1. TREASURY_TOPUP: DR HOT_WALLET_NATIVE 或 CUSTODY_HOT_PAYOUT / CR TREASURY_EQUITY_CAPITAL
	TemplateTreasuryTopup: {
		{
			Debits:  []LedgerAccountCode{AccountHotWalletNative, AccountCustodyHotPayout},
			Credits: []LedgerAccountCode{AccountTreasuryEquityCapital},
		},
	},
	// 2. INTERNAL_TREASURY_REBALANCE: DR CUSTODY_HOT_PAYOUT / CR CUSTODY_COLD (不虚增权益)
	TemplateInternalTreasuryRebalance: {
		{
			Debits:  []LedgerAccountCode{AccountCustodyHotPayout},
			Credits: []LedgerAccountCode{AccountCustodyCold},
		},
	},
	// 3. DEPOSIT_FINALIZED: DR CUSTODY_UNSWEPT / CR ORDER_ESCROW_LIAB(正常活跃单)
	//    | SUSPENSE_REFUNDABLE_LIAB(终态单二次误充) | SUSPENSE_UNMATCHED_LIAB(异币误充)
	TemplateDepositFinalized: {
		{
			Debits: []LedgerAccountCode{AccountCustodyUnswept},
			Credits: []LedgerAccountCode{
				AccountOrderEscrowLiab, AccountSuspenseRefundableLiab, AccountSuspenseUnmatchedLiab,
			},
		},
	},
	// 4. ORDER_SETTLE: 正常达标 DR ORDER_ESCROW_LIAB / CR MERCHANT_AVAILABLE + FEE_REVENUE
	//    (+ SUSPENSE_OVERPAY_LIAB 溢缴差额); 挂起单人工接受履约 DR SUSPENSE_REFUNDABLE_LIAB.
	TemplateOrderSettle: {
		{
			Debits: []LedgerAccountCode{AccountOrderEscrowLiab},
			Credits: []LedgerAccountCode{
				AccountMerchantAvailable, AccountFeeRevenue, AccountSuspenseOverpayLiab,
			},
		},
		{
			Debits:  []LedgerAccountCode{AccountSuspenseRefundableLiab},
			Credits: []LedgerAccountCode{AccountMerchantAvailable, AccountFeeRevenue},
		},
	},
	// 5. ORDER_SUSPENSE: DR ORDER_ESCROW_LIAB / CR SUSPENSE_REFUNDABLE_LIAB
	TemplateOrderSuspense: {
		{
			Debits:  []LedgerAccountCode{AccountOrderEscrowLiab},
			Credits: []LedgerAccountCode{AccountSuspenseRefundableLiab},
		},
	},
	// 6. SWEEP_SETTLE: DR CUSTODY_COLD / CR CUSTODY_UNSWEPT
	TemplateSweepSettle: {
		{
			Debits:  []LedgerAccountCode{AccountCustodyCold},
			Credits: []LedgerAccountCode{AccountCustodyUnswept},
		},
	},
	// 7. GAS_SETTLE: DR GAS_EXPENSE / CR HOT_WALLET_NATIVE (成功与 Revert 均必记)
	TemplateGasSettle: {
		{
			Debits:  []LedgerAccountCode{AccountGasExpense},
			Credits: []LedgerAccountCode{AccountHotWalletNative},
		},
	},
	// 8. PAYOUT_FLOW: 申请冻结 DR source_account_type / CR PAYOUT_LOCKED_LIAB;
	//    链上成功 DR PAYOUT_LOCKED_LIAB / CR CUSTODY_HOT_PAYOUT;
	//    取消解冻 DR PAYOUT_LOCKED_LIAB / CR source_account_type.
	TemplatePayoutFlow: {
		{
			Debits:  PayoutSourceAccounts,
			Credits: []LedgerAccountCode{AccountPayoutLockedLiab},
		},
		{
			Debits:  []LedgerAccountCode{AccountPayoutLockedLiab},
			Credits: []LedgerAccountCode{AccountCustodyHotPayout},
		},
		{
			Debits:  []LedgerAccountCode{AccountPayoutLockedLiab},
			Credits: PayoutSourceAccounts,
		},
	},
	// 9. REORG_REVERSAL: 出站 Gas 冲回 DR HOT_WALLET_NATIVE / CR GAS_EXPENSE;
	//    出站归集冲回 DR CUSTODY_UNSWEPT / CR CUSTODY_COLD;
	//    出站提现冲回 DR CUSTODY_HOT_PAYOUT / CR PAYOUT_LOCKED_LIAB;
	//    入站挂账冲回 DR ORDER_ESCROW_LIAB 或 SUSPENSE_*_LIAB / CR CUSTODY_UNSWEPT;
	//    入站已结单冲回 DR MERCHANT_AVAILABLE + FEE_REVENUE + MERCHANT_REORG_RECEIVABLE(透支差额)
	//    / CR CUSTODY_UNSWEPT (LIFO 顺序保证余额充足).
	TemplateReorgReversal: {
		{
			Debits:  []LedgerAccountCode{AccountHotWalletNative},
			Credits: []LedgerAccountCode{AccountGasExpense},
		},
		{
			Debits:  []LedgerAccountCode{AccountCustodyUnswept},
			Credits: []LedgerAccountCode{AccountCustodyCold},
		},
		{
			Debits:  []LedgerAccountCode{AccountCustodyHotPayout},
			Credits: []LedgerAccountCode{AccountPayoutLockedLiab},
		},
		{
			Debits:  append([]LedgerAccountCode{AccountOrderEscrowLiab}, SuspenseLiabilityAccounts...),
			Credits: []LedgerAccountCode{AccountCustodyUnswept},
		},
		{
			Debits: []LedgerAccountCode{
				AccountMerchantAvailable, AccountFeeRevenue, AccountMerchantReorgReceivable,
			},
			Credits: []LedgerAccountCode{AccountCustodyUnswept},
		},
	},
}

// Allows 校验 (debit -> credit) 单腿是否在该模板白名单内 (§6.2).
func (t EntryTemplateCode) Allows(debit, credit LedgerAccountCode) bool {
	for _, rule := range entryTemplateRules[t] {
		if containsAccount(rule.Debits, debit) && containsAccount(rule.Credits, credit) {
			return true
		}
	}
	return false
}

// LegRules 返回该模板的分组规则 (只读拷贝, 供实现端校验与文档生成).
func (t EntryTemplateCode) LegRules() []TemplateLegRule {
	rules := entryTemplateRules[t]
	out := make([]TemplateLegRule, len(rules))
	for i, r := range rules {
		out[i] = TemplateLegRule{
			Debits:  append([]LedgerAccountCode(nil), r.Debits...),
			Credits: append([]LedgerAccountCode(nil), r.Credits...),
		}
	}
	return out
}

func containsAccount(set []LedgerAccountCode, a LedgerAccountCode) bool {
	for _, s := range set {
		if s == a {
			return true
		}
	}
	return false
}
