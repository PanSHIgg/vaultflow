//! SHA-256 append-only audit hash chain (tamper-evident).
//!
//! Frozen formula:
//! `entry_hash = SHA256(seq || timestamp || action || request_digest
//!                     || decision || reason || prev_hash)`
//!
//! Field encoding (finalized here — big-endian fixed ints,
//! u32-BE-length-prefixed strings, raw 32B digests):
//!   seq            u64 BE (8B)
//!   timestamp      i64 BE (8B, unix seconds)
//!   action         u32 BE len || UTF-8 bytes
//!   request_digest 32B raw
//!   decision       u32 BE len || UTF-8 bytes
//!   reason         u32 BE len || UTF-8 bytes
//!   prev_hash      32B raw
//!
//! Every approve/revoke/reject appends atomically inside the same SQLite
//! transaction as the state change it describes (see core.rs).

use rusqlite::{params, Connection, OptionalExtension, Transaction};
use sha2::{Digest, Sha256};

/// Genesis `prev_hash` for seq==1.
pub const GENESIS_PREV_HASH: [u8; 32] = [0u8; 32];

/// Canonical action strings (stable — part of the hashed surface).
pub mod actions {
    pub const REGISTER_WITHDRAWAL: &str = "REGISTER_WITHDRAWAL_ADDRESS";
    pub const REVOKE_WITHDRAWAL: &str = "REVOKE_WITHDRAWAL_ADDRESS";
    pub const SIGN_TRANSACTION: &str = "SIGN_TRANSACTION";
    pub const GENERATE_WALLET: &str = "GENERATE_WALLET";
    pub const UPDATE_COLD_WALLETS: &str = "UPDATE_COLD_WALLETS";
}

/// Canonical decision strings.
pub mod decisions {
    pub const APPROVED: &str = "APPROVED";
    pub const REJECTED: &str = "REJECTED";
}

/// Copy up to 32 bytes of a stored blob into a fixed array — tampered rows
/// with short/long blobs simply fail the hash check instead of panicking.
fn blob32(v: &[u8]) -> [u8; 32] {
    let mut a = [0u8; 32];
    let n = v.len().min(32);
    a[..n].copy_from_slice(&v[..n]);
    a
}

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
pub fn entry_hash(e: &AuditEntry) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(e.seq.to_be_bytes());
    h.update(e.timestamp_unix.to_be_bytes());
    h.update((e.action.len() as u32).to_be_bytes());
    h.update(e.action.as_bytes());
    h.update(e.request_digest);
    h.update((e.decision.len() as u32).to_be_bytes());
    h.update(e.decision.as_bytes());
    h.update((e.reason.len() as u32).to_be_bytes());
    h.update(e.reason.as_bytes());
    h.update(e.prev_hash);
    let out = h.finalize();
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&out);
    arr
}

/// Chain tip `(seq, entry_hash)`; `(0, GENESIS)` when empty.
pub fn tip(conn: &Connection) -> rusqlite::Result<(u64, [u8; 32])> {
    let row: Option<(i64, Vec<u8>)> = conn
        .query_row(
            "SELECT seq, entry_hash FROM audit_logs ORDER BY seq DESC LIMIT 1",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .optional()?;
    Ok(match row {
        Some((seq, hash)) => (seq as u64, blob32(&hash)),
        None => (0, GENESIS_PREV_HASH),
    })
}

/// Append one entry inside `tx` — atomically with the state change it records.
/// Returns the assigned `seq`.
pub fn append_in_tx(
    tx: &Transaction<'_>,
    timestamp_unix: i64,
    action: &str,
    request_digest: &[u8; 32],
    decision: &str,
    reason: &str,
) -> rusqlite::Result<u64> {
    let (prev_seq, prev_hash): (i64, Vec<u8>) = tx
        .query_row(
            "SELECT seq, entry_hash FROM audit_logs ORDER BY seq DESC LIMIT 1",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .optional()?
        .unwrap_or((0, GENESIS_PREV_HASH.to_vec()));
    let seq = (prev_seq as u64) + 1;
    let prev = blob32(&prev_hash);

    let mut entry = AuditEntry {
        seq,
        timestamp_unix,
        action: action.to_string(),
        request_digest: *request_digest,
        decision: decision.to_string(),
        reason: reason.to_string(),
        prev_hash: prev,
        entry_hash: [0u8; 32],
    };
    entry.entry_hash = entry_hash(&entry);

    tx.execute(
        "INSERT INTO audit_logs(
            seq, timestamp_unix, action, request_digest,
            decision, reason, prev_hash, entry_hash)
         VALUES(?1,?2,?3,?4,?5,?6,?7,?8)",
        params![
            seq as i64,
            timestamp_unix,
            entry.action,
            request_digest.as_slice(),
            entry.decision,
            entry.reason,
            entry.prev_hash.as_slice(),
            entry.entry_hash.as_slice(),
        ],
    )?;
    Ok(seq)
}

/// Result of a chain verification pass.
#[derive(Debug, Clone)]
pub struct ChainVerification {
    pub valid: bool,
    pub entries_checked: u64,
    /// First seq whose stored prev_hash/entry_hash fails verification.
    pub first_bad_seq: u64,
    /// `entry_hash` of the last entry examined (tip), or the expected
    /// prev_hash at range start when the range is empty.
    pub tip_hash: [u8; 32],
}

/// Recompute the chain in `audit_logs` over `[from_seq, to_seq]`
/// (0/0 = full range). Verifies, per row, both the stored `prev_hash` link and
/// the recomputed `entry_hash` — detecting edits, deletions and insertions.
pub fn verify_chain(
    conn: &Connection,
    from_seq: u64,
    to_seq: u64,
) -> anyhow::Result<ChainVerification> {
    let start = if from_seq == 0 { 1 } else { from_seq };

    // Expected prev_hash at range start: genesis zeros for seq==1, otherwise
    // the stored entry_hash of row `start-1` (absent -> verification of the
    // range can't establish linkage; treat as bad from the first row).
    let mut expected_prev = if start <= 1 {
        GENESIS_PREV_HASH
    } else {
        match conn
            .query_row(
                "SELECT entry_hash FROM audit_logs WHERE seq=?1",
                params![(start - 1) as i64],
                |r| r.get::<_, Vec<u8>>(0),
            )
            .optional()?
        {
            Some(h) => blob32(&h),
            // No predecessor row — anchor impossible.
            None => {
                return Ok(ChainVerification {
                    valid: false,
                    entries_checked: 0,
                    first_bad_seq: start,
                    tip_hash: [0u8; 32],
                });
            }
        }
    };

    let mut stmt = conn.prepare(
        "SELECT seq, timestamp_unix, action, request_digest, decision, reason,
                prev_hash, entry_hash
         FROM audit_logs
         WHERE seq >= ?1 AND (?2 = 0 OR seq <= ?2)
         ORDER BY seq ASC",
    )?;

    let rows = stmt.query_map(params![start as i64, to_seq as i64], |r| {
        Ok((
            r.get::<_, i64>(0)?,
            r.get::<_, i64>(1)?,
            r.get::<_, String>(2)?,
            r.get::<_, Vec<u8>>(3)?,
            r.get::<_, String>(4)?,
            r.get::<_, String>(5)?,
            r.get::<_, Vec<u8>>(6)?,
            r.get::<_, Vec<u8>>(7)?,
        ))
    })?;

    let mut checked = 0u64;
    let mut first_bad = 0u64;
    let mut tip_hash = expected_prev;
    for row in rows {
        let (seq, ts, action, digest, decision, reason, prev, hash) = row?;
        let seq = seq as u64;
        checked += 1;
        let prev_a = blob32(&prev);
        let digest_a = blob32(&digest);
        let stored_hash = blob32(&hash);

        let mut entry = AuditEntry {
            seq,
            timestamp_unix: ts,
            action,
            request_digest: digest_a,
            decision,
            reason,
            prev_hash: prev_a,
            entry_hash: [0u8; 32],
        };
        entry.entry_hash = entry_hash(&entry);

        let link_ok = prev_a == expected_prev;
        let hash_ok = entry.entry_hash == stored_hash;
        if !(link_ok && hash_ok) {
            first_bad = seq;
            break;
        }
        expected_prev = stored_hash;
        tip_hash = stored_hash;
    }

    Ok(ChainVerification {
        valid: first_bad == 0,
        entries_checked: checked,
        first_bad_seq: first_bad,
        tip_hash,
    })
}

/// Convenience: sha256 of an arbitrary request blob for `request_digest`.
pub fn request_digest(bytes: &[u8]) -> [u8; 32] {
    let out = Sha256::digest(bytes);
    let mut a = [0u8; 32];
    a.copy_from_slice(&out);
    a
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::db;

    fn db_with_entries(n: usize) -> db::SignerDb {
        let mut db = db::open_in_memory().unwrap();
        let tx = db.conn.transaction().unwrap();
        for i in 0..n {
            append_in_tx(
                &tx,
                1_700_000_000 + i as i64,
                actions::SIGN_TRANSACTION,
                &request_digest(format!("req-{i}").as_bytes()),
                decisions::APPROVED,
                &format!("policy=PAYOUT tx_hash=0x{i:064x}"),
            )
            .unwrap();
        }
        tx.commit().unwrap();
        db
    }

    #[test]
    fn empty_chain_verifies() {
        let db = db::open_in_memory().unwrap();
        let v = verify_chain(&db.conn, 0, 0).unwrap();
        assert!(v.valid);
        assert_eq!(v.entries_checked, 0);
        assert_eq!(v.tip_hash, GENESIS_PREV_HASH);
    }

    #[test]
    fn healthy_chain_verifies() {
        let db = db_with_entries(5);
        let v = verify_chain(&db.conn, 0, 0).unwrap();
        assert!(v.valid);
        assert_eq!(v.entries_checked, 5);
        let (tip_seq, tip) = tip(&db.conn).unwrap();
        assert_eq!(tip_seq, 5);
        assert_eq!(v.tip_hash, tip);
    }

    #[test]
    fn tampered_row_detected() {
        let db = db_with_entries(5);
        // Edit the reason of seq=3 — entry_hash no longer matches.
        db.conn
            .execute("UPDATE audit_logs SET reason='forged' WHERE seq=3", [])
            .unwrap();
        let v = verify_chain(&db.conn, 0, 0).unwrap();
        assert!(!v.valid);
        assert_eq!(v.first_bad_seq, 3);
        assert_eq!(v.entries_checked, 3); // stopped at first bad row
    }

    #[test]
    fn deleted_middle_row_detected() {
        let db = db_with_entries(5);
        db.conn.execute("DELETE FROM audit_logs WHERE seq=3", []).unwrap();
        let v = verify_chain(&db.conn, 0, 0).unwrap();
        assert!(!v.valid);
        assert_eq!(v.first_bad_seq, 4); // link into seq=4 broken
    }

    #[test]
    fn tampered_prev_link_detected() {
        let db = db_with_entries(4);
        db.conn
            .execute("UPDATE audit_logs SET prev_hash=X'FF' WHERE seq=2", [])
            .unwrap();
        let v = verify_chain(&db.conn, 0, 0).unwrap();
        assert!(!v.valid);
        assert_eq!(v.first_bad_seq, 2);
    }

    #[test]
    fn range_verification() {
        let db = db_with_entries(10);
        let v = verify_chain(&db.conn, 5, 7).unwrap();
        assert!(v.valid);
        assert_eq!(v.entries_checked, 3);
    }
}
