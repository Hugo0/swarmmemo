// SPDX-License-Identifier: Apache-2.0
// The SwarmMemo runs loader. It takes one signed run from the SwarmMemo
// server, loads the code as a Dynamic Worker with its own CPU and subrequest
// limits, and answers with the output, the CPU time and the egress log,
// signed the same way.
//
//   POST /run     body: protocol.ts RunRequest; X-Runs-Timestamp, X-Runs-Signature
//   GET  /health  "ok"
//
// globalOutbound is always null, so the runtime's own fetch() and connect()
// throw in every run: raw TCP (cloudflare:sockets) never leaves the sandbox.
// Network on: the run's env carries one binding, EGRESS, the EgressGateway
// entrypoint, and the harness points globalThis.fetch at it; the gateway sees
// every outbound request and applies the run's policy (egress.ts). A
// gateway as globalOutbound is not enough: a deployed run's connect() opened
// a socket that the gateway never logged (smoke test, 2026-09-30).
// One RunLedger Durable Object per run holds the egress counters and log, the
// tail event (CPU time, outcome, console output) and the replay marker.

import { DurableObject, WorkerEntrypoint } from "cloudflare:workers";
import { sign, verify, SIGNATURE_HEADER, TIMESTAMP_HEADER } from "./auth.ts";
import { EgressBook, gatewayFetch, readCapped, type Admission, type Attempt, type Completion } from "./egress.ts";
import { buildModules, parseHarness, type HarnessOutput } from "./harness.ts";
import {
  COMPAT_DATE,
  LIMITS,
  truncateUTF8,
  utf8Length,
  validateRunRequest,
  type EgressSummary,
  type NetworkPolicy,
  type RunRequest,
  type RunResponse,
  type RunStatus,
} from "./protocol.ts";

export interface Env {
  LOADER: WorkerLoader;
  RUN_LEDGER: DurableObjectNamespace<RunLedger>;
  RUNS_HMAC_SECRET: string;
}

// What the runtime's tail event says about a run.
export interface TailInfo {
  cpu_ms: number | null;
  wall_ms: number | null;
  outcome: string;
  stdout: string;
  stderr: string;
}

const REPLAY_MEMORY_MS = 10 * 60_000;

function ledgerFor(env: Env, runId: string): DurableObjectStub<RunLedger> {
  return env.RUN_LEDGER.get(env.RUN_LEDGER.idFromName(runId));
}

// ---------------------------------------------------------------- RunLedger

export class RunLedger extends DurableObject<Env> {
  book: EgressBook | null = null;
  tailInfo: TailInfo | null = null;
  waiters: (() => void)[] = [];

  // start is false when this run ID was seen before: a replayed request.
  async start(policy: NetworkPolicy, nowMs: number): Promise<boolean> {
    if (this.book || (await this.ctx.storage.get("started")) !== undefined) return false;
    await this.ctx.storage.put("started", nowMs);
    await this.ctx.storage.setAlarm(nowMs + REPLAY_MEMORY_MS);
    this.book = new EgressBook(policy, nowMs);
    return true;
  }

  async admit(a: Attempt): Promise<Admission> {
    if (!this.book) return { ok: false, reason: "not_started" };
    return this.book.admit(a, Date.now());
  }

  async complete(seq: number, c: Completion): Promise<boolean> {
    return this.book ? this.book.complete(seq, c) : false;
  }

  async block(a: Attempt, reason: string): Promise<void> {
    this.book?.block(a, reason, Date.now());
  }

  async tail(info: TailInfo): Promise<void> {
    this.tailInfo = info;
    for (const w of this.waiters.splice(0)) w();
  }

  // finish closes the run to further egress and waits up to waitMs for the
  // tail event, which the runtime delivers after the run's answer.
  async finish(waitMs: number): Promise<{ egress: EgressSummary | null; tail: TailInfo | null }> {
    this.book?.seal();
    if (!this.tailInfo && waitMs > 0) {
      await new Promise<void>((resolve) => {
        const t = setTimeout(resolve, waitMs);
        this.waiters.push(() => {
          clearTimeout(t);
          resolve();
        });
      });
    }
    return { egress: this.book ? this.book.summary() : null, tail: this.tailInfo };
  }

  async alarm(): Promise<void> {
    await this.ctx.storage.deleteAll();
  }
}

// ---------------------------------------------------------------- entrypoints

type GatewayProps = { runId: string; policy: NetworkPolicy };

const RAW_TCP_ATTEMPT: Attempt = { method: "CONNECT", host: "", port: 0, path_sha256: "", bytes_out: 0 };

// SOCKETS_MODULE is the raw TCP module. Code naming it is refused before it
// is loaded and logged as a blocked connect. This is defence in depth only
// (a computed import name passes it); the fix is globalOutbound null.
const SOCKETS_MODULE = "cloudflare:sockets";

export class EgressGateway extends WorkerEntrypoint<Env, GatewayProps> {
  async fetch(request: Request): Promise<Response> {
    const ledger = ledgerFor(this.env, this.ctx.props.runId);
    return gatewayFetch(
      request,
      this.ctx.props.policy,
      { admit: (a) => ledger.admit(a), complete: (s, c) => ledger.complete(s, c), block: (a, r) => ledger.block(a, r) },
      (input, init) => fetch(input, init),
    );
  }

  // A socket opened on the EGRESS binding itself (env.EGRESS.connect())
  // arrives here. It is never forwarded: it is closed at once, then logged as
  // blocked.
  async connect(socket: { close?: () => unknown } | undefined): Promise<void> {
    try {
      await socket?.close?.();
    } catch {
      // already closed
    }
    await ledgerFor(this.env, this.ctx.props.runId).block(RAW_TCP_ATTEMPT, "raw_tcp");
  }
}

interface TraceLike {
  cpuTime?: number;
  wallTime?: number;
  outcome?: string;
  logs?: { level?: string; message?: unknown }[];
  exceptions?: { name?: string; message?: string }[];
}

function show(v: unknown): string {
  if (typeof v === "string") return v;
  try {
    const s = JSON.stringify(v);
    return s === undefined ? String(v) : s;
  } catch {
    return String(v);
  }
}

// tailInfo reads the one trace event of a run. Its console lines are only
// used when the harness never answered (the run was killed).
export function tailInfo(events: TraceLike[]): TailInfo {
  const ev = events[0] ?? {};
  let stdout = "";
  let stderr = "";
  for (const l of ev.logs ?? []) {
    const line = (Array.isArray(l.message) ? l.message.map(show).join(" ") : show(l.message)) + "\n";
    if (l.level === "warn" || l.level === "error") stderr += line;
    else stdout += line;
    if (stdout.length + stderr.length > 2 * LIMITS.outputBytesMax) break;
  }
  for (const e of ev.exceptions ?? []) stderr += `${e.name ?? "Error"}: ${e.message ?? ""}\n`;
  const num = (x: unknown) => (typeof x === "number" && Number.isFinite(x) && x >= 0 ? x : null);
  return {
    cpu_ms: num(ev.cpuTime),
    wall_ms: num(ev.wallTime),
    outcome: typeof ev.outcome === "string" ? ev.outcome.slice(0, 32) : "",
    stdout: truncateUTF8(stdout, LIMITS.outputBytesMax)[0],
    stderr: truncateUTF8(stderr, LIMITS.outputBytesMax)[0],
  };
}

export class RunTail extends WorkerEntrypoint<Env, { runId: string }> {
  async tail(events: TraceLike[]): Promise<void> {
    await ledgerFor(this.env, this.ctx.props.runId).tail(tailInfo(events));
  }
}

// ---------------------------------------------------------------- running

const TIMEOUT = Symbol("timeout");

function withTimeout<T>(p: Promise<T>, ms: number): Promise<T | typeof TIMEOUT> {
  let t: ReturnType<typeof setTimeout> | undefined;
  const timer = new Promise<typeof TIMEOUT>((resolve) => {
    t = setTimeout(() => resolve(TIMEOUT), ms);
  });
  return Promise.race([p, timer]).finally(() => clearTimeout(t));
}

export function classify(err: unknown): RunStatus {
  const m = String((err as { message?: unknown })?.message ?? err).toLowerCase();
  if (m.includes("cpu")) return "cpu_exceeded";
  if (m.includes("subrequest")) return "subrequests_exceeded";
  if (m.includes("memory")) return "memory_exceeded";
  return "error";
}

function emptyEgress(): EgressSummary {
  return { requests: 0, blocked: 0, bytes_out: 0, bytes_in: 0, log: [], log_truncated: false };
}

export interface RunDeps {
  loader: WorkerLoader;
  exports: LoaderExports;
  ledger: {
    finish(waitMs: number): Promise<{ egress: EgressSummary | null; tail: TailInfo | null }>;
    block(a: Attempt, reason: string): Promise<void>;
  };
  now: () => number;
  tailWaitMs?: number;
}

// execute runs one validated request in a fresh Dynamic Worker.
export async function execute(run: RunRequest, deps: RunDeps): Promise<RunResponse> {
  if (run.code.includes(SOCKETS_MODULE)) {
    await deps.ledger.block(RAW_TCP_ATTEMPT, "raw_tcp");
    const fin = await deps.ledger.finish(0);
    return {
      schema: 1,
      run_id: run.run_id,
      status: "error",
      error: `raw TCP (${SOCKETS_MODULE}) is not available to runs; use fetch()`,
      result_json: "",
      stdout: "",
      stderr: "",
      truncated: { stdout: false, stderr: false, result: false },
      cpu_ms: run.limits.cpu_ms,
      cpu_source: "limit",
      wall_ms: 0,
      outcome: "",
      network: run.network.enabled ? "on" : "off",
      egress: fin.egress ?? emptyEgress(),
    };
  }
  const mods = buildModules(run);
  const networkOn = run.network.enabled;
  // Subrequests are the run's outbound requests. With the network off there
  // are none to make; the floor of 1 keeps the limit a valid positive value.
  const limits = { cpuMs: run.limits.cpu_ms, subRequests: networkOn ? Math.max(1, Math.min(run.limits.subrequests, run.network.max_requests)) : 1 };
  const code: WorkerCode = {
    compatibilityDate: COMPAT_DATE,
    compatibilityFlags: mods.compatibilityFlags,
    mainModule: mods.mainModule,
    modules: mods.modules,
    env: networkOn ? { EGRESS: deps.exports.EgressGateway({ props: { runId: run.run_id, policy: run.network } }) } : {},
    globalOutbound: null,
    tails: [deps.exports.RunTail({ props: { runId: run.run_id } })],
    limits,
  };
  const t0 = deps.now();
  let status: RunStatus = "ok";
  let error = "";
  let harness: HarnessOutput | null = null;
  try {
    const worker = deps.loader.load(code);
    const entry = worker.getEntrypoint(null, { limits });
    const answer = await withTimeout(
      entry.fetch("https://run.invalid/", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ input: run.input ?? null }) }),
      run.limits.wall_ms,
    );
    if (answer === TIMEOUT) {
      status = "timeout";
      error = `the run passed its ${run.limits.wall_ms} ms wall-clock limit`;
    } else {
      // JSON escaping at most sextuples a character; beyond that the answer
      // cannot be the harness's.
      const cap = Math.min(LIMITS.responseBytesMax * 4, 6 * (run.limits.stdout_bytes + run.limits.stderr_bytes + run.limits.result_bytes) + 6 * LIMITS.errorBytes + 4096);
      // The body gets what is left of wall_ms: the run's code can make the
      // answer's body never end (security review 1.20, H4).
      const got = await readCapped(answer.body, cap, run.limits.wall_ms - (deps.now() - t0));
      harness = got.over || got.timedOut ? null : parseHarness(new TextDecoder().decode(got.bytes));
      if (got.timedOut) {
        status = "timeout";
        error = `the run passed its ${run.limits.wall_ms} ms wall-clock limit`;
      } else if (!harness) {
        status = "bad_output";
        error = "the sandbox did not answer with the harness output";
      } else if (!harness.ok) {
        status = "error";
        error = harness.error ?? "";
      }
    }
  } catch (e) {
    status = classify(e);
    error = String((e as { message?: unknown })?.message ?? e);
  }
  const wall = Math.max(0, deps.now() - t0);
  const fin = await deps.ledger.finish(deps.tailWaitMs ?? LIMITS.tailWaitMs);
  const tail = fin.tail;
  if (tail?.outcome === "exceededCpu") status = "cpu_exceeded";
  else if (tail?.outcome === "exceededMemory") status = "memory_exceeded";
  let cpu = tail?.cpu_ms ?? null;
  const cpuSource = cpu === null ? "limit" : "tail";
  // Without a tail event the CPU time is unknown and counts as the limit.
  if (cpu === null) cpu = run.limits.cpu_ms;
  if (status === "cpu_exceeded") cpu = Math.max(cpu, run.limits.cpu_ms);
  const [stdout, cutOut] = truncateUTF8(harness ? harness.stdout : (tail?.stdout ?? ""), run.limits.stdout_bytes);
  const [stderr, cutErr] = truncateUTF8(harness ? harness.stderr : (tail?.stderr ?? ""), run.limits.stderr_bytes);
  let result = harness?.result_json ?? "";
  let cutResult = false;
  if (utf8Length(result) > run.limits.result_bytes) {
    result = "";
    cutResult = true;
  }
  return {
    schema: 1,
    run_id: run.run_id,
    status,
    error: truncateUTF8(error, LIMITS.errorBytes)[0],
    result_json: result,
    stdout,
    stderr,
    truncated: { stdout: cutOut || !!harness?.truncated.stdout, stderr: cutErr || !!harness?.truncated.stderr, result: cutResult },
    cpu_ms: Math.ceil(cpu),
    cpu_source: cpuSource,
    wall_ms: Math.round(wall),
    outcome: tail?.outcome ?? "",
    network: networkOn ? "on" : "off",
    egress: fin.egress ?? emptyEgress(),
  };
}

// ---------------------------------------------------------------- HTTP

// fit encodes a response within LIMITS.responseBytesMax. Only output that
// JSON escaping inflates (control characters) can overflow: then the egress
// log goes (its totals stay), then the result, then stdout and stderr are cut
// to 16 KiB each, which always fits.
export function fit(result: RunResponse): Uint8Array {
  const enc = new TextEncoder();
  let bytes = enc.encode(JSON.stringify(result));
  if (bytes.length <= LIMITS.responseBytesMax) return bytes;
  result.egress = { ...result.egress, log: [], log_truncated: true };
  bytes = enc.encode(JSON.stringify(result));
  if (bytes.length <= LIMITS.responseBytesMax) return bytes;
  if (result.result_json) {
    result.result_json = "";
    result.truncated.result = true;
    bytes = enc.encode(JSON.stringify(result));
    if (bytes.length <= LIMITS.responseBytesMax) return bytes;
  }
  const [out, cutOut] = truncateUTF8(result.stdout, 16 * 1024);
  const [err, cutErr] = truncateUTF8(result.stderr, 16 * 1024);
  result.stdout = out;
  result.stderr = err;
  result.truncated.stdout ||= cutOut;
  result.truncated.stderr ||= cutErr;
  return enc.encode(JSON.stringify(result));
}

function problem(status: number, error: string, message: string): Response {
  return new Response(JSON.stringify({ error, message }), { status, headers: { "content-type": "application/json" } });
}

export async function handle(request: Request, env: Env, ctx: { exports: LoaderExports }, now: () => number = Date.now): Promise<Response> {
  const url = new URL(request.url);
  if (request.method === "GET" && url.pathname === "/health") return new Response("ok\n");
  if (request.method !== "POST" || url.pathname !== "/run") return problem(404, "not_found", "POST /run is the only route.");
  const raw = await readCapped(request.body, LIMITS.requestBytes);
  if (raw.over) return problem(413, "too_large", `The body is larger than ${LIMITS.requestBytes} bytes.`);
  const nowSec = Math.floor(now() / 1000);
  const ok = await verify(env.RUNS_HMAC_SECRET, "req", request.headers.get(TIMESTAMP_HEADER), request.headers.get(SIGNATURE_HEADER), raw.bytes, nowSec);
  if (!ok) return problem(401, "unauthorized", "Missing, stale or wrong signature.");
  let body: unknown;
  try {
    body = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(raw.bytes));
  } catch {
    return problem(400, "invalid_request", "The body is not UTF-8 JSON.");
  }
  const check = validateRunRequest(body);
  if (!check.ok) return problem(400, "invalid_request", check.error);
  const run = check.run;
  const ledger = ledgerFor(env, run.run_id);
  if (!(await ledger.start(run.network, now()))) return problem(409, "replayed", "This run ID was already run.");
  const result = await execute(run, { loader: env.LOADER, exports: ctx.exports, ledger, now });
  const bytes = fit(result);
  const ts = String(Math.floor(now() / 1000));
  return new Response(bytes, {
    headers: {
      "content-type": "application/json",
      [TIMESTAMP_HEADER]: ts,
      [SIGNATURE_HEADER]: await sign(env.RUNS_HMAC_SECRET, "res", ts, bytes),
    },
  });
}

export default {
  async fetch(request: Request, env: Env, ctx: ExecutionContext & { exports: LoaderExports }): Promise<Response> {
    return handle(request, env, ctx);
  },
};
