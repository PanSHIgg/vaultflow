//! JSON runtime config — the signer's ONLY source of firewall parameters.
//! Loaded from a file the operator controls; never from request fields.

use std::path::Path;

use alloy_primitives::{Address, U256};
use serde::Deserialize;

use crate::grpc::mtls::CertPaths;
use crate::policy::ChainPolicyConfig;

#[derive(Debug, Clone, Deserialize)]
pub struct SignerConfig {
    /// gRPC listen address, e.g. "127.0.0.1:50051".
    pub listen_addr: String,
    /// SQLite state file path.
    pub db_path: String,
    /// KEK credential file (32 raw bytes or 64-char hex, optional 0x).
    pub kek_file: String,
    /// Admin approver Ed25519 public key, 64-char hex (optional 0x).
    pub admin_approver_pubkey_hex: String,
    /// PEM cert paths for the mTLS server.
    pub certs: CertPathsCfg,
    /// Cert hot-reload poll interval (default 5s).
    #[serde(default)]
    pub cert_reload_interval_secs: Option<u64>,
    /// Per-chain firewall configs.
    pub chains: Vec<ChainCfg>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct CertPathsCfg {
    pub server_cert: String,
    pub server_key: String,
    pub client_ca: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ChainCfg {
    pub chain_id: u64,
    pub relayer_hot_wallet: String,
    pub payout_hot_wallet: String,
    pub batch_sweeper: String,
    pub eip3009_router: String,
    #[serde(default)]
    pub custody_tokens: Vec<String>,
    #[serde(default)]
    pub allowed_cold_wallets: Vec<String>,
    /// Whitelist cooldown in seconds (e.g. 86400 = 24h).
    #[serde(default = "default_cooldown")]
    pub cooldown_secs: u64,
    /// Global 24h breaker limit in ATOMIC units as a decimal string
    /// (U256 cannot cross JSON numbers safely). Omit to disable.
    #[serde(default)]
    pub daily_payout_limit_atomic: Option<String>,
}

fn default_cooldown() -> u64 {
    86_400
}

pub fn parse_addr(s: &str) -> anyhow::Result<Address> {
    let raw = hex::decode(s.trim_start_matches("0x").trim_start_matches("0X"))?;
    if raw.len() != 20 {
        anyhow::bail!("address must be 20 bytes (hex): {s}");
    }
    Ok(Address::from_slice(&raw))
}

pub fn parse_addr_list(list: &[String]) -> anyhow::Result<Vec<Address>> {
    list.iter().map(|s| parse_addr(s)).collect()
}

impl ChainCfg {
    pub fn into_policy(&self) -> anyhow::Result<ChainPolicyConfig> {
        let daily_payout_limit_atomic = match &self.daily_payout_limit_atomic {
            Some(s) => Some(
                U256::from_str_radix(s.trim(), 10)
                    .map_err(|e| anyhow::anyhow!("daily_payout_limit_atomic not decimal: {e}"))?,
            ),
            None => None,
        };
        Ok(ChainPolicyConfig {
            chain_id: self.chain_id,
            relayer_hot_wallet: parse_addr(&self.relayer_hot_wallet)?,
            payout_hot_wallet: parse_addr(&self.payout_hot_wallet)?,
            batch_sweeper: parse_addr(&self.batch_sweeper)?,
            eip3009_router: parse_addr(&self.eip3009_router)?,
            custody_tokens: parse_addr_list(&self.custody_tokens)?,
            allowed_cold_wallets: parse_addr_list(&self.allowed_cold_wallets)?,
            cooldown_secs: self.cooldown_secs,
            daily_payout_limit_atomic,
        })
    }
}

impl SignerConfig {
    pub fn cert_paths(&self) -> CertPaths {
        CertPaths {
            server_cert: self.certs.server_cert.clone().into(),
            server_key: self.certs.server_key.clone().into(),
            client_ca: self.certs.client_ca.clone().into(),
        }
    }

    pub fn admin_pubkey(&self) -> anyhow::Result<[u8; 32]> {
        let raw = hex::decode(
            self.admin_approver_pubkey_hex
                .trim_start_matches("0x")
                .trim_start_matches("0X"),
        )?;
        if raw.len() != 32 {
            anyhow::bail!("admin_approver_pubkey must be 32 bytes hex");
        }
        Ok(raw.try_into().unwrap())
    }

    pub fn chain_policies(&self) -> anyhow::Result<Vec<ChainPolicyConfig>> {
        self.chains.iter().map(|c| c.into_policy()).collect()
    }
}

/// Load + parse the JSON config file.
pub fn load(path: &Path) -> anyhow::Result<SignerConfig> {
    let text = std::fs::read_to_string(path)
        .map_err(|e| anyhow::anyhow!("read config {}: {e}", path.display()))?;
    let cfg: SignerConfig = serde_json::from_str(&text)
        .map_err(|e| anyhow::anyhow!("parse config {}: {e}", path.display()))?;
    if cfg.chains.is_empty() {
        anyhow::bail!("config must declare at least one chain");
    }
    Ok(cfg)
}
