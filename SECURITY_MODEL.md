# VaultFlow threat model

Scope: `PanSHIgg/vaultflow` default branch. No `SECURITY_MODEL.md` / `THREAT_MODEL.md` was present, so this model is derived from the code and the user's answers. The user marked all major assets as important, included unauthenticated payers, malicious/compromised merchant API keys, compromised gateway/internal services, and on-chain/RPC/reorg/malicious-token attackers, and declared no additional exclusions. No separate infrastructure repository is included; deployment files inside this repo are covered under **Infra**.

## System overview

VaultFlow is a multi-chain stablecoin payment and custodial settlement system:

- **Web** — Next.js cashier and operations dashboard (`web/src`). The public cashier displays order payment details and lets a payer sign EIP-3009 `ReceiveWithAuthorization` typed data in their own wallet (`web/src/app/pay/[orderId]/cashier.tsx`, `web/src/lib/eip3009.ts`). Dashboard pages call merchant/admin APIs (`web/src/lib/api.ts`). Browser-held secrets include the demo merchant API key / admin JWT from `NEXT_PUBLIC_*` env vars (`web/src/lib/env.ts`) and an in-memory Ed25519 admin key used to sign whitelist/refund payloads (`web/src/lib/admin-key.tsx`).
- **Gateway** — Go modular monolith (`services/gateway-core`) exposing REST/SSE, order/payout/ledger services, indexer, sweeper, tx manager, webhook outbox, and dev simulator. It is the main internet-facing service and the only normal path from business requests to the signer (`services/gateway-core/cmd/server/main.go`, `internal/api/server.go`).
- **Signer** — Rust `vault-signer` (`services/vault-signer`) behind mandatory mTLS gRPC (`grpc/mtls.rs`, `proto/signer/v1/signer.proto`). It is the only component intended to hold hot-wallet DEKs and the only component that signs outbound transactions. It verifies locally decoded EIP-1559 transaction bytes against per-chain policy, whitelist leases, refund tickets, nonce slots, and a 24h payout limit (`engine.rs`, `policy.rs`, `crypto.rs`, `db.rs`, `kms.rs`).
- **Contracts** — Solidity contracts (`contracts/src`): `VaultFactory`, `DepositVault` implementation, `BatchSweeper`, and `EIP3009PayRouter`. CREATE2 vault addresses are derived per order; sweeps move exact finalized amounts to a cold wallet; the router relays payer-signed EIP-3009 authorizations into the derived vault (`VaultFactory.sol`, `DepositVault.sol`, `BatchSweeper.sol`, `EIP3009PayRouter.sol`).
- **Infra** — deployment/dev orchestration in this repo: `docker-compose.yml`, `deploy/Dockerfile.*`, `services/*/Dockerfile`, `scripts/deploy-and-seed.sh`, `scripts/gen-mtls-certs.sh`, migrations, and env/cert material generation. This is not a separate infrastructure repository.

## Trust boundaries

1. **Public Internet → Web/Gateway HTTP**
   - Gateway listens on `:8080` (`cmd/server/main.go`). Public routes are unauthenticated but IP rate-limited: `GET /v1/public/payments/{orderId}`, `GET /v1/public/payments/{orderId}/events`, `POST /v1/public/payments/{orderId}/eip3009` (`internal/api/server.go`).
   - Merchant routes require `X-Api-Key`, resolved by SHA-256 hash to a merchant (`internal/api/middleware.go`, `cmd/server/store.go`). Order, payout, whitelist and ledger-account handlers scope by the authenticated merchant. Known gaps (scan `scan-6923bfbc07044ad29bf256727581acb7`): `GET /v1/ledger/transactions` and `GET /v1/ledger/entries` apply no merchant filter (`internal/api/handlers_ledger.go`, `cmd/server/store.go`), and `source_order_id` on refunds/payouts is not checked against the caller's merchant (`internal/api/handlers_payouts.go`).
   - Admin routes require `Authorization: Bearer <JWT>` HS256 with a non-empty `sub` (`internal/api/middleware.go`). `ADMIN_JWT_SECRET` has a dev fallback value in `cmd/server/config.go`; production must override it.
   - Simulator routes are registered but gated by `ENABLE_DEV_SIMULATOR`; because the user declared no exclusions, treat simulator exposure as in-scope whenever it can be enabled or misconfigured in a deployed environment.

2. **Gateway → Signer gRPC**
   - `vault-signer` requires client certificates chaining to the configured client CA (`grpc/mtls.rs`). Gateway uses `MTLS_CLIENT_CERT/KEY/CA`; an admin-approver direct path is also described by the cert layout.
   - The signer does not trust request metadata: it decodes `unsigned_tx` locally, derives the policy from `to`/calldata, and checks chain config, contract addresses, cold-wallet allowlist, EIP-3009 auth nonce, ERC-20 transfer shape, payout whitelist/refund ticket, nonce slot, and daily limit (`engine.rs`, `policy.rs`).
   - Signer state is local SQLite: encrypted hot-wallet DEKs, withdrawal whitelist, lease proofs, intent slots, rolling usage, audit hash chain (`db.rs`, `audit.rs`). KEK is a local file in the dev path (`kms.rs`, `main.rs`); production KMS is an open question.

3. **Gateway → PostgreSQL / Redis / EVM RPC / Webhooks**
   - PostgreSQL is the business system of record: merchants, orders, deposits, payouts, ledger accounts/transactions/entries, outbound intents/attempts, webhook outbox (`migrations/001_init.sql`). Redis is optional for rate limits, idempotency, and SSE; in-process fallbacks exist.
   - EVM RPC endpoints are configured per chain and treated as untrusted inputs: indexer consumes logs/events, checks confirmations/reorgs, and tx manager broadcasts signed transactions (`internal/chain`, `internal/indexer`, `internal/txmanager`).
   - Webhook delivery uses merchant-configured `webhook_url` and `webhook_secret_enc`, HMAC-SHA256 signatures, HTTPS by default with an insecure-HTTP opt-out (`internal/webhook/webhook.go`). SSRF/private-network reachability depends on how webhook URLs are provisioned and validated.

4. **Chain → Gateway indexer / contracts**
   - Deposit matching, sweep finalization, payout settlement, and reorg rollback depend on chain logs/receipts and configured contract addresses (`internal/indexer`, `internal/sweeper`, `internal/txmanager`, `contracts/src`).

5. **Browser wallet/admin key → Gateway/Signer payloads**
   - Payers sign EIP-3009 authorizations in their wallet; gateway validates binding fields before queueing relay (`handlers_public.go`, `web/src/lib/eip3009.ts`).
   - Admin Ed25519 signatures authorize withdrawal-address leases and refund tickets. The web dashboard can hold the admin private key in browser memory (`web/src/lib/admin-key.tsx`); signatures are verified by `vault-signer`, not by the gateway.

## Assets and data sensitivity

All are treated as important per the user's answer.

- **Custodied funds**: ERC-20 stablecoins held by CREATE2 `DepositVault` proxies, hot wallets, and ultimately cold wallets; incorrect sweep/payout/refund authorization can cause direct loss.
- **Signing keys**: hot-wallet DEKs in `vault-signer`, KEK envelope key, mTLS private keys/certs, admin Ed25519 private key, merchant API keys, webhook secrets, admin JWT secret. Disclosure or misuse can authorize payouts or impersonate admins/merchants.
- **Financial integrity**: payment order state machine, double-entry ledger, payout/refund state, nonce cursors, outbound intents/attempts, signer whitelist/nonce slots/24h usage/audit chain. Corruption can cause double-spend, lost funds, false settlement, or stuck withdrawals.
- **Availability**: gateway API/SSE, signer availability, indexer/sweeper liveness, webhook delivery. Outages or deadlocks can trap funds or delay settlement.
- **Sensitive data**: order/customer payment metadata, merchant ledger views, admin ops views, audit logs, certificate/key files, DB dumps, env files (`deploy/gateway.env`, `deploy/signer-data`, `certs/`).

## Attacker profiles

- **Unauthenticated public payer/customer**: can call public cashier endpoints, submit EIP-3009 authorizations, and attempt to pay, replay, expire-boundary bypass, or manipulate public order state.
- **Malicious or compromised merchant API key**: can create orders, request payouts/refunds, manage withdrawal addresses, and read merchant-scoped ledger/order data; may attempt cross-tenant access, idempotency abuse, whitelist/refund-ticket misuse, or webhook abuse.
- **Compromised gateway-core or internal service**: can reach signer over mTLS, enqueue outbound intents, alter DB state, trigger sweeps/payouts/refunds, access secrets in env/volumes, or attack Postgres/Redis/RPC/webhook integrations.
- **On-chain attacker / malicious RPC / reorg / malicious token**: can submit deposits, exploit confirmation/reorg handling, malicious ERC-20 behavior, contract edge cases, or attempt to influence indexer/ledger/sweep decisions.
- **Privileged admin/ops actor or stolen admin credential**: can run reconciliation, inspect queues/quarantined vaults, unfreeze merchants, and enable/use simulator surfaces; admin JWT or Ed25519 misuse is high impact.

## Existing controls to preserve

- Merchant tenant scoping on order/payout/whitelist/ledger-account handlers, with the two gaps listed under trust boundary 1 (ledger transactions/entries, `source_order_id`); public endpoints expose only cashier projection.
- `Idempotency-Key` + request-body hash on non-GET endpoints, scoped by merchant/admin/public order (`internal/api/middleware.go`, `internal/api/idempotency.go`).
- Rate limiting for merchant/public/admin scopes (`internal/api/limiter.go`, `internal/api/server.go`). The public limit is keyed on the client-supplied `X-Forwarded-For` value (`internal/api/middleware.go`), so it only holds behind a proxy that overwrites that header; none is defined in this repo.
- Signer zero-trust pipeline: local EIP-1559 decode, four-policy classifier, per-chain allowlists, EIP-3009 auth nonce check, payout whitelist/refund-ticket verification, nonce-slot replay protection, daily payout breaker, and audit hash chain (`services/vault-signer/src`).
- Withdrawal whitelist uses offline admin Ed25519 leases with cooldown, expiry, revocation, and anti-replay of previously seen signatures (`engine.rs`, `db.rs`).
- Refund tickets are 127-byte admin-signed payloads checked field-by-field against decoded transaction fields (`engine.rs`, `policy.rs`, `proto/signer/v1/signer.proto`).
- Contracts use CREATE2 deterministic vaults, no `initialize()`, sweeper-only `DepositVault.sweep`, batch/gas caps, bounded returndata, zero-amount quarantine, cold-wallet timelock rotation, custody-token exclusion for rescue, and on-chain EIP-3009 auth-nonce/vault derivation (`contracts/src`).
- Gateway uses safe-head/required-confirmation indexing, reorg rollback, ledger posting templates, webhook HMAC signatures, and mTLS client credentials to the signer (`internal/indexer`, `internal/ledger`, `internal/webhook`, `internal/signerclient`).

## Known risk notes / open questions

- `cmd/server/config.go` defaults `ADMIN_JWT_SECRET` to `vaultflow-dev-admin-secret` when unset; confirm production always overrides it and rotates it.
- `web/src/lib/env.ts` exposes `NEXT_PUBLIC_DEMO_API_KEY` and `NEXT_PUBLIC_ADMIN_TOKEN` to the browser bundle; confirm these are never used with production credentials.
- Webhook SSRF safety depends on URL provisioning/validation not fully visible in the API surface reviewed; confirm merchant webhook URLs cannot target internal services unless intended.
- Production KMS/HSM, mTLS CA management, RPC quorum/required-confirmation values, and real chain/token configuration are deployment-dependent and not fully specified in the repo.
- Dev simulator (`ENABLE_DEV_SIMULATOR`) and mock/test paths are not excluded by the user; report them when reachable in a plausible deployed configuration, but do not treat disabled/test-only code as exploitable.
