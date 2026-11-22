//! vault-signer entry point — process hardening, config load, then the
//! hot-reloadable mTLS gRPC server.

use std::net::SocketAddr;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use vault_signer::{config, db, engine::SignerCore, grpc, kms::LocalFileKms};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    harden_process();
    tracing_subscriber::fmt::init();

    let cfg_path = std::env::var("VAULT_SIGNER_CONFIG")
        .unwrap_or_else(|_| "signer-config.json".to_string());
    let cfg = config::load(Path::new(&cfg_path))?;
    tracing::info!(path = %cfg_path, "config loaded");

    let signer_db = db::open(Path::new(&cfg.db_path))?;
    let kms = Arc::new(LocalFileKms::from_kek_file(Path::new(&cfg.kek_file))?);
    let chains = cfg.chain_policies()?;
    let admin_pubkey = cfg.admin_pubkey()?;
    let core = SignerCore::new(signer_db, kms, chains, admin_pubkey)?;
    tracing::info!(wallets = ?core.loaded_wallets(), "hot wallet DEKs ready");

    let tls = grpc::mtls::ReloadingTls::load(cfg.cert_paths())?;
    let _reloader = tls.spawn_reloader(Duration::from_secs(
        cfg.cert_reload_interval_secs.unwrap_or(5),
    ));

    let addr: SocketAddr = cfg.listen_addr.parse()?;
    let listener = tokio::net::TcpListener::bind(addr).await?;
    tracing::info!(%addr, "vault-signer listening (mTLS)");

    let svc = grpc::service::VaultSignerSvc::new(core);
    grpc::server::serve(listener, tls, svc, async {
        let _ = tokio::signal::ctrl_c().await;
        tracing::info!("shutdown signal received");
    })
    .await
}

/// Unix hardening: forbid core dumps and best-effort lock all memory pages so
/// hot key material can never hit swap. Warn — do not abort — on failure so a
/// dev shell without CAP_IPC_LOCK still runs.
#[cfg(unix)]
fn harden_process() {
    unsafe {
        let no_core = libc::rlimit { rlim_cur: 0, rlim_max: 0 };
        if libc::setrlimit(libc::RLIMIT_CORE, &no_core) != 0 {
            eprintln!("vault-signer: WARN setrlimit(RLIMIT_CORE, 0) failed");
        }
        if libc::mlockall(libc::MCL_CURRENT | libc::MCL_FUTURE) != 0 {
            eprintln!("vault-signer: WARN mlockall failed (needs CAP_IPC_LOCK / ulimit -l)");
        }
    }
}

/// Windows dev build: degrade gracefully. Individual DEK pages are
/// VirtualLock'd at decrypt time (see kms::LockedSecret); process-wide locking
/// is not available on Windows.
#[cfg(windows)]
fn harden_process() {
    eprintln!(
        "vault-signer: WARN windows dev build — per-key-page VirtualLock via kms::LockedSecret only"
    );
}

#[cfg(not(any(unix, windows)))]
fn harden_process() {}
