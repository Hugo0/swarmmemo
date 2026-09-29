// SPDX-License-Identifier: Apache-2.0
// The egress policy: which destinations a run may reach, the per-run request
// and byte caps, and the log of every attempt. Nothing here touches the
// Workers runtime, so it runs under node --test as it runs in production;
// index.ts wires it to the gateway entrypoint and the per-run Durable Object.

import { LIMITS, type EgressEntry, type EgressSummary, type NetworkPolicy } from "./protocol.ts";

// ---------------------------------------------------------------- addresses

// An address is 16 bytes; IPv4 is held IPv4-mapped (::ffff:a.b.c.d), so a
// mapped IPv6 literal meets the same IPv4 ranges.
type Addr = Uint8Array;

export function parseIPv4(s: string): Addr | null {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s);
  if (!m) return null;
  const out = new Uint8Array(16);
  out[10] = 0xff;
  out[11] = 0xff;
  for (let i = 0; i < 4; i++) {
    const part = m[i + 1];
    if (part.length > 1 && part[0] === "0") return null;
    const n = Number(part);
    if (n > 255) return null;
    out[12 + i] = n;
  }
  return out;
}

export function parseIPv6(input: string): Addr | null {
  let s = input.toLowerCase();
  if (s.startsWith("[") && s.endsWith("]")) s = s.slice(1, -1);
  if (s.includes("%") || !/^[0-9a-f:.]+$/.test(s) || !s.includes(":")) return null;
  let tail: number[] = [];
  const last = s.lastIndexOf(":");
  if (s.slice(last + 1).includes(".")) {
    // A trailing dotted quad stands for the last two groups.
    const v4 = parseIPv4(s.slice(last + 1));
    if (!v4) return null;
    tail = [(v4[12] << 8) | v4[13], (v4[14] << 8) | v4[15]];
    s = s.slice(0, last + 1);
    if (!s.endsWith("::")) s = s.slice(0, -1);
  }
  const halves = s.split("::");
  if (halves.length > 2) return null;
  const parse = (h: string): number[] | null => {
    if (h === "") return [];
    const out: number[] = [];
    for (const g of h.split(":")) {
      if (!/^[0-9a-f]{1,4}$/.test(g)) return null;
      out.push(parseInt(g, 16));
    }
    return out;
  };
  const head = parse(halves[0]);
  const rest = halves.length === 2 ? parse(halves[1]) : [];
  if (!head || !rest) return null;
  const right = rest.concat(tail);
  const total = head.length + right.length;
  if (halves.length === 1 ? total !== 8 : total > 7) return null;
  const groups = head.concat(new Array<number>(8 - total).fill(0), right);
  const out = new Uint8Array(16);
  groups.forEach((g, i) => {
    out[2 * i] = g >> 8;
    out[2 * i + 1] = g & 0xff;
  });
  return out;
}

export function parseAddr(s: string): Addr | null {
  return parseIPv4(s) ?? parseIPv6(s);
}

interface Cidr {
  base: Addr;
  bits: number;
  label: string;
}

export function parseCidr(s: string, label = "denied_cidr"): Cidr | null {
  const slash = s.lastIndexOf("/");
  if (slash < 0) return null;
  const ip = s.slice(0, slash);
  const bits = Number(s.slice(slash + 1));
  const v4 = parseIPv4(ip);
  if (v4) return Number.isInteger(bits) && bits >= 0 && bits <= 32 ? { base: v4, bits: bits + 96, label } : null;
  const v6 = parseIPv6(ip);
  return v6 && Number.isInteger(bits) && bits >= 0 && bits <= 128 ? { base: v6, bits, label } : null;
}

function contains(c: Cidr, a: Addr): boolean {
  let bits = c.bits;
  for (let i = 0; i < 16 && bits > 0; i++, bits -= 8) {
    const mask = bits >= 8 ? 0xff : (0xff << (8 - bits)) & 0xff;
    if ((c.base[i] & mask) !== (a[i] & mask)) return false;
  }
  return true;
}

const cidrs = (label: string, list: string[]): Cidr[] => list.map((s) => parseCidr(s, label)!);

// Cloud metadata endpoints are named apart from the ranges that already hold
// most of them, so the log says what was reached for.
const METADATA = cidrs("metadata", ["169.254.169.254/32", "169.254.170.2/32", "168.63.129.16/32", "100.100.100.200/32", "fd00:ec2::254/128", "192.0.0.192/32"]);

// Everything that is not a public unicast address (RFC 6890 and friends).
const PRIVATE = cidrs("private_address", [
  "0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
  "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16", "192.175.48.0/24",
  "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
  "::/96", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
  "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
]);

// ---------------------------------------------------------------- names

// Suffixes that name something inside a network rather than on the Internet.
const INTERNAL_SUFFIXES = [
  "localhost", "localdomain", "local", "internal", "intranet", "private", "corp", "home", "lan", "home.arpa",
  "arpa", "onion", "test", "invalid", "example", "svc", "cluster.local",
];

// Known mining pools and in-browser miners. A pool reached over HTTP(S) is
// refused here; stratum over raw TCP is refused with every other connect().
export const MINING_POOLS = [
  "2miners.com", "antpool.com", "braiins.com", "btc.com", "c3pool.com", "coinhive.com", "coinimp.com", "crypto-pool.fr",
  "cryptoloot.pro", "dwarfpool.com", "emcd.io", "ethermine.org", "f2pool.com", "flexpool.io", "hashvault.pro",
  "herominers.com", "hiveon.net", "kryptex.network", "minergate.com", "minexmr.com", "mining-dutch.nl",
  "miningpoolhub.com", "moneroocean.stream", "nanopool.org", "nicehash.com", "poolin.com", "prohashing.com",
  "slushpool.com", "supportxmr.com", "unmineable.com", "viabtc.com", "webminepool.com", "woolypooly.com",
  "xmrig.com", "xmrpool.eu", "zergpool.com",
];

function suffixMatch(host: string, suffixes: readonly string[]): boolean {
  return suffixes.some((s) => host === s || host.endsWith("." + s));
}

// ---------------------------------------------------------------- decisions

export type Verdict = { ok: true } | { ok: false; reason: string };

const DEFAULT_PORTS = [80, 443];
const METHODS = new Set(["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"]);

export function normaliseHost(hostname: string): string {
  return hostname.toLowerCase().replace(/\.+$/, "");
}

// checkAddress decides an IP address alone.
export function checkAddress(a: Addr, policy: NetworkPolicy): Verdict {
  for (const c of METADATA) if (contains(c, a)) return { ok: false, reason: "metadata" };
  for (const c of PRIVATE) if (contains(c, a)) return { ok: false, reason: "private_address" };
  for (const s of policy.deny_cidrs) {
    const c = parseCidr(s);
    if (c && contains(c, a)) return { ok: false, reason: "denied_cidr" };
  }
  return { ok: true };
}

// checkDestination decides a URL before any network traffic: scheme, port,
// method, literal addresses, internal names, mining pools and the run's own
// deny lists. A hostname still has to pass checkResolved when resolve is on.
export function checkDestination(url: URL, method: string, policy: NetworkPolicy): Verdict {
  if (url.protocol !== "https:" && url.protocol !== "http:") return { ok: false, reason: "scheme" };
  if (!METHODS.has(method)) return { ok: false, reason: "method" };
  const ports = policy.ports.length > 0 ? policy.ports : DEFAULT_PORTS;
  if (!ports.includes(portOf(url))) return { ok: false, reason: "port" };
  const host = normaliseHost(url.hostname);
  if (host === "" || host.length > LIMITS.hostBytesMax) return { ok: false, reason: "host" };
  const addr = parseAddr(host);
  if (addr) return checkAddress(addr, policy);
  if (!host.includes(".")) return { ok: false, reason: "internal_name" };
  if (/^[0-9.]+$/.test(host) || /^0x/i.test(host)) return { ok: false, reason: "host" }; // an address the URL parser did not normalise
  if (suffixMatch(host, INTERNAL_SUFFIXES)) return { ok: false, reason: "internal_name" };
  if (suffixMatch(host, MINING_POOLS)) return { ok: false, reason: "mining_pool" };
  if (suffixMatch(host, policy.deny_hosts)) return { ok: false, reason: "denied_host" };
  return { ok: true };
}

export function portOf(url: URL): number {
  if (url.port) return Number(url.port);
  return url.protocol === "http:" ? 80 : 443;
}

type Fetch = (input: string | Request, init?: RequestInit) => Promise<Response>;

// checkResolved resolves a hostname over DNS-over-HTTPS and refuses it when
// any answer is not a public address (a name pointed at 127.0.0.1 or at a
// metadata address). Cloudflare's edge does not route to private networks
// either; this is the second, logged line.
export async function checkResolved(host: string, policy: NetworkPolicy, doFetch: Fetch): Promise<Verdict> {
  let answers = 0;
  for (const type of ["A", "AAAA"]) {
    let body: { Answer?: { type: number; data: string }[] };
    try {
      const r = await doFetch(`https://cloudflare-dns.com/dns-query?name=${encodeURIComponent(host)}&type=${type}`, {
        headers: { accept: "application/dns-json" },
        signal: AbortSignal.timeout(2000),
      });
      if (!r.ok) return { ok: false, reason: "unresolved" };
      body = await r.json();
    } catch {
      return { ok: false, reason: "unresolved" };
    }
    for (const a of body.Answer ?? []) {
      if (a.type !== 1 && a.type !== 28) continue;
      const addr = parseAddr(String(a.data));
      if (!addr) return { ok: false, reason: "unresolved" };
      const v = checkAddress(addr, policy);
      if (!v.ok) return v;
      answers++;
    }
  }
  return answers > 0 ? { ok: true } : { ok: false, reason: "unresolved" };
}

// ---------------------------------------------------------------- the book

export interface Attempt {
  method: string;
  host: string;
  port: number;
  path_sha256: string;
  bytes_out: number;
}

export type Admission = { ok: true; seq: number; bytesInLeft: number } | { ok: false; reason: string };

export interface Completion {
  status: number;
  bytes_in: number;
  ms: number;
  error?: string;
}

// EgressBook is one run's egress state: caps, counters and the log. The
// per-run Durable Object holds one, so every gateway invocation of a run sees
// the same counters, one call at a time.
export class EgressBook {
  policy: NetworkPolicy;
  startMs: number;
  log: EgressEntry[] = [];
  requests = 0;
  blocked = 0;
  bytesOut = 0;
  bytesIn = 0;
  truncated = false;
  sealed = false;
  next = 0;
  open = new Map<number, EgressEntry>();

  constructor(policy: NetworkPolicy, startMs: number) {
    this.policy = policy;
    this.startMs = startMs;
  }

  private entry(a: Attempt, nowMs: number, verdict: "allowed" | "blocked", reason: string): EgressEntry {
    const e: EgressEntry = {
      seq: this.next++,
      t_ms: Math.max(0, nowMs - this.startMs),
      method: a.method.slice(0, 16),
      host: a.host.slice(0, LIMITS.hostBytesMax),
      port: a.port,
      path_sha256: a.path_sha256,
      bytes_out: a.bytes_out,
      bytes_in: 0,
      status: 0,
      ms: 0,
      verdict,
      reason,
      pending: verdict === "allowed",
    };
    if (this.log.length < LIMITS.egressLogMax) this.log.push(e);
    else this.truncated = true;
    return e;
  }

  block(a: Attempt, reason: string, nowMs: number): void {
    this.blocked++;
    this.entry(a, nowMs, "blocked", reason);
  }

  admit(a: Attempt, nowMs: number): Admission {
    let reason = "";
    if (this.sealed) reason = "run_ended";
    else if (!this.policy.enabled) reason = "network_off";
    else if (this.requests >= this.policy.max_requests) reason = "request_cap";
    else if (this.bytesOut + a.bytes_out > this.policy.max_bytes_out) reason = "bytes_out_cap";
    if (reason) {
      this.block(a, reason, nowMs);
      return { ok: false, reason };
    }
    this.requests++;
    this.bytesOut += a.bytes_out;
    const e = this.entry(a, nowMs, "allowed", "");
    this.open.set(e.seq, e);
    return { ok: true, seq: e.seq, bytesInLeft: Math.max(0, this.policy.max_bytes_in - this.bytesIn) };
  }

  // complete records an admitted request's answer. It is false when the
  // answer pushed the run over its inbound byte cap: the gateway then drops
  // the body, and the bytes still count, since they were transferred.
  complete(seq: number, c: Completion): boolean {
    const e = this.open.get(seq);
    if (!e) return false;
    this.open.delete(seq);
    e.pending = false;
    e.status = c.status;
    e.bytes_in = c.bytes_in;
    e.ms = c.ms;
    this.bytesIn += c.bytes_in;
    if (c.error) e.reason = c.error.slice(0, 64);
    if (this.bytesIn > this.policy.max_bytes_in) {
      e.verdict = "blocked";
      e.reason = "bytes_in_cap";
      this.blocked++;
      return false;
    }
    return true;
  }

  seal(): void {
    this.sealed = true;
  }

  summary(): EgressSummary {
    return {
      requests: this.requests,
      blocked: this.blocked,
      bytes_out: this.bytesOut,
      bytes_in: this.bytesIn,
      log: this.log.map((e) => ({ ...e })),
      log_truncated: this.truncated,
    };
  }
}

// ---------------------------------------------------------------- the gateway

export interface LedgerLike {
  admit(a: Attempt): Promise<Admission>;
  complete(seq: number, c: Completion): Promise<boolean>;
  block(a: Attempt, reason: string): Promise<void>;
}

const DROP_OUT = /^(host|connection|keep-alive|proxy-.*|te|trailer|transfer-encoding|upgrade|forwarded|x-forwarded-.*|x-real-ip|cf-.*)$/;
const DROP_IN = /^(connection|keep-alive|transfer-encoding|content-encoding|content-length|trailer|upgrade)$/;

async function sha256Hex(s: string): Promise<string> {
  const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return Array.from(new Uint8Array(d), (b) => b.toString(16).padStart(2, "0")).join("");
}

// readCapped reads a body into memory, stopping one byte past max.
// With deadlineMs it gives up that many milliseconds from the call, cancels
// the body and answers timedOut: a body that never ends (a run can make its
// own answer one) cannot hold the loader past the run's wall-clock limit.
export async function readCapped(
  body: ReadableStream<Uint8Array> | null,
  max: number,
  deadlineMs?: number,
): Promise<{ bytes: Uint8Array; n: number; over: boolean; timedOut?: boolean }> {
  if (!body) return { bytes: new Uint8Array(0), n: 0, over: false };
  const reader = body.getReader();
  const parts: Uint8Array[] = [];
  let n = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const expired =
    deadlineMs === undefined
      ? null
      : new Promise<"timeout">((resolve) => {
          timer = setTimeout(() => resolve("timeout"), Math.max(0, deadlineMs));
        });
  try {
    for (;;) {
      const next = expired ? await Promise.race([reader.read(), expired]) : await reader.read();
      if (next === "timeout") {
        reader.cancel().catch(() => {}); // not awaited: the source may never settle
        return { bytes: new Uint8Array(0), n, over: false, timedOut: true };
      }
      const { done, value } = next;
      if (done) break;
      n += value.length;
      if (n > max) {
        await reader.cancel().catch(() => {});
        return { bytes: new Uint8Array(0), n, over: true };
      }
      parts.push(value);
    }
  } finally {
    clearTimeout(timer);
  }
  return join(parts, n);
}

function join(parts: Uint8Array[], n: number): { bytes: Uint8Array; n: number; over: boolean } {
  const out = new Uint8Array(n);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return { bytes: out, n, over: false };
}

function refused(reason: string, status = 403): Response {
  return new Response(JSON.stringify({ error: "egress_blocked", reason }), {
    status,
    headers: { "content-type": "application/json", "x-runs-egress": "blocked" },
  });
}

// gatewayFetch is the whole outbound path of one request from a run: decide,
// admit against the run's caps, forward without following redirects (each hop
// comes back through here), read the answer under the byte cap, record.
export async function gatewayFetch(
  request: Request,
  policy: NetworkPolicy,
  ledger: LedgerLike,
  doFetch: Fetch,
  clock: () => number = Date.now,
): Promise<Response> {
  let url: URL;
  try {
    url = new URL(request.url);
  } catch {
    return refused("url");
  }
  const method = request.method.toUpperCase();
  const attempt: Attempt = {
    method,
    host: normaliseHost(url.hostname),
    port: url.protocol === "http:" || url.protocol === "https:" ? portOf(url) : 0,
    path_sha256: await sha256Hex(url.pathname + url.search),
    bytes_out: 0,
  };
  let verdict = checkDestination(url, method, policy);
  if (verdict.ok && request.headers.get("upgrade")) verdict = { ok: false, reason: "upgrade" };
  if (verdict.ok && policy.resolve && !parseAddr(attempt.host)) verdict = await checkResolved(attempt.host, policy, doFetch);
  if (!verdict.ok) {
    await ledger.block(attempt, verdict.reason);
    return refused(verdict.reason);
  }
  const body = method === "GET" || method === "HEAD" ? { bytes: new Uint8Array(0), n: 0, over: false } : await readCapped(request.body, policy.max_bytes_out);
  attempt.bytes_out = body.n;
  if (body.over) {
    await ledger.block(attempt, "bytes_out_cap");
    return refused("bytes_out_cap");
  }
  const admission = await ledger.admit(attempt);
  if (!admission.ok) return refused(admission.reason, 429);
  const headers = new Headers();
  request.headers.forEach((v, k) => {
    if (!DROP_OUT.test(k.toLowerCase())) headers.set(k, v);
  });
  const t0 = clock();
  let upstream: Response;
  try {
    upstream = await doFetch(url.toString(), {
      method,
      headers,
      body: body.n > 0 ? body.bytes : undefined,
      redirect: "manual",
      signal: AbortSignal.timeout(LIMITS.wallMsMax),
    });
  } catch {
    await ledger.complete(admission.seq, { status: 0, bytes_in: 0, ms: clock() - t0, error: "fetch_failed" });
    return refused("fetch_failed", 502);
  }
  if ((upstream as Response & { webSocket?: unknown }).webSocket) {
    await ledger.complete(admission.seq, { status: upstream.status, bytes_in: 0, ms: clock() - t0, error: "websocket" });
    return refused("websocket", 502);
  }
  const answer = await readCapped(upstream.body, admission.bytesInLeft);
  const within = await ledger.complete(admission.seq, { status: upstream.status, bytes_in: answer.n, ms: clock() - t0 });
  if (answer.over || !within) return refused("bytes_in_cap", 502);
  const out = new Headers();
  upstream.headers.forEach((v, k) => {
    if (!DROP_IN.test(k.toLowerCase())) out.set(k, v);
  });
  const nullBody = method === "HEAD" || upstream.status === 204 || upstream.status === 304;
  return new Response(nullBody ? null : answer.bytes, { status: upstream.status, statusText: upstream.statusText, headers: out });
}
