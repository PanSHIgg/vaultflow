//! Local EIP-2718 Type-2 (EIP-1559) decoding + k256 signing primitives.
//!
//! ZERO-TRUST: the signer acts only on fields decoded here from the raw
//! `0x02 || rlp(...)` payload — never on plaintext metadata in the request.
//!
//! Frozen encoding (AGENTS.md §2.4):
//!   unsigned_tx = 0x02 || rlp([chainId, nonce, maxPriorityFeePerGas,
//!                              maxFeePerGas, gasLimit, to, value, data,
//!                              accessList])
//!   signed_tx   = 0x02 || rlp([...same 9 fields..., yParity, r, s])
//!   sighash     = keccak256(unsigned_tx)
//!   tx_hash     = keccak256(signed_tx)
//!
//! The RLP decoder is deliberately strict: canonical (minimal-length)
//! integers, exact 9-field list arity, no trailing bytes. Anything else maps
//! to proto `RejectCode::MALFORMED_TX` at the boundary.

use alloy_primitives::{Address, B256, U256};
use secrecy::{ExposeSecret, SecretVec};
use zeroize::Zeroize;

pub use alloy_primitives::keccak256;

/// EIP-2718 transaction type byte for EIP-1559 (Type-2).
pub const TX_TYPE_EIP1559: u8 = 0x02;

/// Max RLP nesting depth we will ever parse (tx list -> access list -> key
/// list). Guards against stack exhaustion on adversarial payloads.
const MAX_RLP_DEPTH: usize = 8;

// ---------------------------------------------------------------------------
// Minimal canonical RLP codec
// ---------------------------------------------------------------------------

/// A decoded RLP item.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RlpItem {
    /// Byte string item (includes integers — canonical form enforced on read).
    Bytes(Vec<u8>),
    /// List item.
    List(Vec<RlpItem>),
}

#[derive(Debug, thiserror::Error)]
pub enum RlpError {
    #[error("truncated RLP input")]
    Truncated,
    #[error("RLP length prefix overruns input")]
    LengthOverflow,
    #[error("RLP nesting depth exceeded")]
    DepthExceeded,
    #[error("trailing bytes after RLP item")]
    TrailingBytes,
    #[error("expected RLP list")]
    ExpectedList,
    #[error("expected RLP byte string")]
    ExpectedBytes,
    #[error("non-canonical RLP integer (leading zero / non-minimal length)")]
    NonCanonicalInteger,
    #[error("RLP list arity mismatch (expected {expected}, got {actual})")]
    Arity { expected: usize, actual: usize },
}

/// Decode exactly one RLP item and require full input consumption.
pub fn rlp_decode(buf: &[u8]) -> Result<RlpItem, RlpError> {
    let mut pos = 0usize;
    let item = rlp_decode_item(buf, &mut pos, 0)?;
    if pos != buf.len() {
        return Err(RlpError::TrailingBytes);
    }
    Ok(item)
}

fn rlp_decode_item(buf: &[u8], pos: &mut usize, depth: usize) -> Result<RlpItem, RlpError> {
    if depth > MAX_RLP_DEPTH {
        return Err(RlpError::DepthExceeded);
    }
    let first = *buf.get(*pos).ok_or(RlpError::Truncated)?;
    *pos += 1;
    match first {
        // Single byte < 0x80 encodes itself.
        0x00..=0x7f => Ok(RlpItem::Bytes(vec![first])),
        // Short string: 0x80 + len, len in 0..=55.
        0x80..=0xb7 => {
            let len = (first - 0x80) as usize;
            let bytes = take(buf, pos, len)?;
            Ok(RlpItem::Bytes(bytes.to_vec()))
        }
        // Long string: 0xb7 + len-of-len, then len, then payload.
        0xb8..=0xbf => {
            let len_of_len = (first - 0xb7) as usize;
            let len = read_be_uint(buf, pos, len_of_len)?;
            if len <= 55 {
                return Err(RlpError::NonCanonicalInteger);
            }
            let bytes = take(buf, pos, len)?;
            Ok(RlpItem::Bytes(bytes.to_vec()))
        }
        // Short list.
        0xc0..=0xf7 => {
            let len = (first - 0xc0) as usize;
            let payload = take(buf, pos, len)?;
            let mut sub_pos = 0usize;
            let mut items = Vec::new();
            while sub_pos < payload.len() {
                items.push(rlp_decode_item(payload, &mut sub_pos, depth + 1)?);
            }
            debug_assert_eq!(sub_pos, payload.len());
            Ok(RlpItem::List(items))
        }
        // Long list.
        0xf8..=0xff => {
            let len_of_len = (first - 0xf7) as usize;
            let len = read_be_uint(buf, pos, len_of_len)?;
            if len <= 55 {
                return Err(RlpError::NonCanonicalInteger);
            }
            let payload = take(buf, pos, len)?;
            let mut sub_pos = 0usize;
            let mut items = Vec::new();
            while sub_pos < payload.len() {
                items.push(rlp_decode_item(payload, &mut sub_pos, depth + 1)?);
            }
            debug_assert_eq!(sub_pos, payload.len());
            Ok(RlpItem::List(items))
        }
    }
}

fn take<'a>(buf: &'a [u8], pos: &mut usize, len: usize) -> Result<&'a [u8], RlpError> {
    let end = pos.checked_add(len).ok_or(RlpError::LengthOverflow)?;
    let slice = buf.get(*pos..end).ok_or(RlpError::LengthOverflow)?;
    *pos = end;
    Ok(slice)
}

/// Read a big-endian unsigned length field of `len` bytes (1..=8).
fn read_be_uint(buf: &[u8], pos: &mut usize, len: usize) -> Result<usize, RlpError> {
    if len == 0 || len > 8 {
        return Err(RlpError::LengthOverflow);
    }
    let bytes = take(buf, pos, len)?;
    // Canonical: length-of-length fields must not carry leading zeros.
    if bytes[0] == 0 {
        return Err(RlpError::NonCanonicalInteger);
    }
    let mut v = 0usize;
    for &b in bytes {
        v = (v << 8) | b as usize;
    }
    Ok(v)
}

/// Encode an RLP item (canonical).
pub fn rlp_encode(item: &RlpItem) -> Vec<u8> {
    let mut out = Vec::new();
    encode_into(item, &mut out);
    out
}

fn encode_into(item: &RlpItem, out: &mut Vec<u8>) {
    match item {
        RlpItem::Bytes(b) => {
            if b.len() == 1 && b[0] < 0x80 {
                out.push(b[0]);
            } else {
                encode_header(0x80, b.len(), out);
                out.extend_from_slice(b);
            }
        }
        RlpItem::List(items) => {
            let mut payload = Vec::new();
            for it in items {
                encode_into(it, &mut payload);
            }
            encode_header(0xc0, payload.len(), out);
            out.extend_from_slice(&payload);
        }
    }
}

fn encode_header(offset: u8, len: usize, out: &mut Vec<u8>) {
    if len <= 55 {
        out.push(offset + len as u8);
    } else {
        let be = len.to_be_bytes();
        let start = be.iter().position(|&b| b != 0).unwrap_or(be.len() - 1);
        let len_bytes = &be[start..];
        out.push(offset + 0x37 + len_bytes.len() as u8);
        out.extend_from_slice(len_bytes);
    }
}

/// Minimal-length big-endian RLP integer encoding (`0` -> empty string).
fn uint_item(v: &U256) -> RlpItem {
    if v.is_zero() {
        return RlpItem::Bytes(Vec::new());
    }
    let be: [u8; 32] = v.to_be_bytes::<32>();
    let start = be.iter().position(|&b| b != 0).unwrap_or(31);
    RlpItem::Bytes(be[start..].to_vec())
}

fn u64_item(v: u64) -> RlpItem {
    uint_item(&U256::from(v))
}

fn bytes_item(b: &[u8]) -> RlpItem {
    RlpItem::Bytes(b.to_vec())
}

// ---------------------------------------------------------------------------
// EIP-1559 Type-2 unsigned transaction
// ---------------------------------------------------------------------------

/// One access-list entry `(address, [storage_keys])`.
pub type AccessListEntry = (Address, Vec<B256>);

/// A Type-2 unsigned transaction decoded locally (AGENTS §2.4 encoding).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DecodedUnsignedTx {
    /// The verbatim `0x02 || rlp(...)` payload — sighash input.
    pub raw: Vec<u8>,
    pub chain_id: u64,
    pub nonce: u64,
    pub max_priority_fee_per_gas: U256,
    pub max_fee_per_gas: U256,
    pub gas_limit: u64,
    pub to: Address,
    pub value: U256,
    pub data: Vec<u8>,
    /// Locally re-decoded access list (always `[]` per the frozen contract,
    /// but parsed and re-encoded faithfully so any well-formed list signs).
    pub access_list: Vec<AccessListEntry>,
}

impl DecodedUnsignedTx {
    /// keccak256 over the raw unsigned payload — the k256 sighash.
    pub fn sighash(&self) -> B256 {
        keccak256(&self.raw)
    }

    /// GAP_FILLER structural test: to == from && value == 0 && data.is_empty()
    /// && gas_limit == 21_000.
    pub fn is_gap_filler(&self, from: &Address) -> bool {
        self.to == *from
            && self.value.is_zero()
            && self.data.is_empty()
            && self.gas_limit == 21_000
    }

    /// The 9 unsigned fields as RLP items (signing preimage / re-encode).
    fn unsigned_items(&self) -> Vec<RlpItem> {
        vec![
            u64_item(self.chain_id),
            u64_item(self.nonce),
            uint_item(&self.max_priority_fee_per_gas),
            uint_item(&self.max_fee_per_gas),
            u64_item(self.gas_limit),
            bytes_item(self.to.as_slice()),
            uint_item(&self.value),
            bytes_item(&self.data),
            access_list_item(&self.access_list),
        ]
    }
}

fn access_list_item(entries: &[AccessListEntry]) -> RlpItem {
    RlpItem::List(
        entries
            .iter()
            .map(|(addr, keys)| {
                RlpItem::List(vec![
                    bytes_item(addr.as_slice()),
                    RlpItem::List(keys.iter().map(|k| bytes_item(k.as_slice())).collect()),
                ])
            })
            .collect(),
    )
}

/// Interpret a decoded `Bytes` item as a canonical unsigned integer.
/// `max_bytes` bounds the field width (8 for u64, 32 for U256).
fn item_to_uint(item: &RlpItem, max_bytes: usize) -> Result<U256, RlpError> {
    let bytes = match item {
        RlpItem::Bytes(b) => b,
        _ => return Err(RlpError::ExpectedBytes),
    };
    if bytes.len() > max_bytes {
        return Err(RlpError::NonCanonicalInteger);
    }
    // Minimal-length rule: no leading zero byte. (Integer 0 must be the
    // empty string — a [0x00] payload is non-canonical.)
    if bytes.first() == Some(&0) {
        return Err(RlpError::NonCanonicalInteger);
    }
    Ok(U256::from_be_slice(bytes))
}

/// Decode `0x02 || rlp([chainId, nonce, tipCap, feeCap, gasLimit, to, value,
/// data, accessList])`. Errors map to proto `RejectCode::MALFORMED_TX`.
pub fn decode_unsigned_eip1559(raw: &[u8]) -> anyhow::Result<DecodedUnsignedTx> {
    if raw.first() != Some(&TX_TYPE_EIP1559) {
        anyhow::bail!("missing EIP-2718 type byte 0x02");
    }
    let top = rlp_decode(&raw[1..]).map_err(|e| anyhow::anyhow!("RLP: {e}"))?;
    let fields = match &top {
        RlpItem::List(items) => items,
        _ => return Err(anyhow::anyhow!(RlpError::ExpectedList.to_string())),
    };
    if fields.len() != 9 {
        return Err(anyhow::anyhow!(
            "{}",
            RlpError::Arity { expected: 9, actual: fields.len() }
        ));
    }

    let chain_id_u = item_to_uint(&fields[0], 8)?;
    let nonce_u = item_to_uint(&fields[1], 8)?;
    let tip = item_to_uint(&fields[2], 32)?;
    let fee = item_to_uint(&fields[3], 32)?;
    let gas_u = item_to_uint(&fields[4], 8)?;
    let to_bytes = match &fields[5] {
        RlpItem::Bytes(b) => b,
        _ => anyhow::bail!("{}", RlpError::ExpectedBytes),
    };
    if to_bytes.len() != 20 {
        // Contract creation (empty `to`) is outside every signer policy.
        anyhow::bail!("`to` must be a 20-byte address (got {})", to_bytes.len());
    }
    let value = item_to_uint(&fields[6], 32)?;
    let data = match &fields[7] {
        RlpItem::Bytes(b) => b.clone(),
        _ => anyhow::bail!("{}", RlpError::ExpectedBytes),
    };
    let access_list = decode_access_list(&fields[8])?;

    Ok(DecodedUnsignedTx {
        raw: raw.to_vec(),
        chain_id: u64::try_from(chain_id_u).map_err(|_| anyhow::anyhow!("chain_id > u64"))?,
        nonce: u64::try_from(nonce_u).map_err(|_| anyhow::anyhow!("nonce > u64"))?,
        max_priority_fee_per_gas: tip,
        max_fee_per_gas: fee,
        gas_limit: u64::try_from(gas_u).map_err(|_| anyhow::anyhow!("gas_limit > u64"))?,
        to: Address::from_slice(to_bytes),
        value,
        data,
        access_list,
    })
}

fn decode_access_list(item: &RlpItem) -> anyhow::Result<Vec<AccessListEntry>> {
    let entries = match item {
        RlpItem::List(items) => items,
        _ => anyhow::bail!("accessList must be a list"),
    };
    let mut out = Vec::with_capacity(entries.len());
    for entry in entries {
        let pair = match entry {
            RlpItem::List(p) if p.len() == 2 => p,
            _ => anyhow::bail!("accessList entry must be a 2-item list"),
        };
        let addr_bytes = match &pair[0] {
            RlpItem::Bytes(b) if b.len() == 20 => b,
            _ => anyhow::bail!("accessList address must be 20 bytes"),
        };
        let keys = match &pair[1] {
            RlpItem::List(k) => k,
            _ => anyhow::bail!("accessList keys must be a list"),
        };
        let mut storage_keys = Vec::with_capacity(keys.len());
        for k in keys {
            match k {
                RlpItem::Bytes(b) if b.len() == 32 => {
                    storage_keys.push(B256::from_slice(b));
                }
                _ => anyhow::bail!("accessList storage key must be 32 bytes"),
            }
        }
        out.push((Address::from_slice(addr_bytes), storage_keys));
    }
    Ok(out)
}

/// Sign `tx` with a hot-wallet DEK (secp256k1/k256) and return
/// (signed `0x02`-typed raw tx, tx_hash = keccak256(raw)).
///
/// `dek` must remain inside secrecy/zeroize types end-to-end (W2-1 memory
/// hygiene). The DEK itself is never copied out: only the k256 SigningKey
/// (which owns its secret via its own Drop-zeroizing scalar) is materialized.
pub fn sign_eip1559(
    tx: &DecodedUnsignedTx,
    dek: &SecretVec<u8>,
) -> anyhow::Result<(Vec<u8>, B256)> {
    use k256::ecdsa::SigningKey;

    let mut key_bytes = dek.expose_secret().to_vec();
    let sk_result = SigningKey::from_slice(&key_bytes);
    key_bytes.zeroize();
    let sk = sk_result.map_err(|e| anyhow::anyhow!("invalid secp256k1 DEK: {e}"))?;

    sign_with_key(tx, &sk)
}

/// Sign with an already-materialized `SigningKey` (shared by sign/recover
/// paths and tests).
pub fn sign_with_key(
    tx: &DecodedUnsignedTx,
    sk: &k256::ecdsa::SigningKey,
) -> anyhow::Result<(Vec<u8>, B256)> {
    let (sig, recid) = sk
        .sign_prehash_recoverable(tx.sighash().as_slice())
        .map_err(|e| anyhow::anyhow!("k256 prehash sign failed: {e}"))?;

    let y_parity = u64::from(recid.is_y_odd());
    let r = U256::from_be_bytes::<32>(sig.r().to_bytes().into());
    let s = U256::from_be_bytes::<32>(sig.s().to_bytes().into());

    let mut items = tx.unsigned_items();
    items.push(u64_item(y_parity));
    items.push(uint_item(&r));
    items.push(uint_item(&s));

    let mut signed = Vec::with_capacity(tx.raw.len() + 70);
    signed.push(TX_TYPE_EIP1559);
    signed.extend_from_slice(&rlp_encode(&RlpItem::List(items)));
    let hash = keccak256(&signed);
    Ok((signed, hash))
}

/// Derive the EOA address (last-20(keccak256(uncompressed_pubkey[1..]))) of a
/// raw 32-byte secp256k1 secret key. Used at key-load time to prove the DEK
/// really controls the wallet address recorded in `encrypted_keys`.
pub fn address_from_dek(dek: &[u8]) -> anyhow::Result<Address> {
    let sk = k256::ecdsa::SigningKey::from_slice(dek)
        .map_err(|e| anyhow::anyhow!("invalid secp256k1 DEK: {e}"))?;
    Ok(address_from_verifying_key(sk.verifying_key()))
}

/// Address of a k256 verifying key — uncompressed SEC1 point minus its 0x04 tag.
pub fn address_from_verifying_key(vk: &k256::ecdsa::VerifyingKey) -> Address {
    let point = vk.to_encoded_point(false);
    let bytes = point.as_bytes(); // 65B: 0x04 || X || Y
    let hash = keccak256(&bytes[1..]);
    Address::from_slice(&hash[12..])
}

/// Recover the signer address from a signed Type-2 raw tx (diagnostic helper —
/// also useful for inbound forensics and ops tooling).
pub fn recover_signer(signed_raw: &[u8]) -> anyhow::Result<Address> {
    use k256::ecdsa::{RecoveryId, Signature, VerifyingKey};

    if signed_raw.first() != Some(&TX_TYPE_EIP1559) {
        anyhow::bail!("not a Type-2 tx");
    }
    let top = rlp_decode(&signed_raw[1..]).map_err(|e| anyhow::anyhow!("RLP: {e}"))?;
    let fields = match &top {
        RlpItem::List(items) => items,
        _ => anyhow::bail!("expected list"),
    };
    if fields.len() != 12 {
        anyhow::bail!("signed tx must have 12 fields, got {}", fields.len());
    }
    let unsigned_fields = RlpItem::List(fields[..9].to_vec());
    let mut unsigned_raw = vec![TX_TYPE_EIP1559];
    unsigned_raw.extend_from_slice(&rlp_encode(&unsigned_fields));
    let sighash = keccak256(&unsigned_raw);

    let y = item_to_uint(&fields[9], 1)?;
    if y > U256::from(1u64) {
        anyhow::bail!("bad yParity (>1)");
    }
    let r = item_to_uint(&fields[10], 32)?;
    let s = item_to_uint(&fields[11], 32)?;
    let recid = RecoveryId::new(y == U256::from(1u64), false);
    let mut sig_bytes = [0u8; 64];
    sig_bytes[..32].copy_from_slice(&r.to_be_bytes::<32>());
    sig_bytes[32..].copy_from_slice(&s.to_be_bytes::<32>());
    let sig = Signature::from_slice(&sig_bytes)
        .map_err(|e| anyhow::anyhow!("bad signature scalars: {e}"))?;
    let vk = VerifyingKey::recover_from_prehash(sighash.as_slice(), &sig, recid)
        .map_err(|e| anyhow::anyhow!("ecdsa recover: {e}"))?;
    Ok(address_from_verifying_key(&vk))
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    /// The frozen golden vector (GOLDEN_VECTORS.json -> eip2718_unsigned_tx_example):
    /// chainId=31337, nonce=0, tip=1 gwei, maxFee=2 gwei, gas=21000,
    /// to=0x3333..3333, value=0, data=0x, accessList=[].
    const GOLDEN_UNSIGNED: &str =
        "0x02e9827a6980843b9aca0084773594008252089433333333333333333333333333333333333333338080c0";

    fn golden() -> Vec<u8> {
        hex::decode(GOLDEN_UNSIGNED.trim_start_matches("0x")).unwrap()
    }

    #[test]
    fn decodes_golden_unsigned_tx() {
        let raw = golden();
        let tx = decode_unsigned_eip1559(&raw).unwrap();
        assert_eq!(tx.chain_id, 31337);
        assert_eq!(tx.nonce, 0);
        assert_eq!(tx.max_priority_fee_per_gas, U256::from(1_000_000_000u64));
        assert_eq!(tx.max_fee_per_gas, U256::from(2_000_000_000u64));
        assert_eq!(tx.gas_limit, 21_000);
        assert_eq!(tx.to, Address::from([0x33u8; 20]));
        assert!(tx.value.is_zero());
        assert!(tx.data.is_empty());
        assert!(tx.access_list.is_empty());
        assert_eq!(tx.raw, raw);
        assert!(tx.is_gap_filler(&Address::from([0x33u8; 20])));
    }

    #[test]
    fn reencodes_unsigned_preimage_identically() {
        let raw = golden();
        let tx = decode_unsigned_eip1559(&raw).unwrap();
        let mut preimage = vec![TX_TYPE_EIP1559];
        preimage.extend_from_slice(&rlp_encode(&RlpItem::List(tx.unsigned_items())));
        assert_eq!(preimage, raw, "canonical re-encode must round-trip");
    }

    #[test]
    fn rejects_wrong_type_byte() {
        let mut raw = golden();
        raw[0] = 0x01;
        assert!(decode_unsigned_eip1559(&raw).is_err());
    }

    #[test]
    fn rejects_trailing_garbage() {
        let mut raw = golden();
        raw.push(0xde);
        assert!(decode_unsigned_eip1559(&raw).is_err());
    }

    #[test]
    fn rejects_empty_to_contract_creation() {
        // 0x02 || rlp([.., to=0x (empty), ..])
        let raw = hex::decode(
            "02dd827a6980843b9aca008477359400825208808080c0",
        )
        .unwrap();
        assert!(decode_unsigned_eip1559(&raw).is_err());
    }

    #[test]
    fn rejects_noncanonical_integer() {
        // nonce encoded as 0x00 (should be 0x80 empty) inside a list:
        // craft: [chainId=1, nonce=0x00, ...] — manually tweak golden bytes.
        // golden has nonce as 0x80 (empty); replace byte after 0x82 0x7a 0x69.
        // Layout: 02 e9 82 7a69 80 84 3b9aca00 ... ; index of nonce item = 5
        let raw = golden();
        assert_eq!(raw[5], 0x80);
        let mut bad = raw.clone();
        bad[5] = 0x00; // nonce = single 0x00 byte → non-minimal
        assert!(decode_unsigned_eip1559(&bad).is_err());
    }

    #[test]
    fn rejects_arity_mismatch() {
        // drop accessList -> 8 items
        let raw = hex::decode("02d9827a6980843b9aca0084773594008252089433333333333333333333333333333333333333338080").unwrap();
        assert!(decode_unsigned_eip1559(&raw).is_err());
    }

    /// Arbitrary deterministic test scalar (NOT a production secret).
    const DEV_DEK: [u8; 32] = [
        0xac, 0x09, 0x74, 0xbe, 0xc3, 0x9a, 0x17, 0xe3, 0x6b, 0xa4, 0xa6, 0xb4, 0xd2, 0x38, 0xff,
        0x94, 0x4b, 0xac, 0xb4, 0x78, 0xfb, 0xed, 0x5e, 0xfc, 0xae, 0xf7, 0x84, 0xd7, 0xbf, 0x4f,
        0x2f, 0x80,
    ];

    #[test]
    fn signs_and_recovers_roundtrip() {
        let tx = decode_unsigned_eip1559(&golden()).unwrap();
        let dek = SecretVec::new(DEV_DEK.to_vec());
        let (signed, hash) = sign_eip1559(&tx, &dek).unwrap();
        assert_eq!(signed[0], 0x02);
        assert_eq!(hash, keccak256(&signed));

        let recovered = recover_signer(&signed).unwrap();
        let expected = address_from_dek(&DEV_DEK).unwrap();
        assert_eq!(recovered, expected, "recovered signer != DEK address");
    }

    #[test]
    fn signed_tx_has_low_s() {
        let tx = decode_unsigned_eip1559(&golden()).unwrap();
        let dek = SecretVec::new(DEV_DEK.to_vec());
        let (signed, _) = sign_eip1559(&tx, &dek).unwrap();
        let top = rlp_decode(&signed[1..]).unwrap();
        let fields = match top {
            RlpItem::List(f) => f,
            _ => panic!(),
        };
        let s = item_to_uint(&fields[11], 32).unwrap();
        // secp256k1 n/2
        let half_n = U256::from_be_slice(&hex::decode(
            "7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0",
        ).unwrap());
        assert!(s <= half_n, "s must be normalized to low-S");
    }

    #[test]
    fn access_list_preserved() {
        // Build a tx with a non-empty access list and ensure decode->encode->sign
        // round-trips the list verbatim.
        let entries = vec![(
            Address::from([0x11u8; 20]),
            vec![B256::from([0x22u8; 32]), B256::from([0x33u8; 32])],
        )];
        let list = RlpItem::List(vec![
            u64_item(31337),
            u64_item(7),
            uint_item(&U256::from(1u64)),
            uint_item(&U256::from(2u64)),
            u64_item(50_000),
            bytes_item(&[0x33u8; 20]),
            uint_item(&U256::ZERO),
            bytes_item(&[]),
            access_list_item(&entries),
        ]);
        let mut raw = vec![TX_TYPE_EIP1559];
        raw.extend_from_slice(&rlp_encode(&list));
        let tx = decode_unsigned_eip1559(&raw).unwrap();
        assert_eq!(tx.access_list, entries);
        // Re-encode preimage == raw
        let mut preimage = vec![TX_TYPE_EIP1559];
        preimage.extend_from_slice(&rlp_encode(&RlpItem::List(tx.unsigned_items())));
        assert_eq!(preimage, raw);
    }
}
