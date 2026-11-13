//! Four self-proving transaction policies + the dual-field same-nonce
//! idempotent authorization slot (`signer_intent_slots`).
//! See ARCHITECTURE §1.2 and plan.md W2-3.

use alloy_primitives::Address;

use crate::crypto::DecodedUnsignedTx;

/// The four firewall policies — mirrors proto `TxPolicy`; keep 1:1.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Policy {
    /// relayer -> BatchSweeper.deployAndSweepBatch; decoded targetColdWallet
    /// must sit in `allowed_cold_wallets` (dual-track during 48h rotation).
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

/// Reject reasons — mapped onto proto `RejectCode` at the gRPC boundary.
#[derive(Debug, thiserror::Error)]
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
    #[error("withdrawal whitelist check failed: {0}")]
    Whitelist(&'static str),
    #[error("invalid admin Ed25519 signature / refund ticket")]
    InvalidProof,
    #[error("same-nonce slot: (token, recipient, amount) tampered")]
    NonceSlotConflict,
    #[error("24h sliding-window limit exceeded (circuit breaker)")]
    DailyLimitExceeded,
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
    /// Allowed cold wallets; holds old+new pair during 48h rotation windows.
    pub allowed_cold_wallets: Vec<Address>,
    /// Whitelist cooldown (default 24h; test envs may explicitly override).
    pub cooldown_secs: u64,
}

/// Classify `tx` (locally decoded — never the request's `policy_hint`) into one
/// of the four policies and run its field-level firewall checks.
/// `from` is the hot wallet whose DEK would sign — W2-3 wires slot handling for
/// PAYOUT first-signs vs RBF/filler re-signs against `signer_intent_slots`.
pub fn evaluate(
    _cfg: &ChainPolicyConfig,
    _from: &Address,
    _tx: &DecodedUnsignedTx,
) -> Result<Policy, PolicyError> {
    // TODO(W2-3): value==0 / to allowlists / calldata ABI decode
    //             (deployAndSweepBatch, payOrderWithAuthorization, transfer) /
    //             dual-field same-nonce slot checks / 24h sliding breaker.
    Err(PolicyError::UnknownPolicy)
}
