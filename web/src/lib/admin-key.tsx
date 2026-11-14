// W6-3 — in-memory Ed25519 admin key context shared across dashboard pages.
// The CryptoKey lives only in this browser tab's memory: generated
// non-extractable, imported via local PKCS#8 wrap, zeroized on clear() by
// dropping all references (keys are never persisted to storage — refresh = key gone).

"use client";

import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from "react";
import {
  ed25519Available,
  exportRawPublicKey,
  generateAdminKeyPair,
  importAdminSeed,
  signPayload,
  verifyPayload,
} from "./ed25519";
import { bytesToHex0x } from "./bytes";

export interface AdminKeyState {
  /** true once a keypair is loaded in memory. */
  hasKey: boolean;
  /** raw 32B public key, 0x-prefixed lowercase hex. */
  publicKeyHex?: `0x${string}`;
  /** whether the runtime WebCrypto implements Ed25519. */
  supported?: boolean;
  generate: () => Promise<void>;
  importSeed: (seedHex: string) => Promise<void>;
  clear: () => void;
  /** Sign a binary payload -> raw 64B signature hex (0x). Throws if no key. */
  signHex: (payload: Uint8Array) => Promise<`0x${string}`>;
  /** Verify with the in-memory public key. */
  verify: (sigHex: string, payload: Uint8Array) => Promise<boolean>;
  probeSupport: () => Promise<boolean>;
}

const AdminKeyContext = createContext<AdminKeyState | null>(null);

export function AdminKeyProvider({ children }: { children: ReactNode }) {
  const keyRef = useRef<{ priv: CryptoKey; pub: CryptoKey } | null>(null);
  const [publicKeyHex, setPublicKeyHex] = useState<`0x${string}` | undefined>(undefined);
  const [supported, setSupported] = useState<boolean | undefined>(undefined);

  const probeSupport = useCallback(async () => {
    const ok = await ed25519Available().catch(() => false);
    setSupported(ok);
    return ok;
  }, []);

  const generate = useCallback(async () => {
    const kp = await generateAdminKeyPair(false);
    keyRef.current = { priv: kp.privateKey, pub: kp.publicKey };
    setPublicKeyHex(kp.publicKeyHex);
  }, []);

  const importSeed = useCallback(async (seedHex: string) => {
    const kp = await importAdminSeed(seedHex);
    keyRef.current = { priv: kp.privateKey, pub: kp.publicKey };
    setPublicKeyHex(kp.publicKeyHex);
  }, []);

  const clear = useCallback(() => {
    keyRef.current = null;
    setPublicKeyHex(undefined);
  }, []);

  const signHex = useCallback(async (payload: Uint8Array) => {
    if (!keyRef.current) throw new Error("no admin key in memory — generate or import first");
    return bytesToHex0x(await signPayload(keyRef.current.priv, payload));
  }, []);

  const verify = useCallback(async (sigHex: string, payload: Uint8Array) => {
    if (!keyRef.current) throw new Error("no admin key in memory");
    const sig = new Uint8Array(
      (sigHex.startsWith("0x") ? sigHex.slice(2) : sigHex).match(/../g)!.map((h) => parseInt(h, 16)),
    );
    return verifyPayload(keyRef.current.pub, sig, payload);
  }, []);

  const value = useMemo<AdminKeyState>(
    () => ({
      hasKey: publicKeyHex !== undefined,
      publicKeyHex,
      supported,
      generate,
      importSeed,
      clear,
      signHex,
      verify,
      probeSupport,
    }),
    [publicKeyHex, supported, generate, importSeed, clear, signHex, verify, probeSupport],
  );

  return <AdminKeyContext.Provider value={value}>{children}</AdminKeyContext.Provider>;
}

export function useAdminKey(): AdminKeyState {
  const ctx = useContext(AdminKeyContext);
  if (!ctx) throw new Error("useAdminKey must be used inside <AdminKeyProvider>");
  return ctx;
}
