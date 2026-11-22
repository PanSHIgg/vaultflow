-- ============================================================================
-- 008_exchange.sql — 交易所托管模型改造 (exchange-custody remodel)
--
-- VaultFlow 由商户支付网关重构为币安式交易所托管钱包后端:
--   * 商户订单模型整体删除 (merchants / payment_orders / merchant_payouts /
--     merchant_withdrawal_addresses / open_deposit_addresses / deposit_addresses /
--     webhook_outbox / ledger_* 复式账本) — 每个用户每链一个常驻 CREATE2
--     充值地址, 确认数达标后直接贷记 user_balances, 无订单/发票状态机.
--   * 新增 users / user_deposit_addresses / user_balances / platform_wallets.
--   * onchain_deposits 保留并改造: 删 matched_order_id, 增 user_id 与
--     created_at (keyset 分页 + API 契约), 状态机扩为
--     DETECTED→FINALIZED→CREDITED→SWEPT (+ORPHANED_SHALLOW/ORPHANED_DEEP),
--     分类缩为 USER_DEPOSIT/TREASURY_REBALANCE/TOPUP/UNMATCHED.
--   * sweep_batch_items.fk_items_vault 重指向 user_deposit_addresses.
--   * outbound_tx_intents 仅保留 SWEEP_BATCH / NONCE_GAP_FILLER 两类意图.
--   * supported_assets 增补链展示/确认数元数据列 (min_deposit 007 已加, 不重复).
--
-- 保留: chain_sync_cursors, chain_blocks, onchain_deposits, wallet_nonce_cursors,
--   outbound_tx_intents/attempts, sweep_batches/items, supported_assets,
--   signer_audit_anchor.
--
-- Target: PostgreSQL 16. Forward-only — 既有迁移冻结不可改.
-- ============================================================================

BEGIN;

-- ============================================================================
-- 1. 切断商户模型外键依赖 (先拆 FK/索引/列, 才能 DROP 被引用表)
-- ============================================================================

-- onchain_deposits.matched_order_id -> payment_orders(id): 订单模型删除.
DROP INDEX IF EXISTS idx_onchain_deposits_order;
ALTER TABLE onchain_deposits DROP CONSTRAINT fk_deposits_order;
ALTER TABLE onchain_deposits DROP COLUMN matched_order_id;

-- sweep_batch_items.deposit_address_id -> deposit_addresses(id):
-- 先卸 FK, 新表 user_deposit_addresses 建好后在 §4 重指.
ALTER TABLE sweep_batch_items DROP CONSTRAINT fk_items_vault;

-- outbound_tx_intents: 商户付款/EIP-3009 relay 意图类型随商户模型删除.
ALTER TABLE outbound_tx_intents DROP CONSTRAINT chk_intents_type;
ALTER TABLE outbound_tx_intents ADD CONSTRAINT chk_intents_type
    CHECK (intent_type IN ('SWEEP_BATCH', 'NONCE_GAP_FILLER'));

-- 003 的存活意图幂等索引原先覆盖 MERCHANT_PAYOUT/EIP3009_RELAY, 重建为仅
-- SWEEP_BATCH (NONCE_GAP_FILLER 本就豁免 — 同一引用可合法有多个填洞意图).
DROP INDEX uq_intents_live_type_ref;
CREATE UNIQUE INDEX uq_intents_live_type_ref
    ON outbound_tx_intents (intent_type, reference_id)
    WHERE intent_type = 'SWEEP_BATCH'
      AND status IN ('QUEUED', 'IN_FLIGHT', 'INCLUDED_PENDING_SAFE',
                     'CONFIRMED', 'NONCE_STOLEN_HALT');

-- onchain_deposits 状态机与分类 CHECK (新枚举; user_id/created_at 在 §4 补列).
ALTER TABLE onchain_deposits DROP CONSTRAINT chk_deposits_status;
ALTER TABLE onchain_deposits ADD CONSTRAINT chk_deposits_status
    CHECK (status IN ('DETECTED', 'FINALIZED', 'CREDITED', 'SWEPT',
                      'ORPHANED_SHALLOW', 'ORPHANED_DEEP'));
ALTER TABLE onchain_deposits DROP CONSTRAINT chk_deposits_category;
ALTER TABLE onchain_deposits ADD CONSTRAINT chk_deposits_category
    CHECK (deposit_category IN ('USER_DEPOSIT', 'TREASURY_REBALANCE',
                                'TOPUP', 'UNMATCHED'));

-- ============================================================================
-- 2. 删除商户订单模型表 (顺序按 FK 依赖: 被引用方最后删)
--    ledger_* 复式账本随托管余额模型 user_balances 一并删除.
-- ============================================================================
DROP TABLE ledger_entries;
DROP TABLE ledger_transactions;
DROP TABLE ledger_accounts;
-- 001 的复式记账不变量触发器随表删除; 两个 PL/pgSQL 函数为死代码, 一并清理.
DROP FUNCTION IF EXISTS fn_ledger_immutable();
DROP FUNCTION IF EXISTS fn_ledger_check_txn_balanced();

DROP TABLE merchant_payouts;                 -- refs merchants/payment_orders/supported_assets
DROP TABLE merchant_withdrawal_addresses;    -- refs merchants
DROP TABLE webhook_outbox;                   -- refs merchants
DROP TABLE IF EXISTS open_deposit_addresses; -- 006-era 商户开放充值地址 (customer_ref 工件随表消失)
DROP TABLE payment_orders;                   -- refs merchants/deposit_addresses
DROP TABLE merchants;
DROP TABLE deposit_addresses;                -- 订单级 vault 地址, 由 user_deposit_addresses 取代

-- ============================================================================
-- 3. 交易所托管模型新表
-- ============================================================================

-- USERS — 终端用户账户. email 应用侧统一小写; 唯一性由 lower(email) 函数索引
-- 保证 (不依赖列级 UNIQUE, 杜绝大小写绕过).
CREATE TABLE users (
    id            UUID        NOT NULL DEFAULT gen_random_uuid(),
    email         VARCHAR     NOT NULL,
    password_hash VARCHAR     NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pk_users PRIMARY KEY (id)
);
CREATE UNIQUE INDEX uq_users_email_lower ON users (lower(email));

-- USER_DEPOSIT_ADDRESSES — 每用户每链一个常驻 CREATE2 充值地址.
-- 列语义镜像 001 deposit_addresses (address/create2_salt/is_deployed/
-- is_quarantined), 加 user_id 维度取代 merchant/order 绑定.
CREATE TABLE user_deposit_addresses (
    id             UUID        NOT NULL DEFAULT gen_random_uuid(),
    user_id        UUID        NOT NULL,
    chain_id       BIGINT      NOT NULL,
    address        VARCHAR     NOT NULL,   -- CREATE2 推导地址 (0x 小写 hex)
    create2_salt   BYTEA       NOT NULL,   -- 32B salt = keccak256(chainId, userId)
    is_deployed    BOOLEAN     NOT NULL DEFAULT false,
    is_quarantined BOOLEAN     NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pk_user_deposit_addresses PRIMARY KEY (id),
    CONSTRAINT fk_uda_user FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT uq_uda_user_chain UNIQUE (user_id, chain_id),
    CONSTRAINT uq_uda_chain_addr UNIQUE (chain_id, address),
    CONSTRAINT uq_uda_chain_salt UNIQUE (chain_id, create2_salt)
);

-- USER_BALANCES — 用户资产余额 (原子单位 NUMERIC(78,0), 全非负).
CREATE TABLE user_balances (
    user_id      UUID          NOT NULL,
    chain_id     BIGINT        NOT NULL,
    asset_symbol VARCHAR       NOT NULL,
    available    NUMERIC(78,0) NOT NULL DEFAULT 0,
    pending      NUMERIC(78,0) NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT pk_user_balances PRIMARY KEY (user_id, chain_id, asset_symbol),
    CONSTRAINT fk_ub_user FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT fk_ub_asset FOREIGN KEY (chain_id, asset_symbol)
        REFERENCES supported_assets (chain_id, asset_symbol),
    CONSTRAINT chk_ub_nonnegative CHECK (available >= 0 AND pending >= 0)
);

-- PLATFORM_WALLETS — 平台级热/冷钱包, admin CRUD 管理
-- (RELAYER=归集签名热钱包, PAYOUT=出款热钱包, COLD=冷金库).
CREATE TABLE platform_wallets (
    id         UUID        NOT NULL DEFAULT gen_random_uuid(),
    chain_id   BIGINT      NOT NULL,
    role       VARCHAR     NOT NULL,
    address    VARCHAR     NOT NULL,
    status     VARCHAR     NOT NULL DEFAULT 'ACTIVE',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pk_platform_wallets PRIMARY KEY (id),
    CONSTRAINT chk_pw_role CHECK (role IN ('RELAYER', 'PAYOUT', 'COLD')),
    CONSTRAINT chk_pw_status CHECK (status IN ('ACTIVE', 'DISABLED')),
    CONSTRAINT uq_pw_chain_role_addr UNIQUE (chain_id, role, address)
);

-- ============================================================================
-- 4. 依赖新表的回补 ALTER
-- ============================================================================

-- onchain_deposits: user_id (充值归属用户, indexer 由 to_address 反查
-- user_deposit_addresses 填充; TREASURY_*/TOPUP/UNMATCHED 为 NULL) 与
-- created_at (充值记录 API 的 keyset 分页键).
ALTER TABLE onchain_deposits
    ADD COLUMN user_id UUID,
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE onchain_deposits
    ADD CONSTRAINT fk_deposits_user FOREIGN KEY (user_id) REFERENCES users (id);

-- 活跃日志部分唯一索引重建: CREDITED/SWPT 行的日志身份仍须唯一
-- (旧定义仅覆盖 DETECTED/FINALIZED, 入账后同一 (chain,tx,log) 可重复插入).
DROP INDEX uq_onchain_deposits_active_log;
CREATE UNIQUE INDEX uq_onchain_deposits_active_log
    ON onchain_deposits (chain_id, tx_hash, log_index)
    WHERE status IN ('DETECTED', 'FINALIZED', 'CREDITED', 'SWEPT');

-- 用户充值历史 keyset 分页 (created_at DESC, id DESC) 与 credit worker 扫描路径.
CREATE INDEX idx_onchain_deposits_user
    ON onchain_deposits (user_id, created_at DESC, id DESC);
CREATE INDEX idx_onchain_deposits_finalized
    ON onchain_deposits (chain_id, created_at) WHERE status = 'FINALIZED';

-- sweep_batch_items.deposit_address_id 重指向 user_deposit_addresses(id).
ALTER TABLE sweep_batch_items
    ADD CONSTRAINT fk_items_vault FOREIGN KEY (deposit_address_id)
        REFERENCES user_deposit_addresses (id);

-- supported_assets: 链展示与确认数元数据 (API /v1/assets 契约).
ALTER TABLE supported_assets
    ADD COLUMN chain_name             VARCHAR NOT NULL DEFAULT '',
    ADD COLUMN block_time_seconds     INT     NOT NULL DEFAULT 12,
    ADD COLUMN required_confirmations INT     NOT NULL DEFAULT 1,
    ADD COLUMN token_explorer_url     VARCHAR NOT NULL DEFAULT '',
    ADD CONSTRAINT chk_supported_assets_block_time CHECK (block_time_seconds > 0),
    ADD CONSTRAINT chk_supported_assets_confirmations CHECK (required_confirmations >= 0);

COMMIT;
