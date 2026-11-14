//! KEK/KMS boundary — decrypts AES-256-GCM envelope-encrypted DEKs.
//!
//! The `KmsProvider` trait is the single seam between key custody and the rest
//! of the signer: dev uses a controlled local credential file; production swaps
//! in AWS KMS / Vault Transit behind the same trait (ARCHITECTURE §1.1, §9).
//!
//! Envelope format (frozen): `nonce(12B) || ciphertext || GCM tag(16B)`.
//! Plaintext DEKs never leave secrecy/zeroize types; the [`LockedSecret`]
//! wrapper additionally pins the holding page(s) with `mlock`/`VirtualLock`
//! so hot key material can never be swapped to disk.

use std::path::Path;

use aes_gcm::aead::{Aead, KeyInit};
use aes_gcm::{Aes256Gcm, Key, Nonce};
use rand::rngs::OsRng;
use rand::RngCore;
use secrecy::{ExposeSecret, SecretVec};
use zeroize::Zeroize;

/// Abstracts KEK custody. `decrypt_dek` returns plaintext key material wrapped
/// in `SecretVec` (zeroized on drop) — callers must keep it inside
/// secrecy/zeroize types end-to-end and lock pages via mlock/VirtualLock.
pub trait KmsProvider: Send + Sync {
    /// Open an AES-256-GCM envelope (nonce || ciphertext || tag) into a DEK.
    fn decrypt_dek(&self, ciphertext: &[u8]) -> anyhow::Result<SecretVec<u8>>;
}

/// AES-GCM nonce length (96-bit, generated fresh per envelope).
const NONCE_LEN: usize = 12;
/// AES-256 key length.
const KEK_LEN: usize = 32;

/// Dev provider: KEK injected from a controlled credential file.
///
/// The file holds the 32-byte KEK either as raw bytes or as (optionally
/// `0x`-prefixed) 64-char lowercase/uppercase hex. File permission checks
/// (`chmod 600`, ownership) are the deploy SOP's responsibility — the signer
/// refuses nothing here but logs no key material ever.
pub struct LocalFileKms {
    kek: SecretVec<u8>,
}

impl LocalFileKms {
    /// Load the 32-byte KEK from `path`.
    pub fn from_kek_file(path: &Path) -> anyhow::Result<Self> {
        let mut raw = std::fs::read(path)
            .map_err(|e| anyhow::anyhow!("read KEK file {}: {e}", path.display()))?;
        let parsed = parse_kek_bytes(&raw);
        raw.zeroize();
        let kek = match parsed {
            Ok(k) => k,
            Err(e) => anyhow::bail!("malformed KEK in {}: {e}", path.display()),
        };
        Ok(Self { kek })
    }

    /// Build directly from key bytes (tests / provisioning tooling).
    pub fn from_kek_bytes(kek: &[u8; KEK_LEN]) -> Self {
        Self { kek: SecretVec::new(kek.to_vec()) }
    }

    /// Create an envelope (`nonce || ct || tag`) for `plaintext_dek` — used by
    /// provisioning tooling and tests to seed `encrypted_keys`.
    pub fn encrypt_dek(&self, plaintext_dek: &[u8]) -> anyhow::Result<Vec<u8>> {
        let cipher = self.cipher();
        let mut nonce = [0u8; NONCE_LEN];
        OsRng.fill_bytes(&mut nonce);
        let ct = cipher
            .encrypt(Nonce::from_slice(&nonce), plaintext_dek)
            .map_err(|_| anyhow::anyhow!("AES-256-GCM encrypt failed"))?;
        let mut out = Vec::with_capacity(NONCE_LEN + ct.len());
        out.extend_from_slice(&nonce);
        out.extend_from_slice(&ct);
        Ok(out)
    }

    fn cipher(&self) -> Aes256Gcm {
        let key = Key::<Aes256Gcm>::from_slice(self.kek.expose_secret());
        Aes256Gcm::new(key)
    }
}

/// Accept raw-32 or hex-64 KEK material; hex may carry a `0x` prefix.
fn parse_kek_bytes(raw: &[u8]) -> anyhow::Result<SecretVec<u8>> {
    let trimmed: &[u8] = {
        let r = raw;
        let start = r.iter().position(|b| !b.is_ascii_whitespace()).unwrap_or(r.len());
        let end = r.iter().rposition(|b| !b.is_ascii_whitespace()).map_or(start, |i| i + 1);
        &r[start..end]
    };
    if trimmed.len() == KEK_LEN {
        return Ok(SecretVec::new(trimmed.to_vec()));
    }
    let hex_part = trimmed.strip_prefix(b"0x").or(trimmed.strip_prefix(b"0X")).unwrap_or(trimmed);
    if hex_part.len() == 64 && hex_part.iter().all(|b| b.is_ascii_hexdigit()) {
        // Ownership transfers into the SecretVec — single buffer, zeroized on drop.
        return Ok(SecretVec::new(hex::decode(hex_part)?));
    }
    anyhow::bail!("expected 32 raw bytes or 64 hex chars, got {} bytes", trimmed.len())
}

impl KmsProvider for LocalFileKms {
    fn decrypt_dek(&self, ciphertext: &[u8]) -> anyhow::Result<SecretVec<u8>> {
        if ciphertext.len() < NONCE_LEN + 16 {
            anyhow::bail!(
                "envelope too short: {} < {}",
                ciphertext.len(),
                NONCE_LEN + 16
            );
        }
        let (nonce, ct) = ciphertext.split_at(NONCE_LEN);
        let plain = self
            .cipher()
            .decrypt(Nonce::from_slice(nonce), ct)
            .map_err(|_| anyhow::anyhow!("AES-256-GCM open failed (bad KEK or tampered envelope)"))?;
        Ok(SecretVec::new(plain))
    }
}

// ---------------------------------------------------------------------------
// Page-locked secret memory (mlock / VirtualLock)
// ---------------------------------------------------------------------------

/// A `SecretVec` whose backing page(s) are locked against swap for its entire
/// lifetime. Lock failure is logged but non-fatal (dev shells may lack
/// CAP_IPC_LOCK / sufficient `ulimit -l`); the zeroize-on-drop contract is
/// unaffected either way.
pub struct LockedSecret {
    inner: SecretVec<u8>,
}

impl LockedSecret {
    /// Lock `secret`'s pages and adopt it. The lock is released on drop,
    /// immediately after the buffer is zeroized.
    pub fn new(secret: SecretVec<u8>) -> Self {
        let buf = secret.expose_secret();
        if !buf.is_empty() {
            lock_pages(buf.as_ptr(), buf.len());
        }
        Self { inner: secret }
    }

    /// Borrow the plaintext (still inside this RAII guard's lifetime).
    pub fn expose_secret(&self) -> &[u8] {
        self.inner.expose_secret()
    }
}

impl std::fmt::Debug for LockedSecret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("LockedSecret([REDACTED])")
    }
}

impl Drop for LockedSecret {
    fn drop(&mut self) {
        // Replace the secret with an empty buffer first: the moved-out
        // `SecretVec` zeroizes the old allocation when it drops at the end of
        // this scope, *before* we release the page lock below.
        let old = std::mem::replace(&mut self.inner, SecretVec::new(Vec::new()));
        let (ptr, len) = {
            let b = old.expose_secret();
            (b.as_ptr() as usize, b.len())
        };
        drop(old); // zeroize happens here
        if len > 0 {
            unlock_pages(ptr as *const u8, len);
        }
    }
}

#[cfg(unix)]
fn lock_pages(ptr: *const u8, len: usize) {
    let rc = unsafe { libc::mlock(ptr as *const libc::c_void, len) };
    if rc != 0 {
        tracing::warn!("mlock failed (needs CAP_IPC_LOCK / ulimit -l) — key page may swap");
    }
}

#[cfg(unix)]
fn unlock_pages(ptr: *const u8, len: usize) {
    unsafe {
        libc::munlock(ptr as *const libc::c_void, len);
    }
}

#[cfg(windows)]
fn lock_pages(ptr: *const u8, len: usize) {
    let ok = unsafe {
        windows_sys::Win32::System::Memory::VirtualLock(ptr as *mut core::ffi::c_void, len)
    };
    if ok == 0 {
        tracing::warn!("VirtualLock failed — key page may swap");
    }
}

#[cfg(windows)]
fn unlock_pages(ptr: *const u8, len: usize) {
    unsafe {
        windows_sys::Win32::System::Memory::VirtualUnlock(ptr as *mut core::ffi::c_void, len);
    }
}

#[cfg(not(any(unix, windows)))]
fn lock_pages(_ptr: *const u8, _len: usize) {}

#[cfg(not(any(unix, windows)))]
fn unlock_pages(_ptr: *const u8, _len: usize) {}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    fn kek() -> [u8; 32] {
        [0x5Au8; 32]
    }

    #[test]
    fn envelope_roundtrip() {
        let kms = LocalFileKms::from_kek_bytes(&kek());
        let dek = [0x42u8; 32];
        let env = kms.encrypt_dek(&dek).unwrap();
        assert_eq!(env.len(), 12 + 32 + 16);
        let opened = kms.decrypt_dek(&env).unwrap();
        assert_eq!(opened.expose_secret().as_slice(), &dek);
    }

    #[test]
    fn envelope_rejects_tamper_and_wrong_kek() {
        let kms = LocalFileKms::from_kek_bytes(&kek());
        let mut env = kms.encrypt_dek(&[7u8; 32]).unwrap();
        env[20] ^= 1; // flip a ciphertext bit
        assert!(kms.decrypt_dek(&env).is_err());

        let other = LocalFileKms::from_kek_bytes(&[0xA5u8; 32]);
        let env2 = kms.encrypt_dek(&[7u8; 32]).unwrap();
        assert!(other.decrypt_dek(&env2).is_err());
    }

    #[test]
    fn kek_file_parses_hex_and_raw() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("kek.hex");
        std::fs::write(&p, format!("0x{}", hex::encode(kek()))).unwrap();
        let kms = LocalFileKms::from_kek_file(&p).unwrap();
        let env = kms.encrypt_dek(&[9u8; 32]).unwrap();
        assert_eq!(kms.decrypt_dek(&env).unwrap().expose_secret().as_slice(), &[9u8; 32]);

        let p2 = dir.path().join("kek.bin");
        std::fs::write(&p2, kek()).unwrap();
        let kms2 = LocalFileKms::from_kek_file(&p2).unwrap();
        assert_eq!(kms2.decrypt_dek(&env).unwrap().expose_secret().as_slice(), &[9u8; 32]);

        let p3 = dir.path().join("bad");
        std::fs::write(&p3, b"too short").unwrap();
        assert!(LocalFileKms::from_kek_file(&p3).is_err());
    }

    #[test]
    fn locked_secret_exposes_and_drops() {
        let locked = LockedSecret::new(SecretVec::new(vec![0xABu8; 32]));
        assert_eq!(locked.expose_secret(), &[0xABu8; 32]);
        drop(locked); // zeroize + munlock exercised
    }
}
