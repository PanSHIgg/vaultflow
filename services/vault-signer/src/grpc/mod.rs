//! tonic-generated `signer.v1` bindings + the mTLS server assembly (W2-4).

/// Generated stubs from proto/signer/v1/signer.proto
/// (build.rs: server + client — the client side feeds our own mTLS
/// integration tests; gateway-core owns the production Go client).
#[allow(clippy::all)]
pub mod signer_v1 {
    tonic::include_proto!("signer.v1");
}

pub mod mtls;
pub mod server;
pub mod service;
