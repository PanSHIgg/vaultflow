//! tonic-generated `signer.v1` bindings + (W2-4) mTLS server assembly.

/// Generated server stubs from proto/signer/v1/signer.proto
/// (build.rs: build_server=true, build_client=false).
pub mod signer_v1 {
    tonic::include_proto!("signer.v1");
}

// TODO(W2-4): implement `signer_v1::vault_signer_service_server::VaultSignerService`
//             over policy/kms/db/audit; serve via tonic transport with rustls
//             mTLS — client cert REQUIRED, certs hot-reloaded for the 90d
//             rotation SOP; admin direct-connect allowed for
//             RevokeWithdrawalAddress emergencies.
