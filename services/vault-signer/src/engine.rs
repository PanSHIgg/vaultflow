//! Orchestrating core — glues crypto / policy / db / audit / kms into the
//! decision pipeline behind the gRPC boundary.
//!
//! Guarantees enforced here (ARCHITECTURE §1.1–1.2, plan.md W2-3):
//!   * business metadata in requests is NEVER trusted — only locally decoded
//!     EIP-1559 bytes, signer-local SQLite state and admin Ed25519 proofs;
//!   * state mutation + audit append commit atomically in ONE transaction;
//!   * the dual-field same-nonce slot rules for PAYOUT first-sign / RBF replay
//!     / GAP_FILLER re-sign (no double limit deduction, no field overwrite).
//!
//! Borrow discipline: `rusqlite::Connection::transaction()` mutably borrows
//! `self.db.conn` for the transaction's lifetime, so all work executed inside
//! a transaction is expressed as free functions taking only the disjoint
//! fields they need (`chains`, `keys`, `admin_pubkey`) — never `&self`.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use alloy_primitives::{Address, B256, U256};
use ed25519_dalek::VerifyingKey;
use prost::Message;
use rand::rngs::OsRng;
use rusqlite::{Connection, Transaction};
use secrecy::{ExposeSecret, SecretVec};

use crate::audit::{self, actions, decisions};
use crate::crypto::{self, DecodedUnsignedTx};
use crate::db::{self, SignerDb, SlotKind};
use crate::grpc::signer_v1;
use crate::kms::{KmsProvider, LockedSecret};
use crate::policy::{self, ChainPolicyConfig, ClassifiedTx, PolicyError, PolicyOutcome};

/// `"0x"`-prefixed lowercase hex — canonical address string form (AGENTS §2.5).
pub fn fmt_addr(a: &Address) -> String {
    format!("0x{}", hex::encode(a.as_slice()))
}

/// Current unix seconds (signer-local clock — the ONLY trusted clock).
pub fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64
}

/// Outcome of register/revoke — `audit_seq` always populated.
#[derive(Debug)]
pub struct AckOutcome {
    pub accepted: bool,
    /// proto RejectCode numeric value (`0` on accept).
    pub reject_code: i32,
    pub reason: String,
    pub audit_seq: u64,
}

/// Outcome of a SignTransaction request.
#[derive(Debug)]
pub enum SignOutcome {
    Signed {
        raw_tx: Vec<u8>,
        tx_hash: [u8; 32],
        audit_seq: u64,
    },
    Rejected {
        reject_code: i32,
        reason: String,
        audit_seq: u64,
    },
}

/// Health snapshot for `GetSignerHealth`.
#[derive(Debug)]
pub struct HealthInfo {
    pub version: String,
    pub uptime_seconds: u64,
    pub clock_unix: i64,
    pub whitelist_entries: u32,
    pub audit_entries: u64,
    pub audit_tip_hash: [u8; 32],
    pub loaded_wallets: Vec<String>,
}

/// Outcome of a GenerateWallet request.
#[derive(Debug)]
pub struct WalletGenOutcome {
    pub accepted: bool,
    /// proto RejectCode numeric value (`0` on accept).
    pub reject_code: i32,
    pub reason: String,
    pub audit_seq: u64,
    /// `0x`-prefixed lowercase EVM address — populated only when accepted.
    pub address: String,
}

/// One row of the `ListWallets` projection (metadata only — never key material).
#[derive(Debug)]
pub struct HotWalletMeta {
    pub address: String,
    pub label: String,
    pub created_at_unix: i64,
    pub loaded: bool,
}

/// Outcome of an UpdateColdWallets request.
#[derive(Debug)]
pub struct ColdUpdateOutcome {
    pub accepted: bool,
    /// proto RejectCode numeric value (`0` on accept).
    pub reject_code: i32,
    pub reason: String,
    pub audit_seq: u64,
    /// Effective allowlist for the chain after the call (`0x` lowercase,
    /// ascending). Populated even on rejection with the pre-call list.
    pub active_cold_wallets: Vec<String>,
}

/// The whole signer decision engine. Held behind a single Mutex in the gRPC
/// layer; every public method is synchronous.
pub struct SignerCore {
    db: SignerDb,
    chains: BTreeMap<u64, ChainPolicyConfig>,
    admin_pubkey: VerifyingKey,
    /// KEK custody seam — retained so runtime provisioning (GenerateWallet)
    /// can envelope-encrypt freshly generated DEKs without restart.
    kms: Arc<dyn KmsProvider>,
    /// Loaded hot-wallet DEKs: address -> mlock'd, zeroize-on-drop secret.
    keys: BTreeMap<Address, LockedSecret>,
    started: Instant,
}

type Rejection = (i32, String);

fn rej(e: PolicyError) -> Rejection {
    (e.reject_code(), e.to_string())
}

fn rej_msg(e: PolicyError, detail: impl Into<String>) -> Rejection {
    (e.reject_code(), format!("{e}: {}", detail.into()))
}

fn parse_addr(s: &str) -> anyhow::Result<Address> {
    let raw = hex::decode(s.trim_start_matches("0x").trim_start_matches("0X"))?;
    if raw.len() != 20 {
        anyhow::bail!("address must be 20 bytes: {s}");
    }
    Ok(Address::from_slice(&raw))
}

fn verify_admin_sig(vk: &VerifyingKey, payload: &[u8], sig: &[u8]) -> Result<(), ()> {
    let s = ed25519_dalek::Signature::from_slice(sig).map_err(|_| ())?;
    vk.verify_strict(payload, &s).map_err(|_| ())
}

fn sign_with_locked(
    tx: &DecodedUnsignedTx,
    dek: &LockedSecret,
) -> anyhow::Result<(Vec<u8>, B256)> {
    let sk = k256::ecdsa::SigningKey::from_slice(dek.expose_secret())
        .map_err(|e| anyhow::anyhow!("invalid secp256k1 DEK: {e}"))?;
    crypto::sign_with_key(tx, &sk)
}

impl SignerCore {
    /// Decrypt every `encrypted_keys` envelope and verify each DEK really
    /// derives the recorded wallet address — a tampered envelope or wrong KEK
    /// is a hard boot failure, never a silent skip (zero-trust integrity).
    /// The same check covers GenerateWallet-provisioned rows: they are
    /// envelope-encrypted with the identical KMS path and satisfy it by
    /// construction.
    pub fn new(
        mut db: SignerDb,
        kms: Arc<dyn KmsProvider>,
        chains: Vec<ChainPolicyConfig>,
        admin_pubkey: [u8; 32],
    ) -> anyhow::Result<Self> {
        let mut keys = BTreeMap::new();
        for row in db::load_encrypted_keys(&db.conn)? {
            let addr = parse_addr(&row.wallet_address)?;
            let dek = kms.decrypt_dek(&row.dek_envelope).map_err(|e| {
                anyhow::anyhow!("decrypt DEK for {}: {e}", row.wallet_address)
            })?;
            let derived = crypto::address_from_dek(dek.expose_secret()).map_err(|e| {
                anyhow::anyhow!("derive address for {}: {e}", row.wallet_address)
            })?;
            if derived != addr {
                anyhow::bail!(
                    "encrypted_keys integrity failure: DEK for {} derives {} — refusing to boot",
                    row.wallet_address,
                    fmt_addr(&derived)
                );
            }
            keys.insert(addr, LockedSecret::new(dek));
            tracing::info!(wallet = %row.wallet_address, label = %row.label, "hot wallet DEK loaded");
        }
        // cold_wallets boot seeding — atomic so a mid-seed crash can't leave a
        // partial allowlist that would suppress re-seeding on the next boot:
        // config is the bootstrap source iff the table has NO rows for the
        // chain; once any row exists the DB is authoritative (owned by
        // UpdateColdWallets) and config is never consulted again.
        {
            let tx = db.conn.transaction()?;
            for c in &chains {
                db::seed_cold_wallets(&tx, c.chain_id, &c.allowed_cold_wallets, now_unix())?;
            }
            tx.commit()?;
        }
        let chains = chains.into_iter().map(|c| (c.chain_id, c)).collect();
        let admin_pubkey = VerifyingKey::from_bytes(&admin_pubkey)
            .map_err(|e| anyhow::anyhow!("invalid admin_approver_pubkey: {e}"))?;
        Ok(Self {
            db,
            chains,
            admin_pubkey,
            kms,
            keys,
            started: Instant::now(),
        })
    }

    pub fn db(&self) -> &SignerDb {
        &self.db
    }

    pub fn loaded_wallets(&self) -> Vec<Address> {
        self.keys.keys().copied().collect()
    }

    // ------------------------------------------------------------------
    // RegisterWithdrawalAddress
    // ------------------------------------------------------------------

    pub fn register_withdrawal_address(
        &mut self,
        req: &signer_v1::RegisterWithdrawalAddressRequest,
    ) -> AckOutcome {
        let digest = audit::request_digest(&req.encode_to_vec());
        let now = now_unix();
        let chains = &self.chains;
        let vk = &self.admin_pubkey;
        run_audited(
            &mut self.db.conn,
            actions::REGISTER_WITHDRAWAL,
            digest,
            now,
            |tx| register_inner(chains, vk, tx, req, now),
        )
    }

    // ------------------------------------------------------------------
    // RevokeWithdrawalAddress
    // ------------------------------------------------------------------

    pub fn revoke_withdrawal_address(
        &mut self,
        req: &signer_v1::RevokeWithdrawalAddressRequest,
    ) -> AckOutcome {
        let digest = audit::request_digest(&req.encode_to_vec());
        let now = now_unix();
        let chains = &self.chains;
        run_audited(
            &mut self.db.conn,
            actions::REVOKE_WITHDRAWAL,
            digest,
            now,
            |tx| revoke_inner(chains, tx, req, now),
        )
    }

    // ------------------------------------------------------------------
    // GenerateWallet
    // ------------------------------------------------------------------

    /// Provision a new RELAYER/PAYOUT hot wallet at runtime: a fresh
    /// secp256k1 secret is generated in-process, envelope-encrypted under the
    /// KEK (same KMS path as provisioning tooling), persisted to
    /// `encrypted_keys` — and, on commit, loaded into the in-memory key map so
    /// it can sign WITHOUT restart. Only the derived address is ever returned.
    pub fn generate_wallet(
        &mut self,
        req: &signer_v1::GenerateWalletRequest,
    ) -> WalletGenOutcome {
        let digest = audit::request_digest(&req.encode_to_vec());
        let now = now_unix();

        if req.role != "RELAYER" && req.role != "PAYOUT" {
            return self.gen_reject(
                digest,
                now,
                rej_msg(
                    PolicyError::Violation("role must be RELAYER or PAYOUT"),
                    format!("got {:?}", req.role),
                ),
            );
        }
        if !self.chains.contains_key(&req.chain_id) {
            return self.gen_reject(
                digest,
                now,
                rej_msg(
                    PolicyError::Violation("unconfigured chain_id"),
                    format!("chain_id {} not in signer config", req.chain_id),
                ),
            );
        }

        // Fresh secret inside secrecy types end-to-end; only the derived
        // address leaves this function.
        let dek = SecretVec::new(k256::SecretKey::random(&mut OsRng).to_bytes().to_vec());
        let addr = match crypto::address_from_dek(dek.expose_secret()) {
            Ok(a) => a,
            Err(e) => {
                return self.gen_reject(
                    digest,
                    now,
                    rej_msg(PolicyError::Internal, format!("derive address: {e}")),
                )
            }
        };
        let envelope = match self.kms.encrypt_dek(dek.expose_secret()) {
            Ok(e) => e,
            Err(e) => {
                return self.gen_reject(
                    digest,
                    now,
                    rej_msg(PolicyError::Internal, format!("envelope encrypt: {e}")),
                )
            }
        };
        let addr_hex = fmt_addr(&addr);
        let label = format!("role:{}:chain:{}", req.role, req.chain_id);

        // State change + audit append in ONE transaction (same discipline as
        // register/revoke): a failed commit leaves neither the row nor the
        // audit entry — and no in-memory key either (insert below is
        // accept-gated).
        let ack = run_audited(
            &mut self.db.conn,
            actions::GENERATE_WALLET,
            digest,
            now,
            |tx| {
                db::upsert_encrypted_key(tx, &addr_hex, &label, &envelope, now)
                    .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
                Ok(format!(
                    "generated role={} chain_id={} address={addr_hex}",
                    req.role, req.chain_id
                ))
            },
        );

        if ack.accepted {
            // LIVE pickup: join the in-memory key map immediately. The wallet
            // is GAP_FILLER-eligible at once (its self-transfers classify via
            // the hot-wallet set); SWEEP/RELAY `from` stays bound to
            // cfg.relayer_hot_wallet and PAYOUT to cfg.payout_hot_wallet
            // until an operator cutover, matching the frozen `from`-forcing
            // contract in policy::classify.
            self.keys.insert(addr, LockedSecret::new(dek));
            tracing::info!(wallet = %addr_hex, label = %label, "runtime-provisioned hot wallet DEK loaded");
        }
        WalletGenOutcome {
            accepted: ack.accepted,
            reject_code: ack.reject_code,
            reason: ack.reason,
            audit_seq: ack.audit_seq,
            address: if ack.accepted { addr_hex } else { String::new() },
        }
    }

    fn gen_reject(
        &mut self,
        digest: [u8; 32],
        now: i64,
        (code, reason): Rejection,
    ) -> WalletGenOutcome {
        let seq = self.audit_append(
            actions::GENERATE_WALLET,
            digest,
            now,
            decisions::REJECTED,
            &reason,
        );
        WalletGenOutcome {
            accepted: false,
            reject_code: code,
            reason,
            audit_seq: seq,
            address: String::new(),
        }
    }

    // ------------------------------------------------------------------
    // UpdateColdWallets
    // ------------------------------------------------------------------

    /// Runtime COLD allowlist mutation for a chain — idempotent add/remove,
    /// persisted to `cold_wallets`, audited, and effective on the very next
    /// SWEEP sign request (the DB is the sole authority after boot seeding).
    pub fn update_cold_wallets(
        &mut self,
        req: &signer_v1::UpdateColdWalletsRequest,
    ) -> ColdUpdateOutcome {
        let digest = audit::request_digest(&req.encode_to_vec());
        let now = now_unix();

        if !self.chains.contains_key(&req.chain_id) {
            return self.cold_reject(
                req.chain_id,
                digest,
                now,
                rej_msg(
                    PolicyError::Violation("unconfigured chain_id"),
                    format!("chain_id {} not in signer config", req.chain_id),
                ),
            );
        }

        // Every address must be exactly 20B; dedupe within each list.
        let mut add_set = BTreeSet::<[u8; 20]>::new();
        let mut rm_set = BTreeSet::<[u8; 20]>::new();
        let mut bad_len: Option<(&'static str, usize)> = None;
        for raw in &req.add {
            match <[u8; 20]>::try_from(raw.as_slice()) {
                Ok(a) => {
                    add_set.insert(a);
                }
                Err(_) => {
                    bad_len = Some(("add", raw.len()));
                    break;
                }
            }
        }
        if bad_len.is_none() {
            for raw in &req.remove {
                match <[u8; 20]>::try_from(raw.as_slice()) {
                    Ok(a) => {
                        rm_set.insert(a);
                    }
                    Err(_) => {
                        bad_len = Some(("remove", raw.len()));
                        break;
                    }
                }
            }
        }
        if let Some((which, len)) = bad_len {
            return self.cold_reject(
                req.chain_id,
                digest,
                now,
                rej_msg(
                    PolicyError::Violation("cold wallet address must be exactly 20B"),
                    format!("{which} entry is {len}B"),
                ),
            );
        }
        // An address present in BOTH lists is ambiguous operator input —
        // for a security whitelist, reject rather than guess a precedence.
        if let Some(a) = add_set.intersection(&rm_set).next() {
            return self.cold_reject(
                req.chain_id,
                digest,
                now,
                rej_msg(
                    PolicyError::Violation("address present in both add and remove"),
                    fmt_addr(&Address::from(*a)),
                ),
            );
        }

        let mut active: Vec<String> = Vec::new();
        let (n_add, n_rm) = (add_set.len(), rm_set.len());
        let ack = run_audited(
            &mut self.db.conn,
            actions::UPDATE_COLD_WALLETS,
            digest,
            now,
            |tx| {
                for a in &add_set {
                    db::cold_wallet_add(tx, req.chain_id, &Address::from(*a), now)
                        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
                }
                for a in &rm_set {
                    db::cold_wallet_remove(tx, req.chain_id, &Address::from(*a))
                        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
                }
                let list = db::cold_wallets_list(tx, req.chain_id)
                    .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
                active = list.iter().map(fmt_addr).collect();
                Ok(format!(
                    "chain_id={} add={n_add} remove={n_rm} active={}",
                    req.chain_id,
                    active.len()
                ))
            },
        );
        ColdUpdateOutcome {
            accepted: ack.accepted,
            reject_code: ack.reject_code,
            reason: ack.reason,
            audit_seq: ack.audit_seq,
            active_cold_wallets: active,
        }
    }

    /// Audited rejection for UpdateColdWallets; the response still carries the
    /// current effective allowlist so callers can reconcile state.
    fn cold_reject(
        &mut self,
        chain_id: u64,
        digest: [u8; 32],
        now: i64,
        (code, reason): Rejection,
    ) -> ColdUpdateOutcome {
        let seq = self.audit_append(
            actions::UPDATE_COLD_WALLETS,
            digest,
            now,
            decisions::REJECTED,
            &reason,
        );
        let active = db::cold_wallets_list(&self.db.conn, chain_id)
            .map(|v| v.iter().map(fmt_addr).collect())
            .unwrap_or_default();
        ColdUpdateOutcome {
            accepted: false,
            reject_code: code,
            reason,
            audit_seq: seq,
            active_cold_wallets: active,
        }
    }

    // ------------------------------------------------------------------
    // ListWallets / ListColdWallets — signer-cli 运维只读面
    // ------------------------------------------------------------------

    /// 热钱包元数据清单 — 只读查询, 不落审计链 (与 health() 同级).
    /// `loaded` 标记 DEK 当前在内存 map 中 — 可用于签名.
    pub fn list_wallets(&self) -> anyhow::Result<Vec<HotWalletMeta>> {
        let rows = db::list_wallet_key_meta(&self.db.conn)?;
        Ok(rows
            .into_iter()
            .map(|r| HotWalletMeta {
                loaded: parse_addr(&r.wallet_address)
                    .map(|a| self.keys.contains_key(&a))
                    .unwrap_or(false),
                address: r.wallet_address,
                label: r.label,
                created_at_unix: r.created_at,
            })
            .collect())
    }

    /// 指定链当前生效冷钱包白名单 — 只读, DB 权威 (含运行期增删).
    pub fn list_cold_wallets(&self, chain_id: u64) -> anyhow::Result<Vec<String>> {
        if !self.chains.contains_key(&chain_id) {
            anyhow::bail!("chain_id {chain_id} not in signer config");
        }
        let list = db::cold_wallets_list(&self.db.conn, chain_id)?;
        Ok(list.iter().map(fmt_addr).collect())
    }

    // ------------------------------------------------------------------
    // SignTransaction
    // ------------------------------------------------------------------

    pub fn sign_transaction(&mut self, req: &signer_v1::SignTransactionRequest) -> SignOutcome {
        let digest = audit::request_digest(&req.encode_to_vec());
        let now = now_unix();

        // Pre-decode outside the txn — pure function, no state.
        let decoded = match crypto::decode_unsigned_eip1559(&req.unsigned_tx) {
            Ok(t) => t,
            Err(e) => {
                return self.audit_only_reject(
                    actions::SIGN_TRANSACTION,
                    digest,
                    now,
                    rej_msg(PolicyError::MalformedTx, e.to_string()),
                );
            }
        };

        let chains = &self.chains;
        let keys = &self.keys;
        let vk = &self.admin_pubkey;
        let tx = match self.db.conn.transaction() {
            Ok(t) => t,
            Err(e) => return internal_outcome(format!("open txn: {e}")),
        };
        match sign_inner(chains, keys, vk, &tx, &decoded, req, now) {
            Ok((raw, hash, reason)) => {
                let seq = match audit::append_in_tx(
                    &tx,
                    now,
                    actions::SIGN_TRANSACTION,
                    &digest,
                    decisions::APPROVED,
                    &reason,
                ) {
                    Ok(s) => s,
                    Err(e) => {
                        drop(tx);
                        return internal_outcome(format!("audit append: {e}"));
                    }
                };
                if let Err(e) = tx.commit() {
                    return internal_outcome(format!("commit: {e}"));
                }
                SignOutcome::Signed {
                    raw_tx: raw,
                    tx_hash: hash.0,
                    audit_seq: seq,
                }
            }
            Err((code, reason)) => {
                drop(tx); // rollback any partial state
                self.audit_only_reject(actions::SIGN_TRANSACTION, digest, now, (code, reason))
            }
        }
    }

    // ------------------------------------------------------------------
    // VerifyAuditChain / GetSignerHealth
    // ------------------------------------------------------------------

    pub fn verify_audit_chain(
        &self,
        from_seq: u64,
        to_seq: u64,
    ) -> anyhow::Result<audit::ChainVerification> {
        audit::verify_chain(&self.db.conn, from_seq, to_seq)
    }

    pub fn health(&self) -> anyhow::Result<HealthInfo> {
        let (tip_seq, tip_hash) = audit::tip(&self.db.conn)?;
        Ok(HealthInfo {
            version: env!("CARGO_PKG_VERSION").to_string(),
            uptime_seconds: self.started.elapsed().as_secs(),
            clock_unix: now_unix(),
            whitelist_entries: db::whitelist_count(&self.db.conn)? as u32,
            audit_entries: tip_seq,
            audit_tip_hash: tip_hash,
            loaded_wallets: self.loaded_wallets().iter().map(fmt_addr).collect(),
        })
    }

    // ------------------------------------------------------------------
    // Reject helpers (own transaction so audit lands even when the
    // work transaction itself failed).
    // ------------------------------------------------------------------

    fn audit_append(
        &mut self,
        action: &str,
        digest: [u8; 32],
        now: i64,
        decision: &str,
        reason: &str,
    ) -> u64 {
        let tx = match self.db.conn.transaction() {
            Ok(t) => t,
            Err(_) => return 0,
        };
        let seq = audit::append_in_tx(&tx, now, action, &digest, decision, reason).unwrap_or(0);
        let _ = tx.commit();
        seq
    }

    fn audit_only_reject(
        &mut self,
        action: &str,
        digest: [u8; 32],
        now: i64,
        (code, reason): Rejection,
    ) -> SignOutcome {
        let seq = self.audit_append(action, digest, now, decisions::REJECTED, &reason);
        SignOutcome::Rejected {
            reject_code: code,
            reason,
            audit_seq: seq,
        }
    }
}

fn internal_outcome(reason: String) -> SignOutcome {
    SignOutcome::Rejected {
        reject_code: PolicyError::Internal.reject_code(),
        reason,
        audit_seq: 0,
    }
}

// ---------------------------------------------------------------------------
// Audited-work runner: state change + audit append commit atomically.
// ---------------------------------------------------------------------------

fn run_audited<F>(
    conn: &mut Connection,
    action: &str,
    digest: [u8; 32],
    now: i64,
    work: F,
) -> AckOutcome
where
    F: FnOnce(&Transaction<'_>) -> Result<String, Rejection>,
{
    let tx = match conn.transaction() {
        Ok(t) => t,
        Err(e) => {
            return AckOutcome {
                accepted: false,
                reject_code: PolicyError::Internal.reject_code(),
                reason: format!("open txn: {e}"),
                audit_seq: 0,
            }
        }
    };
    match work(&tx) {
        Ok(reason) => {
            let seq = match audit::append_in_tx(&tx, now, action, &digest, decisions::APPROVED, &reason)
            {
                Ok(s) => s,
                Err(e) => {
                    drop(tx);
                    return AckOutcome {
                        accepted: false,
                        reject_code: PolicyError::Internal.reject_code(),
                        reason: format!("audit append: {e}"),
                        audit_seq: 0,
                    };
                }
            };
            let _ = tx.commit();
            AckOutcome {
                accepted: true,
                reject_code: 0,
                reason,
                audit_seq: seq,
            }
        }
        Err((code, reason)) => {
            drop(tx); // rollback
            let seq = match conn.transaction() {
                Ok(tx2) => {
                    let s = audit::append_in_tx(
                        &tx2,
                        now,
                        action,
                        &digest,
                        decisions::REJECTED,
                        &reason,
                    )
                    .unwrap_or(0);
                    let _ = tx2.commit();
                    s
                }
                Err(_) => 0,
            };
            AckOutcome {
                accepted: false,
                reject_code: code,
                reason,
                audit_seq: seq,
            }
        }
    }
}

// ---------------------------------------------------------------------------
// RegisterWithdrawalAddress inner
// ---------------------------------------------------------------------------

fn register_inner(
    chains: &BTreeMap<u64, ChainPolicyConfig>,
    vk: &VerifyingKey,
    tx: &Transaction<'_>,
    req: &signer_v1::RegisterWithdrawalAddressRequest,
    now: i64,
) -> Result<String, Rejection> {
    let p = &req.signed_payload;
    if p.len() != 69 {
        return Err(rej_msg(
            PolicyError::InvalidAdminSignature,
            format!("payload must be exactly 69B, got {}", p.len()),
        ));
    }
    if &p[..17] != b"VAULTFLOW_ADDR_V1" {
        return Err(rej_msg(
            PolicyError::InvalidAdminSignature,
            "bad payload magic",
        ));
    }
    let chain_id = u64::from_be_bytes(p[17..25].try_into().unwrap());
    let merchant_uuid: [u8; 16] = p[25..41].try_into().unwrap();
    let recipient: [u8; 20] = p[41..61].try_into().unwrap();
    let valid_until = i64::from_be_bytes(p[61..69].try_into().unwrap());

    if !chains.contains_key(&chain_id) {
        return Err(rej_msg(
            PolicyError::Violation("unconfigured chain_id in lease payload"),
            format!("chain_id {chain_id} not in signer config"),
        ));
    }
    if req.admin_signature.len() != 64 {
        return Err(rej_msg(
            PolicyError::InvalidAdminSignature,
            format!("signature must be 64B, got {}", req.admin_signature.len()),
        ));
    }
    if verify_admin_sig(vk, p, &req.admin_signature).is_err() {
        return Err(rej(PolicyError::InvalidAdminSignature));
    }

    // Anti-replay (FIX-C / audit P0-5): a REVOKED entry must never be
    // resurrected by a previously-accepted signature — the untrusted gateway
    // holds every historical proof, so an emergency revoke would be voided
    // by re-presenting the pre-revoke lease. A never-seen signature IS a
    // fresh admin approval (deterministic Ed25519 over one key => a changed
    // signature means a changed payload, i.e. new lease terms) and may
    // resurrect; whitelist_register then re-anchors first_seen_at so the
    // revived lease re-serves its cooldown (§1.2 新审批新冷却).
    if let Some(status) = db::whitelist_status(tx, chain_id, &merchant_uuid, &recipient)
        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?
    {
        if status == "REVOKED"
            && db::whitelist_proof_seen(
                tx,
                chain_id,
                &merchant_uuid,
                &recipient,
                &req.admin_signature,
            )
            .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?
        {
            return Err(rej_msg(
                PolicyError::WhitelistRevoked,
                "revoked entry cannot be resurrected by a previously-seen lease signature",
            ));
        }
    }

    db::whitelist_register(
        tx,
        chain_id,
        &merchant_uuid,
        &recipient,
        valid_until,
        &req.admin_signature,
        now,
    )
    .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;

    Ok(format!(
        "registered chain_id={chain_id} merchant={} recipient={} valid_until={valid_until}",
        hex::encode(merchant_uuid),
        fmt_addr(&Address::from(recipient))
    ))
}

// ---------------------------------------------------------------------------
// RevokeWithdrawalAddress inner
// ---------------------------------------------------------------------------

fn revoke_inner(
    chains: &BTreeMap<u64, ChainPolicyConfig>,
    tx: &Transaction<'_>,
    req: &signer_v1::RevokeWithdrawalAddressRequest,
    now: i64,
) -> Result<String, Rejection> {
    if req.merchant_uuid.len() != 16 || req.recipient_address.len() != 20 {
        return Err(rej_msg(
            PolicyError::Violation("bad field length"),
            format!(
                "merchant_uuid={}B recipient={}B",
                req.merchant_uuid.len(),
                req.recipient_address.len()
            ),
        ));
    }
    if !chains.contains_key(&req.chain_id) {
        return Err(rej_msg(
            PolicyError::Violation("unconfigured chain_id"),
            format!("chain_id {} not in signer config", req.chain_id),
        ));
    }
    let merchant_uuid: [u8; 16] = req.merchant_uuid[..16].try_into().unwrap();
    let recipient: [u8; 20] = req.recipient_address[..20].try_into().unwrap();

    let flipped = db::whitelist_revoke(tx, req.chain_id, &merchant_uuid, &recipient, now)
        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
    if flipped == 0 {
        // Already-revoked rows are idempotent accepts; totally-unknown
        // recipients are operator errors worth flagging.
        let rows = db::whitelist_for_recipient(tx, req.chain_id, &Address::from(recipient))
            .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
        let ours = rows.iter().any(|r| r.merchant_uuid == merchant_uuid);
        if !ours {
            return Err(rej(PolicyError::WhitelistNotFound));
        }
        return Ok(format!(
            "already revoked chain_id={} merchant={} recipient={} note={}",
            req.chain_id,
            hex::encode(merchant_uuid),
            fmt_addr(&Address::from(recipient)),
            req.admin_note
        ));
    }
    Ok(format!(
        "revoked chain_id={} merchant={} recipient={} note={}",
        req.chain_id,
        hex::encode(merchant_uuid),
        fmt_addr(&Address::from(recipient)),
        req.admin_note
    ))
}

// ---------------------------------------------------------------------------
// SignTransaction inner
// ---------------------------------------------------------------------------

#[derive(Debug, PartialEq, Eq)]
enum PayoutAuth {
    /// Same-nonce RBF / dropped-rebroadcast / deep-reorg replay — slot fields
    /// matched byte-for-byte; NO additional limit deduction.
    Replay,
    /// First sign for this (chain, from, nonce) — proof verified, slot +
    /// usage row written.
    Fresh,
}

fn sign_inner(
    chains: &BTreeMap<u64, ChainPolicyConfig>,
    keys: &BTreeMap<Address, LockedSecret>,
    vk: &VerifyingKey,
    tx: &Transaction<'_>,
    decoded: &DecodedUnsignedTx,
    req: &signer_v1::SignTransactionRequest,
    now: i64,
) -> Result<(Vec<u8>, B256, String), Rejection> {
    let cfg = chains.get(&req.chain_id).ok_or_else(|| {
        rej_msg(
            PolicyError::UnknownPolicy,
            format!("unconfigured chain_id {}", req.chain_id),
        )
    })?;
    if decoded.chain_id != req.chain_id {
        return Err(rej_msg(
            PolicyError::Violation("chain_id mismatch"),
            format!("decoded {} vs request {}", decoded.chain_id, req.chain_id),
        ));
    }

    // GAP_FILLER eligibility = every DEK loaded in memory (including
    // runtime-provisioned GenerateWallet keys — live without restart) ∪ the
    // configured pair, so a configured-but-unloaded wallet still classifies
    // and fails cleanly at the "no loaded DEK" check below.
    let mut hot_wallets: BTreeSet<Address> = keys.keys().copied().collect();
    hot_wallets.insert(cfg.relayer_hot_wallet);
    hot_wallets.insert(cfg.payout_hot_wallet);

    let c = policy::classify(cfg, decoded, &hot_wallets).map_err(rej)?;

    // policy_hint is advisory-only but must agree with our own conclusion.
    if req.policy_hint != 0 && req.policy_hint != c.policy.proto_code() {
        return Err(rej_msg(
            PolicyError::Violation("policy_hint mismatch"),
            format!("hint={} derived={}", req.policy_hint, c.policy.proto_code()),
        ));
    }

    // SWEEP cold-wallet membership — checked HERE (not in policy::classify)
    // against the DB-authoritative `cold_wallets` table inside this very
    // transaction, so UpdateColdWallets takes effect on the next sign call
    // and the check can never observe a half-applied update.
    if let PolicyOutcome::Sweep {
        target_cold_wallet, ..
    } = &c.outcome
    {
        let allowed = db::cold_wallet_allowed(tx, cfg.chain_id, target_cold_wallet)
            .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
        if !allowed {
            return Err(rej(PolicyError::ColdWalletNotAllowed));
        }
    }

    let auth_note: String = match &c.outcome {
        PolicyOutcome::Payout {
            token,
            recipient,
            amount,
        } => match authorize_payout(
            tx, cfg, &c, decoded, req, vk, *token, *recipient, *amount, now,
        )? {
            PayoutAuth::Replay => "slot=replay".to_string(),
            PayoutAuth::Fresh => "slot=fresh".to_string(),
        },
        PolicyOutcome::GapFiller => authorize_gap_filler(tx, cfg, &c, decoded, now)?.to_string(),
        _ => "stateless".to_string(),
    };

    let dek = keys.get(&c.from).ok_or_else(|| {
        rej_msg(
            PolicyError::Internal,
            format!("no loaded DEK for hot wallet {}", fmt_addr(&c.from)),
        )
    })?;
    let (raw, hash) =
        sign_with_locked(decoded, dek).map_err(|e| rej_msg(PolicyError::Internal, format!("{e}")))?;

    Ok((
        raw,
        hash,
        format!(
            "policy={:?} from={} nonce={} {} tx_hash={:#x}",
            c.policy,
            fmt_addr(&c.from),
            decoded.nonce,
            auth_note,
            hash
        ),
    ))
}

/// First-sign authorization for PAYOUT: limit check + (whitelist lease OR
/// refund ticket) + slot/usage write — all inside `tx`.
#[allow(clippy::too_many_arguments)]
fn authorize_payout(
    tx: &Transaction<'_>,
    cfg: &ChainPolicyConfig,
    c: &ClassifiedTx,
    decoded: &DecodedUnsignedTx,
    req: &signer_v1::SignTransactionRequest,
    vk: &VerifyingKey,
    token: Address,
    recipient: Address,
    amount: U256,
    now: i64,
) -> Result<PayoutAuth, Rejection> {
    let from_hex = fmt_addr(&c.from);
    let slot = db::slot_get(tx, cfg.chain_id, &from_hex, decoded.nonce)
        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;

    if let Some(slot) = slot {
        return match slot.kind {
            SlotKind::GapFiller => Err(rej_msg(
                PolicyError::NonceSlotConflict,
                "nonce already consumed by GAP_FILLER slot",
            )),
            SlotKind::Payout => {
                // Byte-exact replay check of the immutable business fields.
                let req_uuid: Option<[u8; 16]> = (req.payout_uuid.len() == 16)
                    .then(|| req.payout_uuid[..16].try_into().unwrap());
                let fields_match = slot.token == Some(token)
                    && slot.recipient == Some(recipient)
                    && slot.amount == Some(amount)
                    && slot.payout_uuid == req_uuid
                    && slot.retry_seq == Some(req.retry_seq);
                if fields_match {
                    Ok(PayoutAuth::Replay)
                } else {
                    Err(rej_msg(
                        PolicyError::NonceSlotConflict,
                        format!(
                            "(token,recipient,amount,payout_uuid,retry_seq) != slot for nonce {}",
                            decoded.nonce
                        ),
                    ))
                }
            }
        };
    }

    // ---- First sign for this nonce -------------------------------------
    if req.payout_uuid.len() != 16 {
        return Err(rej_msg(
            PolicyError::Violation("payout_uuid must be 16B"),
            format!("got {}B", req.payout_uuid.len()),
        ));
    }
    let payout_uuid: [u8; 16] = req.payout_uuid[..16].try_into().unwrap();

    // (payout_uuid, retry_seq) must not already bind a different nonce.
    if let Some((w, n)) = db::slot_owner_of_payout(tx, cfg.chain_id, &payout_uuid, req.retry_seq)
        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?
    {
        return Err(rej_msg(
            PolicyError::NonceSlotConflict,
            format!("(payout_uuid,retry_seq) already bound to {w} nonce {n}"),
        ));
    }

    // 24h sliding-window circuit breaker — checked BEFORE proofs so a tripped
    // breaker fails fast and never consults the whitelist.
    if let Some(limit) = cfg.daily_payout_limit_atomic {
        let used = db::usage_sum_since(tx, cfg.chain_id, now - 86_400)
            .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
        if used.saturating_add(amount) > limit {
            return Err(rej_msg(
                PolicyError::DailyLimitExceeded,
                format!("used {used} + {amount} > limit {limit}"),
            ));
        }
    }

    // Proof path: refund ticket wins over whitelist when present.
    if let Some(ticket) = &req.refund_ticket {
        verify_refund_ticket(
            cfg, req, ticket, vk, token, recipient, amount, now,
        )?;
    } else {
        check_whitelist_lease(tx, cfg, &recipient, now)?;
    }

    db::slot_insert_payout(
        tx,
        cfg.chain_id,
        &from_hex,
        decoded.nonce,
        &payout_uuid,
        req.retry_seq,
        &token,
        &recipient,
        &amount,
        now,
    )
    .map_err(|e| rej_msg(PolicyError::NonceSlotConflict, e.to_string()))?;
    db::usage_insert(tx, cfg.chain_id, &token, &amount, now)
        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
    Ok(PayoutAuth::Fresh)
}

/// 127B REFUND_V1 payload field view.
struct RefundFields {
    chain_id: u64,
    token: [u8; 20],
    to: [u8; 20],
    exact_amount: U256,
    payout_uuid: [u8; 16],
    retry_seq: u32,
    expiry: i64,
}

fn parse_refund_payload(p: &[u8]) -> Result<RefundFields, Rejection> {
    if p.len() != 127 {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            format!("payload must be exactly 127B, got {}", p.len()),
        ));
    }
    if &p[..19] != b"VAULTFLOW_REFUND_V1" {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            "bad payload magic",
        ));
    }
    Ok(RefundFields {
        chain_id: u64::from_be_bytes(p[19..27].try_into().unwrap()),
        token: p[27..47].try_into().unwrap(),
        to: p[47..67].try_into().unwrap(),
        exact_amount: U256::from_be_bytes::<32>(p[67..99].try_into().unwrap()),
        payout_uuid: p[99..115].try_into().unwrap(),
        retry_seq: u32::from_be_bytes(p[115..119].try_into().unwrap()),
        expiry: i64::from_be_bytes(p[119..127].try_into().unwrap()),
    })
}

#[allow(clippy::too_many_arguments)]
fn verify_refund_ticket(
    cfg: &ChainPolicyConfig,
    req: &signer_v1::SignTransactionRequest,
    ticket: &signer_v1::RefundTicket,
    vk: &VerifyingKey,
    token: Address,
    recipient: Address,
    amount: U256,
    now: i64,
) -> Result<(), Rejection> {
    let f = parse_refund_payload(&ticket.signed_payload)?;
    // Field-exact checks against the locally-decoded transfer params
    // (chain_id, token, recipient, amount) plus request binding
    // (payout_uuid, retry_seq) and lease validity.
    if f.chain_id != cfg.chain_id {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            format!("ticket chain_id {} != {}", f.chain_id, cfg.chain_id),
        ));
    }
    if Address::from_slice(&f.token) != token {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            "ticket token != tx.to",
        ));
    }
    if Address::from_slice(&f.to) != recipient {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            "ticket to != decoded recipient",
        ));
    }
    if f.exact_amount != amount {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            "ticket amount != decoded amount",
        ));
    }
    if f.payout_uuid[..] != req.payout_uuid[..16] {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            "ticket payout_uuid != request payout_uuid",
        ));
    }
    if f.retry_seq != req.retry_seq {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            "ticket retry_seq != request retry_seq",
        ));
    }
    if now >= f.expiry {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            format!("ticket expired at {}", f.expiry),
        ));
    }
    if ticket.admin_signature.len() != 64 || verify_admin_sig(vk, &ticket.signed_payload, &ticket.admin_signature).is_err() {
        return Err(rej_msg(
            PolicyError::InvalidRefundTicket,
            "admin Ed25519 signature verification failed",
        ));
    }
    Ok(())
}

fn check_whitelist_lease(
    tx: &Transaction<'_>,
    cfg: &ChainPolicyConfig,
    recipient: &Address,
    now: i64,
) -> Result<(), Rejection> {
    // NOTE (FIX-C report): rows are keyed (chain_id, merchant_uuid, recipient)
    // but SignTransactionRequest carries NO merchant identity field, so the
    // lease check cannot bind the payout to a specific merchant — any ACTIVE
    // lease for (chain, recipient) authorizes the transfer regardless of
    // which merchant holds it. Adding a merchant_uuid field is a wire change
    // deferred to Wave-2 contract review (signer.proto is wire-frozen).
    let rows = db::whitelist_for_recipient(tx, cfg.chain_id, recipient)
        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
    if rows.is_empty() {
        return Err(rej(PolicyError::WhitelistNotFound));
    }
    let mut cooldown_blocked = false;
    let mut expired_seen = false;
    for r in &rows {
        if r.status == "REVOKED" {
            continue;
        }
        if now >= r.valid_until {
            expired_seen = true;
            continue;
        }
        if now < r.first_seen_at + cfg.cooldown_secs as i64 {
            cooldown_blocked = true;
            continue;
        }
        // Fully usable ACTIVE lease.
        return Ok(());
    }
    if cooldown_blocked {
        return Err(rej(PolicyError::WhitelistCooldown));
    }
    if expired_seen {
        return Err(rej(PolicyError::WhitelistExpired));
    }
    Err(rej(PolicyError::WhitelistRevoked)) // every row REVOKED
}

/// GAP_FILLER slot transition: mark `filler_signed` on an existing PAYOUT
/// slot (never overwriting business fields), allow idempotent re-signs, or
/// create a filler-only slot for a bare nonce hole.
fn authorize_gap_filler(
    tx: &Transaction<'_>,
    cfg: &ChainPolicyConfig,
    c: &ClassifiedTx,
    decoded: &DecodedUnsignedTx,
    now: i64,
) -> Result<&'static str, Rejection> {
    let from_hex = fmt_addr(&c.from);
    match db::slot_get(tx, cfg.chain_id, &from_hex, decoded.nonce)
        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?
    {
        Some(slot) => match slot.kind {
            SlotKind::Payout => {
                if !slot.filler_signed {
                    db::slot_mark_filler_signed(tx, cfg.chain_id, &from_hex, decoded.nonce)
                        .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
                    Ok("slot=filler_marked")
                } else {
                    Ok("slot=filler_replay")
                }
            }
            SlotKind::GapFiller => Ok("slot=filler_replay"),
        },
        None => {
            db::slot_insert_gap_filler(tx, cfg.chain_id, &from_hex, decoded.nonce, now)
                .map_err(|e| rej_msg(PolicyError::Internal, e.to_string()))?;
            Ok("slot=filler_created")
        }
    }
}
