// SPDX-License-Identifier: Apache-2.0
// The harness modules a run's code is loaded with. The agent's code is one
// module ("user.js" or "user.py") that exports run(input); the harness is the
// main module: it captures console or print output under the run's byte caps,
// calls run(input), and answers with one JSON object. The harness runs inside
// the same sandbox as the agent's code, so its answer is only as trustworthy
// as that code: the loader re-checks its shape and re-caps every field, and
// the CPU time it reports comes from the runtime's tail event, not from here.

import type { RunRequest } from "./protocol.ts";

// What the harness answers.
export interface HarnessOutput {
  ok: boolean;
  result_json: string | null;
  error: string | null;
  stdout: string;
  stderr: string;
  truncated: { stdout: boolean; stderr: boolean };
}

const CAPTURE_JS = (stdout: number, stderr: number) => `
// Evaluated before the agent's module: the harness answers with the
// runtime's own Response and JSON, whatever the agent's code replaces.
export const Response0 = globalThis.Response;
export const stringify0 = JSON.stringify.bind(JSON);
const limit = { stdout: ${stdout}, stderr: ${stderr} };
export const capture = { stdout: [], stderr: [], n: { stdout: 0, stderr: 0 }, cut: { stdout: false, stderr: false } };
const show = (v) => {
  if (typeof v === "string") return v;
  if (v instanceof Error) return v.stack || String(v);
  try { const s = JSON.stringify(v); return s === undefined ? String(v) : s; } catch { return String(v); }
};
const push = (k, args) => {
  if (capture.cut[k]) return;
  const s = args.map(show).join(" ") + "\\n";
  const room = limit[k] - capture.n[k];
  if (s.length > room) { capture[k].push(s.slice(0, Math.max(0, room))); capture.n[k] = limit[k]; capture.cut[k] = true; return; }
  capture[k].push(s); capture.n[k] += s.length;
};
for (const m of ["log", "info", "debug", "trace"]) console[m] = (...a) => push("stdout", a);
for (const m of ["warn", "error"]) console[m] = (...a) => push("stderr", a);
// The runtime's own fetch() and connect() throw (globalOutbound is null).
// With the network on, fetch() goes to env.EGRESS, the egress gateway.
let egress = null;
export const useEgress = (binding) => { egress = binding || null; };
globalThis.fetch = (input, init) =>
  egress ? egress.fetch(input, init) : Promise.reject(new TypeError("Network access is off for this run."));
`;

const MAIN_JS = `
import { capture, useEgress, Response0, stringify0 } from "./__capture.js";
import * as user from "./user.js";
export default {
  async fetch(request, env) {
    useEgress(env && env.EGRESS);
    let input = null;
    try { input = (await request.json()).input; } catch {}
    const fn = typeof user.run === "function" ? user.run : typeof user.default === "function" ? user.default : null;
    let ok = false, result_json = null, error = null;
    if (!fn) {
      error = "the module must export a function run(input), or a default function";
    } else {
      try {
        const r = await fn(input);
        const s = r === undefined ? undefined : JSON.stringify(r);
        result_json = s === undefined ? null : s;
        ok = true;
      } catch (e) {
        error = String((e && e.stack) || e);
      }
    }
    const body = stringify0({ ok, result_json, error, stdout: capture.stdout.join(""), stderr: capture.stderr.join(""), truncated: capture.cut });
    return new Response0(body, { headers: { "content-type": "application/json" } });
  },
};
`;

const MAIN_PY = (stdout: number, stderr: number) => `
import inspect
import json
import sys
import traceback

from workers import Response, WorkerEntrypoint


class _Capture:
    def __init__(self, limit):
        self.parts = []
        self.n = 0
        self.limit = limit
        self.cut = False

    def write(self, s):
        if self.cut:
            return len(s)
        room = self.limit - self.n
        if len(s) > room:
            self.parts.append(s[: max(0, room)])
            self.n = self.limit
            self.cut = True
        else:
            self.parts.append(s)
            self.n += len(s)
        return len(s)

    def flush(self):
        pass

    def text(self):
        return "".join(self.parts)


_out = _Capture(${stdout})
_err = _Capture(${stderr})
sys.stdout = _out
sys.stderr = _err
_import_error = None
try:
    import user
except BaseException:
    user = None
    _import_error = traceback.format_exc()


def _route_fetch(env):
    # The runtime's own fetch() and connect() throw (globalOutbound is null).
    # With the network on, fetch() goes to env.EGRESS, the egress gateway.
    egress = getattr(env, "EGRESS", None) if env is not None else None
    if egress is None:
        return
    import js

    js.globalThis.fetch = egress.fetch.bind(egress)


class Default(WorkerEntrypoint):
    async def fetch(self, request):
        _route_fetch(getattr(self, "env", None))
        ok = False
        result_json = None
        error = _import_error
        fn = getattr(user, "run", None) if user is not None else None
        if error is None and not callable(fn):
            error = "the module must define a function run(input)"
        if error is None:
            try:
                body = json.loads(await request.text())
                r = fn(body.get("input"))
                if inspect.isawaitable(r):
                    r = await r
                result_json = None if r is None else json.dumps(r)
                ok = True
            except BaseException:
                error = traceback.format_exc()
        answer = {
            "ok": ok,
            "result_json": result_json,
            "error": error,
            "stdout": _out.text(),
            "stderr": _err.text(),
            "truncated": {"stdout": _out.cut, "stderr": _err.cut},
        }
        return Response(json.dumps(answer), headers={"content-type": "application/json"})
`;

export interface Modules {
  mainModule: string;
  modules: Record<string, string>;
  compatibilityFlags: string[];
}

// buildModules is the WorkerCode module set for a run.
export function buildModules(run: RunRequest): Modules {
  const { stdout_bytes, stderr_bytes } = run.limits;
  if (run.language === "python") {
    return {
      mainModule: "main.py",
      modules: { "main.py": MAIN_PY(stdout_bytes, stderr_bytes), "user.py": run.code },
      compatibilityFlags: ["python_workers"],
    };
  }
  return {
    mainModule: "__main.js",
    modules: { "__main.js": MAIN_JS, "__capture.js": CAPTURE_JS(stdout_bytes, stderr_bytes), "user.js": run.code },
    compatibilityFlags: [],
  };
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// parseHarness checks the harness's answer. null means the sandbox answered
// something else (the code replaced the harness, or broke its output).
export function parseHarness(text: string): HarnessOutput | null {
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch {
    return null;
  }
  if (!isObject(v) || !isObject(v.truncated)) return null;
  const str = (x: unknown) => typeof x === "string";
  if (
    typeof v.ok !== "boolean" ||
    !(v.result_json === null || str(v.result_json)) ||
    !(v.error === null || str(v.error)) ||
    !str(v.stdout) ||
    !str(v.stderr) ||
    typeof v.truncated.stdout !== "boolean" ||
    typeof v.truncated.stderr !== "boolean"
  ) {
    return null;
  }
  if (typeof v.result_json === "string") {
    try {
      JSON.parse(v.result_json);
    } catch {
      return null;
    }
  }
  return v as unknown as HarnessOutput;
}
