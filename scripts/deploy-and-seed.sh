#!/usr/bin/env bash
# ============================================================================
# deploy-and-seed.sh — VaultFlow Wave-2 I1 一次性可重入部署+种子脚本.
#
# 执行环境: WSL ubuntu (forge/cast/anvil/python3/docker 均在其内).
# 顺序约定: 本脚本先行 (生成 certs/deploy 制品 + 部署合约 + 种子),
#           之后 `docker compose up --build -d` 一键拉起全栈.
#
# 幂等语义:
#   - certs:        gen-mtls-certs.sh 内部幂等 (存在即跳过);
#   - anvil 镜像:   deploy/Dockerfile.anvil + WSL foundry 二进制, 本地构建;
#   - 合约:         deploy/deployed.json + `cast code` 校验仍在链上 -> 复用;
#   - 迁移/种子:    schema_migrations 跟踪 + ON CONFLICT upsert;
#   - signer DB:    sqlite INSERT ... ON CONFLICT DO UPDATE (重跑刷新 envelope);
#   - 密钥材料:     deploy/ 已存在则复用 (KEK / admin Ed25519 / 商户 API key).
#
# 用法:
#   bash scripts/deploy-and-seed.sh            # 正常 (可重入)
#   bash scripts/deploy-and-seed.sh --fresh    # 重置 anvil 链态后重部署
# ============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DEPLOY_DIR="$ROOT/deploy"
SIGNER_DATA="$DEPLOY_DIR/signer-data"
DEPLOYED_JSON="$DEPLOY_DIR/deployed.json"
GATEWAY_ENV="$DEPLOY_DIR/gateway.env"
SIGNER_CFG="$DEPLOY_DIR/signer-config.json"
SEED_SQL="$DEPLOY_DIR/seed.sql"
ADMIN_KEYS="$DEPLOY_DIR/admin-ed25519.json"
SMOKE_WL="$DEPLOY_DIR/smoke-whitelist-request.json"
MERCHANT_KEYS="$DEPLOY_DIR/merchant-demo.json"

FRESH=0
[ "${1:-}" = "--fresh" ] && FRESH=1

# log 一律走 stderr — deploy_chain 的 stdout 只承载 JSON 回值.
log()  { printf '\033[1;34m[seed]\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 0. 前置检查
# ---------------------------------------------------------------------------
export PATH="$HOME/.foundry/bin:$HOME/.cargo/bin:$HOME/go/bin:$PATH"
need() { command -v "$1" >/dev/null 2>&1 || die "missing tool: $1"; }
need docker; need forge; need cast; need cargo; need python3; need openssl
docker compose version >/dev/null 2>&1 || die "docker compose plugin missing"
python3 -c 'import cryptography' 2>/dev/null \
    || die "python3 'cryptography' module required (pip install cryptography)"

# ---------------------------------------------------------------------------
# 1. 钱包分配 (anvil 确定性 mnemonic 账户; 地址由 cast 反推, 防手抄错)
#    chain A (31337): relayer=#1 payout=#3 cold=#2
#    chain B (31338): relayer=#4 payout=#5 cold=#6
#    funding/deployer: #0 (两链同)
# ---------------------------------------------------------------------------
DEPLOYER_PK=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80
FUNDING=0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266

A_RELAYER_PK=0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d
A_PAYOUT_PK=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6
B_RELAYER_PK=0x47e179ec197488593b187f80a00eb0da91f1b9d0b13f8733639f19c30a34926a
B_PAYOUT_PK=0x8b3a350cf5c34c9194ca85829a2df0ec3153be0318b5e2d3348e872092edffba

addr_of() { cast wallet address --private-key "$1"; }
A_RELAYER=$(addr_of "$A_RELAYER_PK" | tr 'A-F' 'a-f')
A_PAYOUT=$(addr_of  "$A_PAYOUT_PK"  | tr 'A-F' 'a-f')
B_RELAYER=$(addr_of "$B_RELAYER_PK" | tr 'A-F' 'a-f')
B_PAYOUT=$(addr_of  "$B_PAYOUT_PK"  | tr 'A-F' 'a-f')
A_COLD=0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc   # anvil #2
B_COLD=0x976ea74026e726554db657fa54763abd0c3a0aa9   # anvil #6
SMOKE_RECIPIENT=0xa0ee7a142d267c1f36714e4a8f75612f20a79720 # anvil #9 (客户提现地址)

RPC_A=http://127.0.0.1:8545
RPC_B=http://127.0.0.1:8546
RPC_A_INNER=http://anvil-a:8545   # compose 内网 DNS
RPC_B_INNER=http://anvil-b:8545

log "hot wallets: A.relayer=$A_RELAYER A.payout=$A_PAYOUT B.relayer=$B_RELAYER B.payout=$B_PAYOUT"

# ---------------------------------------------------------------------------
# 2. mTLS 证书 (幂等)
# ---------------------------------------------------------------------------
bash scripts/gen-mtls-certs.sh >/dev/null
log "mTLS certs ready under certs/"

# ---------------------------------------------------------------------------
# 3. anvil 构建上下文 (deploy/Dockerfile.anvil + WSL 拷贝的 anvil/cast)
#    ghcr.io/foundry-rs/foundry 本环境不可拉取 — 用本地 foundry 二进制自建镜像.
# ---------------------------------------------------------------------------
mkdir -p "$DEPLOY_DIR" "$SIGNER_DATA"
FOUNDRY_BIN="${FOUNDRY_BIN:-$HOME/.foundry/bin}"
[ -x "$FOUNDRY_BIN/anvil" ] || die "anvil not found under $FOUNDRY_BIN"
if [ ! -x "$DEPLOY_DIR/anvil" ] || [ ! -x "$DEPLOY_DIR/cast" ]; then
    cp "$FOUNDRY_BIN/anvil" "$FOUNDRY_BIN/cast" "$DEPLOY_DIR/"
    chmod +x "$DEPLOY_DIR/anvil" "$DEPLOY_DIR/cast"
    log "copied anvil/cast into deploy/ (image build context)"
fi
cat > "$DEPLOY_DIR/Dockerfile.anvil" <<'DOCKERFILE'
FROM debian:bookworm-slim
COPY anvil /usr/local/bin/anvil
COPY cast  /usr/local/bin/cast
ENTRYPOINT ["anvil"]
DOCKERFILE

# --- signer 预编译二进制 (buildkit 内 cargo registry 拉取在本环境不可靠 —
#     WSL cargo 走已配置镜像; 官方多阶段 services/vault-signer/Dockerfile 保留
#     为网络正常环境的 canonical 路径).
if [ ! -x "$DEPLOY_DIR/vault-signer" ] || [ "$FRESH" = "1" ]; then
    log "building vault-signer in WSL (cargo --release)"
    (cd "$ROOT/services/vault-signer" && cargo build --release --locked --bin vault-signer) \
        || die "WSL cargo build of vault-signer failed"
    cp "$ROOT/services/vault-signer/target/release/vault-signer" "$DEPLOY_DIR/vault-signer"
    chmod +x "$DEPLOY_DIR/vault-signer"
    log "copied vault-signer binary into deploy/"
fi
cat > "$DEPLOY_DIR/Dockerfile.signer" <<'DOCKERFILE'
# ubuntu:24.04 = glibc 2.39 — 与 WSL cargo 编译产物所需符号版本匹配.
FROM ubuntu:24.04
RUN useradd -r -u 10001 -m vaultsigner
WORKDIR /var/lib/vault-signer
COPY vault-signer /usr/local/bin/vault-signer
ENV VAULT_SIGNER_CONFIG=/etc/vaultflow/signer-config.json
EXPOSE 50051
USER vaultsigner
ENTRYPOINT ["/usr/local/bin/vault-signer"]
DOCKERFILE

# gateway.env 占位 — compose env_file(required:false) 兜底; 真实值后面覆写.
[ -f "$GATEWAY_ENV" ] || { echo '# placeholder — regenerated by deploy-and-seed.sh' > "$GATEWAY_ENV"; }

# ---------------------------------------------------------------------------
# 4. 起链 + PG (--fresh 先清链态)
# ---------------------------------------------------------------------------
if [ "$FRESH" = "1" ]; then
    log "--fresh: wiping anvil volumes"
    docker compose rm -sf anvil-a anvil-b >/dev/null 2>&1 || true
    docker volume rm -f vaultflow_anvil-a-data vaultflow_anvil-b-data >/dev/null 2>&1 || true
    rm -f "$DEPLOYED_JSON"
    # signer nonce 槽位绑定属旧链状态 — 链态整清后 nonce 从 0 起算, 旧绑定会
    # 让首笔出站被 SIGNER_REJECTED(same-nonce slot tampered) -> GAP_FILLER
    # 抢占 -> 业务意图 CANCELLED_BY_FILLER. 整库删除: DEKs 由第 9 步按同一
    # 私钥材料重供, signer 自身 schema 于启动迁移重建.
    rm -f "$SIGNER_DATA/signer.db"{,-wal,-shm}
    log "--fresh: wiped signer.db (nonce-slot bindings bound to wiped chain)"
fi

docker compose up -d postgres anvil-a anvil-b

wait_rpc() {
    local url=$1 name=$2 tries=60
    while [ $tries -gt 0 ]; do
        if cast block-number --rpc-url "$url" >/dev/null 2>&1; then return 0; fi
        sleep 2; tries=$((tries-1))
    done
    die "$name RPC not ready at $url"
}
wait_rpc "$RPC_A" "anvil-a(31337)"
wait_rpc "$RPC_B" "anvil-b(31338)"
log "anvil RPCs ready"

# ---------------------------------------------------------------------------
# 5. 合约部署 (forge script -> broadcast/run-latest.json 解析; 可复用跳过)
# ---------------------------------------------------------------------------
deploy_chain() {
    # $1=rpc $2=chainid $3=relayer $4=cold -> echoes JSON {usdt,usdc,factory,sweeper,router}
    local rpc=$1 cid=$2 relayer=$3 cold=$4
    log "deploying contracts on chain $cid via $rpc"
    # forge stdout 全部转 stderr — 本函数 stdout 仅允许末尾 python 打印 JSON.
    (cd "$ROOT/contracts" && \
        DEPLOYER_PK="$DEPLOYER_PK" RELAYER_HOT_WALLET="$relayer" COLD_WALLET="$cold" \
        forge script script/Deploy.s.sol:Deploy \
            --broadcast --rpc-url "$rpc" --silent >&2 || \
        DEPLOYER_PK="$DEPLOYER_PK" RELAYER_HOT_WALLET="$relayer" COLD_WALLET="$cold" \
        forge script script/Deploy.s.sol:Deploy \
            --broadcast --rpc-url "$rpc" >&2) \
        || die "forge deploy failed on chain $cid"
    python3 - "$ROOT/contracts/broadcast/Deploy.s.sol/$cid/run-latest.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    run = json.load(f)
out = {}
for tx in run.get("transactions", []):
    name = tx.get("contractName"); addr = tx.get("contractAddress")
    if not name or not addr:
        continue
    key = {"MockUSDT": "usdt", "MockUSDC": "usdc",
           "VaultFactory": "factory", "BatchSweeper": "sweeper",
           "EIP3009PayRouter": "router"}.get(name)
    if key:
        out[key] = addr.lower()
missing = {"usdt", "usdc", "factory", "sweeper", "router"} - out.keys()
if missing:
    sys.exit("broadcast run-latest.json missing: %s" % ",".join(sorted(missing)))
print(json.dumps(out))
PY
}

have_code() { # $1=rpc $2=addr -> 0 if non-empty code
    local code
    code=$(cast code --rpc-url "$1" "$2" 2>/dev/null || echo "0x")
    [ "$code" != "0x" ] && [ -n "$code" ]
}

reusable() {
    [ -f "$DEPLOYED_JSON" ] || return 1
    python3 - "$DEPLOYED_JSON" > /tmp/vf_deploy_addrs.$$ <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
c = d["chains"]
for cid in ("31337", "31338"):
    for k in ("usdt", "usdc", "factory", "sweeper", "router"):
        print(c[cid][k])
PY
    while read -r a; do :; done < /tmp/vf_deploy_addrs.$$
    local ok=0
    while IFS=' ' read -r cid addr; do
        rpc=$RPC_A; [ "$cid" = "31338" ] && rpc=$RPC_B
        have_code "$rpc" "$addr" || { rm -f /tmp/vf_deploy_addrs.$$; return 1; }
    done < <(python3 - "$DEPLOYED_JSON" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for cid, c in d["chains"].items():
    for k in ("usdt", "usdc", "factory", "sweeper", "router"):
        print(cid, c[k])
PY
)
    rm -f /tmp/vf_deploy_addrs.$$
    # 双链 factory 必须同址 (CHAIN_CONFIG_JSON 全局 factory 字段约束)
    python3 - "$DEPLOYED_JSON" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
f = [d["chains"]["31337"]["factory"], d["chains"]["31338"]["factory"]]
sys.exit(0 if f[0] == f[1] else 1)
PY
}

if reusable; then
    log "contracts already deployed & live — reusing $DEPLOYED_JSON"
else
    [ -f "$DEPLOYED_JSON" ] && warn "stale deployment detected — redeploying"
    A_JSON=$(deploy_chain "$RPC_A" 31337 "$A_RELAYER" "$A_COLD") || die "chain A deploy"
    B_JSON=$(deploy_chain "$RPC_B" 31338 "$B_RELAYER" "$B_COLD") || die "chain B deploy"
    python3 - "$DEPLOYED_JSON" "$A_JSON" "$B_JSON" <<'PY'
import json, sys
path, a, b = sys.argv[1], json.loads(sys.argv[2]), json.loads(sys.argv[3])
if a["factory"] != b["factory"]:
    sys.exit("FACTORY MISMATCH across chains (%s vs %s) — rerun with --fresh"
             % (a["factory"], b["factory"]))
json.dump({"chains": {"31337": a, "31338": b}}, open(path, "w"), indent=2)
PY
    log "contracts deployed -> $DEPLOYED_JSON"
fi

# ---------------------------------------------------------------------------
# 6. 链上派生字段 (implementation + initCodeHash — 权威来源取链上值)
# ---------------------------------------------------------------------------
FACTORY_A=$(python3 -c 'import json;print(json.load(open("'"$DEPLOYED_JSON"'"))["chains"]["31337"]["factory"])')
IMPL=$(cast call --rpc-url "$RPC_A" "$FACTORY_A" 'implementation()(address)' | tr 'A-F' 'a-f')
ICH_A=$(cast call --rpc-url "$RPC_A" "$FACTORY_A" 'initCodeHash()(bytes32)')
ICH_B=$(cast call --rpc-url "$RPC_B" "$(python3 -c 'import json;print(json.load(open("'"$DEPLOYED_JSON"'"))["chains"]["31338"]["factory"])')" 'initCodeHash()(bytes32)')
[ "$ICH_A" = "$ICH_B" ] || die "initCodeHash differs across chains ($ICH_A vs $ICH_B) — --fresh required"
log "factory=$FACTORY_A impl=$IMPL initCodeHash=$ICH_A"

# ---------------------------------------------------------------------------
# 7. 储备注资: 冷钱包 mint 1_000_000 USDT+USDC / 链 (mock unrestricted mint)
# ---------------------------------------------------------------------------
RESERVE=1000000000000  # 1e6 * 10^6 atomic
for cid in 31337 31338; do
    rpc=$RPC_A; cold=$A_COLD; [ "$cid" = "31338" ] && { rpc=$RPC_B; cold=$B_COLD; }
    for sym in usdt usdc; do
        tok=$(python3 -c 'import json;print(json.load(open("'"$DEPLOYED_JSON"'"))["chains"]["'"$cid"'"]["'"$sym"'"])')
        bal=$(cast call --rpc-url "$rpc" "$tok" 'balanceOf(address)(uint256)' "$cold" | awk '{print $1}')
        if [ "${bal:-0}" -lt "$RESERVE" ]; then
            cast send --rpc-url "$rpc" --private-key "$DEPLOYER_PK" \
                "$tok" 'mint(address,uint256)' "$cold" "$RESERVE" >/dev/null
            log "minted 1e6 $sym to cold $cold on chain $cid"
        fi
    done
done

# ---------------------------------------------------------------------------
# 8. 密钥材料: KEK (AES-256) + admin Ed25519 审批密钥 (幂等复用)
# ---------------------------------------------------------------------------
if [ ! -f "$SIGNER_DATA/kek.hex" ]; then
    openssl rand -hex 32 > "$SIGNER_DATA/kek.hex"
    log "generated KEK -> deploy/signer-data/kek.hex"
fi
if [ ! -f "$ADMIN_KEYS" ]; then
    python3 - "$ADMIN_KEYS" <<'PY'
import json, sys
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives import serialization
sk = Ed25519PrivateKey.generate()
priv = sk.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw,
                        serialization.NoEncryption())
pub = sk.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
json.dump({"private_key_hex": priv.hex(), "public_key_hex": pub.hex()}, open(sys.argv[1], "w"), indent=2)
PY
    log "generated admin Ed25519 approver keypair -> deploy/admin-ed25519.json"
fi
ADMIN_PUB=$(python3 -c 'import json;print(json.load(open("'"$ADMIN_KEYS"'"))["public_key_hex"])')

# ---------------------------------------------------------------------------
# 9. signer SQLite 供应: encrypted_keys AES-GCM envelope (nonce||ct||tag)
# ---------------------------------------------------------------------------
KEK_HEX=$(cat "$SIGNER_DATA/kek.hex")
python3 - "$SIGNER_DATA/signer.db" "$KEK_HEX" \
        "$A_RELAYER" "$A_RELAYER_PK" "$A_PAYOUT" "$A_PAYOUT_PK" \
        "$B_RELAYER" "$B_RELAYER_PK" "$B_PAYOUT" "$B_PAYOUT_PK" <<'PY'
import os, sqlite3, sys, time
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
db, kek = sys.argv[1], bytes.fromhex(sys.argv[2])
pairs = sys.argv[3:]
aes = AESGCM(kek)
conn = sqlite3.connect(db)
conn.execute("""CREATE TABLE IF NOT EXISTS encrypted_keys (
    wallet_address TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '',
    dek_envelope BLOB NOT NULL, created_at INTEGER NOT NULL)""")
now = int(time.time())
for i in range(0, len(pairs), 2):
    addr, pk = pairs[i].lower(), pairs[i+1]
    dek = bytes.fromhex(pk.removeprefix("0x"))
    nonce = os.urandom(12)
    env = nonce + aes.encrypt(nonce, dek, None)   # nonce(12)||ct||tag(16)
    label = "hot-wallet-%d" % (i // 2)
    conn.execute("""INSERT INTO encrypted_keys (wallet_address,label,dek_envelope,created_at)
                    VALUES (?,?,?,?)
                    ON CONFLICT(wallet_address) DO UPDATE
                      SET label=excluded.label, dek_envelope=excluded.dek_envelope""",
                 (addr, label, env, now))
conn.commit(); conn.close()
PY
chmod 644 "$SIGNER_DATA/signer.db" "$SIGNER_DATA/kek.hex"
chmod 755 "$SIGNER_DATA"
log "signer sqlite provisioned: 4 hot-wallet DEKs under deploy/signer-data/signer.db"
# --fresh: 运行中的 signer 仍持有已删除库的旧 inode — 重启以读入新库
# (sqlite 按 inode 持有, 目录 bind-mount 下新建文件对已开句柄不可见).
if [ "$FRESH" = "1" ] && docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "vaultflow-signer"; then
    docker compose restart vault-signer >/dev/null
    log "--fresh: restarted vault-signer (dropped stale nonce-slot bindings)"
fi

# ---------------------------------------------------------------------------
# 10. signer-config.json (容器内路径; 策略地址 = 已部署合约 + 热钱包)
# ---------------------------------------------------------------------------
python3 - "$SIGNER_CFG" "$DEPLOYED_JSON" "$ADMIN_PUB" \
        "$A_RELAYER" "$A_PAYOUT" "$A_COLD" "$B_RELAYER" "$B_PAYOUT" "$B_COLD" <<'PY'
import json, sys
out, dep, pub = sys.argv[1], json.load(open(sys.argv[2])), sys.argv[3]
ra, pa, ca, rb, pb, cb = [a.lower() for a in sys.argv[4:10]]
C = dep["chains"]
def chain(cid, relayer, payout, cold):
    c = C[cid]
    return {
        "chain_id": int(cid),
        "relayer_hot_wallet": relayer,
        "payout_hot_wallet": payout,
        "batch_sweeper": c["sweeper"],
        "eip3009_router": c["router"],
        "custody_tokens": [c["usdt"], c["usdc"]],
        "allowed_cold_wallets": [cold],
        "cooldown_secs": 60,
        "daily_payout_limit_atomic": "100000000000000"  # 100M USDC breaker
    }
cfg = {
    "listen_addr": "0.0.0.0:50051",
    "db_path": "/var/lib/vault-signer/signer.db",
    "kek_file": "/var/lib/vault-signer/kek.hex",
    "admin_approver_pubkey_hex": pub,
    "certs": {
        "server_cert": "/etc/vaultflow/certs/server/vault-signer.crt",
        "server_key":  "/etc/vaultflow/certs/server/vault-signer.key",
        "client_ca":   "/etc/vaultflow/certs/ca/ca.crt"
    },
    "chains": [chain("31337", ra, pa, ca), chain("31338", rb, pb, cb)]
}
json.dump(cfg, open(out, "w"), indent=2)
PY
log "wrote $SIGNER_CFG"

# ---------------------------------------------------------------------------
# 11. gateway.env (CHAIN_CONFIG_JSON + 装配 env)
# ---------------------------------------------------------------------------
python3 - "$GATEWAY_ENV" "$DEPLOYED_JSON" "$IMPL" "$ICH_A" \
        "$RPC_A_INNER" "$RPC_B_INNER" \
        "$A_RELAYER" "$A_PAYOUT" "$A_COLD" "$B_RELAYER" "$B_PAYOUT" "$B_COLD" "$FUNDING" <<'PY'
import json, sys
out, dep, impl, ich = sys.argv[1], json.load(open(sys.argv[2])), sys.argv[3], sys.argv[4]
ra_inner, rb_inner = sys.argv[5], sys.argv[6]
ra, pa, ca, rb, pb, cb, funding = [a.lower() for a in sys.argv[7:14]]
C = dep["chains"]
def c(cid, rpc, relayer, payout, cold):
    x = C[cid]
    return {
        "chain_id": int(cid), "rpc_urls": [rpc],
        "required_confirmations": 1, "devnet_tags": True,
        "eip3009_router": x["router"], "batch_sweeper": x["sweeper"],
        "relayer_hot_wallet": relayer, "payout_hot_wallet": payout,
        "cold_wallets": [cold],
        "relay_gas_limit": 500000, "payout_gas_limit": 150000,
        "max_fee_per_gas": "2000000000", "max_priority_fee_per_gas": "1000000000",
        "start_height": None, "funding_account": funding,
    }
ccj = {
    "factory": C["31337"]["factory"],
    "implementation": impl,
    "init_code_hash": ich,
    "anchor_chain_id": 31337,
    "chains": {
        "31337": c("31337", ra_inner, ra, pa, ca),
        "31338": c("31338", rb_inner, rb, pb, cb),
    },
}
env = {
    "DATABASE_URL": "postgres://vaultflow:vaultflow@postgres:5432/vaultflow?sslmode=disable",
    "REDIS_URL": "redis://redis:6379",
    "SIGNER_ADDR": "vault-signer:50051",
    "SIGNER_ID": "vault-signer",
    "ANCHOR_CHAIN_ID": "31337",
    "MTLS_CLIENT_CERT": "/etc/vaultflow/certs/clients/gateway-core.crt",
    "MTLS_CLIENT_KEY": "/etc/vaultflow/certs/clients/gateway-core.key",
    "MTLS_CA_CERT": "/etc/vaultflow/certs/ca/ca.crt",
    "MTLS_SERVER_NAME": "vault-signer",
    "ADMIN_JWT_SECRET": "vaultflow-dev-admin-secret",
    "ENABLE_DEV_SIMULATOR": "true",
    "WEBHOOK_ALLOW_INSECURE_HTTP": "true",
    "MIGRATIONS_DIR": "/app/migrations",
    "PORT": "8080",
    "CHAIN_CONFIG_JSON": json.dumps(ccj, separators=(",", ":")),
}
with open(out, "w") as f:
    f.write("# generated by deploy-and-seed.sh — secrets are dev-only\n")
    for k, v in env.items():
        f.write(f"{k}={v}\n")
PY
log "wrote $GATEWAY_ENV"

# ---------------------------------------------------------------------------
# 12. 迁移 (psql 应用 + schema_migrations 跟踪 — 与 gateway 启动路径同序)
# ---------------------------------------------------------------------------
psql() { docker compose exec -T postgres psql -U vaultflow -d vaultflow -v ON_ERROR_STOP=1 "$@"; }
until psql -c 'SELECT 1' >/dev/null 2>&1; do sleep 2; done
psql -c 'CREATE TABLE IF NOT EXISTS schema_migrations (version INT PRIMARY KEY)' >/dev/null
for f in services/gateway-core/migrations/*.sql; do
    v=$(basename "$f" | cut -d_ -f1); v=$((10#$v))
    applied=$(psql -tAc "SELECT 1 FROM schema_migrations WHERE version=$v")
    if [ -z "$applied" ]; then
        docker compose exec -T postgres psql -U vaultflow -d vaultflow -v ON_ERROR_STOP=1 -f - < "$f" >/dev/null
        psql -c "INSERT INTO schema_migrations (version) VALUES ($v)" >/dev/null
        log "migration $v applied ($(basename "$f"))"
    fi
done

# ---------------------------------------------------------------------------
# 13. 种子: 商户 + API key + supported_assets + 金库储备账本行
# ---------------------------------------------------------------------------
MERCHANT_ID=11111111-2222-3333-4444-555555555555
if [ ! -f "$MERCHANT_KEYS" ]; then
    python3 - "$MERCHANT_KEYS" <<'PY'
import hashlib, json, secrets, sys
key = "vf_demo_" + secrets.token_hex(24)
json.dump({"api_key": key, "api_key_hash": hashlib.sha256(key.encode()).hexdigest()},
          open(sys.argv[1], "w"), indent=2)
PY
fi
API_KEY=$(python3 -c 'import json;print(json.load(open("'"$MERCHANT_KEYS"'"))["api_key"])')
API_KEY_HASH=$(python3 -c 'import json;print(json.load(open("'"$MERCHANT_KEYS"'"))["api_key_hash"])')

# 游标种子: 用真实 genesis hash 锚定 — 防止 indexer 首启在 anvil 状态未恢复
# 窗口把游标锚成 (0, 0x00..0) 后首块父哈希校验误判 FORK_DEPTH_EXCEEDED.
GENESIS_A=$(cast rpc eth_getBlockByNumber 0x0 false --rpc-url "$RPC_A" | python3 -c 'import json,sys;print(json.load(sys.stdin)["hash"])')
GENESIS_B=$(cast rpc eth_getBlockByNumber 0x0 false --rpc-url "$RPC_B" | python3 -c 'import json,sys;print(json.load(sys.stdin)["hash"])')

# FIX-G BUG-3: 原生 gas 注资账 — 逐热钱包查询真实链上 ETH 余额 (wei 十进制,
# 超 int64 故在 python 端合计). GAS_SETTLE 每笔出站结算 CR HOT_WALLET_NATIVE;
# 种子漏记本科目期初注资会让首笔 GAS_SETTLE 因余额不足回滚整个业务结算.
NAT_A_RELAYER=$(cast balance "$A_RELAYER" --rpc-url "$RPC_A")
NAT_A_PAYOUT=$(cast balance "$A_PAYOUT" --rpc-url "$RPC_A")
NAT_B_RELAYER=$(cast balance "$B_RELAYER" --rpc-url "$RPC_B")
NAT_B_PAYOUT=$(cast balance "$B_PAYOUT" --rpc-url "$RPC_B")

python3 - "$SEED_SQL" "$DEPLOYED_JSON" "$MERCHANT_ID" "$API_KEY_HASH" \
        "$A_COLD" "$B_COLD" "$RESERVE" "$GENESIS_A" "$GENESIS_B" \
        "$NAT_A_RELAYER" "$NAT_A_PAYOUT" "$NAT_B_RELAYER" "$NAT_B_PAYOUT" <<'PY'
import json, sys
out, dep, mid, khash = sys.argv[1], json.load(open(sys.argv[2])), sys.argv[3], sys.argv[4]
ca, cb, reserve = sys.argv[5], sys.argv[6], int(sys.argv[7])
gen_a, gen_b = sys.argv[8], sys.argv[9]
# FIX-G BUG-3: 真实链上热钱包 ETH 合计 (wei) — 注资账本金额权威来源.
nat = {"31337": int(sys.argv[10]) + int(sys.argv[11]),
       "31338": int(sys.argv[12]) + int(sys.argv[13])}
C = dep["chains"]
sa = []
for cid in ("31337", "31338"):
    sa.append("(%s,'USDT','%s',6,false)" % (cid, C[cid]["usdt"]))
    sa.append("(%s,'USDC','%s',6,true)"  % (cid, C[cid]["usdc"]))
acct = []
for cid in ("31337", "31338"):
    for sym in ("USDT", "USDC"):
        acct.append("('00000000-0000-0000-0000-000000000000',%s,'%s','CUSTODY_COLD','DEBIT',%d)" % (cid, sym, reserve))
        acct.append("('00000000-0000-0000-0000-000000000000',%s,'%s','TREASURY_EQUITY_CAPITAL','CREDIT',%d)" % (cid, sym, reserve))
sql = f"""-- deploy-and-seed.sh generated — idempotent dev seed
INSERT INTO merchants (id, name, api_key_hash, webhook_url, webhook_secret_enc, fee_rate_bps)
VALUES ('{mid}', 'Demo Merchant', '{khash}', NULL, NULL, 25)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, api_key_hash = EXCLUDED.api_key_hash;

INSERT INTO supported_assets (chain_id, asset_symbol, token_contract, decimals, supports_eip3009)
VALUES {",".join(sa)}
ON CONFLICT (chain_id, asset_symbol) DO UPDATE
    SET token_contract = EXCLUDED.token_contract,
        decimals = EXCLUDED.decimals,
        supports_eip3009 = EXCLUDED.supports_eip3009;

-- 平台金库储备 (系统哨兵商户) — 与链上 cold wallet mint 额对齐
INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
VALUES {",".join(acct)}
ON CONFLICT (owner_merchant_id, chain_id, asset_symbol, account_type)
    DO UPDATE SET balance = EXCLUDED.balance;

-- 游标锚定: 仅在“首启误锚 (height=0, hash=0x0)”或空表时修正为真实 genesis hash;
-- 已推进的游标不动 (幂等重跑不打断同步进度).
INSERT INTO chain_sync_cursors (chain_id, latest_synced_height, latest_synced_hash, safe_head_height)
VALUES (31337, 0, '{gen_a}', 0), (31338, 0, '{gen_b}', 0)
ON CONFLICT (chain_id) DO UPDATE
    SET latest_synced_hash = EXCLUDED.latest_synced_hash
    WHERE chain_sync_cursors.latest_synced_height = 0
      AND chain_sync_cursors.latest_synced_hash =
          '0x0000000000000000000000000000000000000000000000000000000000000000';
"""

def topup_block(cid, target):
    # FIX-G BUG-3: TREASURY_TOPUP 模板合法腿
    # (DR HOT_WALLET_NATIVE / CR TREASURY_EQUITY_CAPITAL), 金额 = 链上真实
    # 热钱包 ETH 合计 (wei). 重跑按差额补足 (幂等 + 兼容 --fresh 后旧账漂移);
    # ledger 延迟约束在语句提交时校验 balance_after 链 — 单 DO 块自洽.
    return """-- FIX-G BUG-3: hot-wallet native ETH gas funding for chain {cid}
-- (real on-chain aggregate = {target} wei at seed time).
DO $$
DECLARE
    v_txn  uuid;
    v_bal  numeric(78,0);
    v_acct uuid;
    v_aft  numeric(78,0);
BEGIN
    SELECT balance INTO v_bal FROM ledger_accounts
     WHERE owner_merchant_id = '00000000-0000-0000-0000-000000000000'
       AND chain_id = {cid} AND asset_symbol = 'ETH'
       AND account_type = 'HOT_WALLET_NATIVE'
     FOR UPDATE;
    v_bal := COALESCE(v_bal, 0);
    IF v_bal >= {target} THEN
        RETURN;  -- 已注资到位 (幂等重跑)
    END IF;
    INSERT INTO ledger_transactions (reference_type, reference_id, description)
    VALUES ('TREASURY_TOPUP', 'seed:hot-native:{cid}:' || ({target} - v_bal)::text,
            'deploy seed: initial hot-wallet native gas funding (delta to on-chain total)')
    RETURNING id INTO v_txn;
    INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
    VALUES ('00000000-0000-0000-0000-000000000000', {cid}, 'ETH', 'HOT_WALLET_NATIVE', 'DEBIT', {target} - v_bal)
    ON CONFLICT (owner_merchant_id, chain_id, asset_symbol, account_type)
      DO UPDATE SET balance = ledger_accounts.balance + EXCLUDED.balance
    RETURNING id, balance INTO v_acct, v_aft;
    INSERT INTO ledger_entries (transaction_id, account_id, direction, amount, balance_after)
    VALUES (v_txn, v_acct, 'DEBIT', {target} - v_bal, v_aft);
    INSERT INTO ledger_accounts (owner_merchant_id, chain_id, asset_symbol, account_type, normal_side, balance)
    VALUES ('00000000-0000-0000-0000-000000000000', {cid}, 'ETH', 'TREASURY_EQUITY_CAPITAL', 'CREDIT', {target} - v_bal)
    ON CONFLICT (owner_merchant_id, chain_id, asset_symbol, account_type)
      DO UPDATE SET balance = ledger_accounts.balance + EXCLUDED.balance
    RETURNING id, balance INTO v_acct, v_aft;
    INSERT INTO ledger_entries (transaction_id, account_id, direction, amount, balance_after)
    VALUES (v_txn, v_acct, 'CREDIT', {target} - v_bal, v_aft);
END $$;
""".format(cid=cid, target=target)

sql += topup_block("31337", nat["31337"])
sql += topup_block("31338", nat["31338"])
open(out, "w").write(sql)
PY
# --fresh: anvil 已整链清空 — DB 中一切链派生与每轮业务状态必须同步归零,
# 否则旧 chain_sync_cursors.latest_synced_height > 新链 latest 使
# Pool.Heights 的滞后节点剔除把全部节点当轮踢空 -> ErrNoQuorum 永久死锁
# (旧 genesis hash 亦会让分叉扫描找不到公共祖先). merchants/supported_assets
# 等配置类行由下方 seed 幂等重建, 不在重置集内.
if [ "$FRESH" = "1" ]; then
    docker compose exec -T postgres psql -U vaultflow -d vaultflow -v ON_ERROR_STOP=1 -c \
        "TRUNCATE chain_sync_cursors, chain_blocks, onchain_deposits,
                  deposit_addresses, payment_orders, merchant_payouts,
                  outbound_tx_attempts, outbound_tx_intents, wallet_nonce_cursors,
                  sweep_batch_items, sweep_batches, webhook_outbox,
                  signer_audit_anchor, merchant_withdrawal_addresses,
                  ledger_entries, ledger_transactions, ledger_accounts
         RESTART IDENTITY CASCADE" >/dev/null
    log "--fresh: reset chain-derived + business domain tables"
fi

docker compose exec -T postgres psql -U vaultflow -d vaultflow -v ON_ERROR_STOP=1 -f - < "$SEED_SQL" >/dev/null
log "seeded merchant $MERCHANT_ID + supported_assets(4) + ledger reserve accounts(8)" \
    "+ HOT_WALLET_NATIVE gas funding (A=${NAT_A_RELAYER}+${NAT_A_PAYOUT} B=${NAT_B_RELAYER}+${NAT_B_PAYOUT} wei)"

# ---------------------------------------------------------------------------
# 14. 冒烟白名单请求 (admin Ed25519 签好的 69B ADDR_V1 载荷演示)
# ---------------------------------------------------------------------------
python3 - "$ADMIN_KEYS" "$SMOKE_WL" "$MERCHANT_ID" "$SMOKE_RECIPIENT" <<'PY'
import json, sys, time, uuid
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives import serialization
keys = json.load(open(sys.argv[1]))
mid, recipient = sys.argv[3], sys.argv[4]
sk = Ed25519PrivateKey.from_private_bytes(bytes.fromhex(keys["private_key_hex"]))
valid_until = int(time.time()) + 30 * 86400
payload = (b"VAULTFLOW_ADDR_V1" + (31337).to_bytes(8, "big")
           + uuid.UUID(mid).bytes + bytes.fromhex(recipient.removeprefix("0x"))
           + valid_until.to_bytes(8, "big"))
assert len(payload) == 69
sig = sk.sign(payload).hex()
json.dump({"chain_id": 31337, "address": recipient,
           "valid_until_unix": valid_until, "admin_signature": sig},
          open(sys.argv[2], "w"), indent=2)
PY
log "wrote smoke whitelist request -> $SMOKE_WL"

# ---------------------------------------------------------------------------
# 15. 汇总
# ---------------------------------------------------------------------------
python3 - "$DEPLOYED_JSON" "$IMPL" "$ICH_A" "$MERCHANT_ID" "$API_KEY" \
        "$A_RELAYER" "$A_PAYOUT" "$B_RELAYER" "$B_PAYOUT" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print("""
=================  VaultFlow dev seed summary  =================
chains        : 31337 (anvil-a http://127.0.0.1:8545) | 31338 (anvil-b http://127.0.0.1:8546)
factory       : %s    impl: %s
initCodeHash  : %s
chain 31337   : usdt=%s usdc=%s sweeper=%s router=%s
chain 31338   : usdt=%s usdc=%s sweeper=%s router=%s
hot wallets   : A.relayer=%s A.payout=%s
                B.relayer=%s B.payout=%s
merchant      : %s
api key       : %s   (X-Api-Key header; deploy/merchant-demo.json)
artifacts     : deploy/{gateway.env,signer-config.json,signer-data/,deployed.json,
                admin-ed25519.json,seed.sql,smoke-whitelist-request.json}
next          : docker compose up --build -d
smoke         : curl -sf localhost:8080/healthz
                curl -X POST localhost:8080/v1/orders -H 'X-Api-Key: <api key>'
                     -H 'Idempotency-Key: <uuid>' -d '{"chain_id":31337,"asset_symbol":"USDC","amount":"1000000"}'
===============================================================""" % (
    d["chains"]["31337"]["factory"], sys.argv[2], sys.argv[3],
    d["chains"]["31337"]["usdt"], d["chains"]["31337"]["usdc"],
    d["chains"]["31337"]["sweeper"], d["chains"]["31337"]["router"],
    d["chains"]["31338"]["usdt"], d["chains"]["31338"]["usdc"],
    d["chains"]["31338"]["sweeper"], d["chains"]["31338"]["router"],
    sys.argv[6], sys.argv[7], sys.argv[8], sys.argv[9],
    sys.argv[4], sys.argv[5]))
PY
