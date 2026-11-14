# syntax=docker/dockerfile:1
# vault-signer — multi-stage build (Rust stable, tonic mTLS gRPC).
# Build context MUST be the repository root: build.rs compiles
# ../../proto/signer/v1/signer.proto (protoc vendored via protoc-bin-vendored).
FROM rust:1-bookworm AS build
# crates.io mirror (USTC) — cargo env-based source replacement.
ENV CARGO_SOURCE_CRATES_IO_REPLACE_WITH=ustc \
    CARGO_SOURCE_USTC_REGISTRY="sparse+https://mirrors.ustc.edu.cn/crates.io-index/" \
    CARGO_NET_GIT_FETCH_WITH_CLI=true \
    CARGO_TERM_COLOR=never
WORKDIR /src
COPY proto/ proto/
COPY services/vault-signer/Cargo.toml services/vault-signer/Cargo.lock services/vault-signer/build.rs services/vault-signer/
COPY services/vault-signer/src services/vault-signer/src
WORKDIR /src/services/vault-signer
RUN cargo build --release --locked --bin vault-signer \
    && strip target/release/vault-signer

FROM debian:bookworm-slim
RUN useradd -r -u 10001 -m vaultsigner
WORKDIR /var/lib/vault-signer
COPY --from=build /src/services/vault-signer/target/release/vault-signer /usr/local/bin/vault-signer
ENV VAULT_SIGNER_CONFIG=/etc/vaultflow/signer-config.json
EXPOSE 50051
USER vaultsigner
ENTRYPOINT ["/usr/local/bin/vault-signer"]
