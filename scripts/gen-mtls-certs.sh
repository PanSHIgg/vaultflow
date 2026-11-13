#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# gen-mtls-certs.sh — offline dev PKI for vault-signer mTLS (ARCHITECTURE §1.1).
#
# Generates under certs/ (repo root — gitignored, never commit keys):
#   certs/ca/ca.crt                      RSA-4096 root CA, "VaultFlow Dev Root CA", 10y
#   certs/server/vault-signer.{key,crt}  server cert, CN=vault-signer, 90d,
#                                      SAN DNS:localhost,DNS:vault-signer,IP:127.0.0.1
#   certs/clients/gateway-core.*         client cert, CN=gateway-core, 90d (clientAuth)
#   certs/clients/admin-approver.*       client cert, CN=admin-approver, 90d (clientAuth)
#
# Idempotent: existing files are skipped on re-run.
# --force wipes the WHOLE chain and regenerates (old leaves become untrusted —
# rotation SOP: roll leaves first, CA last).
# Usage: scripts/gen-mtls-certs.sh [--force]
# ---------------------------------------------------------------------------
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CERTS="$ROOT/certs"
CA_DIR="$CERTS/ca"
SRV_DIR="$CERTS/server"
CLI_DIR="$CERTS/clients"
DAYS_CA=3650   # 10y root
DAYS_LEAF=90   # 90-day rolling leaf validity

case "${1:-}" in
    --force)
        echo "[force] wiping $CERTS"
        rm -rf "$CERTS"
        ;;
    -h|--help)
        sed -n '2,20p' "$0"; exit 0
        ;;
    "") ;;
    *) echo "unknown flag: $1 (only --force supported)" >&2; exit 2 ;;
esac

mkdir -p "$CA_DIR" "$SRV_DIR" "$CLI_DIR"

# --- Root CA ---------------------------------------------------------------
if [[ -f "$CA_DIR/ca.key" && -f "$CA_DIR/ca.crt" ]]; then
    echo "[skip] root CA already present: $CA_DIR/ca.crt"
else
    openssl genrsa -out "$CA_DIR/ca.key" 4096
    openssl req -x509 -new -nodes -key "$CA_DIR/ca.key" -sha256 -days "$DAYS_CA" \
        -subj "/CN=VaultFlow Dev Root CA/O=VaultFlow Dev/C=CN" \
        -out "$CA_DIR/ca.crt"
    chmod 600 "$CA_DIR/ca.key"
    echo "[ok] root CA -> $CA_DIR/ca.crt"
fi

# --- Extension profiles ------------------------------------------------------
EXT_DIR="$(mktemp -d)"
trap 'rm -rf "$EXT_DIR"' EXIT

cat >"$EXT_DIR/server.ext" <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:localhost,DNS:vault-signer,IP:127.0.0.1
EOF

cat >"$EXT_DIR/client.ext" <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=clientAuth
EOF

# --- Leaf issuer -------------------------------------------------------------
# $1=file-name $2=out-dir $3=CN $4=ext-profile
issue() {
    local name="$1" dir="$2" cn="$3" ext="$4"
    if [[ -f "$dir/$name.key" && -f "$dir/$name.crt" ]]; then
        echo "[skip] $name already present"
        return 0
    fi
    openssl genrsa -out "$dir/$name.key" 2048
    openssl req -new -key "$dir/$name.key" \
        -subj "/CN=$cn/O=VaultFlow Dev/C=CN" -out "$dir/$name.csr"
    openssl x509 -req -in "$dir/$name.csr" \
        -CA "$CA_DIR/ca.crt" -CAkey "$CA_DIR/ca.key" -CAcreateserial \
        -days "$DAYS_LEAF" -sha256 -extfile "$EXT_DIR/$ext" \
        -out "$dir/$name.crt"
    rm -f "$dir/$name.csr"
    chmod 600 "$dir/$name.key"
    echo "[ok] $name -> $dir/"
}

issue "vault-signer"   "$SRV_DIR" "vault-signer"   "server.ext"
issue "gateway-core"   "$CLI_DIR" "gateway-core"   "client.ext"
issue "admin-approver" "$CLI_DIR" "admin-approver" "client.ext"

cat <<NOTES

Done. Dev mTLS bundle layout:
  certs/ca/ca.crt                      -> shared trust root (BOTH sides)
  certs/server/vault-signer.{key,crt}  -> tonic server creds (CN=vault-signer)
  certs/clients/gateway-core.*         -> gateway-core client creds
  certs/clients/admin-approver.*       -> offline admin direct-connect creds

Leaf validity: ${DAYS_LEAF} days (90-day rotation SOP — re-roll via --force or
per-leaf re-issue before expiry). Verify any leaf with:
  openssl verify -CAfile certs/ca/ca.crt certs/server/vault-signer.crt
  openssl verify -CAfile certs/ca/ca.crt certs/clients/gateway-core.crt
NOTES
