// W6-3 — browser WebCrypto offline Ed25519 approval panel.
// Generates/imports the admin approver key entirely in memory (private key
// never leaves the browser), assembles the frozen 69B ADDR_V1 lease payload
// and the 127B REFUND_V1 RefundTicket payload byte-for-byte, signs, and
// one-click fills the corresponding API approval forms.

"use client";

import { useEffect, useMemo, useState } from "react";
import {
  AddressText,
  Badge,
  btnCls,
  btnDangerCls,
  btnGhostCls,
  Card,
  CopyButton,
  ErrorBox,
  Field,
  inputCls,
  Spinner,
} from "@/components/ui";
import { useAdminKey } from "@/lib/admin-key";
import { bytesToHex, bytesToHex0x } from "@/lib/bytes";
import {
  ADDR_V1_PAYLOAD_BYTES,
  buildAddrV1Payload,
  buildRefundV1Payload,
  REFUND_V1_PAYLOAD_BYTES,
} from "@/lib/ed25519";
import { assembleRefundTicketFields } from "@/lib/refund-ticket";
import { ANVIL } from "@/lib/mock-data";
import { assetInfo } from "@/lib/chains";
import { demoMerchantId } from "@/lib/env";
import { fmtDateTime, shortAddr, WHITELIST_STATUS_LABEL } from "@/lib/format";
import {
  useCreateRefund,
  useRegisterAddress,
  useRevokeAddress,
  useWhitelist,
} from "@/lib/hooks";

const TONE: Record<string, "ok" | "warn" | "bad" | "muted"> = {
  ACTIVE: "ok",
  PENDING_COOLDOWN: "warn",
  EXPIRED: "muted",
  REVOKED: "bad",
};

export default function ApprovalsPage() {
  const key = useAdminKey();
  const whitelist = useWhitelist();
  const register = useRegisterAddress();
  const revoke = useRevokeAddress();
  const createRefund = useCreateRefund();

  const [seed, setSeed] = useState("");
  const [keyErr, setKeyErr] = useState<unknown>();

  // ---- whitelist lease form ----
  // merchant_uuid must equal the merchant that owns the API key — the gateway
  // rebuilds the 69B payload server-side with the *authenticated* merchant id,
  // so a mismatched uuid yields an unconditionally invalid admin signature.
  const [wl, setWl] = useState({
    chain_id: String(ANVIL),
    merchant_uuid: demoMerchantId(),
    address: "",
    valid_until_unix: String(Math.floor(Date.now() / 1000) + 30 * 24 * 3600),
  });
  const [wlResult, setWlResult] = useState<{ payloadHex: string; signature: string } | null>(null);

  // ---- refund ticket form ----
  // token must equal supported_assets.token_contract for (chain_id, asset_symbol):
  // the gateway rebuilds the 127B payload from the asset registry, not the form.
  const [rt, setRt] = useState({
    chain_id: String(ANVIL),
    token: assetInfo(ANVIL, "USDC")?.tokenContract ?? "",
    to: "",
    exact_amount: "1500000",
    payout_uuid: crypto.randomUUID(),
    retry_seq: "0",
    expiry_unix: String(Math.floor(Date.now() / 1000) + 3600),
    source_account: "SUSPENSE_REFUNDABLE_LIAB" as const,
    asset_symbol: "USDC",
  });
  const [rtResult, setRtResult] = useState<{ payloadHex: string; signature: string } | null>(null);

  const [formErr, setFormErr] = useState<unknown>();
  const [done, setDone] = useState<string>();

  useEffect(() => {
    void key.probeSupport();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const wlPayload = useMemo(() => {
    try {
      const p = buildAddrV1Payload({
        chainId: BigInt(wl.chain_id || "0"),
        merchantUuid: wl.merchant_uuid,
        recipientAddress: wl.address || "0x0000000000000000000000000000000000000000",
        validUntilUnix: BigInt(wl.valid_until_unix || "0"),
      });
      return { bytes: p, hex: bytesToHex(p), error: null as string | null };
    } catch (e) {
      return { bytes: null, hex: null, error: e instanceof Error ? e.message : String(e) };
    }
  }, [wl]);

  const rtPayload = useMemo(() => {
    try {
      const p = buildRefundV1Payload({
        chainId: BigInt(rt.chain_id || "0"),
        tokenAddress: rt.token || "0x0000000000000000000000000000000000000000",
        toAddress: rt.to || "0x0000000000000000000000000000000000000000",
        exactAmount: BigInt(rt.exact_amount || "0"),
        payoutUuid: rt.payout_uuid,
        retrySeq: BigInt(rt.retry_seq || "0"),
        expiryUnix: BigInt(rt.expiry_unix || "0"),
      });
      return { bytes: p, hex: bytesToHex(p), error: null as string | null };
    } catch (e) {
      return { bytes: null, hex: null, error: e instanceof Error ? e.message : String(e) };
    }
  }, [rt]);

  const signWl = async () => {
    if (!wlPayload.bytes) return;
    const signature = await key.signHex(wlPayload.bytes);
    setWlResult({ payloadHex: wlPayload.hex!, signature });
  };

  const submitWl = async () => {
    if (!wlResult) return;
    setFormErr(undefined);
    setDone(undefined);
    try {
      await register.mutateAsync({
        chain_id: Number(wl.chain_id),
        address: wl.address as `0x${string}`,
        valid_until_unix: Number(wl.valid_until_unix),
        admin_signature: wlResult.signature,
      });
      setDone(`白名单地址 ${shortAddr(wl.address, 10, 6)} 已提交注册（PENDING_COOLDOWN，24h 冷却后激活）`);
      setWlResult(null);
    } catch (e) {
      setFormErr(e);
    }
  };

  const signRt = async () => {
    if (!rtPayload.bytes) return;
    const signature = await key.signHex(rtPayload.bytes);
    setRtResult({ payloadHex: rtPayload.hex!, signature });
  };

  const submitRt = async () => {
    if (!rtResult) return;
    setFormErr(undefined);
    setDone(undefined);
    try {
      const payout = await createRefund.mutateAsync({
        payout_uuid: rt.payout_uuid,
        source_account: rt.source_account,
        chain_id: Number(rt.chain_id),
        asset_symbol: rt.asset_symbol,
        amount: rt.exact_amount,
        // refund_ticket.payout_uuid is sent explicitly: when both levels carry
        // it the API requires byte-equality, and the signer byte-compares the
        // ticket uuid against merchant_payouts.id — keep them identical.
        refund_ticket: assembleRefundTicketFields({
          payout_uuid: rt.payout_uuid,
          to_address: rt.to,
          retry_seq: Number(rt.retry_seq || 0),
          expiry_unix: Number(rt.expiry_unix || 0),
          admin_signature: rtResult.signature,
          signed: null,
        }),
      });
      setDone(`退款票据已受理 → payout ${shortAddr(payout.id, 10, 6)} (${payout.status})`);
      setRtResult(null);
      setRt({ ...rt, payout_uuid: crypto.randomUUID() });
    } catch (e) {
      setFormErr(e);
    }
  };

  return (
    <div className="space-y-6">
      <section>
        <h1 className="mb-1 text-xl font-bold">离线 Ed25519 审批面板</h1>
        <p className="text-sm text-neutral-500">
          管理员密钥仅存于本标签页内存（刷新即销毁，绝不落盘/上传）· 严格按冻结字节布局裸二进制拼接载荷后签名
        </p>
      </section>

      {/* key management */}
      <Card title="管理员密钥（WebCrypto · 内存态）" className={key.hasKey ? "border-emerald-800" : "border-amber-800"}>
        {key.supported === false && (
          <ErrorBox error={new Error("当前运行时不支持 WebCrypto Ed25519 — 请使用 Node22+/现代 Chrome 环境")} />
        )}
        <div className="flex flex-wrap items-center gap-3">
          {key.hasKey ? (
            <>
              <Badge tone="ok">密钥已加载</Badge>
              <span className="font-mono text-xs text-neutral-300">pubkey: {key.publicKeyHex}</span>
              <CopyButton text={key.publicKeyHex!} label="复制公钥" />
              <button type="button" className={btnDangerCls} onClick={key.clear}>
                清除内存密钥
              </button>
            </>
          ) : (
            <>
              <button
                type="button"
                className={btnCls}
                onClick={() => void key.generate().catch(setKeyErr)}
                disabled={key.supported === false}
              >
                生成新密钥对
              </button>
              <span className="text-xs text-neutral-500">或</span>
              <input
                className={inputCls + " max-w-sm"}
                placeholder="导入 32B seed hex（0x…64 hex）"
                value={seed}
                onChange={(e) => setSeed(e.target.value)}
              />
              <button
                type="button"
                className={btnGhostCls}
                disabled={key.supported === false || !seed.trim()}
                onClick={() =>
                  void key
                    .importSeed(seed.trim())
                    .then(() => setSeed(""))
                    .catch(setKeyErr)
                }
              >
                导入 seed
              </button>
            </>
          )}
        </div>
        {keyErr != null && <div className="mt-2"><ErrorBox error={keyErr} /></div>}
        <p className="mt-2 text-[11px] text-neutral-500">
          生成采用非 extractable CryptoKey；导入 seed 仅在本地 wrap 为 PKCS#8 供 WebCrypto 使用，
          任何字节都不会离开浏览器。验签与签名全部在页面内完成。
        </p>
      </Card>

      <div className="grid gap-4 xl:grid-cols-2">
        {/* 69B whitelist lease */}
        <Card title={`① 提现白名单租期 — ADDR_V1 载荷（${ADDR_V1_PAYLOAD_BYTES}B）`}>
          <div className="space-y-3">
            <div className="grid grid-cols-2 gap-3">
              <Field label="chain_id (u64 BE)">
                <input className={inputCls} value={wl.chain_id} onChange={(e) => setWl({ ...wl, chain_id: e.target.value })} />
              </Field>
              <Field label="valid_until_unix (u64 BE)">
                <input className={inputCls} value={wl.valid_until_unix} onChange={(e) => setWl({ ...wl, valid_until_unix: e.target.value })} />
              </Field>
            </div>
            <Field label="merchant_uuid (raw16)" hint="服务端按已认证商户绑定，本地预填演示商户">
              <input className={inputCls} value={wl.merchant_uuid} onChange={(e) => setWl({ ...wl, merchant_uuid: e.target.value })} />
            </Field>
            <Field label="recipient_address (bytes20)">
              <input className={inputCls} value={wl.address} onChange={(e) => setWl({ ...wl, address: e.target.value })} placeholder="0x…" />
            </Field>
            <div className="rounded-lg border border-neutral-800 bg-neutral-950 p-2">
              <div className="mb-1 flex items-center justify-between text-[11px] text-neutral-500">
                <span>载荷预览（"VAULTFLOW_ADDR_V1" ‖ chain_id ‖ merchant ‖ recipient ‖ valid_until）</span>
                <span>{wlPayload.bytes ? `${wlPayload.bytes.length}B` : "—"}</span>
              </div>
              <div className="break-all font-mono text-[10px] leading-relaxed text-emerald-400/80">
                {wlPayload.hex ?? `字段不完整：${wlPayload.error}`}
              </div>
            </div>
            {wlResult && (
              <div className="rounded-lg border border-emerald-800 bg-emerald-950/30 p-2">
                <div className="text-[11px] text-neutral-400">Ed25519 signature (64B)</div>
                <div className="mt-1 break-all font-mono text-[10px] text-emerald-300">{wlResult.signature}</div>
                <CopyButton text={wlResult.signature} label="复制签名" />
              </div>
            )}
            <div className="flex flex-wrap gap-2">
              <button type="button" className={btnGhostCls} disabled={!key.hasKey || !wlPayload.bytes} onClick={() => void signWl().catch(setFormErr)}>
                签署 69B 载荷
              </button>
              <button type="button" className={btnCls} disabled={!wlResult || register.isPending} onClick={() => void submitWl()}>
                {register.isPending && <Spinner />}提交注册（PENDING_COOLDOWN）
              </button>
            </div>
          </div>
        </Card>

        {/* 127B refund ticket */}
        <Card title={`② 客户异常退款票据 — REFUND_V1 载荷（${REFUND_V1_PAYLOAD_BYTES}B）`}>
          <div className="space-y-3">
            <div className="grid grid-cols-2 gap-3">
              <Field label="chain_id (u64 BE)">
                <input className={inputCls} value={rt.chain_id} onChange={(e) => setRt({ ...rt, chain_id: e.target.value })} />
              </Field>
              <Field label="expiry_unix (u64 BE)">
                <input className={inputCls} value={rt.expiry_unix} onChange={(e) => setRt({ ...rt, expiry_unix: e.target.value })} />
              </Field>
              <Field label="token_address (bytes20)">
                <input className={inputCls} value={rt.token} onChange={(e) => setRt({ ...rt, token: e.target.value })} />
              </Field>
              <Field label="exact_amount (u256 BE 原子单位)">
                <input className={inputCls} value={rt.exact_amount} onChange={(e) => setRt({ ...rt, exact_amount: e.target.value })} />
              </Field>
            </div>
            <Field label="to_address (bytes20 客户收款地址)">
              <input className={inputCls} value={rt.to} onChange={(e) => setRt({ ...rt, to: e.target.value })} placeholder="0x…" />
            </Field>
            <div className="grid grid-cols-2 gap-3">
              <Field label="payout_uuid (raw16 幂等锚)">
                <input className={inputCls} value={rt.payout_uuid} onChange={(e) => setRt({ ...rt, payout_uuid: e.target.value })} />
              </Field>
              <Field label="retry_seq (u32 BE)">
                <input className={inputCls} value={rt.retry_seq} onChange={(e) => setRt({ ...rt, retry_seq: e.target.value })} />
              </Field>
            </div>
            <div className="rounded-lg border border-neutral-800 bg-neutral-950 p-2">
              <div className="mb-1 flex items-center justify-between text-[11px] text-neutral-500">
                <span>载荷预览（19B magic ‖ 8B chain ‖ 20B token ‖ 20B to ‖ 32B amount ‖ 16B payout ‖ 4B retry ‖ 8B expiry）</span>
                <span>{rtPayload.bytes ? `${rtPayload.bytes.length}B` : "—"}</span>
              </div>
              <div className="break-all font-mono text-[10px] leading-relaxed text-emerald-400/80">
                {rtPayload.hex ?? `字段不完整：${rtPayload.error}`}
              </div>
            </div>
            {rtResult && (
              <div className="rounded-lg border border-emerald-800 bg-emerald-950/30 p-2">
                <div className="text-[11px] text-neutral-400">Ed25519 signature (64B)</div>
                <div className="mt-1 break-all font-mono text-[10px] text-emerald-300">{rtResult.signature}</div>
                <CopyButton text={rtResult.signature} label="复制签名" />
              </div>
            )}
            <div className="flex flex-wrap gap-2">
              <button type="button" className={btnGhostCls} disabled={!key.hasKey || !rtPayload.bytes} onClick={() => void signRt().catch(setFormErr)}>
                签署 127B 载荷
              </button>
              <button type="button" className={btnCls} disabled={!rtResult || createRefund.isPending} onClick={() => void submitRt()}>
                {createRefund.isPending && <Spinner />}提交退款（LOCKED_QUEUED）
              </button>
            </div>
          </div>
        </Card>
      </div>

      {formErr != null && <ErrorBox error={formErr} />}
      {done && (
        <div className="rounded-lg border border-emerald-800 bg-emerald-950/40 px-4 py-3 text-sm text-emerald-300">{done}</div>
      )}

      {/* whitelist table */}
      <Card title="提现白名单（merchant_withdrawal_addresses）">
        <table className="w-full text-left text-sm">
          <thead className="text-xs text-neutral-500">
            <tr>
              <th className="py-1">地址</th>
              <th>链</th>
              <th>状态</th>
              <th>租期至</th>
              <th>signer 确认吊销</th>
              <th className="text-right">操作</th>
            </tr>
          </thead>
          <tbody className="text-xs">
            {(whitelist.data?.data ?? []).map((w) => (
              <tr key={w.id} className="border-t border-neutral-800">
                <td className="py-2">
                  <AddressText value={w.address} copy />
                </td>
                <td>{w.chain_id}</td>
                <td>
                  <Badge tone={TONE[w.status] ?? "muted"}>{WHITELIST_STATUS_LABEL[w.status]}</Badge>
                </td>
                <td>{fmtDateTime(w.valid_until)}</td>
                <td>{w.signer_revoked ? "✓" : "—"}</td>
                <td className="py-2 text-right">
                  {w.status !== "REVOKED" && (
                    <button type="button" className={btnDangerCls} disabled={revoke.isPending} onClick={() => void revoke.mutateAsync(w.id).catch(setFormErr)}>
                      吊销
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>
    </div>
  );
}
