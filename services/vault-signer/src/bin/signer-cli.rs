//! signer-cli — vault-signer 运维管理端 (mTLS gRPC 客户端).
//!
//! 冷热钱包管理是签名域内的运维操作: 本工具以专用客户端证书直连 signer
//! (默认 admin-approver 身份), 全部写操作经 policy 校验 + 审计链落盘;
//! 热钱包私钥材料永不离开 signer 进程 — CLI 只见派生地址.
//!
//! 用法:
//!   signer-cli health
//!   signer-cli hot list
//!   signer-cli hot generate --chain-id <N> --role RELAYER|PAYOUT
//!   signer-cli cold list    --chain-id <N>
//!   signer-cli cold add     --chain-id <N> --address 0x... [--address 0x...]
//!   signer-cli cold remove  --chain-id <N> --address 0x... [--address 0x...]
//!
//! 连接参数 (旗标 > 环境变量 > 容器内默认):
//!   --endpoint     $SIGNER_CLI_ENDPOINT | https://127.0.0.1:50051
//!   --server-name  $MTLS_SERVER_NAME    | vault-signer
//!   --cert         $MTLS_CLIENT_CERT    | /etc/vaultflow/certs/clients/admin-approver.crt
//!   --key          $MTLS_CLIENT_KEY     | /etc/vaultflow/certs/clients/admin-approver.key
//!   --ca           $MTLS_CA_CERT        | /etc/vaultflow/certs/ca/ca.crt
//!
//! 热钱包无删/改命令 — 密钥均为派生品, 角色由 config/label 决定;
//! 下线权威热钱包需经 signer-config cutover (见 ARCHITECTURE).

use std::{env, fs, process};

use anyhow::{anyhow, bail, Context};
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity};

use vault_signer::grpc::signer_v1::vault_signer_service_client::VaultSignerServiceClient;
use vault_signer::grpc::signer_v1::*;

const USAGE: &str = "\
signer-cli — vault-signer ops client (mTLS)

  health
  hot list
  hot generate --chain-id <N> --role RELAYER|PAYOUT
  cold list    --chain-id <N>
  cold add     --chain-id <N> --address 0x... [--address 0x...]
  cold remove  --chain-id <N> --address 0x... [--address 0x...]

flags: --endpoint --server-name --cert --key --ca";

fn env_or(key: &str, default: &str) -> String {
    env::var(key).ok().filter(|v| !v.is_empty()).unwrap_or_else(|| default.to_string())
}

fn parse_addr20(s: &str) -> anyhow::Result<Vec<u8>> {
    let h = s.strip_prefix("0x").unwrap_or(s);
    let b = hex::decode(h).with_context(|| format!("bad hex address {s}"))?;
    if b.len() != 20 {
        bail!("address {s} is {}B — must be exactly 20B", b.len());
    }
    Ok(b)
}

fn ts(unix: i64) -> String {
    // seconds -> "YYYY-MM-DD HH:MM:SS" UTC (no chrono dep — civil conversion).
    let days = unix.div_euclid(86_400);
    let secs = unix.rem_euclid(86_400);
    // Howard Hinnant civil-from-days algorithm.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    format!("{y:04}-{m:02}-{d:02} {:02}:{:02}:{:02}Z", secs / 3600, secs % 3600 / 60, secs % 60)
}

#[tokio::main]
async fn main() {
    if let Err(e) = run().await {
        eprintln!("error: {e:#}");
        process::exit(1);
    }
}

async fn run() -> anyhow::Result<()> {
    let mut positional: Vec<String> = Vec::new();
    let mut endpoint = env_or("SIGNER_CLI_ENDPOINT", "https://127.0.0.1:50051");
    let mut server_name = env_or("MTLS_SERVER_NAME", "vault-signer");
    let mut cert_p = env_or("MTLS_CLIENT_CERT", "/etc/vaultflow/certs/clients/admin-approver.crt");
    let mut key_p = env_or("MTLS_CLIENT_KEY", "/etc/vaultflow/certs/clients/admin-approver.key");
    let mut ca_p = env_or("MTLS_CA_CERT", "/etc/vaultflow/certs/ca/ca.crt");
    let mut chain_id: Option<u64> = None;
    let mut role: Option<String> = None;
    let mut addresses: Vec<String> = Vec::new();

    let argv: Vec<String> = env::args().skip(1).collect();
    let mut i = 0;
    while i < argv.len() {
        let a = argv[i].as_str();
        let (k, v) = if let Some((kk, vv)) = a.strip_prefix("--").and_then(|s| s.split_once('=')) {
            (format!("--{kk}"), vv.to_string())
        } else if a.starts_with("--") {
            i += 1;
            let vv = argv.get(i).ok_or_else(|| anyhow!("{a} needs a value"))?.clone();
            (a.to_string(), vv)
        } else {
            positional.push(a.to_string());
            i += 1;
            continue;
        };
        match k.as_str() {
            "--endpoint" => endpoint = v,
            "--server-name" => server_name = v,
            "--cert" => cert_p = v,
            "--key" => key_p = v,
            "--ca" => ca_p = v,
            "--chain-id" => chain_id = Some(v.parse().with_context(|| format!("--chain-id {v}"))?),
            "--role" => role = Some(v.to_uppercase()),
            "--address" => addresses.push(v),
            "--help" | "-h" => {
                println!("{USAGE}");
                return Ok(());
            }
            _ => bail!("unknown flag {k}\n{USAGE}"),
        }
        i += 1;
    }
    if positional.is_empty() {
        bail!("missing command\n{USAGE}");
    }

    let tls = ClientTlsConfig::new()
        .domain_name(&server_name)
        .ca_certificate(Certificate::from_pem(
            fs::read(&ca_p).with_context(|| format!("read CA {ca_p}"))?,
        ))
        .identity(Identity::from_pem(
            fs::read(&cert_p).with_context(|| format!("read client cert {cert_p}"))?,
            fs::read(&key_p).with_context(|| format!("read client key {key_p}"))?,
        ));
    let channel = Channel::from_shared(endpoint.clone())
        .map_err(|e| anyhow!("endpoint {endpoint}: {e}"))?
        .tls_config(tls)
        .context("tls config")?
        .connect()
        .await
        .with_context(|| format!("connect {endpoint}"))?;
    let mut c = VaultSignerServiceClient::new(channel);

    let need_chain = |what: &str| -> anyhow::Result<u64> {
        chain_id.ok_or_else(|| anyhow!("{what} needs --chain-id"))
    };
    let need_addrs = |what: &str| -> anyhow::Result<Vec<Vec<u8>>> {
        if addresses.is_empty() {
            bail!("{what} needs at least one --address");
        }
        addresses.iter().map(|a| parse_addr20(a)).collect()
    };

    match positional.iter().map(String::as_str).collect::<Vec<_>>().as_slice() {
        ["health"] => {
            let h = c.get_signer_health(GetSignerHealthRequest {}).await?.into_inner();
            println!("version           {}", h.version);
            println!("uptime            {}s", h.uptime_seconds);
            println!("signer_clock      {}", ts(h.signer_clock_unix));
            println!("whitelist_entries {}", h.whitelist_entries);
            println!("audit_entries     {}", h.audit_entries);
            println!("audit_tip         0x{}", hex::encode(&h.audit_tip_hash));
            println!("loaded_wallets    {}", h.loaded_wallets.len());
            for w in &h.loaded_wallets {
                println!("  {w}");
            }
        }
        ["hot", "list"] => {
            let r = c.list_wallets(ListWalletsRequest {}).await?.into_inner();
            if r.wallets.is_empty() {
                println!("(no hot wallets)");
            }
            for w in r.wallets {
                println!(
                    "{}  {:<38}  created={}  loaded={}",
                    w.address,
                    if w.label.is_empty() { "-" } else { w.label.as_str() },
                    ts(w.created_at_unix),
                    w.loaded
                );
            }
        }
        ["hot", "generate"] => {
            let cid = need_chain("hot generate")?;
            let r = role.ok_or_else(|| anyhow!("hot generate needs --role RELAYER|PAYOUT"))?;
            let resp = c
                .generate_wallet(GenerateWalletRequest { chain_id: cid, role: r.clone() })
                .await
                .map_err(|s| anyhow!("generate_wallet rejected: {}", s.message()))?
                .into_inner();
            println!("generated {} hot wallet on chain {cid}:", r);
            println!("  address   {}", resp.address);
            println!("  audit_seq {}", resp.audit_seq);
        }
        ["cold", "list"] => {
            let cid = need_chain("cold list")?;
            let r = c
                .list_cold_wallets(ListColdWalletsRequest { chain_id: cid })
                .await?
                .into_inner();
            if r.active_cold_wallets.is_empty() {
                println!("(no cold wallets on chain {cid})");
            }
            for w in r.active_cold_wallets {
                println!("{w}");
            }
        }
        ["cold", "add"] | ["cold", "remove"] => {
            let cid = need_chain("cold")?;
            let addrs = need_addrs("cold")?;
            let is_add = positional[1] == "add";
            let (add, remove) = if is_add { (addrs, vec![]) } else { (vec![], addrs) };
            let req = UpdateColdWalletsRequest { chain_id: cid, add, remove };
            let resp = c.update_cold_wallets(req).await?.into_inner();
            if !resp.accepted {
                bail!("update rejected (code={}): {}", resp.reject_code, resp.reason);
            }
            println!("cold wallets on chain {cid} after update (audit_seq={}):", resp.audit_seq);
            for w in resp.active_cold_wallets {
                println!("  {w}");
            }
        }
        other => bail!("unknown command {:?}\n{}", other.join(" "), USAGE),
    }
    Ok(())
}
