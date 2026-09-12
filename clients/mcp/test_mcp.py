"""Real SDK stdio-process compatibility and offline safety fixtures."""
import base64
import hashlib
import json
import os
from pathlib import Path
import select
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest

import anyio
from mcp import Client
from mcp.client.stdio import StdioServerParameters

HERE = Path(__file__).resolve().parent


class ProcessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="swarmmemo-mcp-sdk-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.state = self.root / "state"; self.state.mkdir(mode=0o700)
        key = bytes(range(32))
        self.value = {"schema": 1, "origin": "https://unused.invalid", "service_id": "fixture",
                      "public_key": base64.urlsafe_b64encode(key).decode().rstrip("="), "room": "lab",
                      "delegation": {"schema": 1, "grant_id": hashlib.sha256(key).hexdigest(), "generation": "a" * 32},
                      "operations": ["post", "work.claim"], "state_dir": str(self.state)}
        self.path = self.root / "profile.json"; self.write()

    def write(self):
        self.path.write_text(json.dumps(self.value)); self.path.chmod(0o600)

    def argv(self):
        return [sys.executable, "-I", "-B", str(HERE / "swarmmemo_mcp.py"), "--profile", str(self.path)]

    def test_offline_config_no_state(self):
        result = subprocess.run(self.argv() + ["--check-config"], capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)["valid"])
        self.assertEqual(result.stderr, b"")
        self.assertEqual(list(self.state.iterdir()), [])

    async def session(self, mode):
        parameters = StdioServerParameters(command=self.argv()[0], args=self.argv()[1:])
        async with Client(parameters, mode=mode, read_timeout_seconds=10) as client:
            tools = await client.list_tools()
            self.assertEqual(client.protocol_version, "2025-11-25" if mode == "legacy" else "2026-07-28")
            names = {tool.name for tool in tools.tools}
            self.assertEqual(names, {"local_status", "find_work", "read_work", "read_thread", "stage_post", "stage_work"})
            self.assertEqual(list(self.state.iterdir()), [])
            result = await client.call_tool("local_status", {})
            self.assertFalse(result.is_error, result)
            self.assertEqual(list(self.state.iterdir()), [])
            args = {"intent_id": "sdk-fixture", "text": "Unicode ☃ \u2028 inert participant text"}
            first = await client.call_tool("stage_post", args)
            self.assertFalse(first.is_error, first)
            second = await client.call_tool("stage_post", args)
            self.assertFalse(second.is_error, second)
            self.assertIn("queued_not_sent", json.dumps(first.structured_content))
            invalid = await client.call_tool("find_work", {"limit": True})
            self.assertTrue(invalid.is_error)
            self.assertNotIn("unused.invalid", json.dumps(invalid.structured_content))
            self.assertTrue((self.state / "mcp-policy.json").exists())
            self.value["room"] = "other-room"; self.write()
            drift = await client.call_tool("local_status", {})
            self.assertTrue(drift.is_error)
            self.assertIn(drift.structured_content["error"], ("binding_mismatch", "profile_changed"))

    def test_current_sdk_actual_process(self): anyio.run(self.session, "2026-07-28")

    def test_legacy_sdk_actual_process(self): anyio.run(self.session, "legacy")

    def test_raw_duplicate_null_oversize_frames_fail_closed(self):
        for raw in (b'{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stage_post","arguments":null}}\n',
                    b'{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stage_post","arguments":{"text":"secret-sentinel","text":"other"}}}\n',
                    b'x' * (256 * 1024 + 1), b'\xff\n'):
            result = subprocess.run(self.argv(), input=raw, capture_output=True, timeout=10)
            self.assertNotIn(b"secret-sentinel", result.stdout + result.stderr)
            self.assertNotIn(b"Traceback", result.stdout + result.stderr)
            self.assertEqual(list(self.state.iterdir()), [])

    def test_eof_and_repeated_sigterm_reap_inflight_network_worker(self):
        for termination in ("eof", "signal", "kill", "flood"):
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0)); listener.listen(); listener.settimeout(8)
                self.value["origin"] = "http://127.0.0.1:" + str(listener.getsockname()[1]); self.write()
                process = subprocess.Popen(self.argv(), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                try:
                    def send(value):
                        process.stdin.write(json.dumps(value).encode() + b"\n"); process.stdin.flush()
                    send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "fixture", "version": "1"}}})
                    self.assertTrue(select.select([process.stdout], [], [], 8)[0])
                    self.assertEqual(json.loads(process.stdout.readline())["id"], 1)
                    send({"jsonrpc": "2.0", "method": "notifications/initialized"})
                    send({"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "find_work", "arguments": {}}})
                    connection, _ = listener.accept()
                    with connection:
                        children = Path(f"/proc/{process.pid}/task/{process.pid}/children").read_text().split()
                        self.assertEqual(len(children), 1)
                        start = time.monotonic()
                        if termination == "flood":
                            for request_id in range(3, 33):
                                send({"jsonrpc": "2.0", "id": request_id, "method": "tools/list", "params": {}})
                            process.wait(timeout=3)
                        elif termination == "kill": os.kill(process.pid, signal.SIGKILL)
                        elif termination == "signal":
                            os.kill(process.pid, signal.SIGTERM); os.kill(process.pid, signal.SIGTERM)
                        else:
                            process.stdin.close(); process.stdin = None
                        stdout, stderr = process.communicate(timeout=3)
                        self.assertLess(time.monotonic() - start, 3)
                        self.assertNotIn(b"Traceback", stderr)
                        for child in children:
                            # After SIGKILL the bridge cannot reap; Linux's
                            # adopter owns any transient zombie, never a signer.
                            deadline = time.monotonic() + 1
                            while Path(f"/proc/{child}/stat").exists():
                                if Path(f"/proc/{child}/stat").read_text().split()[2] == "Z": break
                                self.assertLess(time.monotonic(), deadline)
                                time.sleep(.01)
                        connection.settimeout(1)
                        while connection.recv(65536): pass
                finally:
                    if process.poll() is None: process.kill()
                    process.communicate(timeout=3)

    def test_unread_stdout_request_flood_closes_boundedly(self):
        process = subprocess.Popen(self.argv(), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            initialize = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "fixture", "version": "1"}}}
            process.stdin.write(json.dumps(initialize).encode() + b"\n"); process.stdin.flush()
            self.assertTrue(select.select([process.stdout], [], [], 8)[0])
            self.assertEqual(json.loads(process.stdout.readline())["id"], 1)
            raw = b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n' + b"".join(json.dumps({"jsonrpc": "2.0", "id": i, "method": "tools/list", "params": {}}).encode() + b"\n" for i in range(2, 1002))
            os.set_blocking(process.stdin.fileno(), False)
            sent = 0; deadline = time.monotonic() + 4
            # Deliberately never read stdout while flooding requests.
            while sent < len(raw) and process.poll() is None:
                self.assertLess(time.monotonic(), deadline)
                try: sent += os.write(process.stdin.fileno(), raw[sent:])
                except BlockingIOError: time.sleep(.01)
                except BrokenPipeError: break
            process.wait(timeout=4)
            stdout, stderr = process.communicate(timeout=1)
            self.assertLess(len(stdout), 256 * 1024)
            self.assertNotIn(b"Traceback", stderr)
            self.assertEqual(list(self.state.iterdir()), [])
        finally:
            if process.poll() is None: process.kill()
            process.communicate(timeout=3)

    def test_worker_refuses_parent_mismatch_before_dispatch(self):
        result = subprocess.run([sys.executable, "-I", "-B", str(HERE / "worker.py"), str(self.path), str(os.getpid() + 1000000)], input=b"{}", capture_output=True, timeout=3)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout + result.stderr, b"")
        self.assertEqual(list(self.state.iterdir()), [])

    def test_repeated_cancelled_requests_release_admission_after_cleanup(self):
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0)); listener.listen(); listener.settimeout(8)
            self.value["origin"] = "http://127.0.0.1:" + str(listener.getsockname()[1]); self.write()
            process = subprocess.Popen(self.argv(), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                def send(value):
                    process.stdin.write(json.dumps(value).encode() + b"\n"); process.stdin.flush()
                send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "fixture", "version": "1"}}})
                self.assertTrue(select.select([process.stdout], [], [], 8)[0])
                self.assertEqual(json.loads(process.stdout.readline())["id"], 1)
                send({"jsonrpc": "2.0", "method": "notifications/initialized"})
                for request_id in range(2, 12):
                    send({"jsonrpc": "2.0", "id": request_id, "method": "tools/call", "params": {"name": "find_work", "arguments": {}}})
                    connection, _ = listener.accept()
                    with connection:
                        children = Path(f"/proc/{process.pid}/task/{process.pid}/children").read_text().split()
                        self.assertEqual(len(children), 1)
                        send({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": request_id}})
                        deadline = time.monotonic() + 2
                        while Path(f"/proc/{children[0]}").exists():
                            self.assertLess(time.monotonic(), deadline)
                            time.sleep(.01)
                send({"jsonrpc": "2.0", "id": 99, "method": "tools/call", "params": {"name": "local_status", "arguments": {}}})
                self.assertTrue(select.select([process.stdout], [], [], 8)[0])
                result = json.loads(process.stdout.readline())
                self.assertEqual(result["id"], 99)
                self.assertTrue(result["result"]["structuredContent"]["ok"])
            finally:
                process.stdin.close(); process.stdin = None
                try: process.communicate(timeout=3)
                except subprocess.TimeoutExpired: process.kill(); process.communicate(timeout=3)


if __name__ == "__main__": unittest.main()
