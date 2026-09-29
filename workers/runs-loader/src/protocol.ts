// SPDX-License-Identifier: Apache-2.0
// The wire contract between the SwarmMemo server (internal/services/runs.go)
// and this loader: the run request, its hard bounds, and the response. The Go
// side mirrors every constant here; a change is a change on both sides.

export const SCHEMA = 1;

// Hard bounds. The server's own configured limits must sit inside these; the
// loader refuses anything outside them whatever the server asks.
export const LIMITS = {
  requestBytes: 256 * 1024,
  codeBytes: 64 * 1024,
  inputBytes: 16 * 1024,
  cpuMsMax: 30_000,
  wallMsMin: 100,
  wallMsMax: 25_000,
  subrequestsMax: 64,
  outputBytesMax: 64 * 1024, // each of stdout, stderr and the result
  errorBytes: 2 * 1024,
  egressRequestsMax: 64,
  egressBytesOutMax: 1 << 20,
  egressBytesInMax: 8 << 20,
  egressLogMax: 128,
  denyEntriesMax: 256,
  hostBytesMax: 253,
  responseBytesMax: 512 * 1024,
  clockSkewSec: 60,
  tailWaitMs: 1500,
} as const;

// The compatibility date every dynamic Worker runs under. Moving it is a
// reviewed change: it changes the runtime agents' code sees.
export const COMPAT_DATE = "2026-09-01";

export type Language = "javascript" | "python";

export interface RunLimits {
  cpu_ms: number;
  subrequests: number;
  wall_ms: number;
  stdout_bytes: number;
  stderr_bytes: number;
  result_bytes: number;
}

export interface NetworkPolicy {
  enabled: boolean;
  max_requests: number;
  max_bytes_out: number;
  max_bytes_in: number;
  ports: number[];
  deny_hosts: string[];
  deny_cidrs: string[];
  allow_connect: boolean;
  resolve: boolean;
}

export interface RunRequest {
  schema: 1;
  run_id: string;
  language: Language;
  code: string;
  input: unknown;
  limits: RunLimits;
  network: NetworkPolicy;
}

export type RunStatus =
  | "ok" // the code returned
  | "error" // the code threw, failed to load, or exported no run function
  | "cpu_exceeded"
  | "subrequests_exceeded"
  | "memory_exceeded"
  | "timeout" // wall_ms passed
  | "bad_output"; // the sandbox answered something that is not the harness's JSON

export interface EgressEntry {
  seq: number;
  t_ms: number; // since the run started
  method: string;
  host: string;
  port: number;
  path_sha256: string; // of path and query; the path itself is never kept
  bytes_out: number;
  bytes_in: number;
  status: number; // upstream HTTP status; 0 when none
  ms: number;
  verdict: "allowed" | "blocked";
  reason: string; // "" when allowed
  pending: boolean; // admitted, not completed when the run ended
}

export interface EgressSummary {
  requests: number; // admitted
  blocked: number;
  bytes_out: number;
  bytes_in: number;
  log: EgressEntry[];
  log_truncated: boolean;
}

export interface RunResponse {
  schema: 1;
  run_id: string;
  status: RunStatus;
  error: string;
  result_json: string; // the returned value as JSON text; "" when none
  stdout: string;
  stderr: string;
  truncated: { stdout: boolean; stderr: boolean; result: boolean };
  cpu_ms: number;
  cpu_source: "tail" | "limit";
  wall_ms: number;
  outcome: string; // the runtime's own outcome from the tail event; "" when unknown
  network: "on" | "off";
  egress: EgressSummary;
}

const RUN_ID = /^[0-9a-f]{32}$/;
const HOST = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/;
const CIDR = /^[0-9a-f:.]+\/[0-9]{1,3}$/;

type Check = { ok: true; run: RunRequest } | { ok: false; error: string };

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function exactKeys(v: Record<string, unknown>, keys: readonly string[]): boolean {
  const got = Object.keys(v);
  return got.length === keys.length && keys.every((k) => Object.prototype.hasOwnProperty.call(v, k));
}

function int(v: unknown, min: number, max: number): v is number {
  return typeof v === "number" && Number.isInteger(v) && v >= min && v <= max;
}

export function utf8Length(s: string): number {
  return new TextEncoder().encode(s).length;
}

// validateRunRequest checks a parsed body strictly: exact keys at every level,
// integers inside the hard bounds, no raw TCP.
export function validateRunRequest(body: unknown): Check {
  const bad = (error: string): Check => ({ ok: false, error });
  if (!isObject(body) || !exactKeys(body, ["schema", "run_id", "language", "code", "input", "limits", "network"])) {
    return bad("the body must be exactly {schema, run_id, language, code, input, limits, network}");
  }
  if (body.schema !== SCHEMA) return bad("schema must be 1");
  if (typeof body.run_id !== "string" || !RUN_ID.test(body.run_id)) return bad("run_id must be 32 lowercase hex");
  if (body.language !== "javascript" && body.language !== "python") return bad("language must be javascript or python");
  if (typeof body.code !== "string" || body.code.length === 0 || utf8Length(body.code) > LIMITS.codeBytes) {
    return bad(`code must be 1 to ${LIMITS.codeBytes} bytes`);
  }
  const input = JSON.stringify(body.input ?? null);
  if (input === undefined || utf8Length(input) > LIMITS.inputBytes) return bad(`input must be JSON of at most ${LIMITS.inputBytes} bytes`);
  const l = body.limits;
  if (!isObject(l) || !exactKeys(l, ["cpu_ms", "subrequests", "wall_ms", "stdout_bytes", "stderr_bytes", "result_bytes"])) {
    return bad("limits must be exactly {cpu_ms, subrequests, wall_ms, stdout_bytes, stderr_bytes, result_bytes}");
  }
  if (
    !int(l.cpu_ms, 1, LIMITS.cpuMsMax) ||
    !int(l.subrequests, 0, LIMITS.subrequestsMax) ||
    !int(l.wall_ms, LIMITS.wallMsMin, LIMITS.wallMsMax) ||
    !int(l.stdout_bytes, 0, LIMITS.outputBytesMax) ||
    !int(l.stderr_bytes, 0, LIMITS.outputBytesMax) ||
    !int(l.result_bytes, 0, LIMITS.outputBytesMax)
  ) {
    return bad("a limit is outside the loader's hard bounds");
  }
  const n = body.network;
  if (
    !isObject(n) ||
    !exactKeys(n, ["enabled", "max_requests", "max_bytes_out", "max_bytes_in", "ports", "deny_hosts", "deny_cidrs", "allow_connect", "resolve"])
  ) {
    return bad("network must be exactly {enabled, max_requests, max_bytes_out, max_bytes_in, ports, deny_hosts, deny_cidrs, allow_connect, resolve}");
  }
  if (typeof n.enabled !== "boolean" || typeof n.allow_connect !== "boolean" || typeof n.resolve !== "boolean") {
    return bad("network flags must be booleans");
  }
  if (
    !int(n.max_requests, 0, LIMITS.egressRequestsMax) ||
    !int(n.max_bytes_out, 0, LIMITS.egressBytesOutMax) ||
    !int(n.max_bytes_in, 0, LIMITS.egressBytesInMax)
  ) {
    return bad("a network cap is outside the loader's hard bounds");
  }
  // Raw TCP cannot be screened by destination through a gateway today, so the
  // loader never allows it, whatever the policy says.
  if (n.allow_connect) return bad("raw connect() is not supported; allow_connect must be false");
  if (!Array.isArray(n.ports) || n.ports.length > 16 || !n.ports.every((p) => int(p, 1, 65535))) {
    return bad("ports must be at most 16 port numbers");
  }
  for (const [name, list, re] of [
    ["deny_hosts", n.deny_hosts, HOST],
    ["deny_cidrs", n.deny_cidrs, CIDR],
  ] as const) {
    if (
      !Array.isArray(list) ||
      list.length > LIMITS.denyEntriesMax ||
      !list.every((h) => typeof h === "string" && h.length <= LIMITS.hostBytesMax && re.test(h))
    ) {
      return bad(`${name} must be at most ${LIMITS.denyEntriesMax} lowercase entries`);
    }
  }
  return { ok: true, run: body as unknown as RunRequest };
}

// truncateUTF8 cuts s to at most max UTF-8 bytes without splitting a
// character, and says whether it cut.
export function truncateUTF8(s: string, max: number): [string, boolean] {
  const bytes = new TextEncoder().encode(s);
  if (bytes.length <= max) return [s, false];
  let end = max;
  while (end > 0 && (bytes[end] & 0xc0) === 0x80) end--;
  return [new TextDecoder().decode(bytes.subarray(0, end)), true];
}
