-- 009_balances_per_asset.sql
-- 用户余额按资产聚合: user_balances 主键 (user_id, asset_symbol),
-- 不再按 chain 拆分 — 链仅是入金通道, 同资产跨链 1:1 计入同一 USD 余额.
-- supported_assets 复合外键随之移除 (资产符号仍以白名单为准).

BEGIN;

CREATE TABLE user_balances_v2 (
    user_id      UUID          NOT NULL,
    asset_symbol VARCHAR       NOT NULL,
    available    NUMERIC(78,0) NOT NULL DEFAULT 0,
    pending      NUMERIC(78,0) NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT pk_user_balances_v2 PRIMARY KEY (user_id, asset_symbol),
    CONSTRAINT fk_ub_user FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT chk_ub_nonnegative CHECK (available >= 0 AND pending >= 0)
);

-- 历史行按 (user, symbol) 跨链合并
INSERT INTO user_balances_v2 (user_id, asset_symbol, available, pending, updated_at)
SELECT user_id, asset_symbol, SUM(available), SUM(pending), MAX(updated_at)
  FROM user_balances
 GROUP BY user_id, asset_symbol;

DROP TABLE user_balances;
ALTER TABLE user_balances_v2 RENAME TO user_balances;
ALTER INDEX pk_user_balances_v2 RENAME TO pk_user_balances;

COMMIT;
