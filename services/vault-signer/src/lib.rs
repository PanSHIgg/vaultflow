//! vault-signer — VaultFlow's process-isolated signing vault.
//!
//! Trust boundary (ARCHITECTURE §1.1): gateway-core is an untrusted business
//! zone holding zero key material; this crate enforces the four self-proving
//! transaction policies against locally-decoded EIP-1559 bytes, signer-local
//! SQLite state and offline admin Ed25519 proofs — never request metadata.

pub mod audit;
pub mod crypto;
pub mod db;
pub mod grpc;
pub mod kms;
pub mod policy;
