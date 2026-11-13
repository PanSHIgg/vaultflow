//! Local EIP-2718 Type-2 (EIP-1559) decoding + k256 signing primitives.
//!
//! ZERO-TRUST: the signer acts only on fields decoded here from the raw
//! `0x02 || rlp(...)` payload — never on plaintext metadata in the request.

use alloy_primitives::{Address, B256, U256};
use secrecy::SecretVec;

pub use alloy_primitives::keccak256;

/// A Type-2 unsigned transaction decoded locally (P0-1 §3 encoding).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DecodedUnsignedTx {
    pub chain_id: u64,
    pub nonce: u64,
    pub max_priority_fee_per_gas: U256,
    pub max_fee_per_gas: U256,
    pub gas_limit: u64,
    pub to: Address,
    pub value: U256,
    pub data: Vec<u8>,
}

impl DecodedUnsignedTx {
    /// keccak256 over the raw unsigned payload — the k256 sighash.
    pub fn sighash(&self) -> B256 {
        // TODO(W2-1): keccak256(0x02 || rlp(unsigned_fields)).
        unimplemented!("sighash: W2-1")
    }

    /// GAP_FILLER structural test: to == from && value == 0 && data.is_empty()
    /// && gas_limit == 21_000.
    pub fn is_gap_filler(&self, _from: &Address) -> bool {
        // TODO(W2-3): implement per ARCHITECTURE §1.2 GAP_FILLER_POLICY.
        unimplemented!("is_gap_filler: W2-3")
    }
}

/// Decode `0x02 || rlp([chainId, nonce, tipCap, feeCap, gasLimit, to, value,
/// data, accessList])`. Errors map to proto `RejectCode::MALFORMED_TX`.
pub fn decode_unsigned_eip1559(_raw: &[u8]) -> anyhow::Result<DecodedUnsignedTx> {
    // TODO(W2-1): minimal hand-rolled RLP parser — list arity, `to` exactly 20B,
    //             canonical (minimal-length) integers, accessList handling.
    anyhow::bail!("decode_unsigned_eip1559 not implemented (W2-1)")
}

/// Sign `tx` with a hot-wallet DEK (secp256k1/k256) and return
/// (signed `0x02`-typed raw tx, tx_hash = keccak256(raw)). `dek` must remain
/// inside secrecy/zeroize types end-to-end (W2-1 memory hygiene).
pub fn sign_eip1559(_tx: &DecodedUnsignedTx, dek: &SecretVec<u8>) -> anyhow::Result<(Vec<u8>, B256)> {
    // TODO(W2-1): k256 ecdsa sign(sighash) -> yParity -> assemble signed RLP.
    let _ = dek;
    anyhow::bail!("sign_eip1559 not implemented (W2-1)")
}
