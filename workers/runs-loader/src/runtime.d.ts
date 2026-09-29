// SPDX-License-Identifier: Apache-2.0
// The slice of the Workers runtime types this loader uses, so `tsc --noEmit`
// checks it without @cloudflare/workers-types. `npx wrangler types` generates
// the full set when the steward deploys.

declare module "cloudflare:workers" {
  export abstract class DurableObject<E = unknown> {
    ctx: DurableObjectState;
    env: E;
    constructor(ctx: DurableObjectState, env: E);
  }
  export abstract class WorkerEntrypoint<E = unknown, P = unknown> {
    ctx: ExecutionContext & { props: P };
    env: E;
    constructor(ctx: ExecutionContext, env: E);
  }
}

interface DurableObjectState {
  storage: {
    get(key: string): Promise<unknown>;
    put(key: string, value: unknown): Promise<void>;
    setAlarm(when: number): Promise<void>;
    deleteAll(): Promise<void>;
  };
}

interface ExecutionContext {
  waitUntil(p: Promise<unknown>): void;
}

interface DurableObjectId {}

type DurableObjectStub<T> = {
  [K in keyof T]: T[K] extends (...a: infer A) => infer R ? (...a: A) => R extends Promise<unknown> ? R : Promise<R> : never;
};

interface DurableObjectNamespace<T> {
  idFromName(name: string): DurableObjectId;
  get(id: DurableObjectId): DurableObjectStub<T>;
}

interface ServiceStub {
  fetch(input: string | Request, init?: RequestInit): Promise<Response>;
}

interface WorkerCode {
  compatibilityDate: string;
  compatibilityFlags?: string[];
  mainModule: string;
  modules: Record<string, string>;
  env?: Record<string, unknown>;
  globalOutbound?: ServiceStub | null;
  tails?: ServiceStub[];
  limits?: { cpuMs?: number; subRequests?: number };
}

interface WorkerStub {
  getEntrypoint(name?: string | null, options?: { limits?: { cpuMs?: number; subRequests?: number } }): ServiceStub;
}

interface WorkerLoader {
  load(code: WorkerCode): WorkerStub;
  get(id: string, getCode: () => Promise<WorkerCode>): WorkerStub;
}

// ctx.exports: loopback bindings for this Worker's own entrypoints.
interface LoaderExports {
  EgressGateway(options: { props: { runId: string; policy: import("./protocol.ts").NetworkPolicy } }): ServiceStub;
  RunTail(options: { props: { runId: string } }): ServiceStub;
}
