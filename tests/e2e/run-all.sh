#!/usr/bin/env bash
# VaultFlow Wave-3 — adversarial E2E one-command entry. MUST run inside WSL.
#
#   tests/e2e/run-all.sh                 # deploy(reuse artifacts) + compose + all 4 scenarios
#   tests/e2e/run-all.sh --fresh         # wipe anvil/signer state, redeploy, run all 4
#   tests/e2e/run-all.sh --no-deploy     # skip deploy+compose, just run scenarios
#   tests/e2e/run-all.sh s1 s4           # run a subset (order preserved: recon last)
#
# Scenarios: s1 reorg LIFO · s2 quarantine+revert-gas · s4 signer penetration ·
#            s3 two-layer reconciliation (always scheduled last by default).
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH="$HOME/.foundry/bin:$HOME/.local/node/bin:$HOME/go/bin:$HOME/.cargo/bin:$PATH"

DEPLOY=1; FRESH=""; SCEN=()
for a in "$@"; do
  case "$a" in
    --fresh)     FRESH="--fresh";;
    --no-deploy) DEPLOY=0;;
    s1|s2|s3|s4) SCEN+=("$a");;
    *) echo "unknown arg: $a" >&2; exit 2;;
  esac
done
[ ${#SCEN[@]} -eq 0 ] && SCEN=(s1 s2 s4 s3)

if [ "$DEPLOY" = "1" ]; then
  echo "==> scripts/deploy-and-seed.sh $FRESH"
  ./scripts/deploy-and-seed.sh $FRESH
  echo "==> docker compose up --build -d"
  docker compose up --build -d
fi

echo "==> waiting for gateway http://localhost:8080/healthz"
ok=0
for _ in $(seq 1 90); do
  if curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
[ "$ok" = "1" ] || { echo "gateway unhealthy" >&2; exit 1; }
echo "    gateway healthy; signer check:"
# Admin JWT minted with the per-deploy secret from deploy/gateway.env
# (never the public dev literal); iss/aud mirror gateway adminAuth contract.
ADMIN_JWT_SECRET="$(sed -n 's/^ADMIN_JWT_SECRET=//p' deploy/gateway.env 2>/dev/null | tail -1 || true)"
if [ -z "$ADMIN_JWT_SECRET" ]; then
  echo "    (skipped: deploy/gateway.env missing ADMIN_JWT_SECRET)"
else
curl -fsS -H "Authorization: Bearer $(ADMIN_JWT_SECRET="$ADMIN_JWT_SECRET" node -e '
  const c=require("crypto");
  const h=(s)=>c.createHmac("sha256",process.env.ADMIN_JWT_SECRET).update(s).digest("base64url");
  const p=(o)=>Buffer.from(JSON.stringify(o)).toString("base64url");
  const hdr=p({alg:"HS256",typ:"JWT"});
  const pl=p({iss:"vaultflow-gateway",aud:"vaultflow-admin",sub:"e2e-admin",role:"admin",exp:Math.floor(Date.now()/1000)+600});
  process.stdout.write(`${hdr}.${pl}.`+h(`${hdr}.${pl}`));
')" http://localhost:8080/v1/admin/signer/health || true
fi
echo

for s in "${SCEN[@]}"; do
  echo
  echo "############ scenario $s ############"
  case "$s" in
    s1) node tests/e2e/s1-reorg.mjs;;
    s2) node tests/e2e/s2-quarantine-gas.mjs;;
    s3) node tests/e2e/s3-invariant.mjs;;
    s4) node tests/e2e/s4-signer.mjs;;
  esac
done
echo
echo "==> ALL SELECTED SCENARIOS PASSED (${SCEN[*]})"
