//! SHA-256 append-only audit hash chain (tamper-evident, W2-2).
//!
//! Frozen formula:
//! `entry_hash = SHA256(seq || timestamp || action || request_digest
//!                     || decision || reason || prev_hash)`
//! Field encoding (big-endian fixed ints, u32-BE-length-prefixed strings) is
//! finalized in W2-2 — every approve/revoke/reject/allow appends atomically.

use sha2::{Digest, Sha256};

/// One `audit_logs` row. `seq` starts at 1; genesis `prev_hash == [0u8; 32]`.
#[derive(Debug, Clone)]
pub struct AuditEntry {
    pub seq: u64,
    pub timestamp_unix: i64,
    pub action: String,
    pub request_digest: [u8; 32],
    pub decision: String,
    pub reason: String,
    pub prev_hash: [u8; 32],
    pub entry_hash: [u8; 32],
}

/// Compute `entry_hash` for an entry under the frozen field encoding above.
pub fn entry_hash(_e: &AuditEntry) -> [u8; 32] {
    // TODO(W2-2): stream fields into sha2::Sha256 in the frozen order.
    let _hasher = Sha256::new();
    unimplemented!("entry_hash: W2-2")
}

/// Recompute the chain in `audit_logs` over `[from_seq, to_seq]` (0 = open end)
/// and report (valid, first_bad_seq) — backs the VerifyAuditChain RPC that the
/// ops watchdog calls every 5 minutes (W2-2/W2-4).
pub fn verify_chain(
    _conn: &rusqlite::Connection,
    _from_seq: u64,
    _to_seq: u64,
) -> anyhow::Result<(bool, u64)> {
    anyhow::bail!("verify_chain not implemented (W2-2)")
}
