//! Physically-isolated embedded SQLite state — reachable ONLY by this signer
//! process (gateway-core has no access; zero-trust boundary, §1.1).
//!
//! Frozen table set (W2-2 owns the full DDL):
//!   encrypted_keys          — AES-256-GCM envelope-encrypted DEKs
//!   withdrawal_whitelist    — leased entries (first_seen_at, valid_until, status)
//!   signer_intent_slots     — dual-field same-nonce idempotent auth slots
//!   rolling_24h_usage       — sliding-window payout counter (circuit breaker)
//!   audit_logs              — SHA-256 hash chain (see audit.rs)

use std::path::Path;

/// Owned handle to the signer state DB.
pub struct SignerDb {
    pub conn: rusqlite::Connection,
}

/// Frozen DDL for the five tables — executed inside `open` (W2-2 fills in).
pub const SCHEMA_SQL: &str = r#"
-- TODO(W2-2): encrypted_keys / withdrawal_whitelist / signer_intent_slots /
--             rolling_24h_usage / audit_logs
"#;

/// Open (creating if needed) the signer DB at `path`; applies pragmas + schema.
pub fn open(path: &Path) -> anyhow::Result<SignerDb> {
    let conn = rusqlite::Connection::open(path)?;
    // TODO(W2-2): PRAGMA journal_mode=WAL, foreign_keys=ON, execute SCHEMA_SQL.
    Ok(SignerDb { conn })
}
