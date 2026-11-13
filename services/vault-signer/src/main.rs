//! vault-signer entry point — process hardening, then (W2-4) the mTLS gRPC server.

fn main() -> anyhow::Result<()> {
    harden_process();
    tracing_subscriber::fmt::init();

    tracing::info!("vault-signer skeleton booted — gRPC server lands in W2-4");
    // TODO(W2-4): open db::open(), build kms::KmsProvider, assemble tonic
    //             rustls mTLS server (client cert required, 90d rotation
    //             hot-reload) serving grpc::signer_v1::VaultSignerService.
    let _ = &vault_signer::grpc::signer_v1::SignTransactionRequest::default; // keep lib linked
    Ok(())
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

/// Windows dev build: degrade gracefully. VirtualLock is applied per-DEK-page
/// after KMS decryption in W2-1 (process-wide locking is not available).
#[cfg(windows)]
fn harden_process() {
    // TODO(W2-1): windows_sys::Win32::System::Memory::VirtualLock each decrypted
    //             key page; release with VirtualUnlock before zeroize-on-drop.
    eprintln!("vault-signer: WARN windows build — VirtualLock per-key-page pending (W2-1)");
}
