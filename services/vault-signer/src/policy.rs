//! Four self-proving transaction policies + the dual-field same-nonce
//! idempotent authorization slot (`signer_intent_slots`).
//! See ARCHITECTURE §1.2 and plan.md W2-3.
//!
//! The classifier works ONLY on locally-decoded EIP-1559 fields — the request's
//! `policy_hint` is checked afterwards in core.rs as a consistency hint, never
//! as authority.
//!
//! ABI surfaces (frozen with contracts/src):
//!   BatchSweeper.deployAndSweepBatch((bytes32,uint256)[],address,address)
//!   EIP3009PayRouter.payOrderWithAuthorization(bytes32,address,uint256,
//!       uint256,uint256,bytes32,uint8,bytes32,bytes32)
//!   ERC20.transfer(address,uint256)  — selector 0xa9059cbb

use std::collections::BTreeSet;

use alloy_primitives::{keccak256, Address, B256, U256};

use crate::crypto::DecodedUnsignedTx;

/// The four firewall policies — mirrors proto `TxPolicy`; keep 1:1.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Policy {
    /// relayer -> BatchSweeper.deployAndSweepBatch; decoded targetColdWallet
    /// must sit in the signer's dynamic `cold_wallets` table — membership is
    /// enforced by the engine (DB-authoritative), not by this pure decoder.
    Sweep,
    /// relayer -> EIP3009PayRouter.payOrderWithAuthorization; signer locally
    /// verifies authNonce == keccak256(abi.encode(chain_id, salt)).
    Eip3009Relay,
    /// payout -> whitelisted stablecoin `transfer`; needs 24h breaker clear AND
    /// (leased whitelist entry) OR (valid Ed25519 RefundTicket).
    Payout,
    /// hot wallet 0-ETH self-transfer: to == from, empty data, gas 21_000.
    GapFiller,
}

impl Policy {
    /// proto `TxPolicy` numeric value.
    pub fn proto_code(self) -> i32 {
        match self {
            Policy::Sweep => 1,
            Policy::Eip3009Relay => 2,
            Policy::Payout => 3,
            Policy::GapFiller => 4,
        }
    }
}

/// Reject reasons — mapped onto proto `RejectCode` at the gRPC boundary.
#[derive(Debug, thiserror::Error, Clone, Copy, PartialEq, Eq)]
pub enum PolicyError {
    #[error("EIP-2718 Type-2 RLP decode failed")]
    MalformedTx,
    #[error("matches none of SWEEP / EIP3009_RELAY / PAYOUT / GAP_FILLER")]
    UnknownPolicy,
    #[error("policy field violation: {0}")]
    Violation(&'static str),
    #[error("cold wallet not in allowed_cold_wallets")]
    ColdWalletNotAllowed,
    #[error("authNonce != keccak256(chain_id, salt)")]
    AuthNonceMismatch,
    #[error("withdrawal recipient not whitelisted")]
    WhitelistNotFound,
    #[error("whitelist entry still in cooldown")]
    WhitelistCooldown,
    #[error("whitelist lease expired")]
    WhitelistExpired,
    #[error("whitelist entry revoked")]
    WhitelistRevoked,
    #[error("invalid admin Ed25519 signature / malformed 69B payload")]
    InvalidAdminSignature,
    #[error("invalid refund ticket (signature, field match or expiry)")]
    InvalidRefundTicket,
    #[error("same-nonce slot: (token, recipient, amount) tampered or payout bound elsewhere")]
    NonceSlotConflict,
    #[error("24h sliding-window limit exceeded (circuit breaker)")]
    DailyLimitExceeded,
    #[error("internal signer failure")]
    Internal,
}

impl PolicyError {
    /// proto `RejectCode` numeric value (see signer.proto enum).
    pub fn reject_code(self) -> i32 {
        match self {
            PolicyError::MalformedTx => 1,
            PolicyError::UnknownPolicy => 2,
            PolicyError::Violation(_) => 3,
            PolicyError::ColdWalletNotAllowed => 4,
            PolicyError::AuthNonceMismatch => 5,
            PolicyError::WhitelistNotFound => 6,
            PolicyError::WhitelistCooldown => 7,
            PolicyError::WhitelistExpired => 8,
            PolicyError::WhitelistRevoked => 9,
            PolicyError::InvalidAdminSignature => 10,
            PolicyError::InvalidRefundTicket => 11,
            PolicyError::NonceSlotConflict => 12,
            PolicyError::DailyLimitExceeded => 13,
            PolicyError::Internal => 14,
        }
    }
}

/// Per-chain read-only firewall config — loaded from the signer's own config,
/// NEVER from gateway-core request fields.
#[derive(Debug, Clone)]
pub struct ChainPolicyConfig {
    pub chain_id: u64,
    pub relayer_hot_wallet: Address,
    pub payout_hot_wallet: Address,
    pub batch_sweeper: Address,
    pub eip3009_router: Address,
    /// Whitelisted custody stablecoin contracts (PAYOUT `to` allowlist).
    pub custody_tokens: Vec<Address>,
    /// Bootstrap seed for the `cold_wallets` SQLite table (applied iff the
    /// table is empty for the chain). Once seeded the DB is the ONLY
    /// authority — this field is never consulted at sign time. Holds old+new
    /// pair during 48h rotation windows at first boot.
    pub allowed_cold_wallets: Vec<Address>,
    /// Whitelist cooldown (default 24h; test envs may explicitly override).
    pub cooldown_secs: u64,
    /// Global 24h sliding-window circuit breaker over summed payout amounts
    /// (atomic units). `None` disables the breaker (still records usage rows).
    pub daily_payout_limit_atomic: Option<U256>,
}

/// What `classify` proved about the calldata — the fields later stages need.
#[derive(Debug, Clone)]
pub enum PolicyOutcome {
    Sweep {
        token: Address,
        target_cold_wallet: Address,
        item_count: usize,
    },
    Eip3009Relay {
        salt: B256,
        auth_nonce: B256,
        payer: Address,
        amount: U256,
    },
    Payout {
        token: Address,
        recipient: Address,
        amount: U256,
    },
    GapFiller,
}

/// Classification result: proven policy + the wallet that must sign.
#[derive(Debug, Clone)]
pub struct ClassifiedTx {
    pub policy: Policy,
    /// The hot wallet whose DEK signs — forced by policy, never by request:
    ///   SWEEP / EIP3009_RELAY -> cfg.relayer_hot_wallet
    ///   PAYOUT                -> cfg.payout_hot_wallet
    ///   GAP_FILLER            -> tx.to (self-transfer of a hot wallet)
    pub from: Address,
    pub outcome: PolicyOutcome,
}

// ---------------------------------------------------------------------------
// Selector & ABI-word helpers
// ---------------------------------------------------------------------------

/// 4-byte function selector = keccak256(sig)[..4].
fn selector(sig: &str) -> [u8; 4] {
    let h = keccak256(sig.as_bytes());
    [h[0], h[1], h[2], h[3]]
}

/// Read a 32-byte ABI word at byte offset `off` inside `data`.
fn word<'a>(data: &'a [u8], off: usize) -> Result<&'a [u8; 32], PolicyError> {
    data.get(off..off + 32)
        .and_then(|s| <&[u8; 32]>::try_from(s).ok())
        .ok_or(PolicyError::Violation("calldata truncated (ABI word out of bounds)"))
}

/// ABI word -> address; requires the upper 12 padding bytes to be zero.
fn word_to_addr(w: &[u8; 32], what: &'static str) -> Result<Address, PolicyError> {
    if w[..12].iter().any(|b| *b != 0) {
        return Err(PolicyError::Violation(match what {
            "token" => "token word has non-zero padding",
            "target" => "target cold wallet word has non-zero padding",
            "payer" => "payer word has non-zero padding",
            _ => "address word has non-zero padding",
        }));
    }
    Ok(Address::from_slice(&w[12..]))
}

fn word_to_u256(w: &[u8; 32]) -> U256 {
    U256::from_be_bytes(*w)
}

// ---------------------------------------------------------------------------
// Calldata decoders
// ---------------------------------------------------------------------------

/// `deployAndSweepBatch(SweepItem[] items, address token, address target)`
/// where `SweepItem = (bytes32 salt, uint256 exactAmount)`.
/// Head layout: `[sel][items-offset][token][targetColdWallet]`, items tail at
/// `4 + offset`: `[count][count * (salt32 || amount64B)]`.
/// Returns `(token, target_cold_wallet, item_count)`.
fn decode_deploy_and_sweep_batch(
    data: &[u8],
) -> Result<(Address, Address, usize), PolicyError> {
    const MAX_ITEMS: usize = 50; // BatchSweeper.MAX_BATCH_ITEMS
    let sel = selector("deployAndSweepBatch((bytes32,uint256)[],address,address)");
    if data.len() < 4 + 96 {
        return Err(PolicyError::Violation("calldata too short for deployAndSweepBatch"));
    }
    if data[..4] != sel {
        return Err(PolicyError::Violation("selector != deployAndSweepBatch"));
    }
    let items_off_u = word_to_u256(word(data, 4)?);
    let items_off = usize::try_from(items_off_u)
        .map_err(|_| PolicyError::Violation("items offset overflows usize"))?;
    let token = word_to_addr(word(data, 4 + 32)?, "token")?;
    let target = word_to_addr(word(data, 4 + 64)?, "target")?;
    // Canonical encoders place the head at 96; accept larger offsets only if
    // they still point inside the payload.
    if items_off < 96 {
        return Err(PolicyError::Violation("items offset inside head"));
    }
    let tail = 4usize
        .checked_add(items_off)
        .ok_or(PolicyError::Violation("items offset overflow"))?;
    let count_u = word_to_u256(word(data, tail)?);
    let count = usize::try_from(count_u)
        .map_err(|_| PolicyError::Violation("items count overflows usize"))?;
    if count > MAX_ITEMS {
        return Err(PolicyError::Violation("items count exceeds MAX_BATCH_ITEMS=50"));
    }
    let need = tail
        .checked_add(32)
        .and_then(|v| v.checked_add(count.checked_mul(64)?))
        .ok_or(PolicyError::Violation("items region overflow"))?;
    if data.len() < need {
        return Err(PolicyError::Violation("items array truncated"));
    }
    Ok((token, target, count))
}

/// `payOrderWithAuthorization(bytes32 salt, address payer, uint256 amount,
/// uint256 validAfter, uint256 validBefore, bytes32 authNonce,
/// uint8 v, bytes32 r, bytes32 s)` — 9 static words, exact 292B calldata.
/// Returns `(salt, auth_nonce, payer, amount)`.
fn decode_pay_order_with_authorization(
    data: &[u8],
) -> Result<(B256, B256, Address, U256), PolicyError> {
    const EXPECTED_LEN: usize = 4 + 9 * 32;
    let sel = selector(
        "payOrderWithAuthorization(bytes32,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)",
    );
    if data.len() != EXPECTED_LEN {
        return Err(PolicyError::Violation("bad calldata length for payOrderWithAuthorization"));
    }
    if data[..4] != sel {
        return Err(PolicyError::Violation("selector != payOrderWithAuthorization"));
    }
    let salt = B256::from_slice(word(data, 4)?);
    let payer = word_to_addr(word(data, 4 + 32)?, "payer")?;
    let amount = word_to_u256(word(data, 4 + 64)?);
    // validAfter (word3), validBefore (word4) are enforced on-chain by the
    // payer's EIP-712 signature + USDC contract — no signer-side condition.
    let auth_nonce = B256::from_slice(word(data, 4 + 5 * 32)?);
    let v = word_to_u256(word(data, 4 + 6 * 32)?);
    if v > U256::from(255u64) {
        return Err(PolicyError::Violation("v does not fit uint8"));
    }
    Ok((salt, auth_nonce, payer, amount))
}

/// `transfer(address recipient, uint256 amount)` — exact 68B calldata.
/// Returns `(recipient, amount)`.
fn decode_erc20_transfer(data: &[u8]) -> Result<(Address, U256), PolicyError> {
    const TRANSFER_SEL: [u8; 4] = [0xa9, 0x05, 0x9c, 0xbb];
    if data.len() != 4 + 64 {
        return Err(PolicyError::Violation("bad calldata length for transfer"));
    }
    if data[..4] != TRANSFER_SEL {
        return Err(PolicyError::Violation("selector != transfer(address,uint256)"));
    }
    let recipient = word_to_addr(word(data, 4)?, "recipient")?;
    if recipient == Address::ZERO {
        return Err(PolicyError::Violation("transfer to zero address"));
    }
    let amount = word_to_u256(word(data, 4 + 32)?);
    if amount.is_zero() {
        return Err(PolicyError::Violation("zero-amount transfer"));
    }
    Ok((recipient, amount))
}

/// Expected `auth_nonce` for EIP-3009 relay:
/// `keccak256(abi.encode(uint256 chain_id, bytes32 salt))` (AGENTS §2.1).
pub fn expected_auth_nonce(chain_id: u64, salt: &B256) -> B256 {
    let mut blob = [0u8; 64];
    blob[..32].copy_from_slice(&U256::from(chain_id).to_be_bytes::<32>());
    blob[32..].copy_from_slice(salt.as_slice());
    keccak256(blob)
}

// ---------------------------------------------------------------------------
// Classifier
// ---------------------------------------------------------------------------

/// Classify `tx` (locally decoded — never the request's `policy_hint`) into one
/// of the four policies and run its field-level firewall checks.
///
/// Dispatch is driven by the decoded `to` address — the four target sets are
/// mutually exclusive by configuration:
///   batch_sweeper        -> SWEEP
///   eip3009_router       -> EIP3009_RELAY
///   custody token        -> PAYOUT
///   a hot wallet (self)  -> GAP_FILLER
///   anything else        -> UNKNOWN_POLICY
///
/// `hot_wallets` is the engine-supplied set of signer-custodied hot wallets
/// (configured relayer/payout ∪ every DEK loaded into memory — including
/// runtime-provisioned GenerateWallet keys, which are live without restart).
/// Cold-wallet membership for SWEEP is deliberately NOT checked here: the
/// allowlist lives in the `cold_wallets` SQLite table and the engine consults
/// it inside the signing transaction so runtime updates take effect
/// immediately. This function stays a pure decoder/validator.
pub fn classify(
    cfg: &ChainPolicyConfig,
    tx: &DecodedUnsignedTx,
    hot_wallets: &BTreeSet<Address>,
) -> Result<ClassifiedTx, PolicyError> {
    if tx.to == cfg.batch_sweeper {
        if !tx.value.is_zero() {
            return Err(PolicyError::Violation("SWEEP requires value == 0"));
        }
        let (token, target, item_count) = decode_deploy_and_sweep_batch(&tx.data)?;
        return Ok(ClassifiedTx {
            policy: Policy::Sweep,
            from: cfg.relayer_hot_wallet,
            outcome: PolicyOutcome::Sweep {
                token,
                target_cold_wallet: target,
                item_count,
            },
        });
    }

    if tx.to == cfg.eip3009_router {
        if !tx.value.is_zero() {
            return Err(PolicyError::Violation("EIP3009_RELAY requires value == 0"));
        }
        let (salt, auth_nonce, payer, amount) =
            decode_pay_order_with_authorization(&tx.data)?;
        if auth_nonce != expected_auth_nonce(cfg.chain_id, &salt) {
            return Err(PolicyError::AuthNonceMismatch);
        }
        return Ok(ClassifiedTx {
            policy: Policy::Eip3009Relay,
            from: cfg.relayer_hot_wallet,
            outcome: PolicyOutcome::Eip3009Relay {
                salt,
                auth_nonce,
                payer,
                amount,
            },
        });
    }

    if cfg.custody_tokens.contains(&tx.to) {
        if !tx.value.is_zero() {
            return Err(PolicyError::Violation("PAYOUT requires value == 0"));
        }
        let (recipient, amount) = decode_erc20_transfer(&tx.data)?;
        return Ok(ClassifiedTx {
            policy: Policy::Payout,
            from: cfg.payout_hot_wallet,
            outcome: PolicyOutcome::Payout {
                token: tx.to,
                recipient,
                amount,
            },
        });
    }

    if hot_wallets.contains(&tx.to) {
        if !tx.is_gap_filler(&tx.to) {
            return Err(PolicyError::Violation(
                "GAP_FILLER requires to==from, value==0, empty data, gas==21000",
            ));
        }
        return Ok(ClassifiedTx {
            policy: Policy::GapFiller,
            from: tx.to,
            outcome: PolicyOutcome::GapFiller,
        });
    }

    Err(PolicyError::UnknownPolicy)
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    /// Engine-supplied hot-wallet set: configured relayer/payout ∪ loaded
    /// DEKs. Unit tests only need the configured pair.
    fn hot() -> BTreeSet<Address> {
        let c = cfg();
        [c.relayer_hot_wallet, c.payout_hot_wallet]
            .into_iter()
            .collect()
    }

    fn cfg() -> ChainPolicyConfig {
        ChainPolicyConfig {
            chain_id: 31337,
            relayer_hot_wallet: Address::from([0x11u8; 20]),
            payout_hot_wallet: Address::from([0x22u8; 20]),
            batch_sweeper: Address::from([0xAAu8; 20]),
            eip3009_router: Address::from([0xBBu8; 20]),
            custody_tokens: vec![Address::from([0x44u8; 20]), Address::from([0x45u8; 20])],
            allowed_cold_wallets: vec![Address::from([0xCCu8; 20])],
            cooldown_secs: 86_400,
            daily_payout_limit_atomic: None,
        }
    }

    fn tx(to: [u8; 20], data: Vec<u8>) -> DecodedUnsignedTx {
        DecodedUnsignedTx {
            raw: vec![],
            chain_id: 31337,
            nonce: 0,
            max_priority_fee_per_gas: U256::from(1u64),
            max_fee_per_gas: U256::from(2u64),
            gas_limit: 21_000,
            to: Address::from(to),
            value: U256::ZERO,
            data,
            access_list: vec![],
        }
    }

    fn word32(b: &[u8]) -> Vec<u8> {
        let mut w = vec![0u8; 32];
        w[32 - b.len()..].copy_from_slice(b);
        w
    }

    fn addr_word(a: [u8; 20]) -> Vec<u8> {
        word32(&a)
    }

    fn u256_word(v: u64) -> Vec<u8> {
        word32(&v.to_be_bytes())
    }

    #[test]
    fn sweep_selector_decodes() {
        let sel = selector("deployAndSweepBatch((bytes32,uint256)[],address,address)");
        // sanity: stable selector across calls
        assert_eq!(sel, selector("deployAndSweepBatch((bytes32,uint256)[],address,address)"));
        let mut data = Vec::new();
        data.extend_from_slice(&sel);
        data.extend_from_slice(&u256_word(96));          // items offset
        data.extend_from_slice(&addr_word([0x44u8; 20])); // token
        data.extend_from_slice(&addr_word([0xCCu8; 20])); // target cold wallet
        // tail: count=1, item = salt||amount
        data.extend_from_slice(&u256_word(1));
        data.extend_from_slice(&[0x77u8; 32]);
        data.extend_from_slice(&u256_word(1_500_000));

        let t = tx([0xAAu8; 20], data);
        let c = classify(&cfg(), &t, &hot()).unwrap();
        assert_eq!(c.policy, Policy::Sweep);
        assert_eq!(c.from, cfg().relayer_hot_wallet);
        match c.outcome {
            PolicyOutcome::Sweep { token, target_cold_wallet, item_count } => {
                assert_eq!(token, Address::from([0x44u8; 20]));
                assert_eq!(target_cold_wallet, Address::from([0xCCu8; 20]));
                assert_eq!(item_count, 1);
            }
            _ => panic!(),
        }
    }

    /// Membership against the dynamic `cold_wallets` table is enforced by the
    /// ENGINE inside the signing transaction — `classify` itself only decodes
    /// and field-validates, so an arbitrary target address still classifies as
    /// SWEEP here (engine-level tests cover COLD_WALLET_NOT_ALLOWED).
    #[test]
    fn sweep_classifies_before_cold_membership_check() {
        let sel = selector("deployAndSweepBatch((bytes32,uint256)[],address,address)");
        let mut data = Vec::new();
        data.extend_from_slice(&sel);
        data.extend_from_slice(&u256_word(96));
        data.extend_from_slice(&addr_word([0x44u8; 20]));
        data.extend_from_slice(&addr_word([0xEEu8; 20])); // not in any allowlist
        data.extend_from_slice(&u256_word(0));
        let t = tx([0xAAu8; 20], data);
        let c = classify(&cfg(), &t, &hot()).unwrap();
        match c.outcome {
            PolicyOutcome::Sweep { target_cold_wallet, .. } => {
                assert_eq!(target_cold_wallet, Address::from([0xEEu8; 20]));
            }
            _ => panic!("expected SWEEP classification"),
        }
    }

    #[test]
    fn relay_nonce_golden_vector() {
        // GOLDEN_VECTORS: chain_id=31337, salt=0x55b2...6834 ->
        // auth_nonce=0x3337d78303e32c3481fbe13adfa72942f14970c28e974855c2023a9cee9ac225
        let salt = B256::from_slice(
            &hex::decode("55b28547100539cc5b92b67e6265adcc0b81e207dcb758222285a14b58e16834").unwrap(),
        );
        let expected = B256::from_slice(
            &hex::decode("3337d78303e32c3481fbe13adfa72942f14970c28e974855c2023a9cee9ac225").unwrap(),
        );
        assert_eq!(expected_auth_nonce(31337, &salt), expected);
    }

    fn relay_calldata(salt: &B256, auth_nonce: &B256) -> Vec<u8> {
        let sel = selector(
            "payOrderWithAuthorization(bytes32,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)",
        );
        let mut data = Vec::new();
        data.extend_from_slice(&sel);
        data.extend_from_slice(salt.as_slice());
        data.extend_from_slice(&addr_word([0x99u8; 20])); // payer
        data.extend_from_slice(&u256_word(1_500_000));    // amount
        data.extend_from_slice(&u256_word(0));            // validAfter
        data.extend_from_slice(&u256_word(1_800_000_000));// validBefore
        data.extend_from_slice(auth_nonce.as_slice());
        data.extend_from_slice(&u256_word(27));           // v
        data.extend_from_slice(&[0x01u8; 32]);            // r
        data.extend_from_slice(&[0x02u8; 32]);            // s
        data
    }

    #[test]
    fn relay_accepts_correct_nonce() {
        let salt = B256::from([0xABu8; 32]);
        let nonce = expected_auth_nonce(31337, &salt);
        let t = tx([0xBBu8; 20], relay_calldata(&salt, &nonce));
        let c = classify(&cfg(), &t, &hot()).unwrap();
        assert_eq!(c.policy, Policy::Eip3009Relay);
        assert_eq!(c.from, cfg().relayer_hot_wallet);
    }

    #[test]
    fn relay_rejects_tampered_salt() {
        let salt = B256::from([0xABu8; 32]);
        let bad_salt = B256::from([0xACu8; 32]);
        // authNonce computed over ORIGINAL salt; calldata carries bad salt.
        let nonce = expected_auth_nonce(31337, &salt);
        let t = tx([0xBBu8; 20], relay_calldata(&bad_salt, &nonce));
        assert_eq!(
            classify(&cfg(), &t, &hot()).unwrap_err(),
            PolicyError::AuthNonceMismatch
        );
    }

    #[test]
    fn payout_decodes_transfer() {
        let mut data = vec![0xa9, 0x05, 0x9c, 0xbb];
        data.extend_from_slice(&addr_word([0x66u8; 20]));
        data.extend_from_slice(&u256_word(2_500_000));
        let t = tx([0x44u8; 20], data);
        let c = classify(&cfg(), &t, &hot()).unwrap();
        assert_eq!(c.policy, Policy::Payout);
        assert_eq!(c.from, cfg().payout_hot_wallet);
        match c.outcome {
            PolicyOutcome::Payout { token, recipient, amount } => {
                assert_eq!(token, Address::from([0x44u8; 20]));
                assert_eq!(recipient, Address::from([0x66u8; 20]));
                assert_eq!(amount, U256::from(2_500_000u64));
            }
            _ => panic!(),
        }
    }

    #[test]
    fn payout_rejects_non_custody_token() {
        let mut data = vec![0xa9, 0x05, 0x9c, 0xbb];
        data.extend_from_slice(&addr_word([0x66u8; 20]));
        data.extend_from_slice(&u256_word(1));
        let t = tx([0xFEu8; 20], data); // non-whitelisted token contract
        assert_eq!(
            classify(&cfg(), &t, &hot()).unwrap_err(),
            PolicyError::UnknownPolicy
        );
    }

    #[test]
    fn gap_filler_self_transfer() {
        let t = tx([0x22u8; 20], vec![]);
        let c = classify(&cfg(), &t, &hot()).unwrap();
        assert_eq!(c.policy, Policy::GapFiller);
        assert_eq!(c.from, cfg().payout_hot_wallet);
    }

    #[test]
    fn gap_filler_rejects_non_21000_gas() {
        let mut t = tx([0x11u8; 20], vec![]);
        t.gas_limit = 50_000;
        assert!(matches!(classify(&cfg(), &t, &hot()), Err(PolicyError::Violation(_))));
    }

    #[test]
    fn random_eoa_unknown_policy() {
        let t = tx([0x77u8; 20], vec![]);
        assert_eq!(
            classify(&cfg(), &t, &hot()).unwrap_err(),
            PolicyError::UnknownPolicy
        );
    }
}
