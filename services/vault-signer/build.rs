//! Compiles proto/signer/v1/signer.proto — server stubs only (vault-signer is
//! the gRPC *server*; gateway-core owns the generated client side in Go).
//!
//! Paths are relative to this crate's manifest dir (services/vault-signer):
//!   proto file   -> ../../proto/signer/v1/signer.proto
//!   import root  -> ../../proto

fn main() -> Result<(), Box<dyn std::error::Error>> {
    // protoc is vendored — no system protoc required on dev machines.
    let protoc = protoc_bin_vendored::protoc_bin_path()?;
    std::env::set_var("PROTOC", protoc);

    #[allow(deprecated)] // compile() may be soft-deprecated in favor of compile_protos
    tonic_build::configure()
        .build_server(true)
        .build_client(false)
        .compile(&["../../proto/signer/v1/signer.proto"], &["../../proto"])?;

    println!("cargo:rerun-if-changed=../../proto/signer/v1/signer.proto");
    Ok(())
}
