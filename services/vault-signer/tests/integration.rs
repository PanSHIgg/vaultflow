//! W2-4 adversarial + e2e suite: full mTLS round-trips, all four policy
//! signatures, every documented attack class, dual-field nonce-slot
//! transitions and audit-chain tamper detection — anchored on the frozen
//! values in docs/GOLDEN_VECTORS.json wherever possible.

use std::net::{IpAddr, Ipv4Addr};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use alloy_primitives::{Address, B256, U256};
use ed25519_dalek::{Signer, SigningKey};
use secrecy::ExposeSecret;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};
use tonic::Request;

use vault_signer::crypto::{self, RlpItem};
use vault_signer::db;
use vault_signer::engine::{fmt_addr, SignerCore};
use vault_signer::grpc::mtls::{CertPaths, ReloadingTls};
use vault_signer::grpc::service::VaultSignerSvc;
use vault_signer::grpc::signer_v1::vault_signer_service_client::VaultSignerServiceClient;
use vault_signer::grpc::signer_v1::vault_signer_service_server::VaultSignerService;
use vault_signer::grpc::signer_v1::*;
use vault_signer::kms::{KmsProvider, LocalFileKms};
use vault_signer::policy::ChainPolicyConfig;

// ---------------------------------------------------------------------------
// Golden-vector constants (docs/GOLDEN_VECTORS.json)
// ---------------------------------------------------------------------------

const ADMIN_SEED: [u8; 32] = [
    0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
    0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
];
const ADMIN_PUB_HEX: &str = "79b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664";

/// ADDR_V1: chain 31337, merchant 0x0011..eeff, recipient 0x3333..3333,
/// valid_until 0x70dbd880 (~2030).
const GOLDEN_ADDR_PAYLOAD: &str = "0x5641554c54464c4f575f414444525f56310000000000007a6900112233445566778899aabbccddeeff33333333333333333333333333333333333333330000000070dbd880";
const GOLDEN_ADDR_SIG: &str = "0x804a83e46c4d22b4045463d578b505c0474f01950062537b4e7462a321a384e5b49b4e026e8ef3215e6ff8c7ae4bd641ce3c51dbbb2ea252713abc4f5998bf02";

/// REFUND_V1: chain 31337, token 0x4444..44, to 0x5555..55, amount 1500000,
/// payout_uuid 0xffee..1100, retry_seq 0, expiry 0x70dbd880.
const GOLDEN_REFUND_PAYLOAD: &str = "0x5641554c54464c4f575f524546554e445f56310000000000007a6944444444444444444444444444444444444444445555555555555555555555555555555555555555000000000000000000000000000000000000000000000000000000000016e360ffeeddccbbaa99887766554433221100000000000000000070dbd880";
const GOLDEN_REFUND_SIG: &str = "0x3f07be8703bdc1842b1ab083785f8457b1185e92814993b697b10c92e1bfa0fe01554bcaf957420c67b37e1d4f080a58a20b9835ad321e9117e9b885089d2205";

/// Golden EIP-3009 salt/auth_nonce pair (chain 31337).
const GOLDEN_SALT: &str = "55b28547100539cc5b92b67e6265adcc0b81e207dcb758222285a14b58e16834";
const GOLDEN_AUTH_NONCE: &str =
    "3337d78303e32c3481fbe13adfa72942f14970c28e974855c2023a9cee9ac225";

const CHAIN: u64 = 31337;
const RELAYER_DEK: [u8; 32] = [0x11u8; 32];
const PAYOUT_DEK: [u8; 32] = [0x22u8; 32];
const KEK: [u8; 32] = [0x5Au8; 32];

const MERCHANT_GOLDEN: [u8; 16] = [
    0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
];

fn unhex(s: &str) -> Vec<u8> {
    hex::decode(s.trim_start_matches("0x")).unwrap()
}

// ---------------------------------------------------------------------------
// Tx / calldata builders
// ---------------------------------------------------------------------------

fn rbytes(b: &[u8]) -> RlpItem {
    RlpItem::Bytes(b.to_vec())
}

fn ruint(v: &U256) -> RlpItem {
    if v.is_zero() {
        return RlpItem::Bytes(vec![]);
    }
    let be: [u8; 32] = v.to_be_bytes::<32>();
    let start = be.iter().position(|&b| b != 0).unwrap_or(31);
    RlpItem::Bytes(be[start..].to_vec())
}

fn ru64(v: u64) -> RlpItem {
    ruint(&U256::from(v))
}

#[allow(clippy::too_many_arguments)]
fn unsigned_tx(chain: u64, nonce: u64, to: Address, data: Vec<u8>, tip: u64, fee: u64, gas: u64) -> Vec<u8> {
    let items = vec![
        ru64(chain),
        ru64(nonce),
        ruint(&U256::from(tip)),
        ruint(&U256::from(fee)),
        ru64(gas),
        rbytes(to.as_slice()),
        ruint(&U256::ZERO),
        rbytes(&data),
        RlpItem::List(vec![]),
    ];
    let mut out = vec![0x02u8];
    out.extend_from_slice(&crypto::rlp_encode(&RlpItem::List(items)));
    out
}

fn w_addr(a: [u8; 20]) -> Vec<u8> {
    let mut w = vec![0u8; 32];
    w[12..].copy_from_slice(&a);
    w
}

fn w_u256(v: u64) -> Vec<u8> {
    let mut w = vec![0u8; 32];
    w[24..].copy_from_slice(&v.to_be_bytes());
    w
}

fn w_u256_be(v: &U256) -> Vec<u8> {
    v.to_be_bytes::<32>().to_vec()
}

fn transfer_calldata(recipient: [u8; 20], amount: &U256) -> Vec<u8> {
    let mut d = vec![0xa9, 0x05, 0x9c, 0xbb];
    d.extend_from_slice(&w_addr(recipient));
    d.extend_from_slice(&w_u256_be(amount));
    d
}

fn sweep_calldata(token: [u8; 20], target_cold: [u8; 20], items: &[[u8; 32]]) -> Vec<u8> {
    let sel = alloy_primitives::keccak256(
        b"deployAndSweepBatch((bytes32,uint256)[],address,address)",
    );
    let mut d = sel[..4].to_vec();
    d.extend_from_slice(&w_u256(96));
    d.extend_from_slice(&w_addr(token));
    d.extend_from_slice(&w_addr(target_cold));
    d.extend_from_slice(&w_u256(items.len() as u64));
    for salt in items {
        d.extend_from_slice(salt);
        d.extend_from_slice(&w_u256(1_000_000));
    }
    d
}

fn relay_calldata(salt: &B256, auth_nonce: &B256) -> Vec<u8> {
    let sel = alloy_primitives::keccak256(
        b"payOrderWithAuthorization(bytes32,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)",
    );
    let mut d = sel[..4].to_vec();
    d.extend_from_slice(salt.as_slice());
    d.extend_from_slice(&w_addr([0x99; 20]));
    d.extend_from_slice(&w_u256(1_500_000));
    d.extend_from_slice(&w_u256(0));
    d.extend_from_slice(&w_u256(1_900_000_000));
    d.extend_from_slice(auth_nonce.as_slice());
    d.extend_from_slice(&w_u256(27));
    d.extend_from_slice(&[0x01; 32]);
    d.extend_from_slice(&[0x02; 32]);
    d
}

fn addr_v1_payload(chain: u64, merchant: &[u8; 16], recipient: &[u8; 20], valid_until: u64) -> Vec<u8> {
    let mut p = b"VAULTFLOW_ADDR_V1".to_vec();
    p.extend_from_slice(&chain.to_be_bytes());
    p.extend_from_slice(merchant);
    p.extend_from_slice(recipient);
    p.extend_from_slice(&valid_until.to_be_bytes());
    assert_eq!(p.len(), 69);
    p
}

#[allow(clippy::too_many_arguments)]
fn refund_v1_payload(
    chain: u64,
    token: &[u8; 20],
    to: &[u8; 20],
    amount: &U256,
    payout_uuid: &[u8; 16],
    retry_seq: u32,
    expiry: u64,
) -> Vec<u8> {
    let mut p = b"VAULTFLOW_REFUND_V1".to_vec();
    p.extend_from_slice(&chain.to_be_bytes());
    p.extend_from_slice(token);
    p.extend_from_slice(to);
    p.extend_from_slice(&amount.to_be_bytes::<32>());
    p.extend_from_slice(payout_uuid);
    p.extend_from_slice(&retry_seq.to_be_bytes());
    p.extend_from_slice(&expiry.to_be_bytes());
    assert_eq!(p.len(), 127);
    p
}

fn ed_sign(sk: &SigningKey, payload: &[u8]) -> Vec<u8> {
    sk.sign(payload).to_bytes().to_vec()
}

// ---------------------------------------------------------------------------
// Test fixture
// ---------------------------------------------------------------------------

struct Fx {
    svc: VaultSignerSvc,
    cfg: ChainPolicyConfig,
    admin_sk: SigningKey,
    relayer: Address,
    payout: Address,
    token: Address,
    cold: Address,
}

fn fixture(cooldown_secs: u64, daily_limit: Option<U256>) -> Fx {
    let kms = LocalFileKms::from_kek_bytes(&KEK);
    let relayer = crypto::address_from_dek(&RELAYER_DEK).unwrap();
    let payout = crypto::address_from_dek(&PAYOUT_DEK).unwrap();
    let token = Address::from([0x44u8; 20]);
    let token2 = Address::from([0x45u8; 20]);
    let cold = Address::from([0xCCu8; 20]);
    let cold2 = Address::from([0xCDu8; 20]);

    let db = db::open_in_memory().unwrap();
    db::upsert_encrypted_key(
        &db.conn,
        &fmt_addr(&relayer),
        "relayer",
        &kms.encrypt_dek(&RELAYER_DEK).unwrap(),
        0,
    )
    .unwrap();
    db::upsert_encrypted_key(
        &db.conn,
        &fmt_addr(&payout),
        "payout",
        &kms.encrypt_dek(&PAYOUT_DEK).unwrap(),
        0,
    )
    .unwrap();

    let cfg = ChainPolicyConfig {
        chain_id: CHAIN,
        relayer_hot_wallet: relayer,
        payout_hot_wallet: payout,
        batch_sweeper: Address::from([0xAAu8; 20]),
        eip3009_router: Address::from([0xBBu8; 20]),
        custody_tokens: vec![token, token2],
        allowed_cold_wallets: vec![cold, cold2],
        cooldown_secs,
        daily_payout_limit_atomic: daily_limit,
    };
    let admin_pubkey: [u8; 32] = unhex(ADMIN_PUB_HEX).try_into().unwrap();
    let core = SignerCore::new(db, Arc::new(kms), vec![cfg.clone()], admin_pubkey).unwrap();
    Fx {
        svc: VaultSignerSvc::new(core),
        cfg,
        admin_sk: SigningKey::from_bytes(&ADMIN_SEED),
        relayer,
        payout,
        token,
        cold,
    }
}

fn sign_req(unsigned: Vec<u8>, hint: i32, payout_uuid: Option<[u8; 16]>, retry: u32) -> SignTransactionRequest {
    SignTransactionRequest {
        chain_id: CHAIN,
        unsigned_tx: unsigned.into(),
        policy_hint: hint,
        payout_uuid: payout_uuid.map(|u| u.to_vec().into()).unwrap_or_default(),
        retry_seq: retry,
        refund_ticket: None,
    }
}

async fn sign(svc: &VaultSignerSvc, req: SignTransactionRequest) -> sign_transaction_response::Result {
    svc.sign_transaction(Request::new(req))
        .await
        .unwrap()
        .into_inner()
        .result
        .unwrap()
}

fn expect_reject(r: &sign_transaction_response::Result, code: RejectCode) -> String {
    match r {
        sign_transaction_response::Result::Rejected(rej) => {
            assert_eq!(rej.code, code as i32, "expected {code:?}, got {rej:?}");
            assert!(rej.audit_seq > 0, "rejection must land on the audit chain");
            rej.reason.clone()
        }
        _ => panic!("expected rejection {code:?}, got {r:?}"),
    }
}

fn expect_signed(r: &sign_transaction_response::Result) -> SignedTx {
    match r {
        sign_transaction_response::Result::Signed(s) => {
            assert!(s.audit_seq > 0);
            s.clone()
        }
        _ => panic!("expected signed, got {r:?}"),
    }
}

async fn register(svc: &VaultSignerSvc, payload: Vec<u8>, sig: Vec<u8>) -> RegisterWithdrawalAddressResponse {
    svc.register_withdrawal_address(Request::new(RegisterWithdrawalAddressRequest {
        signed_payload: payload.into(),
        admin_signature: sig.into(),
    }))
    .await
    .unwrap()
    .into_inner()
}

// ---------------------------------------------------------------------------
// 0. Golden Ed25519 sanity (seed -> pubkey, deterministic signatures)
// ---------------------------------------------------------------------------

#[test]
fn golden_ed25519_seed_matches_vectors() {
    let sk = SigningKey::from_bytes(&ADMIN_SEED);
    assert_eq!(hex::encode(sk.verifying_key().to_bytes()), ADMIN_PUB_HEX);
    let sig = sk.sign(&unhex(GOLDEN_ADDR_PAYLOAD));
    assert_eq!(
        format!("0x{}", hex::encode(sig.to_bytes())),
        GOLDEN_ADDR_SIG,
        "golden ADDR_V1 signature mismatch"
    );
    let sig2 = sk.sign(&unhex(GOLDEN_REFUND_PAYLOAD));
    assert_eq!(
        format!("0x{}", hex::encode(sig2.to_bytes())),
        GOLDEN_REFUND_SIG,
        "golden REFUND_V1 signature mismatch"
    );
}

// ---------------------------------------------------------------------------
// 1. All four policy classes sign successfully (whitelist + refund paths)
// ---------------------------------------------------------------------------

#[tokio::test]
async fn all_four_policies_sign() {
    let fx = fixture(0, None);
    let svc = &fx.svc;

    // -- register the golden whitelist lease (exact frozen bytes + sig) ------
    let resp = register(svc, unhex(GOLDEN_ADDR_PAYLOAD), unhex(GOLDEN_ADDR_SIG)).await;
    assert!(resp.accepted, "golden lease rejected: {}", resp.reason);

    // -- PAYOUT via whitelist (recipient 0x3333..33 from the golden lease) ---
    let data = transfer_calldata([0x33u8; 20], &U256::from(1_000_000u64));
    let tx = unsigned_tx(CHAIN, 0, fx.token, data, 1, 2, 60_000);
    let s = expect_signed(&sign(svc, sign_req(tx, 3, Some([0xAAu8; 16]), 0)).await);
    let rec = crypto::recover_signer(&s.raw_tx).unwrap();
    assert_eq!(rec, fx.payout, "payout must be signed by payout_hot_wallet");

    // -- SWEEP (golden salt as one batch item) -------------------------------
    let data = sweep_calldata(*fx.token.0, *fx.cold.0, &[unhex(GOLDEN_SALT).try_into().unwrap()]);
    let tx = unsigned_tx(CHAIN, 0, fx.cfg.batch_sweeper, data, 1, 2, 200_000);
    let s = expect_signed(&sign(svc, sign_req(tx, 1, None, 0)).await);
    assert_eq!(crypto::recover_signer(&s.raw_tx).unwrap(), fx.relayer);

    // -- EIP3009_RELAY with the golden salt + computed nonce -----------------
    let salt = B256::from_slice(&unhex(GOLDEN_SALT));
    let nonce = B256::from_slice(&unhex(GOLDEN_AUTH_NONCE));
    let data = relay_calldata(&salt, &nonce);
    let tx = unsigned_tx(CHAIN, 1, fx.cfg.eip3009_router, data, 1, 2, 250_000);
    let s = expect_signed(&sign(svc, sign_req(tx, 2, None, 0)).await);
    assert_eq!(crypto::recover_signer(&s.raw_tx).unwrap(), fx.relayer);

    // -- GAP_FILLER self transfer --------------------------------------------
    let tx = unsigned_tx(CHAIN, 2, fx.relayer, vec![], 1, 2, 21_000);
    let s = expect_signed(&sign(svc, sign_req(tx, 4, None, 0)).await);
    assert_eq!(crypto::recover_signer(&s.raw_tx).unwrap(), fx.relayer);

    // -- PAYOUT via golden RefundTicket (no whitelist needed for 0x5555..55) -
    let data = transfer_calldata([0x55u8; 20], &U256::from(1_500_000u64));
    let tx = unsigned_tx(CHAIN, 10, fx.token, data, 1, 2, 60_000);
    let payout_uuid: [u8; 16] = unhex("ffeeddccbbaa99887766554433221100").try_into().unwrap();
    let mut req = sign_req(tx, 3, Some(payout_uuid), 0);
    req.refund_ticket = Some(RefundTicket {
        signed_payload: unhex(GOLDEN_REFUND_PAYLOAD).into(),
        admin_signature: unhex(GOLDEN_REFUND_SIG).into(),
    });
    let s = expect_signed(&sign(svc, req).await);
    assert_eq!(crypto::recover_signer(&s.raw_tx).unwrap(), fx.payout);

    // audit chain: 1 register + 5 signs all APPROVED -> verifies
    let v = svc
        .verify_audit_chain(Request::new(VerifyAuditChainRequest { from_seq: 0, to_seq: 0 }))
        .await
        .unwrap()
        .into_inner();
    assert!(v.valid);
    assert_eq!(v.entries_checked, 6);
}

// ---------------------------------------------------------------------------
// 2. Tampered cold wallet -> COLD_WALLET_NOT_ALLOWED
// ---------------------------------------------------------------------------

#[tokio::test]
async fn sweep_tampered_cold_wallet_rejected() {
    let fx = fixture(0, None);
    let evil = Address::from([0xEEu8; 20]);
    let data = sweep_calldata(*fx.token.0, *evil.0, &[[0x77u8; 32]]);
    let tx = unsigned_tx(CHAIN, 0, fx.cfg.batch_sweeper, data, 1, 2, 200_000);
    let r = sign(&fx.svc, sign_req(tx, 1, None, 0)).await;
    expect_reject(&r, RejectCode::ColdWalletNotAllowed);

    // Dual-track rotation: the SECOND allowed cold wallet is also accepted.
    let cold2 = Address::from([0xCDu8; 20]);
    let data = sweep_calldata(*fx.token.0, *cold2.0, &[[0x77u8; 32]]);
    let tx = unsigned_tx(CHAIN, 1, fx.cfg.batch_sweeper, data, 1, 2, 200_000);
    expect_signed(&sign(&fx.svc, sign_req(tx, 1, None, 0)).await);
}

// ---------------------------------------------------------------------------
// 3. Tampered salt -> AUTH_NONCE_MISMATCH
// ---------------------------------------------------------------------------

#[tokio::test]
async fn relay_tampered_salt_rejected() {
    let fx = fixture(0, None);
    let salt = B256::from_slice(&unhex(GOLDEN_SALT));
    // authNonce computed over the REAL salt; calldata smuggles a DIFFERENT salt.
    let good_nonce = B256::from_slice(&unhex(GOLDEN_AUTH_NONCE));
    let bad_salt = B256::from([0xDEu8; 32]);
    let data = relay_calldata(&bad_salt, &good_nonce);
    let tx = unsigned_tx(CHAIN, 0, fx.cfg.eip3009_router, data, 1, 2, 250_000);
    let r = sign(&fx.svc, sign_req(tx, 2, None, 0)).await;
    expect_reject(&r, RejectCode::AuthNonceMismatch);

    // Also: swapped authNonce field entirely.
    let data = relay_calldata(&salt, &B256::from([0x77u8; 32]));
    let tx = unsigned_tx(CHAIN, 1, fx.cfg.eip3009_router, data, 1, 2, 250_000);
    let r = sign(&fx.svc, sign_req(tx, 2, None, 0)).await;
    expect_reject(&r, RejectCode::AuthNonceMismatch);
}

// ---------------------------------------------------------------------------
// 4. Whitelist cooldown / expiry / revoked / not-found interception
// ---------------------------------------------------------------------------

#[tokio::test]
async fn whitelist_lease_states_enforced() {
    let fx = fixture(86_400, None); // production cooldown 24h
    let svc = &fx.svc;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();

    // (a) fresh lease -> still inside cooldown -> WHITELIST_COOLDOWN
    let recip_a = [0xA1u8; 20];
    let payload = addr_v1_payload(CHAIN, &MERCHANT_GOLDEN, &recip_a, now + 30 * 86_400);
    let resp = register(svc, payload.clone(), ed_sign(&fx.admin_sk, &payload)).await;
    assert!(resp.accepted);
    let data = transfer_calldata(recip_a, &U256::from(1u64));
    let tx = unsigned_tx(CHAIN, 0, fx.token, data, 1, 2, 60_000);
    let r = sign(svc, sign_req(tx, 3, Some([0xB1u8; 16]), 0)).await;
    expect_reject(&r, RejectCode::WhitelistCooldown);

    // (b) lease already expired -> WHITELIST_EXPIRED
    let recip_b = [0xA2u8; 20];
    let payload = addr_v1_payload(CHAIN, &MERCHANT_GOLDEN, &recip_b, now - 10);
    let resp = register(svc, payload.clone(), ed_sign(&fx.admin_sk, &payload)).await;
    assert!(resp.accepted);
    let data = transfer_calldata(recip_b, &U256::from(1u64));
    let tx = unsigned_tx(CHAIN, 1, fx.token, data, 1, 2, 60_000);
    let r = sign(svc, sign_req(tx, 3, Some([0xB2u8; 16]), 0)).await;
    expect_reject(&r, RejectCode::WhitelistExpired);

    // (c) never-registered recipient -> WHITELIST_NOT_FOUND
    let data = transfer_calldata([0xA3u8; 20], &U256::from(1u64));
    let tx = unsigned_tx(CHAIN, 2, fx.token, data, 1, 2, 60_000);
    let r = sign(svc, sign_req(tx, 3, Some([0xB3u8; 16]), 0)).await;
    expect_reject(&r, RejectCode::WhitelistNotFound);

    // (d) revoked lease -> WHITELIST_REVOKED
    let recip_d = [0xA4u8; 20];
    let payload = addr_v1_payload(CHAIN, &MERCHANT_GOLDEN, &recip_d, now + 30 * 86_400);
    register(svc, payload.clone(), ed_sign(&fx.admin_sk, &payload)).await;
    let rev = svc
        .revoke_withdrawal_address(Request::new(RevokeWithdrawalAddressRequest {
            chain_id: CHAIN,
            merchant_uuid: MERCHANT_GOLDEN.to_vec().into(),
            recipient_address: recip_d.to_vec().into(),
            admin_note: "emergency".into(),
        }))
        .await
        .unwrap()
        .into_inner();
    assert!(rev.accepted);
    let data = transfer_calldata(recip_d, &U256::from(1u64));
    let tx = unsigned_tx(CHAIN, 3, fx.token, data, 1, 2, 60_000);
    let r = sign(svc, sign_req(tx, 3, Some([0xB4u8; 16]), 0)).await;
    expect_reject(&r, RejectCode::WhitelistRevoked);
}

// ---------------------------------------------------------------------------
// 5. Forged / mismatched / expired refund tickets -> INVALID_REFUND_TICKET
// ---------------------------------------------------------------------------

#[tokio::test]
async fn forged_refund_ticket_rejected() {
    let fx = fixture(0, None);
    let svc = &fx.svc;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let to = [0x66u8; 20];
    let amount = U256::from(2_000_000u64);
    let uuid = [0xC1u8; 16];

    let mk_req = |ticket_payload: Vec<u8>, sig: Vec<u8>, nonce: u64| {
        let data = transfer_calldata(to, &amount);
        let mut req = sign_req(unsigned_tx(CHAIN, nonce, fx.token, data, 1, 2, 60_000), 3, Some(uuid), 0);
        req.refund_ticket = Some(RefundTicket {
            signed_payload: ticket_payload.into(),
            admin_signature: sig.into(),
        });
        req
    };

    // (a) signed by the WRONG Ed25519 key
    let payload = refund_v1_payload(CHAIN, &[0x44u8; 20], &to, &amount, &uuid, 0, now + 3600);
    let evil_sk = SigningKey::from_bytes(&[0xFFu8; 32]);
    let r = sign(svc, mk_req(payload.clone(), ed_sign(&evil_sk, &payload), 0)).await;
    expect_reject(&r, RejectCode::InvalidRefundTicket);

    // (b) ticket claims a DIFFERENT amount than the decoded transfer
    let payload = refund_v1_payload(CHAIN, &[0x44u8; 20], &to, &U256::from(9_999_999u64), &uuid, 0, now + 3600);
    let r = sign(svc, mk_req(payload.clone(), ed_sign(&fx.admin_sk, &payload), 1)).await;
    expect_reject(&r, RejectCode::InvalidRefundTicket);

    // (c) ticket for a different recipient than the decoded transfer
    let payload = refund_v1_payload(CHAIN, &[0x44u8; 20], &[0x77u8; 20], &amount, &uuid, 0, now + 3600);
    let r = sign(svc, mk_req(payload.clone(), ed_sign(&fx.admin_sk, &payload), 2)).await;
    expect_reject(&r, RejectCode::InvalidRefundTicket);

    // (d) expired ticket
    let payload = refund_v1_payload(CHAIN, &[0x44u8; 20], &to, &amount, &uuid, 0, now - 5);
    let r = sign(svc, mk_req(payload.clone(), ed_sign(&fx.admin_sk, &payload), 3)).await;
    expect_reject(&r, RejectCode::InvalidRefundTicket);

    // (e) control: valid ticket signs
    let payload = refund_v1_payload(CHAIN, &[0x44u8; 20], &to, &amount, &uuid, 0, now + 3600);
    let r = sign(svc, mk_req(payload.clone(), ed_sign(&fx.admin_sk, &payload), 4)).await;
    expect_signed(&r);
}

// ---------------------------------------------------------------------------
// 6. Dual-field nonce slot: RBF replay / tamper reject / filler transitions
// ---------------------------------------------------------------------------

#[tokio::test]
async fn nonce_slot_dual_field_transitions() {
    let fx = fixture(0, Some(U256::from(100_000_000u64)));
    let svc = &fx.svc;
    let recip = [0x33u8; 20];
    let uuid = [0xD1u8; 16];

    // whitelist the recipient (cooldown=0)
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let payload = addr_v1_payload(CHAIN, &MERCHANT_GOLDEN, &recip, now + 30 * 86_400);
    register(svc, payload.clone(), ed_sign(&fx.admin_sk, &payload)).await;

    // nonce 5: first PAYOUT signs
    let data = transfer_calldata(recip, &U256::from(60_000_000u64));
    let tx5 = unsigned_tx(CHAIN, 5, fx.token, data.clone(), 1, 2, 60_000);
    expect_signed(&sign(svc, sign_req(tx5, 3, Some(uuid), 0)).await);

    // nonce 5: RBF replay (higher fee, identical business triple) -> signed,
    // and NOT a second limit deduction (60+60=120>100 would trip otherwise).
    let tx5r = unsigned_tx(CHAIN, 5, fx.token, data.clone(), 5, 10, 60_000);
    expect_signed(&sign(svc, sign_req(tx5r, 3, Some(uuid), 0)).await);

    // nonce 5: tampered recipient -> NONCE_SLOT_CONFLICT
    let bad = transfer_calldata([0xEEu8; 20], &U256::from(60_000_000u64));
    let tx5b = unsigned_tx(CHAIN, 5, fx.token, bad, 5, 10, 60_000);
    let r = sign(svc, sign_req(tx5b, 3, Some(uuid), 0)).await;
    expect_reject(&r, RejectCode::NonceSlotConflict);

    // nonce 5: GAP_FILLER cancel -> signed (slot gains filler_signed, keeps fields)
    let fill = unsigned_tx(CHAIN, 5, fx.payout, vec![], 5, 10, 21_000);
    expect_signed(&sign(svc, sign_req(fill, 4, None, 0)).await);

    // nonce 5: deep-reorg replay of the ORIGINAL business tx -> still signed
    let tx5o = unsigned_tx(CHAIN, 5, fx.token, data.clone(), 1, 2, 60_000);
    expect_signed(&sign(svc, sign_req(tx5o, 3, Some(uuid), 0)).await);

    // nonce 6: GAP_FILLER first (bare hole) -> signed, creates filler slot
    let hole = unsigned_tx(CHAIN, 6, fx.payout, vec![], 1, 2, 21_000);
    expect_signed(&sign(svc, sign_req(hole, 4, None, 0)).await);

    // nonce 6: PAYOUT on a filler-burned nonce -> NONCE_SLOT_CONFLICT
    let tx6 = unsigned_tx(CHAIN, 6, fx.token, data.clone(), 1, 2, 60_000);
    let r = sign(svc, sign_req(tx6, 3, Some([0xD2u8; 16]), 0)).await;
    expect_reject(&r, RejectCode::NonceSlotConflict);

    // (payout_uuid, retry_seq) already bound to nonce 5 -> new nonce rejected
    let tx7 = unsigned_tx(CHAIN, 7, fx.token, data.clone(), 1, 2, 60_000);
    let r = sign(svc, sign_req(tx7, 3, Some(uuid), 0)).await;
    expect_reject(&r, RejectCode::NonceSlotConflict);
}

// ---------------------------------------------------------------------------
// 7. 24h sliding-window circuit breaker
// ---------------------------------------------------------------------------

#[tokio::test]
async fn daily_limit_breaker_trips() {
    let fx = fixture(0, Some(U256::from(100u64)));
    let svc = &fx.svc;
    let recip = [0x33u8; 20];
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let payload = addr_v1_payload(CHAIN, &MERCHANT_GOLDEN, &recip, now + 30 * 86_400);
    register(svc, payload.clone(), ed_sign(&fx.admin_sk, &payload)).await;

    // 60 ok
    let data = transfer_calldata(recip, &U256::from(60u64));
    let tx = unsigned_tx(CHAIN, 1, fx.token, data.clone(), 1, 2, 60_000);
    expect_signed(&sign(svc, sign_req(tx, 3, Some([0xE1u8; 16]), 0)).await);

    // +50 -> 110 > 100 -> DAILY_LIMIT_EXCEEDED
    let tx2 = unsigned_tx(CHAIN, 2, fx.token, transfer_calldata(recip, &U256::from(50u64)), 1, 2, 60_000);
    let r = sign(svc, sign_req(tx2, 3, Some([0xE2u8; 16]), 0)).await;
    expect_reject(&r, RejectCode::DailyLimitExceeded);

    // replay of nonce-1 tx at higher gas -> signed (no double deduction)
    let tx1r = unsigned_tx(CHAIN, 1, fx.token, data, 9, 20, 60_000);
    expect_signed(&sign(svc, sign_req(tx1r, 3, Some([0xE1u8; 16]), 0)).await);
}

// ---------------------------------------------------------------------------
// 8. Audit-chain tamper detection via the RPC
// ---------------------------------------------------------------------------

#[tokio::test]
async fn audit_chain_detects_db_tamper() {
    let fx = fixture(0, None);
    let svc = &fx.svc;
    let resp = register(svc, unhex(GOLDEN_ADDR_PAYLOAD), unhex(GOLDEN_ADDR_SIG)).await;
    assert!(resp.accepted);
    let data = transfer_calldata([0x33u8; 20], &U256::from(1u64));
    let tx = unsigned_tx(CHAIN, 0, fx.token, data, 1, 2, 60_000);
    expect_signed(&sign(svc, sign_req(tx, 3, Some([0xAAu8; 16]), 0)).await);

    // Tamper: forge the reason on seq=1 — VerifyAuditChain must flag seq 1.
    svc.core_handle()
        .lock()
        .unwrap()
        .db()
        .conn
        .execute("UPDATE audit_logs SET reason='forged-by-attacker' WHERE seq=1", [])
        .unwrap();
    let v = svc
        .verify_audit_chain(Request::new(VerifyAuditChainRequest { from_seq: 0, to_seq: 0 }))
        .await
        .unwrap()
        .into_inner();
    assert!(!v.valid);
    assert_eq!(v.first_bad_seq, 1);

    // Tamper #2: delete the last row — the chain itself still verifies over
    // the remaining rows (truncation is surfaced via tip_hash/entries_checked
    // for the watchdog to diff against a stored tip).
    svc.core_handle()
        .lock()
        .unwrap()
        .db()
        .conn
        .execute("DELETE FROM audit_logs WHERE seq=2", [])
        .unwrap();
    let v2 = svc
        .verify_audit_chain(Request::new(VerifyAuditChainRequest { from_seq: 0, to_seq: 0 }))
        .await
        .unwrap()
        .into_inner();
    assert_eq!(v2.entries_checked, 1);
}

// ---------------------------------------------------------------------------
// 9. Full mTLS e2e: real rustls handshake, client-cert enforcement,
//    admin direct-connect, and cert hot-reload.
// ---------------------------------------------------------------------------

struct Pki {
    ca_pem: String,
    ca_der: Vec<u8>,
    server_cert: rcgen::Certificate,
    server_key: rcgen::KeyPair,
    client_pem: String,
    client_key: String,
    admin_pem: String,
    admin_key: String,
    ca_cert: rcgen::Certificate,
    ca_key: rcgen::KeyPair,
}

fn make_pki() -> Pki {
    use rcgen::*;
    let ca_key = KeyPair::generate().unwrap();
    let mut cap = CertificateParams::new(Vec::<String>::new()).unwrap();
    cap.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    cap.distinguished_name.push(DnType::CommonName, "vaultflow-test-ca");
    let ca_cert = cap.self_signed(&ca_key).unwrap();

    let issue = |cn: &str, eku: ExtendedKeyUsagePurpose, san_dns: bool| -> (String, String) {
        let key = KeyPair::generate().unwrap();
        let mut p = if san_dns {
            CertificateParams::new(vec!["localhost".to_string()]).unwrap()
        } else {
            CertificateParams::new(Vec::<String>::new()).unwrap()
        };
        if san_dns {
            p.subject_alt_names
                .push(SanType::IpAddress(IpAddr::V4(Ipv4Addr::LOCALHOST)));
        }
        p.distinguished_name.push(DnType::CommonName, cn);
        p.extended_key_usages = vec![eku];
        let cert = p.signed_by(&key, &ca_cert, &ca_key).unwrap();
        (cert.pem(), key.serialize_pem())
    };

    let (client_pem, client_key) = issue("gateway-core", ExtendedKeyUsagePurpose::ClientAuth, false);
    let (admin_pem, admin_key) = issue("admin-approver", ExtendedKeyUsagePurpose::ClientAuth, false);

    // Server leaf — keep the rcgen objects so the DER can be asserted after
    // hot-reload rotation.
    let skey = KeyPair::generate().unwrap();
    let mut sp = CertificateParams::new(vec!["localhost".to_string()]).unwrap();
    sp.subject_alt_names
        .push(SanType::IpAddress(IpAddr::V4(Ipv4Addr::LOCALHOST)));
    sp.distinguished_name.push(DnType::CommonName, "vault-signer");
    sp.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    let server_cert = sp.signed_by(&skey, &ca_cert, &ca_key).unwrap();

    Pki {
        ca_pem: ca_cert.pem(),
        ca_der: ca_cert.der().to_vec(),
        server_cert,
        server_key: skey,
        client_pem,
        client_key,
        admin_pem,
        admin_key,
        ca_cert,
        ca_key,
    }
}

fn write_pems(dir: &Path, ca: &str, cert: &str, key: &str) -> CertPaths {
    let ca_p = dir.join("ca.pem");
    let cert_p = dir.join("server.pem");
    let key_p = dir.join("server.key");
    std::fs::write(&ca_p, ca).unwrap();
    std::fs::write(&cert_p, cert).unwrap();
    std::fs::write(&key_p, key).unwrap();
    CertPaths {
        server_cert: cert_p,
        server_key: key_p,
        client_ca: ca_p,
    }
}

fn client_channel(
    port: u16,
    ca_pem: &str,
    identity: Option<(&str, &str)>,
) -> impl std::future::Future<Output = Result<Channel, tonic::transport::Error>> {
    let mut tls = ClientTlsConfig::new().ca_certificate(Certificate::from_pem(ca_pem));
    if let Some((cert, key)) = identity {
        tls = tls.identity(Identity::from_pem(cert, key));
    }
    let ep = Endpoint::from_shared(format!("https://localhost:{port}"))
        .unwrap()
        .tls_config(tls)
        .unwrap();
    async move { ep.connect().await }
}

#[tokio::test]
async fn mtls_end_to_end() {
    let dir = tempfile::tempdir().unwrap();
    let pki = make_pki();
    let paths = write_pems(
        dir.path(),
        &pki.ca_pem,
        &pki.server_cert.pem(),
        &pki.server_key.serialize_pem(),
    );

    let fx = fixture(0, None);
    let tls = ReloadingTls::load(paths.clone()).unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let svc = fx.svc.clone();
    let tls_task = tls.clone();
    let server = tokio::spawn(async move {
        vault_signer::grpc::server::serve(listener, tls_task, svc, std::future::pending()).await
    });
    tls.spawn_reloader(Duration::from_millis(50));
    tokio::time::sleep(Duration::from_millis(150)).await;

    // -- gateway-core client (mTLS) ------------------------------------------
    let channel = client_channel(port, &pki.ca_pem, Some((&pki.client_pem, &pki.client_key)))
        .await
        .expect("mTLS connect failed");
    let mut client = VaultSignerServiceClient::new(channel);

    let health = client
        .get_signer_health(GetSignerHealthRequest {})
        .await
        .unwrap()
        .into_inner();
    assert!(!health.version.is_empty());
    assert_eq!(health.loaded_wallets.len(), 2);

    // sign a gap filler through the wire
    let tx = unsigned_tx(CHAIN, 0, fx.relayer, vec![], 1, 2, 21_000);
    let resp = client
        .sign_transaction(SignTransactionRequest {
            chain_id: CHAIN,
            unsigned_tx: tx.into(),
            policy_hint: 4,
            payout_uuid: vec![].into(),
            retry_seq: 0,
            refund_ticket: None,
        })
        .await
        .unwrap()
        .into_inner();
    match resp.result.unwrap() {
        sign_transaction_response::Result::Signed(s) => {
            assert_eq!(crypto::recover_signer(&s.raw_tx).unwrap(), fx.relayer);
        }
        r => panic!("expected signed, got {r:?}"),
    }

    // -- admin direct-connect (own client cert) -------------------------------
    let channel = client_channel(port, &pki.ca_pem, Some((&pki.admin_pem, &pki.admin_key)))
        .await
        .expect("admin mTLS connect failed");
    let mut admin = VaultSignerServiceClient::new(channel);
    // register first so there's something to revoke (golden payload+sig)
    let reg = admin
        .register_withdrawal_address(RegisterWithdrawalAddressRequest {
            signed_payload: unhex(GOLDEN_ADDR_PAYLOAD).into(),
            admin_signature: unhex(GOLDEN_ADDR_SIG).into(),
        })
        .await
        .unwrap()
        .into_inner();
    assert!(reg.accepted, "{}", reg.reason);
    let rev = admin
        .revoke_withdrawal_address(RevokeWithdrawalAddressRequest {
            chain_id: CHAIN,
            merchant_uuid: MERCHANT_GOLDEN.to_vec().into(),
            recipient_address: vec![0x33u8; 20].into(),
            admin_note: "e2e revoke".into(),
        })
        .await
        .unwrap()
        .into_inner();
    assert!(rev.accepted, "{}", rev.reason);

    // -- NO client cert -> the TLS alert races connect(), so the observable
    //    enforcement is that every RPC fails before ever being served. ------
    let no_cert = client_channel(port, &pki.ca_pem, None).await;
    match no_cert {
        Ok(ch) => {
            let mut c = VaultSignerServiceClient::new(ch);
            assert!(
                c.get_signer_health(GetSignerHealthRequest {}).await.is_err(),
                "cert-less client must not be served"
            );
        }
        Err(_) => {} // eager handshake failure is equally valid enforcement
    }

    // -- hot reload: rotate server leaf; new connections see the NEW cert -----
    let new_key = rcgen::KeyPair::generate().unwrap();
    let mut sp = rcgen::CertificateParams::new(vec!["localhost".to_string()]).unwrap();
    sp.subject_alt_names
        .push(rcgen::SanType::IpAddress(IpAddr::V4(Ipv4Addr::LOCALHOST)));
    sp.distinguished_name
        .push(rcgen::DnType::CommonName, "vault-signer");
    sp.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
    let new_cert = sp.signed_by(&new_key, &pki.ca_cert, &pki.ca_key).unwrap();
    std::fs::write(&paths.server_cert, new_cert.pem()).unwrap();
    std::fs::write(&paths.server_key, new_key.serialize_pem()).unwrap();

    // wait for the reloader (50ms interval)
    let mut reloaded = false;
    for _ in 0..40 {
        if tls.check_reload().unwrap() {
            reloaded = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert!(reloaded, "reload never happened");

    // fresh mTLS connection still works
    let channel = client_channel(port, &pki.ca_pem, Some((&pki.client_pem, &pki.client_key)))
        .await
        .expect("post-rotation connect failed");
    let mut client2 = VaultSignerServiceClient::new(channel);
    client2
        .verify_audit_chain(VerifyAuditChainRequest { from_seq: 0, to_seq: 0 })
        .await
        .unwrap();

    // prove the NEW cert is actually presented: raw TLS handshake, inspect
    // the peer chain and compare DER to the rotated leaf.
    use tokio_rustls::rustls::pki_types::ServerName;
    use tokio_rustls::rustls::{ClientConfig, RootCertStore};
    use tokio_rustls::TlsConnector;
    let mut roots = RootCertStore::empty();
    roots
        .add(tokio_rustls::rustls::pki_types::CertificateDer::from(pki.ca_der.clone()))
        .unwrap();
    let certs: Vec<_> = rustls_pemfile::certs(&mut pki.client_pem.as_bytes())
        .collect::<Result<_, _>>()
        .unwrap();
    let key = rustls_pemfile::private_key(&mut pki.client_key.as_bytes())
        .unwrap()
        .unwrap();
    let cc = ClientConfig::builder()
        .with_root_certificates(roots)
        .with_client_auth_cert(certs, key)
        .unwrap();
    let connector = TlsConnector::from(Arc::new(cc));
    let sock = tokio::net::TcpStream::connect(("127.0.0.1", port)).await.unwrap();
    let server_name = ServerName::try_from("localhost").unwrap().to_owned();
    let tstream = connector.connect(server_name, sock).await.unwrap();
    let (_, session) = tstream.get_ref();
    let peer = session.peer_certificates().unwrap();
    assert_eq!(
        peer[0].as_ref(),
        new_cert.der().as_ref(),
        "server did not present the rotated certificate"
    );

    server.abort();
}

// ---------------------------------------------------------------------------
// 10. FIX-C / audit P0-5: revoked-lease replay defense + resurrection rules
// ---------------------------------------------------------------------------

/// A REVOKED whitelist entry must not be resurrected by replaying ANY
/// previously-accepted signature (the untrusted gateway holds them all);
/// a never-seen signature is a fresh admin approval and may resurrect —
/// with `first_seen_at` re-anchored so the new lease re-serves its cooldown.
#[tokio::test]
async fn revoked_lease_replay_rejected_fresh_sig_resurrects() {
    // cooldown = 1h so a resurrected lease's fresh cooldown is observable.
    let fx = fixture(3_600, None);
    let svc = &fx.svc;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let recip = [0xC5u8; 20];

    // (a) Register lease A, then backdate first_seen_at so its cooldown is
    //     already served — the sign path is proven usable pre-revoke.
    let payload_a = addr_v1_payload(CHAIN, &MERCHANT_GOLDEN, &recip, now + 30 * 86_400);
    let sig_a = ed_sign(&fx.admin_sk, &payload_a);
    let resp = register(svc, payload_a.clone(), sig_a.clone()).await;
    assert!(resp.accepted);
    {
        let core = svc.core_handle();
        let g = core.lock().unwrap();
        g.db()
            .conn
            .execute(
                "UPDATE withdrawal_whitelist SET first_seen_at=?1
                 WHERE chain_id=?2 AND recipient_address=?3",
                rusqlite::params![now as i64 - 100_000, CHAIN as i64, recip.as_slice()],
            )
            .unwrap();
    }
    let data = transfer_calldata(recip, &U256::from(1u64));
    let tx = unsigned_tx(CHAIN, 0, fx.token, data.clone(), 1, 2, 60_000);
    expect_signed(&sign(svc, sign_req(tx, 3, Some([0xC6u8; 16]), 0)).await);

    // (b) Emergency revoke, then replay the SAME payload + signature ->
    //     rejected as a stale proof (entry stays REVOKED).
    let rev = svc
        .revoke_withdrawal_address(Request::new(RevokeWithdrawalAddressRequest {
            chain_id: CHAIN,
            merchant_uuid: MERCHANT_GOLDEN.to_vec().into(),
            recipient_address: recip.to_vec().into(),
            admin_note: "emergency".into(),
        }))
        .await
        .unwrap()
        .into_inner();
    assert!(rev.accepted);

    let replay = register(svc, payload_a.clone(), sig_a.clone()).await;
    assert!(!replay.accepted, "stale proof must not resurrect a revoked lease");
    assert_eq!(replay.reject_code, RejectCode::WhitelistRevoked as i32);
    {
        let core = svc.core_handle();
        let g = core.lock().unwrap();
        let rows = db::whitelist_for_recipient(&g.db().conn, CHAIN, &Address::from(recip)).unwrap();
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].status, "REVOKED", "rejected replay must leave the row revoked");
    }

    // (c) A FRESH lease (different valid_until => different signature) is a
    //     new admin approval and DOES resurrect the row…
    let payload_b = addr_v1_payload(CHAIN, &MERCHANT_GOLDEN, &recip, now + 45 * 86_400);
    let sig_b = ed_sign(&fx.admin_sk, &payload_b);
    let resp = register(svc, payload_b.clone(), sig_b.clone()).await;
    assert!(resp.accepted, "fresh admin approval must resurrect: {}", resp.reason);

    // …and first_seen_at is re-anchored (new approval => new cooldown,
    // ARCHITECTURE §1.2): the backdated anchor is gone and signing now hits
    // WHITELIST_COOLDOWN again.
    {
        let core = svc.core_handle();
        let g = core.lock().unwrap();
        let rows = db::whitelist_for_recipient(&g.db().conn, CHAIN, &Address::from(recip)).unwrap();
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].status, "ACTIVE");
        assert!(
            rows[0].first_seen_at > now as i64 - 100_000,
            "resurrection must refresh first_seen_at, got {}",
            rows[0].first_seen_at
        );
    }
    let tx = unsigned_tx(CHAIN, 1, fx.token, data.clone(), 1, 2, 60_000);
    let r = sign(svc, sign_req(tx, 3, Some([0xC7u8; 16]), 0)).await;
    expect_reject(&r, RejectCode::WhitelistCooldown);

    // (d) Revoke again: BOTH previously-seen signatures are now burned —
    //     superseded proofs (sig_a) can't resurrect either, not only the
    //     currently-stored one.
    let rev2 = svc
        .revoke_withdrawal_address(Request::new(RevokeWithdrawalAddressRequest {
            chain_id: CHAIN,
            merchant_uuid: MERCHANT_GOLDEN.to_vec().into(),
            recipient_address: recip.to_vec().into(),
            admin_note: "second".into(),
        }))
        .await
        .unwrap()
        .into_inner();
    assert!(rev2.accepted);
    for (p, s) in [(payload_a.clone(), sig_a.clone()), (payload_b.clone(), sig_b.clone())] {
        let r = register(svc, p, s).await;
        assert!(!r.accepted, "any previously-seen proof must stay burned");
        assert_eq!(r.reject_code, RejectCode::WhitelistRevoked as i32);
    }

    // (e) Yet another fresh approval still resurrects — the kill-switch
    //     stays meaningful while legitimate re-onboarding remains possible.
    let payload_c = addr_v1_payload(CHAIN, &MERCHANT_GOLDEN, &recip, now + 50 * 86_400);
    let resp = register(svc, payload_c.clone(), ed_sign(&fx.admin_sk, &payload_c)).await;
    assert!(resp.accepted, "fresh approval must resurrect after every revoke: {}", resp.reason);
}

// ---------------------------------------------------------------------------
// 11. UpdateColdWallets: dynamic COLD allowlist — runtime add/remove takes
//     effect on the very next SWEEP sign call; idempotent; validated.
// ---------------------------------------------------------------------------

async fn update_cold(svc: &VaultSignerSvc, chain: u64, add: Vec<Vec<u8>>, remove: Vec<Vec<u8>>) -> UpdateColdWalletsResponse {
    svc.update_cold_wallets(Request::new(UpdateColdWalletsRequest {
        chain_id: chain,
        add: add.into_iter().map(Into::into).collect(),
        remove: remove.into_iter().map(Into::into).collect(),
    }))
    .await
    .unwrap()
    .into_inner()
}

#[tokio::test]
async fn update_cold_wallets_runtime_rotation() {
    let fx = fixture(0, None);
    let svc = &fx.svc;
    let cold = Address::from([0xCCu8; 20]);   // seeded from config
    let cold2 = Address::from([0xCDu8; 20]);  // seeded from config
    let new_cold = Address::from([0xDDu8; 20]);

    // Boot seeding: config allowlist lands in the DB.
    let r = update_cold(svc, CHAIN, vec![], vec![]).await;
    assert!(r.accepted, "{}", r.reason);
    assert_eq!(
        r.active_cold_wallets,
        vec![fmt_addr(&cold), fmt_addr(&cold2)]
    );

    // Rotation window: add new wallet, keep both (dual-track).
    let r = update_cold(
        svc,
        CHAIN,
        vec![new_cold.as_slice().to_vec()],
        vec![],
    )
    .await;
    assert!(r.accepted, "{}", r.reason);
    assert_eq!(
        r.active_cold_wallets,
        vec![fmt_addr(&cold), fmt_addr(&cold2), fmt_addr(&new_cold)]
    );

    // SWEEP to the NEW cold wallet now signs (live effect, no restart).
    let data = sweep_calldata(*fx.token.0, *new_cold.0, &[[0x77u8; 32]]);
    let tx = unsigned_tx(CHAIN, 0, fx.cfg.batch_sweeper, data, 1, 2, 200_000);
    expect_signed(&sign(svc, sign_req(tx, 1, None, 0)).await);

    // Cutover complete: remove BOTH old wallets.
    let r = update_cold(
        svc,
        CHAIN,
        vec![],
        vec![cold.as_slice().to_vec(), cold2.as_slice().to_vec()],
    )
    .await;
    assert!(r.accepted, "{}", r.reason);
    assert_eq!(r.active_cold_wallets, vec![fmt_addr(&new_cold)]);

    // SWEEP to the removed cold wallet is now COLD_WALLET_NOT_ALLOWED.
    let data = sweep_calldata(*fx.token.0, *cold.0, &[[0x77u8; 32]]);
    let tx = unsigned_tx(CHAIN, 1, fx.cfg.batch_sweeper, data, 1, 2, 200_000);
    let r = sign(svc, sign_req(tx, 1, None, 0)).await;
    expect_reject(&r, RejectCode::ColdWalletNotAllowed);

    // Idempotent replay of the last update — same effective list.
    let r = update_cold(
        svc,
        CHAIN,
        vec![],
        vec![cold.as_slice().to_vec(), cold2.as_slice().to_vec()],
    )
    .await;
    assert!(r.accepted, "{}", r.reason);
    assert_eq!(r.active_cold_wallets, vec![fmt_addr(&new_cold)]);
}

#[tokio::test]
async fn update_cold_wallets_rejects_bad_input() {
    let fx = fixture(0, None);
    let svc = &fx.svc;
    let cold = Address::from([0xCCu8; 20]);

    // (a) malformed address length -> POLICY_VIOLATION (audited reject).
    let r = update_cold(svc, CHAIN, vec![vec![0xAAu8; 19]], vec![]).await;
    assert!(!r.accepted);
    assert_eq!(r.reject_code, RejectCode::PolicyViolation as i32);
    assert!(r.audit_seq > 0, "rejected update must land on audit chain");

    // (b) same address in add AND remove -> ambiguous, rejected.
    let r = update_cold(
        svc,
        CHAIN,
        vec![cold.as_slice().to_vec()],
        vec![cold.as_slice().to_vec()],
    )
    .await;
    assert!(!r.accepted);
    assert_eq!(r.reject_code, RejectCode::PolicyViolation as i32);

    // (c) unconfigured chain_id -> rejected; response still carries the
    //     (empty) effective list for that chain.
    let r = update_cold(svc, 999, vec![], vec![]).await;
    assert!(!r.accepted);
    assert_eq!(r.reject_code, RejectCode::PolicyViolation as i32);
    assert!(r.active_cold_wallets.is_empty());

    // The seeded list survived all rejections untouched.
    let r = update_cold(svc, CHAIN, vec![], vec![]).await;
    assert_eq!(
        r.active_cold_wallets,
        vec![fmt_addr(&cold), fmt_addr(&Address::from([0xCDu8; 20]))]
    );
}

// ---------------------------------------------------------------------------
// 12. GenerateWallet: runtime RELAYER/PAYOUT provisioning — envelope lands in
//     encrypted_keys satisfying the boot integrity check; the DEK joins the
//     in-memory key map WITHOUT restart (GAP_FILLER signable at once); only
//     the address is ever returned.
// ---------------------------------------------------------------------------

#[tokio::test]
async fn generate_wallet_provisions_and_live_picks_up() {
    let fx = fixture(0, None);
    let svc = &fx.svc;

    // (a) valid PAYOUT provisioning.
    let resp = svc
        .generate_wallet(Request::new(GenerateWalletRequest {
            chain_id: CHAIN,
            role: "PAYOUT".to_string(),
        }))
        .await
        .unwrap()
        .into_inner();
    assert!(resp.audit_seq > 0);
    assert!(resp.address.starts_with("0x") && resp.address.len() == 42);
    let new_addr = Address::from_slice(&unhex(&resp.address));

    // (b) RELAYER also accepted; bogus role / chain -> INVALID_ARGUMENT and
    //     a REJECTED audit entry.
    let resp2 = svc
        .generate_wallet(Request::new(GenerateWalletRequest {
            chain_id: CHAIN,
            role: "RELAYER".to_string(),
        }))
        .await
        .unwrap()
        .into_inner();
    assert!(resp2.address.starts_with("0x") && resp2.address != resp.address);

    let err = svc
        .generate_wallet(Request::new(GenerateWalletRequest {
            chain_id: CHAIN,
            role: "COLD".to_string(),
        }))
        .await
        .unwrap_err();
    assert_eq!(err.code(), tonic::Code::InvalidArgument);
    let err = svc
        .generate_wallet(Request::new(GenerateWalletRequest {
            chain_id: 999,
            role: "PAYOUT".to_string(),
        }))
        .await
        .unwrap_err();
    assert_eq!(err.code(), tonic::Code::InvalidArgument);

    // (c) persisted envelope satisfies the boot integrity check: decrypting
    //     it under the same KEK derives exactly the returned address, and the
    //     label records role + chain.
    {
        let core = svc.core_handle();
        let g = core.lock().unwrap();
        let rows = db::load_encrypted_keys(&g.db().conn).unwrap();
        let row = rows
            .iter()
            .find(|r| r.wallet_address == resp.address)
            .expect("generated key must be persisted");
        assert_eq!(row.label, format!("role:PAYOUT:chain:{CHAIN}"));
        let kms2 = LocalFileKms::from_kek_bytes(&KEK);
        let dek = kms2.decrypt_dek(&row.dek_envelope).unwrap();
        assert_eq!(
            crypto::address_from_dek(dek.expose_secret()).unwrap(),
            new_addr,
            "envelope must derive the returned address (boot integrity)"
        );
        // Live pickup: wallet already in the in-memory key map.
        assert!(g.loaded_wallets().contains(&new_addr));
    }

    // (d) LIVE signing: a GAP_FILLER self-transfer of the brand-new wallet
    //     signs WITHOUT restart and is recoverable back to it.
    let tx = unsigned_tx(CHAIN, 0, new_addr, vec![], 1, 2, 21_000);
    let s = expect_signed(&sign(svc, sign_req(tx, 4, None, 0)).await);
    assert_eq!(crypto::recover_signer(&s.raw_tx).unwrap(), new_addr);

    // (e) audit chain intact over register/sign/generate/update traffic.
    let v = svc
        .verify_audit_chain(Request::new(VerifyAuditChainRequest { from_seq: 0, to_seq: 0 }))
        .await
        .unwrap()
        .into_inner();
    assert!(v.valid);
}
