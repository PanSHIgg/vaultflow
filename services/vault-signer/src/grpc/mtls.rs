//! mTLS plumbing: PEM cert loading, a hot-reloadable `TlsAcceptor`, and the
//! TCP->TLS incoming stream fed to `serve_with_incoming`.
//!
//! Hot reload (90-day rolling rotation SOP): a background task watches the
//! mtime of (server cert, server key, client-CA) and swaps the whole
//! `rustls::ServerConfig` atomically — new connections get fresh credentials
//! while in-flight handshakes/sessions are unaffected. A failed rebuild keeps
//! the previous config and retries on the next tick (covers half-written
//! rotation artifacts).
//!
//! Client certs are MANDATORY (`WebPkiClientVerifier` with require-auth) —
//! both the gateway-core client and the admin-approver direct-connect path
//! must present leaves chaining to the offline root CA.

use std::fs;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, SystemTime};

use tokio::net::TcpListener;
use tokio_rustls::rustls::pki_types::{CertificateDer, PrivateKeyDer};
use tokio_rustls::rustls::server::WebPkiClientVerifier;
use tokio_rustls::rustls::{RootCertStore, ServerConfig};
use tokio_rustls::TlsAcceptor;
use tokio_stream::wrappers::TcpListenerStream;
use tokio_stream::{Stream, StreamExt};

/// The three PEM artifacts the server watches: its own cert+key and the CA
/// bundle used to verify client certs.
#[derive(Debug, Clone)]
pub struct CertPaths {
    pub server_cert: PathBuf,
    pub server_key: PathBuf,
    pub client_ca: PathBuf,
}

/// Install the `ring` CryptoProvider as process default — idempotent.
/// (We select ring to mirror tonic 0.12's tokio-rustls feature set, so the
/// dependency graph carries exactly one provider.)
pub fn install_crypto_provider() {
    let _ = tokio_rustls::rustls::crypto::ring::default_provider().install_default();
}

fn load_cert_chain(path: &PathBuf) -> anyhow::Result<Vec<CertificateDer<'static>>> {
    let mut pem: &[u8] = &fs::read(path)
        .map_err(|e| anyhow::anyhow!("read cert {}: {e}", path.display()))?;
    let certs: Vec<CertificateDer<'static>> =
        rustls_pemfile::certs(&mut pem).collect::<Result<_, _>>()?;
    if certs.is_empty() {
        anyhow::bail!("no PEM certificates in {}", path.display());
    }
    Ok(certs)
}

fn load_private_key(path: &PathBuf) -> anyhow::Result<PrivateKeyDer<'static>> {
    let mut pem: &[u8] = &fs::read(path)
        .map_err(|e| anyhow::anyhow!("read key {}: {e}", path.display()))?;
    rustls_pemfile::private_key(&mut pem)?
        .ok_or_else(|| anyhow::anyhow!("no PEM private key in {}", path.display()))
}

/// Build a `ServerConfig` enforcing client-certificate verification against
/// `client_ca`, plus the `h2` ALPN tag gRPC requires.
fn build_server_config(paths: &CertPaths) -> anyhow::Result<ServerConfig> {
    let certs = load_cert_chain(&paths.server_cert)?;
    let key = load_private_key(&paths.server_key)?;

    let mut roots = RootCertStore::empty();
    for ca in load_cert_chain(&paths.client_ca)? {
        roots.add(ca)?;
    }
    let verifier = WebPkiClientVerifier::builder(Arc::new(roots)).build()?;

    let mut cfg = ServerConfig::builder()
        .with_client_cert_verifier(verifier)
        .with_single_cert(certs, key)?;
    cfg.alpn_protocols = vec![b"h2".to_vec()];
    Ok(cfg)
}

/// Hot-reloadable TLS acceptor (see module docs).
pub struct ReloadingTls {
    paths: CertPaths,
    acceptor: RwLock<TlsAcceptor>,
    mtimes: Mutex<[Option<SystemTime>; 3]>,
}

impl ReloadingTls {
    /// Initial load — hard failure if certs/keys are invalid (boot-time
    /// misconfig must be loud, unlike rotation-time reload which degrades).
    pub fn load(paths: CertPaths) -> anyhow::Result<Arc<Self>> {
        install_crypto_provider();
        let acceptor = TlsAcceptor::from(Arc::new(build_server_config(&paths)?));
        let mtimes = Self::current_mtimes(&paths);
        Ok(Arc::new(Self {
            paths,
            acceptor: RwLock::new(acceptor),
            mtimes: Mutex::new(mtimes),
        }))
    }

    fn current_mtimes(paths: &CertPaths) -> [Option<SystemTime>; 3] {
        [&paths.server_cert, &paths.server_key, &paths.client_ca].map(|p| {
            fs::metadata(p).and_then(|m| m.modified()).ok()
        })
    }

    /// Current acceptor snapshot (cheap — Arc clone inside).
    pub fn acceptor(&self) -> TlsAcceptor {
        self.acceptor
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// Rebuild the acceptor iff any watched file changed. Returns Ok(true) on
    /// swap; Ok(false) when unchanged; Err keeps the previous config live.
    pub fn check_reload(&self) -> anyhow::Result<bool> {
        let new_mtimes = Self::current_mtimes(&self.paths);
        {
            let cur = self.mtimes.lock().unwrap_or_else(|e| e.into_inner());
            if *cur == new_mtimes {
                return Ok(false);
            }
        }
        let acceptor = TlsAcceptor::from(Arc::new(build_server_config(&self.paths)?));
        *self.acceptor.write().unwrap_or_else(|e| e.into_inner()) = acceptor;
        *self.mtimes.lock().unwrap_or_else(|e| e.into_inner()) = new_mtimes;
        Ok(true)
    }

    /// Spawn the mtime-watcher task (rotation SOP: leaf certs roll every 90d).
    pub fn spawn_reloader(self: &Arc<Self>, interval: Duration) -> tokio::task::JoinHandle<()> {
        let this = Arc::clone(self);
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(interval);
            tick.tick().await; // skip immediate
            loop {
                tick.tick().await;
                match this.check_reload() {
                    Ok(true) => tracing::info!("mTLS credentials hot-reloaded from disk"),
                    Ok(false) => {}
                    Err(e) => tracing::warn!("mTLS reload failed, keeping previous config: {e}"),
                }
            }
        })
    }
}

/// Stream of fully-negotiated `tokio_rustls::server::TlsStream`s for
/// `Server::serve_with_incoming`. Handshakes are accepted one at a time —
/// appropriate for a low-QPS vault; slowloris-style handshake stalls are
/// bounded by rustls + tokio timeouts at the TCP layer.
pub fn tls_incoming(
    listener: TcpListener,
    tls: Arc<ReloadingTls>,
) -> impl Stream<
    Item = Result<tokio_rustls::server::TlsStream<tokio::net::TcpStream>, std::io::Error>,
> {
    TcpListenerStream::new(listener).then(move |res| {
        let tls = Arc::clone(&tls);
        async move {
            let sock = res?;
            let _ = sock.set_nodelay(true);
            tls.acceptor().accept(sock).await
        }
    })
}
