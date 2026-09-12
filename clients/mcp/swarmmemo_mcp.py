"""Local child-only stdio MCP adapter. Discovery is offline; tools are supervised."""
from __future__ import annotations

import argparse
import asyncio
import json
import logging
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

# Permit isolated (-I) launch without accepting ambient Python import paths.
sys.path.insert(0, str(Path(__file__).resolve().parent))

import anyio
from mcp.server import Server
from mcp.server.stdio import stdio_server
from mcp.shared.message import ServerMessageMetadata, SessionMessage
from mcp.types import CallToolResult, JSONRPCRequest, ListToolsResult, TextContent, Tool, ToolAnnotations

from policy import BridgeError, load_profile, check_binding
from operations import tool_schemas, TOOL_DESCRIPTIONS, TOOL_ANNOTATIONS
from worker import encoded, strict_json

MAX_BYTES = 256 * 1024
MAX_PENDING = 8
OUTPUT_SECONDS = 30
DEADLINE_SECONDS = 30
CLEANUP_SECONDS = 1
WORKER = Path(__file__).resolve().with_name("worker.py")
LOCAL_ERRORS = {"bridge_busy", "worker_failed", "worker_ownership_lost", "worker_deadline",
                "worker_cleanup_failed", "profile_changed", "response_too_large", "invalid_request"}


def filtered_environment():
    # No proxies, cloud credentials, Python injection, telemetry or ambient SDK
    # config. Executable and module paths are fixed argv values, not PATH lookup.
    return {"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "PATH": "/usr/bin:/bin"}


class Supervisor:
    def __init__(self, profile_path, profile):
        self.profile_path = str(Path(profile_path).absolute())
        self.fingerprint = profile.fingerprint
        self.busy = False

    async def call(self, action, arguments):
        if self.busy: return {"ok": False, "error": "bridge_busy"}
        self.busy = True
        process = None
        deadline = time.monotonic() + DEADLINE_SECONDS
        work_deadline = deadline - CLEANUP_SECONDS
        try:
            if signal.getsignal(signal.SIGCHLD) != signal.SIG_DFL:
                return {"ok": False, "error": "worker_ownership_lost"}
            raw = encoded({"action": action, "arguments": arguments,
                           "expected_profile_fingerprint": self.fingerprint})
            if len(raw) > MAX_BYTES: return {"ok": False, "error": "invalid_request"}
            process = subprocess.Popen([sys.executable, "-I", "-B", str(WORKER), self.profile_path, str(os.getpid())],
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                       env=filtered_environment(), start_new_session=True, close_fds=True)
            os.set_blocking(process.stdin.fileno(), False)
            os.set_blocking(process.stdout.fileno(), False)
            output = bytearray(); sent = 0
            while True:
                if time.monotonic() >= work_deadline:
                    return {"ok": False, "error": "worker_deadline"}
                if process.stdin is not None:
                    try: sent += os.write(process.stdin.fileno(), raw[sent:])
                    except BlockingIOError: pass
                    if sent == len(raw): process.stdin.close(); process.stdin = None
                try: chunk = os.read(process.stdout.fileno(), min(65536, MAX_BYTES + 1 - len(output)))
                except BlockingIOError: chunk = None
                if chunk == b"": break
                if chunk:
                    output.extend(chunk)
                    if len(output) > MAX_BYTES: return {"ok": False, "error": "response_too_large"}
                await anyio.sleep(0.01)
            result = strict_json(bytes(output))
            if (not isinstance(result, dict) or type(result.get("ok")) is not bool
                    or set(result) != ({"ok", "result"} if result["ok"] else {"ok", "error"})):
                return {"ok": False, "error": "worker_failed"}
            if result["ok"] and not isinstance(result["result"], dict):
                return {"ok": False, "error": "worker_failed"}
            if not result["ok"]:
                code = result["error"]
                if not isinstance(code, str): return {"ok": False, "error": "worker_failed"}
                safe = BridgeError(code).code
                if code not in LOCAL_ERRORS and safe != code: result["error"] = "worker_failed"
            return result
        except Exception:
            return {"ok": False, "error": "worker_failed"}
        finally:
            try:
                if process is not None:
                    # SDK cancellation uses an AnyIO cancel scope. Shield cleanup
                    # so cancellation/EOF cannot orphan a signer or its children.
                    with anyio.CancelScope(shield=True):
                        await self.reap(process, deadline)
            finally:
                self.busy = False

    @staticmethod
    async def reap(process, deadline):
        try:
            try:
                # Popen.poll/async child watchers would reap early and break
                # group ownership. Observe without reaping, kill, then wait.
                os.waitid(os.P_PID, process.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
            except ChildProcessError:
                raise RuntimeError("worker_ownership_lost") from None
            try: os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            while True:
                try: process.wait(timeout=0); return
                except subprocess.TimeoutExpired:
                    if time.monotonic() >= deadline: raise RuntimeError("worker_cleanup_failed") from None
                    await anyio.sleep(min(0.01, max(0, deadline - time.monotonic())))
        finally:
            for stream in (process.stdin, process.stdout):
                if stream is not None: stream.close()


class Admission:
    """Count replies until fully written, not merely until a handler returns."""
    def __init__(self):
        self.pending = set()
        self.cancelled = set()
        self.initialized = False

    @staticmethod
    def key(value):
        # The SDK aliases integer and string IDs internally. Mirror that alias
        # to prevent duplicate IDs replacing another request's cancellation.
        if type(value) not in (int, str) or len(str(value).encode("utf-8")) > 128:
            raise ValueError("invalid_request")
        if isinstance(value, str):
            try: return str(int(value))
            except ValueError: pass
        return str(value)

    def admit(self, message):
        if "id" in message:
            key = self.key(message["id"])
            if key in self.pending or len(self.pending) >= MAX_PENDING:
                raise ValueError("request_window_exceeded")
            self.pending.add(key)
            return True
        # This server requests no client capabilities. Ignore unsupported
        # notifications BEFORE SDK task creation; only one initialize and one
        # cancellation per outstanding request can enter its dispatcher.
        if message.get("method") == "notifications/initialized" and not self.initialized:
            self.initialized = True
            return True
        if message.get("method") == "notifications/cancelled":
            params = message.get("params")
            if not isinstance(params, dict) or "requestId" not in params: return False
            key = self.key(params["requestId"])
            if key in self.pending and key not in self.cancelled:
                self.cancelled.add(key)
                return True
        return False

    def written(self, message):
        if "id" in message and message["id"] is not None:
            key = self.key(message["id"])
            self.pending.discard(key)
            self.cancelled.discard(key)


class BoundedInput:
    """Strict bounded framing in front of the SDK's protocol parser/dispatcher."""
    def __init__(self, fd, eof, admission):
        self.fd, self.eof, self.buffer, self.admission = fd, eof, bytearray(), admission
        os.set_blocking(fd, False)

    def __aiter__(self): return self

    async def __anext__(self):
        while True:
            newline = self.buffer.find(b"\n")
            if newline >= 0:
                line = bytes(self.buffer[:newline + 1]); del self.buffer[:newline + 1]
                if len(line) > MAX_BYTES: raise ValueError("invalid_request")
                message = strict_json(line)
                if not isinstance(message, dict): raise ValueError("invalid_request")
                if message.get("method") == "tools/call":
                    params = message.get("params")
                    if not isinstance(params, dict) or ("arguments" in params and not isinstance(params["arguments"], dict)):
                        raise ValueError("invalid_request")
                if not self.admission.admit(message): continue
                return line.decode("utf-8")
            if len(self.buffer) >= MAX_BYTES: raise ValueError("invalid_request")
            await anyio.wait_readable(self.fd)
            try: chunk = os.read(self.fd, min(65536, MAX_BYTES - len(self.buffer)))
            except BlockingIOError: continue
            if not chunk:
                self.eof()
                raise StopAsyncIteration
            self.buffer.extend(chunk)


class BoundedOutput:
    def __init__(self, fd, admission):
        self.fd, self.admission = fd, admission
        os.set_blocking(fd, False)

    async def write(self, text):
        raw = text.encode("utf-8")
        if len(raw) > MAX_BYTES: raise ValueError("response_too_large")
        sent = 0
        with anyio.fail_after(OUTPUT_SECONDS):
            while sent < len(raw):
                await anyio.wait_writable(self.fd)
                try: sent += os.write(self.fd, raw[sent:])
                except BlockingIOError: continue
        self.admission.written(strict_json(raw))

    async def flush(self): pass


class SettledInput:
    """Use the SDK's transport hook when cancellation emits no reply."""
    def __init__(self, stream, admission): self.stream, self.admission = stream, admission

    async def receive(self):
        item = await self.stream.receive()
        if isinstance(item, SessionMessage) and isinstance(item.message, JSONRPCRequest):
            request_id = item.message.id
            async def unanswered(): self.admission.written({"id": request_id})
            item.metadata = ServerMessageMetadata(on_request_unanswered=unanswered, can_send_request=False)
        return item

    def __aiter__(self): return self
    async def __anext__(self):
        try: return await self.receive()
        except anyio.EndOfStream: raise StopAsyncIteration from None
    async def aclose(self): await self.stream.aclose()
    async def __aenter__(self): await self.stream.__aenter__(); return self
    async def __aexit__(self, *args): return await self.stream.__aexit__(*args)


def make_server(profile, supervisor):
    schemas = tool_schemas(profile)
    tools = [Tool(name=name, description=TOOL_DESCRIPTIONS[name], input_schema=schema,
                  annotations=ToolAnnotations(**TOOL_ANNOTATIONS[name])) for name, schema in sorted(schemas.items())]

    async def list_tools(_ctx, _params): return ListToolsResult(tools=tools)

    async def call_tool(_ctx, params):
        if params.name not in schemas:
            response = {"ok": False, "error": "invalid_request"}
        else:
            response = await supervisor.call(params.name, params.arguments if params.arguments is not None else {})
        result = CallToolResult(content=[TextContent(type="text", text=encoded(response).decode())],
                                structured_content=response, is_error=not response["ok"])
        # Include room for SDK result/server metadata and the JSON-RPC envelope.
        if len(result.model_dump_json(by_alias=True).encode()) > MAX_BYTES - 2048:
            response = {"ok": False, "error": "response_too_large"}
            result = CallToolResult(content=[TextContent(type="text", text=encoded(response).decode())],
                                    structured_content=response, is_error=True)
        return result

    return Server("swarmmemo-local", version="0.1.0", on_list_tools=list_tools, on_call_tool=call_tool)


async def serve(profile_path, profile):
    supervisor = Supervisor(profile_path, profile)
    server = make_server(profile, supervisor)
    loop = asyncio.get_running_loop()
    old_handlers = {sig: signal.getsignal(sig) for sig in (signal.SIGTERM, signal.SIGINT)}
    with anyio.CancelScope() as shutdown:
        admission = Admission()
        for sig in old_handlers: loop.add_signal_handler(sig, shutdown.cancel)
        try:
            async with stdio_server(BoundedInput(0, shutdown.cancel, admission), BoundedOutput(1, admission)) as (read, write):
                await server.run(SettledInput(read, admission), write, server.create_initialization_options())
        finally:
            for sig, handler in old_handlers.items():
                loop.remove_signal_handler(sig); signal.signal(sig, handler)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", type=Path, required=True)
    parser.add_argument("--check-config", action="store_true")
    args = parser.parse_args(argv)
    logging.disable(logging.CRITICAL)  # SDK parser/transport exceptions may include input.
    try:
        if sys.platform != "linux" or signal.getsignal(signal.SIGCHLD) != signal.SIG_DFL:
            raise ValueError()
        profile = load_profile(args.profile)
        check_binding(profile, create=False)
        if args.check_config:
            print(encoded({"valid": True, "profile_fingerprint": profile.fingerprint, "mode": profile.mode}).decode())
        else:
            anyio.run(serve, args.profile, profile)
        return 0
    except BaseException:
        print('{"error":"bridge_failed"}', file=sys.stderr)
        return 1


if __name__ == "__main__": raise SystemExit(main())
