//! Physically-isolated embedded SQLite state — reachable ONLY by this signer
//! process (gateway-core has no access; zero-trust boundary, §1.1).
//!
//! Frozen table set:
//!   encrypted_keys          — AES-256-GCM envelope-encrypted DEKs
//!   withdrawal_whitelist    — leased entries (first_seen_at, valid_until, status)
//!   whitelist_lease_proofs  — every lease signature ever accepted per entry
//!                             (FIX-C: replayed proofs can't resurrect REVOKED rows)
//!   signer_intent_slots     — dual-field same-nonce idempotent auth slots
//!   rolling_24h_usage       — sliding-window payout counter (circuit breaker)
//!   cold_wallets            — runtime-administered COLD allowlist for SWEEP
//!   audit_logs              — SHA-256 hash chain (see audit.rs)
//!
//! Concurrency model: a single `rusqlite::Connection` behind a `Mutex` in the
//! service layer; every mutation that also appends an audit record happens
//! inside ONE SQLite transaction (IMMEDIATE) so state and audit commit or
//! roll back together.

use std::path::Path;

use alloy_primitives::{Address, U256};
use rusqlite::{params, Connection, OptionalExtension, Transaction};

/// Owned handle to the signer state DB.
pub struct SignerDb {
    pub conn: Connection,
}

/// Frozen DDL for the five tables — applied idempotently on open.
pub const SCHEMA_SQL: &str = r#"
CREATE TABLE IF NOT EXISTS encrypted_keys (
    wallet_address TEXT PRIMARY KEY,          -- '0x'-prefixed lowercase EOA
    label          TEXT NOT NULL DEFAULT '',
    dek_envelope   BLOB NOT NULL,             -- nonce(12B) || ct || GCM tag(16B)
    created_at     INTEGER NOT NULL           -- unix seconds
);

CREATE TABLE IF NOT EXISTS withdrawal_whitelist (
    chain_id          INTEGER NOT NULL,
    merchant_uuid     BLOB NOT NULL,          -- UUID raw16
    recipient_address BLOB NOT NULL,          -- EVM raw20
    first_seen_at     INTEGER NOT NULL,       -- unix seconds — cooldown anchor
    valid_until       INTEGER NOT NULL,       -- unix seconds — lease upper bound
    status            TEXT NOT NULL CHECK (status IN ('ACTIVE','REVOKED')),
    admin_signature   BLOB NOT NULL,          -- 64B Ed25519 lease proof
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    PRIMARY KEY (chain_id, merchant_uuid, recipient_address)
);
CREATE INDEX IF NOT EXISTS idx_whitelist_recipient
    ON withdrawal_whitelist(chain_id, recipient_address);

-- FIX-C (audit P0-5): every lease signature ever ACCEPTED for an entry.
-- A REVOKED entry may only be resurrected by a signature ABSENT from this
-- history — replaying any previously-seen proof (the stored one or a
-- superseded older lease) is rejected. Ed25519 is deterministic over the
-- fixed admin key, so a never-seen signature implies a changed payload
-- (new lease terms = a genuinely fresh admin approval).
CREATE TABLE IF NOT EXISTS whitelist_lease_proofs (
    chain_id          INTEGER NOT NULL,
    merchant_uuid     BLOB NOT NULL,
    recipient_address BLOB NOT NULL,
    admin_signature   BLOB NOT NULL,          -- 64B Ed25519 lease proof
    first_used_at     INTEGER NOT NULL,
    PRIMARY KEY (chain_id, merchant_uuid, recipient_address, admin_signature)
);

CREATE TABLE IF NOT EXISTS signer_intent_slots (
    chain_id      INTEGER NOT NULL,
    from_wallet   TEXT NOT NULL,              -- '0x' lowercase hot wallet
    nonce         INTEGER NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('PAYOUT','GAP_FILLER')),
    payout_uuid   BLOB,                       -- 16B, NULL for GAP_FILLER
    retry_seq     INTEGER,
    token         BLOB,                       -- 20B (NULL on GAP_FILLER rows)
    recipient     BLOB,                       -- 20B
    amount        BLOB,                       -- 32B big-endian U256
    filler_signed INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (chain_id, from_wallet, nonce)
);
-- A (payout_uuid, retry_seq) may bind at most ONE nonce slot on a chain.
CREATE UNIQUE INDEX IF NOT EXISTS uq_slot_payout
    ON signer_intent_slots(chain_id, payout_uuid, retry_seq)
    WHERE payout_uuid IS NOT NULL;

CREATE TABLE IF NOT EXISTS rolling_24h_usage (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    chain_id   INTEGER NOT NULL,
    token      BLOB NOT NULL,                 -- 20B custody token contract
    amount     BLOB NOT NULL,                 -- 32B big-endian U256
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_window
    ON rolling_24h_usage(chain_id, created_at);

-- Dynamic COLD allowlist for the SWEEP policy. Bootstrap: at engine boot the
-- chain's `allowed_cold_wallets` JSON config seeds the table iff the table
-- has no rows for that chain; afterwards this table is the ONLY authority
-- (UpdateColdWallets mutates it at runtime; config edits no longer apply).
-- Rotation windows hold old+new addresses simultaneously, exactly as the
-- frozen dual-track comment on ChainPolicyConfig.allowed_cold_wallets states.
CREATE TABLE IF NOT EXISTS cold_wallets (
    chain_id   INTEGER NOT NULL,
    address    BLOB NOT NULL,               -- EVM raw20
    created_at INTEGER NOT NULL,            -- unix seconds
    PRIMARY KEY (chain_id, address)
);

CREATE TABLE IF NOT EXISTS audit_logs (
    seq            INTEGER PRIMARY KEY,       -- dense from 1
    timestamp_unix INTEGER NOT NULL,
    action         TEXT NOT NULL,
    request_digest BLOB NOT NULL,             -- 32B SHA-256
    decision       TEXT NOT NULL,
    reason         TEXT NOT NULL,
    prev_hash      BLOB NOT NULL,             -- 32B
    entry_hash     BLOB NOT NULL              -- 32B
);
"#;

/// A `withdrawal_whitelist` row.
#[derive(Debug, Clone)]
pub struct WhitelistRow {
    pub merchant_uuid: [u8; 16],
    pub first_seen_at: i64,
    pub valid_until: i64,
    pub status: String,
}

/// A `signer_intent_slots` row (the immutable business authorization fields
/// plus the independent `filler_signed` flag).
#[derive(Debug, Clone)]
pub struct IntentSlot {
    pub kind: SlotKind,
    pub payout_uuid: Option<[u8; 16]>,
    pub retry_seq: Option<u32>,
    pub token: Option<Address>,
    pub recipient: Option<Address>,
    pub amount: Option<U256>,
    pub filler_signed: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SlotKind {
    Payout,
    GapFiller,
}

/// An `encrypted_keys` row (wallet, envelope).
#[derive(Debug, Clone)]
pub struct EncryptedKeyRow {
    pub wallet_address: String,
    pub label: String,
    pub dek_envelope: Vec<u8>,
}

/// Open (creating if needed) the signer DB at `path`; applies pragmas + schema.
pub fn open(path: &Path) -> anyhow::Result<SignerDb> {
    if let Some(parent) = path.parent() {
        if !parent.as_os_str().is_empty() {
            std::fs::create_dir_all(parent)?;
        }
    }
    let conn = Connection::open(path)?;
    init_conn(&conn)?;
    Ok(SignerDb { conn })
}

/// In-memory DB for tests.
pub fn open_in_memory() -> anyhow::Result<SignerDb> {
    let conn = Connection::open_in_memory()?;
    init_conn(&conn)?;
    Ok(SignerDb { conn })
}

fn init_conn(conn: &Connection) -> anyhow::Result<()> {
    conn.pragma_update(None, "journal_mode", "WAL")?;
    conn.pragma_update(None, "foreign_keys", "ON")?;
    conn.pragma_update(None, "busy_timeout", 5000i64)?;
    conn.pragma_update(None, "synchronous", "FULL")?; // paranoid durability
    conn.execute_batch(SCHEMA_SQL)?;
    Ok(())
}

// ---------------------------------------------------------------------------
// encrypted_keys
// ---------------------------------------------------------------------------

/// 热钱包元数据投影 — 不含 DEK 信封, ListWallets 专用.
pub struct WalletKeyMeta {
    pub wallet_address: String,
    pub label: String,
    pub created_at: i64,
}

pub fn list_wallet_key_meta(conn: &Connection) -> rusqlite::Result<Vec<WalletKeyMeta>> {
    let mut stmt = conn.prepare(
        "SELECT wallet_address, label, created_at FROM encrypted_keys
          ORDER BY created_at, wallet_address",
    )?;
    let rows = stmt.query_map([], |r| {
        Ok(WalletKeyMeta {
            wallet_address: r.get(0)?,
            label: r.get(1)?,
            created_at: r.get(2)?,
        })
    })?;
    rows.collect()
}

pub fn load_encrypted_keys(conn: &Connection) -> rusqlite::Result<Vec<EncryptedKeyRow>> {
    let mut stmt = conn.prepare(
        "SELECT wallet_address, label, dek_envelope FROM encrypted_keys ORDER BY wallet_address",
    )?;
    let rows = stmt.query_map([], |r| {
        Ok(EncryptedKeyRow {
            wallet_address: r.get(0)?,
            label: r.get(1)?,
            dek_envelope: r.get(2)?,
        })
    })?;
    rows.collect()
}

/// Provisioning helper (tests / ops tooling): store an envelope for `wallet`.
pub fn upsert_encrypted_key(
    conn: &Connection,
    wallet_address: &str,
    label: &str,
    dek_envelope: &[u8],
    now_unix: i64,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO encrypted_keys(wallet_address,label,dek_envelope,created_at)
         VALUES(?1,?2,?3,?4)
         ON CONFLICT(wallet_address) DO UPDATE SET
            label=excluded.label, dek_envelope=excluded.dek_envelope",
        params![wallet_address, label, dek_envelope, now_unix],
    )?;
    Ok(())
}

// ---------------------------------------------------------------------------
// withdrawal_whitelist
// ---------------------------------------------------------------------------

/// All whitelist rows for `(chain_id, recipient)` — any ACTIVE lease wins.
pub fn whitelist_for_recipient(
    conn: &Connection,
    chain_id: u64,
    recipient: &Address,
) -> rusqlite::Result<Vec<WhitelistRow>> {
    let mut stmt = conn.prepare(
        "SELECT merchant_uuid, first_seen_at, valid_until, status
         FROM withdrawal_whitelist
         WHERE chain_id = ?1 AND recipient_address = ?2
         ORDER BY first_seen_at ASC",
    )?;
    let rows = stmt.query_map(params![chain_id as i64, recipient.as_slice()], |r| {
        let mut uuid = [0u8; 16];
        let raw: Vec<u8> = r.get(0)?;
        let n = raw.len().min(16);
        uuid[..n].copy_from_slice(&raw[..n]);
        Ok(WhitelistRow {
            merchant_uuid: uuid,
            first_seen_at: r.get(1)?,
            valid_until: r.get(2)?,
            status: r.get(3)?,
        })
    })?;
    rows.collect()
}

/// Status of the exact `(chain_id, merchant_uuid, recipient)` lease row,
/// if it exists (`"ACTIVE"` / `"REVOKED"`).
pub fn whitelist_status(
    conn: &Connection,
    chain_id: u64,
    merchant_uuid: &[u8; 16],
    recipient: &[u8; 20],
) -> rusqlite::Result<Option<String>> {
    conn.query_row(
        "SELECT status FROM withdrawal_whitelist
         WHERE chain_id=?1 AND merchant_uuid=?2 AND recipient_address=?3",
        params![chain_id as i64, merchant_uuid.as_slice(), recipient.as_slice()],
        |r| r.get(0),
    )
    .optional()
}

/// Has `admin_signature` ever been ACCEPTED as a lease proof for this entry?
/// The engine consults this before resurrecting a REVOKED row — a replayed
/// proof must never undo an emergency revoke (FIX-C / audit P0-5).
pub fn whitelist_proof_seen(
    conn: &Connection,
    chain_id: u64,
    merchant_uuid: &[u8; 16],
    recipient: &[u8; 20],
    admin_signature: &[u8],
) -> rusqlite::Result<bool> {
    let n: i64 = conn.query_row(
        "SELECT COUNT(*) FROM whitelist_lease_proofs
         WHERE chain_id=?1 AND merchant_uuid=?2 AND recipient_address=?3
           AND admin_signature=?4",
        params![
            chain_id as i64,
            merchant_uuid.as_slice(),
            recipient.as_slice(),
            admin_signature
        ],
        |r| r.get(0),
    )?;
    Ok(n > 0)
}

/// Insert or re-activate a lease inside `tx`; the presented signature is
/// recorded into `whitelist_lease_proofs` (idempotent — the row keyed by the
/// signature itself).
///
/// `first_seen_at` semantics (FIX-C, ARCHITECTURE §1.2 — 新审批新冷却):
///   * fresh insert             -> `first_seen_at = now` (cooldown starts);
///   * ACTIVE re-registration   -> preserved — renewing a live lease must not
///     re-freeze payouts for another cooldown window;
///   * REVOKED -> ACTIVE revive -> `first_seen_at = now` — resurrection is a
///     NEW admin approval and re-serves the full cooldown so operations get a
///     fresh detection window after every emergency revoke.
///
/// `valid_until` semantics (FIX-E/E6 — monotonic lease):
///   * fresh insert / REVOKED resurrection -> the new admin-approved value;
///   * ACTIVE re-registration -> `MAX(existing, new)` — a lease renewal may
///     EXTEND the upper bound but a stale/short replay can never SHRINK an
///     already-granted lease window.
///
/// The caller (engine::register_inner) is responsible for rejecting
/// previously-seen signatures on REVOKED rows BEFORE calling this.
#[allow(clippy::too_many_arguments)]
pub fn whitelist_register(
    tx: &Transaction<'_>,
    chain_id: u64,
    merchant_uuid: &[u8; 16],
    recipient: &[u8; 20],
    valid_until: i64,
    admin_signature: &[u8],
    now_unix: i64,
) -> rusqlite::Result<()> {
    tx.execute(
        "INSERT OR IGNORE INTO whitelist_lease_proofs(
            chain_id, merchant_uuid, recipient_address, admin_signature, first_used_at)
         VALUES(?1,?2,?3,?4,?5)",
        params![
            chain_id as i64,
            merchant_uuid.as_slice(),
            recipient.as_slice(),
            admin_signature,
            now_unix
        ],
    )?;
    tx.execute(
        "INSERT INTO withdrawal_whitelist(
            chain_id, merchant_uuid, recipient_address,
            first_seen_at, valid_until, status, admin_signature,
            created_at, updated_at)
         VALUES(?1,?2,?3,?4,?5,'ACTIVE',?6,?7,?7)
         ON CONFLICT(chain_id, merchant_uuid, recipient_address) DO UPDATE SET
            first_seen_at = CASE
                WHEN withdrawal_whitelist.status = 'REVOKED'
                THEN excluded.first_seen_at
                ELSE withdrawal_whitelist.first_seen_at
            END,
            valid_until = CASE
                WHEN withdrawal_whitelist.status = 'REVOKED'
                THEN excluded.valid_until
                ELSE MAX(withdrawal_whitelist.valid_until, excluded.valid_until)
            END,
            status = 'ACTIVE',
            admin_signature = excluded.admin_signature,
            updated_at = excluded.updated_at",
        params![
            chain_id as i64,
            merchant_uuid.as_slice(),
            recipient.as_slice(),
            now_unix,
            valid_until,
            admin_signature,
            now_unix
        ],
    )?;
    Ok(())
}

/// Emergency revoke — returns rows actually flipped to REVOKED.
pub fn whitelist_revoke(
    tx: &Transaction<'_>,
    chain_id: u64,
    merchant_uuid: &[u8; 16],
    recipient: &[u8; 20],
    now_unix: i64,
) -> rusqlite::Result<usize> {
    tx.execute(
        "UPDATE withdrawal_whitelist
         SET status='REVOKED', updated_at=?4
         WHERE chain_id=?1 AND merchant_uuid=?2 AND recipient_address=?3
           AND status='ACTIVE'",
        params![chain_id as i64, merchant_uuid.as_slice(), recipient.as_slice(), now_unix],
    )
}

/// Count rows (health endpoint).
pub fn whitelist_count(conn: &Connection) -> rusqlite::Result<u64> {
    conn.query_row("SELECT COUNT(*) FROM withdrawal_whitelist", [], |r| r.get(0))
}

// ---------------------------------------------------------------------------
// signer_intent_slots
// ---------------------------------------------------------------------------

pub fn slot_get(
    conn: &Connection,
    chain_id: u64,
    from_wallet: &str,
    nonce: u64,
) -> rusqlite::Result<Option<IntentSlot>> {
    conn.query_row(
        "SELECT kind, payout_uuid, retry_seq, token, recipient, amount, filler_signed
         FROM signer_intent_slots
         WHERE chain_id=?1 AND from_wallet=?2 AND nonce=?3",
        params![chain_id as i64, from_wallet, nonce as i64],
        |r| {
            let kind_str: String = r.get(0)?;
            let payout_uuid: Option<Vec<u8>> = r.get(1)?;
            let retry_seq: Option<i64> = r.get(2)?;
            let token: Option<Vec<u8>> = r.get(3)?;
            let recipient: Option<Vec<u8>> = r.get(4)?;
            let amount: Option<Vec<u8>> = r.get(5)?;
            let filler_signed: i64 = r.get(6)?;
            Ok(IntentSlot {
                kind: if kind_str == "PAYOUT" { SlotKind::Payout } else { SlotKind::GapFiller },
                payout_uuid: payout_uuid.and_then(|v| <[u8; 16]>::try_from(v.as_slice()).ok()),
                retry_seq: retry_seq.map(|v| v as u32),
                token: token.and_then(|v| {
                    (v.len() == 20).then(|| Address::from_slice(&v))
                }),
                recipient: recipient.and_then(|v| {
                    (v.len() == 20).then(|| Address::from_slice(&v))
                }),
                amount: amount.and_then(|v| {
                    <[u8; 32]>::try_from(v.as_slice()).ok().map(U256::from_be_bytes::<32>)
                }),
                filler_signed: filler_signed != 0,
            })
        },
    )
    .optional()
}

/// Is `(payout_uuid, retry_seq)` already bound to a DIFFERENT nonce slot?
/// Returns `(from_wallet, nonce)` of the conflicting slot.
pub fn slot_owner_of_payout(
    conn: &Connection,
    chain_id: u64,
    payout_uuid: &[u8; 16],
    retry_seq: u32,
) -> rusqlite::Result<Option<(String, u64)>> {
    conn.query_row(
        "SELECT from_wallet, nonce FROM signer_intent_slots
         WHERE chain_id=?1 AND payout_uuid=?2 AND retry_seq=?3",
        params![chain_id as i64, payout_uuid.as_slice(), retry_seq as i64],
        |r| Ok((r.get::<_, String>(0)?, r.get::<_, i64>(1)? as u64)),
    )
    .optional()
}

#[allow(clippy::too_many_arguments)]
pub fn slot_insert_payout(
    tx: &Transaction<'_>,
    chain_id: u64,
    from_wallet: &str,
    nonce: u64,
    payout_uuid: &[u8; 16],
    retry_seq: u32,
    token: &Address,
    recipient: &Address,
    amount: &U256,
    now_unix: i64,
) -> rusqlite::Result<()> {
    tx.execute(
        "INSERT INTO signer_intent_slots(
            chain_id, from_wallet, nonce, kind,
            payout_uuid, retry_seq, token, recipient, amount,
            filler_signed, created_at)
         VALUES(?1,?2,?3,'PAYOUT',?4,?5,?6,?7,?8,0,?9)",
        params![
            chain_id as i64,
            from_wallet,
            nonce as i64,
            payout_uuid.as_slice(),
            retry_seq as i64,
            token.as_slice(),
            recipient.as_slice(),
            amount.to_be_bytes::<32>().as_slice(),
            now_unix
        ],
    )?;
    Ok(())
}

/// Insert a filler-only slot for a nonce with no prior business intent.
pub fn slot_insert_gap_filler(
    tx: &Transaction<'_>,
    chain_id: u64,
    from_wallet: &str,
    nonce: u64,
    now_unix: i64,
) -> rusqlite::Result<()> {
    tx.execute(
        "INSERT INTO signer_intent_slots(
            chain_id, from_wallet, nonce, kind, filler_signed, created_at)
         VALUES(?1,?2,?3,'GAP_FILLER',1,?4)",
        params![chain_id as i64, from_wallet, nonce as i64, now_unix],
    )?;
    Ok(())
}

/// Flip `filler_signed` on an existing slot — NEVER touches the immutable
/// business fields (deep-reorg replays must find them intact).
pub fn slot_mark_filler_signed(
    tx: &Transaction<'_>,
    chain_id: u64,
    from_wallet: &str,
    nonce: u64,
) -> rusqlite::Result<()> {
    tx.execute(
        "UPDATE signer_intent_slots SET filler_signed=1
         WHERE chain_id=?1 AND from_wallet=?2 AND nonce=?3",
        params![chain_id as i64, from_wallet, nonce as i64],
    )?;
    Ok(())
}

// ---------------------------------------------------------------------------
// rolling_24h_usage
// ---------------------------------------------------------------------------

/// Sum usage since `since_unix` — returned as U256 with saturating semantics:
/// an overflowing sum is clamped to U256::MAX (i.e. definitely over any sane
/// breaker limit), never silently wrapped.
pub fn usage_sum_since(
    conn: &Connection,
    chain_id: u64,
    since_unix: i64,
) -> rusqlite::Result<U256> {
    let mut stmt = conn.prepare(
        "SELECT amount FROM rolling_24h_usage WHERE chain_id=?1 AND created_at>=?2",
    )?;
    let rows = stmt.query_map(params![chain_id as i64, since_unix], |r| {
        r.get::<_, Vec<u8>>(0)
    })?;
    let mut total = U256::ZERO;
    for row in rows {
        let raw = row?;
        if raw.len() == 32 {
            let amt = U256::from_be_bytes::<32>(
                <[u8; 32]>::try_from(raw.as_slice()).map_err(|_| {
                    rusqlite::Error::InvalidParameterName("bad amount blob".into())
                })?,
            );
            total = total.saturating_add(amt);
        }
    }
    Ok(total)
}

pub fn usage_insert(
    tx: &Transaction<'_>,
    chain_id: u64,
    token: &Address,
    amount: &U256,
    now_unix: i64,
) -> rusqlite::Result<()> {
    tx.execute(
        "INSERT INTO rolling_24h_usage(chain_id, token, amount, created_at)
         VALUES(?1,?2,?3,?4)",
        params![
            chain_id as i64,
            token.as_slice(),
            amount.to_be_bytes::<32>().as_slice(),
            now_unix
        ],
    )?;
    Ok(())
}

// ---------------------------------------------------------------------------
// cold_wallets (dynamic COLD allowlist — SWEEP policy authority)
// ---------------------------------------------------------------------------

/// Effective cold-wallet allowlist for `chain_id`, ascending raw-byte order
/// (== ascending `0x` lowercase hex order at fixed 20B length — deterministic
/// for the wire response). Malformed-length rows are skipped, not trusted.
pub fn cold_wallets_list(conn: &Connection, chain_id: u64) -> rusqlite::Result<Vec<Address>> {
    let mut stmt =
        conn.prepare("SELECT address FROM cold_wallets WHERE chain_id=?1 ORDER BY address")?;
    let rows = stmt.query_map(params![chain_id as i64], |r| r.get::<_, Vec<u8>>(0))?;
    let mut out = Vec::new();
    for r in rows {
        let raw = r?;
        if raw.len() == 20 {
            out.push(Address::from_slice(&raw));
        }
    }
    Ok(out)
}

/// SWEEP firewall check: is `addr` a currently-allowed cold wallet?
pub fn cold_wallet_allowed(
    conn: &Connection,
    chain_id: u64,
    addr: &Address,
) -> rusqlite::Result<bool> {
    let n: i64 = conn.query_row(
        "SELECT COUNT(*) FROM cold_wallets WHERE chain_id=?1 AND address=?2",
        params![chain_id as i64, addr.as_slice()],
        |r| r.get(0),
    )?;
    Ok(n > 0)
}

/// Idempotent add (UpdateColdWallets RPC is specified as idempotent).
pub fn cold_wallet_add(
    tx: &Transaction<'_>,
    chain_id: u64,
    addr: &Address,
    now_unix: i64,
) -> rusqlite::Result<()> {
    tx.execute(
        "INSERT OR IGNORE INTO cold_wallets(chain_id, address, created_at)
         VALUES(?1,?2,?3)",
        params![chain_id as i64, addr.as_slice(), now_unix],
    )?;
    Ok(())
}

/// Idempotent remove — returns rows actually deleted (0 on absent/no-op).
pub fn cold_wallet_remove(
    tx: &Transaction<'_>,
    chain_id: u64,
    addr: &Address,
) -> rusqlite::Result<usize> {
    tx.execute(
        "DELETE FROM cold_wallets WHERE chain_id=?1 AND address=?2",
        params![chain_id as i64, addr.as_slice()],
    )
}

/// Boot seeding inside a caller-provided transaction: if `chain_id` has NO
/// rows, insert `addrs` verbatim from config and return the count seeded;
/// if any row exists the table is already authoritative — config is ignored
/// and `0` is returned.
pub fn seed_cold_wallets(
    tx: &Transaction<'_>,
    chain_id: u64,
    addrs: &[Address],
    now_unix: i64,
) -> rusqlite::Result<usize> {
    let n: i64 = tx.query_row(
        "SELECT COUNT(*) FROM cold_wallets WHERE chain_id=?1",
        params![chain_id as i64],
        |r| r.get(0),
    )?;
    if n > 0 {
        return Ok(0);
    }
    for a in addrs {
        cold_wallet_add(tx, chain_id, a, now_unix)?;
    }
    Ok(addrs.len())
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn schema_creates_seven_tables() {
        let db = open_in_memory().unwrap();
        for t in [
            "encrypted_keys",
            "withdrawal_whitelist",
            "whitelist_lease_proofs",
            "signer_intent_slots",
            "rolling_24h_usage",
            "cold_wallets",
            "audit_logs",
        ] {
            let n: i64 = db
                .conn
                .query_row(
                    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?1",
                    params![t],
                    |r| r.get(0),
                )
                .unwrap();
            assert_eq!(n, 1, "missing table {t}");
        }
    }

    #[test]
    fn payout_slot_unique_constraint() {
        let mut db = open_in_memory().unwrap();
        let uuid = [7u8; 16];
        let token = Address::from([0x44u8; 20]);
        let recip = Address::from([0x55u8; 20]);
        {
            let tx = db.conn.transaction().unwrap();
            slot_insert_payout(&tx, 1, "0xabc", 5, &uuid, 0, &token, &recip, &U256::from(1u64), 100)
                .unwrap();
            tx.commit().unwrap();
        }
        // Same (payout_uuid, retry_seq) under a different nonce -> unique violation.
        let tx = db.conn.transaction().unwrap();
        let dup = slot_insert_payout(&tx, 1, "0xabc", 9, &uuid, 0, &token, &recip, &U256::from(1u64), 100);
        assert!(dup.is_err(), "unique index must block double-binding");
        drop(tx);

        let owner = slot_owner_of_payout(&db.conn, 1, &uuid, 0).unwrap();
        assert_eq!(owner, Some(("0xabc".to_string(), 5)));

        let slot = slot_get(&db.conn, 1, "0xabc", 5).unwrap().unwrap();
        assert_eq!(slot.kind, SlotKind::Payout);
        assert_eq!(slot.token.unwrap(), token);
        assert_eq!(slot.amount.unwrap(), U256::from(1u64));
        assert!(!slot.filler_signed);
    }

    /// FIX-C: resurrection re-anchors `first_seen_at` only when the row was
    /// REVOKED; ACTIVE renewals keep the original cooldown anchor; every
    /// accepted signature lands in `whitelist_lease_proofs` exactly once.
    #[test]
    fn whitelist_register_cooldown_anchor_and_proof_history() {
        let mut db = open_in_memory().unwrap();
        let merchant = [1u8; 16];
        let recip = [2u8; 20];
        let sig_a = [0xAAu8; 64];
        let sig_b = [0xBBu8; 64];
        let sig_c = [0xCCu8; 64];

        // Fresh insert: first_seen_at = registration time.
        {
            let tx = db.conn.transaction().unwrap();
            whitelist_register(&tx, 1, &merchant, &recip, 9_999, &sig_a, 1_000).unwrap();
            tx.commit().unwrap();
        }
        let row = whitelist_for_recipient(&db.conn, 1, &Address::from(recip))
            .unwrap()
            .remove(0);
        assert_eq!(row.first_seen_at, 1_000);
        assert_eq!(row.status, "ACTIVE");

        // ACTIVE renewal preserves the anchor (cooldown already served).
        {
            let tx = db.conn.transaction().unwrap();
            whitelist_register(&tx, 1, &merchant, &recip, 12_000, &sig_b, 2_000).unwrap();
            tx.commit().unwrap();
        }
        let row = whitelist_for_recipient(&db.conn, 1, &Address::from(recip))
            .unwrap()
            .remove(0);
        assert_eq!(row.first_seen_at, 1_000, "renewal must preserve the cooldown anchor");
        assert_eq!(row.valid_until, 12_000);

        // REVOKED -> resurrected with a NEW signature: the anchor refreshes
        // so the revived lease re-serves its cooldown.
        {
            let tx = db.conn.transaction().unwrap();
            assert_eq!(whitelist_revoke(&tx, 1, &merchant, &recip, 3_000).unwrap(), 1);
            whitelist_register(&tx, 1, &merchant, &recip, 15_000, &sig_c, 4_000).unwrap();
            tx.commit().unwrap();
        }
        let row = whitelist_for_recipient(&db.conn, 1, &Address::from(recip))
            .unwrap()
            .remove(0);
        assert_eq!(row.status, "ACTIVE");
        assert_eq!(row.first_seen_at, 4_000, "resurrection must re-anchor the cooldown");

        // Proof history: all three distinct signatures recorded exactly once;
        // re-registering an already-seen signature does not duplicate it.
        for sig in [&sig_a, &sig_b, &sig_c] {
            assert!(whitelist_proof_seen(&db.conn, 1, &merchant, &recip, sig).unwrap());
        }
        assert!(
            !whitelist_proof_seen(&db.conn, 1, &merchant, &recip, &[0xDDu8; 64]).unwrap(),
            "unseen signature must not be in history"
        );
        {
            let tx = db.conn.transaction().unwrap();
            whitelist_register(&tx, 1, &merchant, &recip, 15_000, &sig_a, 5_000).unwrap();
            tx.commit().unwrap();
        }
        let n: i64 = db
            .conn
            .query_row("SELECT COUNT(*) FROM whitelist_lease_proofs WHERE chain_id=1", [], |r| {
                r.get(0)
            })
            .unwrap();
        assert_eq!(n, 3, "seen proofs must not duplicate history rows");
        assert_eq!(
            whitelist_status(&db.conn, 1, &merchant, &recip).unwrap(),
            Some("ACTIVE".to_string())
        );
        assert!(whitelist_status(&db.conn, 1, &[9u8; 16], &recip).unwrap().is_none());
    }

    /// FIX-E/E6: ACTIVE re-registration is monotonic on `valid_until` —
    /// a stale/short replay must never shrink an already-granted lease,
    /// a longer lease still extends it; REVOKED resurrection takes the
    /// fresh admin-approved value verbatim (new cooldown anchor).
    #[test]
    fn whitelist_register_valid_until_is_monotonic() {
        let mut db = open_in_memory().unwrap();
        let merchant = [3u8; 16];
        let recip = [4u8; 20];
        let sig_a = [0x11u8; 64];
        let sig_b = [0x22u8; 64];
        let sig_c = [0x33u8; 64];
        let sig_d = [0x44u8; 64];

        {
            let tx = db.conn.transaction().unwrap();
            whitelist_register(&tx, 1, &merchant, &recip, 10_000, &sig_a, 1_000).unwrap();
            tx.commit().unwrap();
        }
        // Longer renewal extends the bound.
        {
            let tx = db.conn.transaction().unwrap();
            whitelist_register(&tx, 1, &merchant, &recip, 20_000, &sig_b, 2_000).unwrap();
            tx.commit().unwrap();
        }
        let row = whitelist_for_recipient(&db.conn, 1, &Address::from(recip))
            .unwrap()
            .remove(0);
        assert_eq!(row.valid_until, 20_000, "renewal must extend validity");

        // Stale re-registration with a SHORTER lease must NOT shrink it.
        {
            let tx = db.conn.transaction().unwrap();
            whitelist_register(&tx, 1, &merchant, &recip, 8_000, &sig_c, 3_000).unwrap();
            tx.commit().unwrap();
        }
        let row = whitelist_for_recipient(&db.conn, 1, &Address::from(recip))
            .unwrap()
            .remove(0);
        assert_eq!(
            row.valid_until, 20_000,
            "stale registration must not shorten an active lease (E6)"
        );
        assert_eq!(row.first_seen_at, 1_000, "ACTIVE renewal keeps cooldown anchor");

        // REVOKED resurrection takes the fresh admin value verbatim.
        {
            let tx = db.conn.transaction().unwrap();
            assert_eq!(whitelist_revoke(&tx, 1, &merchant, &recip, 4_000).unwrap(), 1);
            whitelist_register(&tx, 1, &merchant, &recip, 9_000, &sig_d, 5_000).unwrap();
            tx.commit().unwrap();
        }
        let row = whitelist_for_recipient(&db.conn, 1, &Address::from(recip))
            .unwrap()
            .remove(0);
        assert_eq!(row.valid_until, 9_000, "revived lease takes fresh admin bound");
        assert_eq!(row.first_seen_at, 5_000, "resurrection re-anchors cooldown");
    }

    /// cold_wallets: config seeds only while the table is empty (afterwards
    /// DB is authoritative); add/remove are idempotent; the SWEEP lookup and
    /// ordered list reflect runtime updates immediately.
    #[test]
    fn cold_wallets_seed_then_db_is_authoritative() {
        let mut db = open_in_memory().unwrap();
        let a = Address::from([0xA1u8; 20]);
        let b = Address::from([0xB2u8; 20]);
        let c = Address::from([0xC3u8; 20]);

        // First boot: empty table -> config seeds verbatim.
        {
            let tx = db.conn.transaction().unwrap();
            assert_eq!(seed_cold_wallets(&tx, 1, &[a, b], 1_000).unwrap(), 2);
            tx.commit().unwrap();
        }
        assert_eq!(cold_wallets_list(&db.conn, 1).unwrap(), vec![a, b]);
        assert!(cold_wallet_allowed(&db.conn, 1, &a).unwrap());
        assert!(!cold_wallet_allowed(&db.conn, 1, &c).unwrap());

        // Runtime update: remove a, add c. Idempotent on both sides.
        {
            let tx = db.conn.transaction().unwrap();
            assert_eq!(cold_wallet_remove(&tx, 1, &a).unwrap(), 1);
            assert_eq!(cold_wallet_remove(&tx, 1, &a).unwrap(), 0); // idempotent
            cold_wallet_add(&tx, 1, &c, 2_000).unwrap();
            cold_wallet_add(&tx, 1, &c, 3_000).unwrap(); // idempotent
            tx.commit().unwrap();
        }
        assert_eq!(cold_wallets_list(&db.conn, 1).unwrap(), vec![b, c]);

        // Re-"boot": rows exist -> config must NOT overwrite runtime state.
        {
            let tx = db.conn.transaction().unwrap();
            assert_eq!(seed_cold_wallets(&tx, 1, &[a], 4_000).unwrap(), 0);
            tx.commit().unwrap();
        }
        assert_eq!(cold_wallets_list(&db.conn, 1).unwrap(), vec![b, c]);

        // Independent per chain_id.
        {
            let tx = db.conn.transaction().unwrap();
            assert_eq!(seed_cold_wallets(&tx, 2, &[a], 5_000).unwrap(), 1);
            tx.commit().unwrap();
        }
        assert!(cold_wallet_allowed(&db.conn, 2, &a).unwrap());
        assert!(!cold_wallet_allowed(&db.conn, 1, &a).unwrap());
    }
}
