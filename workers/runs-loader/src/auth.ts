// SPDX-License-Identifier: Apache-2.0
// HMAC-SHA256 between the SwarmMemo server and the loader, both directions,
// with one shared secret (the RUNS_HMAC_SECRET Worker secret; the server
// reads the same value from a file). The MAC covers a direction label, the
// timestamp and the exact body bytes:
//
//   X-Runs-Timestamp: UNIX seconds
//   X-Runs-Signature: v1=hex(HMAC-SHA256(secret, LABEL "." TIMESTAMP "." BODY))
//
// LABEL is "req" on a request to the loader and "res" on its answer, so a
// captured answer can never be replayed as a request. Verification uses
// crypto.subtle.verify, which compares in constant time.

import { LIMITS } from "./protocol.ts";

export const TIMESTAMP_HEADER = "x-runs-timestamp";
export const SIGNATURE_HEADER = "x-runs-signature";
export const SECRET_MIN_BYTES = 32;

const enc = new TextEncoder();

async function key(secret: string): Promise<CryptoKey> {
  return crypto.subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign", "verify"]);
}

function message(label: "req" | "res", timestamp: string, body: Uint8Array): Uint8Array {
  const head = enc.encode(`${label}.${timestamp}.`);
  const out = new Uint8Array(head.length + body.length);
  out.set(head, 0);
  out.set(body, head.length);
  return out;
}

function hex(bytes: ArrayBuffer): string {
  return Array.from(new Uint8Array(bytes), (b) => b.toString(16).padStart(2, "0")).join("");
}

function unhex(s: string): Uint8Array | null {
  if (!/^[0-9a-f]{64}$/.test(s)) return null;
  const out = new Uint8Array(32);
  for (let i = 0; i < 32; i++) out[i] = parseInt(s.slice(2 * i, 2 * i + 2), 16);
  return out;
}

export async function sign(secret: string, label: "req" | "res", timestamp: string, body: Uint8Array): Promise<string> {
  return "v1=" + hex(await crypto.subtle.sign("HMAC", await key(secret), message(label, timestamp, body)));
}

// verify checks a signature and that the timestamp is a whole number of
// seconds within LIMITS.clockSkewSec of now.
export async function verify(
  secret: string,
  label: "req" | "res",
  timestamp: string | null,
  signature: string | null,
  body: Uint8Array,
  nowSec: number,
): Promise<boolean> {
  if (!secret || enc.encode(secret).length < SECRET_MIN_BYTES) return false;
  if (!timestamp || !/^[0-9]{1,12}$/.test(timestamp) || !signature || !signature.startsWith("v1=")) return false;
  if (Math.abs(nowSec - Number(timestamp)) > LIMITS.clockSkewSec) return false;
  const mac = unhex(signature.slice(3));
  if (!mac) return false;
  return crypto.subtle.verify("HMAC", await key(secret), mac, message(label, timestamp, body));
}
