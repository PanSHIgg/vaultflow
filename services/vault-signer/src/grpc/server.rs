//! gRPC server assembly: tonic transport over the hot-reloadable mTLS
//! incoming stream.

use std::sync::Arc;

use tokio::net::TcpListener;

use super::mtls::{tls_incoming, ReloadingTls};
use super::service::VaultSignerSvc;
use super::signer_v1::vault_signer_service_server::VaultSignerServiceServer;

/// Serve `VaultSignerService` on `listener` until `shutdown` resolves.
/// The listener is bound by the caller so tests can grab the ephemeral port.
pub async fn serve<F>(
    listener: TcpListener,
    tls: Arc<ReloadingTls>,
    svc: VaultSignerSvc,
    shutdown: F,
) -> anyhow::Result<()>
where
    F: std::future::Future<Output = ()> + Send + 'static,
{
    let incoming = tls_incoming(listener, tls);
    tracing::info!("vault-signer gRPC serving (mTLS required)");
    tonic::transport::Server::builder()
        .add_service(VaultSignerServiceServer::new(svc))
        .serve_with_incoming_shutdown(incoming, shutdown)
        .await?;
    Ok(())
}
