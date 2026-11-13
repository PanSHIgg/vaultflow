//! KEK/KMS boundary — decrypts AES-256-GCM envelope-encrypted DEKs.
//!
//! The `KmsProvider` trait is the single seam between key custody and the rest
//! of the signer: dev uses a controlled local credential file; production swaps
//! in AWS KMS / Vault Transit behind the same trait (ARCHITECTURE §1.1, §9).

use std::path::Path;

use secrecy::SecretVec;

/// Abstracts KEK custody. `decrypt_dek` returns plaintext key material wrapped
/// in `SecretVec` (zeroized on drop) — callers must keep it inside
/// secrecy/zeroize types end-to-end and lock pages via mlock/VirtualLock.
pub trait KmsProvider: Send + Sync {
    /// Open an AES-256-GCM envelope (nonce || ciphertext || tag) into a DEK.
    fn decrypt_dek(&self, ciphertext: &[u8]) -> anyhow::Result<SecretVec<u8>>;
}

/// Dev provider: KEK injected from a controlled credential file (W2-1).
pub struct LocalFileKms {
    kek: SecretVec<u8>,
}

impl LocalFileKms {
    /// Load the 32-byte KEK from `path` (file perms checked by caller SOP).
    pub fn from_kek_file(_path: &Path) -> anyhow::Result<Self> {
        // TODO(W2-1): read KEK, wrap in SecretVec, zeroize the source buffer.
        anyhow::bail!("LocalFileKms::from_kek_file not implemented (W2-1)")
    }
}

impl KmsProvider for LocalFileKms {
    fn decrypt_dek(&self, _ciphertext: &[u8]) -> anyhow::Result<SecretVec<u8>> {
        let _ = &self.kek; // field is live — real impl below lands in W2-1
        // TODO(W2-1): aes_gcm::Aes256Gcm::open(nonce || ct || tag); plaintext
        //             lands directly into SecretVec + mlock'd page.
        anyhow::bail!("decrypt_dek not implemented (W2-1)")
    }
}
