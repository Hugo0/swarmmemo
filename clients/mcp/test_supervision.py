"""No external traffic: killable process-boundary adversarial fixtures."""
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import anyio
import swarmmemo_mcp as bridge


class SupervisorTests(unittest.TestCase):
    async def fixture(self, code, *, cancel=False):
        real_popen = subprocess.Popen
        children = []
        def spawn(argv, **kwargs):
            self.assertEqual(argv[1:4], ["-I", "-B", str(bridge.WORKER)])
            self.assertEqual(argv[-1], str(os.getpid()))
            self.assertEqual(kwargs["env"], bridge.filtered_environment())
            process = real_popen([sys.executable, "-I", "-c", code], **kwargs)
            children.append(process)
            return process
        supervisor = bridge.Supervisor("/unused/profile", SimpleNamespace(fingerprint="a" * 64))
        start = time.monotonic()
        with patch.object(bridge.subprocess, "Popen", side_effect=spawn), patch.object(bridge, "DEADLINE_SECONDS", 0.65), patch.object(bridge, "CLEANUP_SECONDS", 0.25):
            if cancel:
                with anyio.move_on_after(0.1) as scope:
                    result = await supervisor.call("local_status", {})
                self.assertTrue(scope.cancel_called)
                result = None
            else:
                result = await supervisor.call("local_status", {})
        self.assertLess(time.monotonic() - start, 1.5)
        self.assertFalse(supervisor.busy)
        for process in children:
            with self.assertRaises(ChildProcessError): os.waitpid(process.pid, os.WNOHANG)
            self.assertTrue(process.stdout.closed)
        return result

    def test_bridge_notice_reaches_the_model_once_per_bridge_process(self):
        # C1030: every call is a new worker, so the supervisor keeps the one notice.
        supervisor = bridge.Supervisor("/unused/profile", SimpleNamespace(fingerprint="a" * 64))
        notice = {"unknown_fields": ["zz_new"], "unknown_count": 1, "update": "u"}
        first, second = {"works": [], "bridge_notice": dict(notice)}, {"works": [], "bridge_notice": dict(notice)}
        with patch.object(bridge.sys, "stderr") as stderr:
            supervisor.notice(first); supervisor.notice(second)
        self.assertEqual((first["bridge_notice"], "bridge_notice" in second), (notice, False))
        lines = "".join(call.args[0] for call in stderr.write.call_args_list if call.args)
        self.assertEqual(lines, "bridge_outdated: server added fields 1; update the checkout\n")

    def test_contract_check_is_asked_until_a_worker_reports_it(self):
        # C1031: check_contract goes to each worker until one answers contract_checked;
        # a contract notice gets its own stderr line.
        code = ("import json,sys\nr=json.loads(sys.stdin.buffer.read())\nout={'ok':True,'result':{'asked':r.get('check_contract',False)}}\n"
                "if r['action']=='find_work': out['contract_checked']=True; out['result']['bridge_notice']={'unknown_fields':[],'unknown_count':0,'update':'u','server_contract_sha256':'f'*64}\n"
                "sys.stdout.write(json.dumps(out))")
        real_popen = subprocess.Popen
        supervisor = bridge.Supervisor("/unused/profile", SimpleNamespace(fingerprint="a" * 64))
        async def calls():
            return [await supervisor.call(action, {}) for action in ("local_status", "find_work", "find_work")]
        with patch.object(bridge.subprocess, "Popen", side_effect=lambda argv, **kw: real_popen([sys.executable, "-I", "-c", code], **kw)), \
                patch.object(bridge.sys, "stderr") as stderr:
            first, second, third = anyio.run(calls)
        self.assertEqual([r["result"]["asked"] for r in (first, second, third)], [True, True, False])
        self.assertEqual(set(second), {"ok", "result"}); self.assertIn("bridge_notice", second["result"]); self.assertNotIn("bridge_notice", third["result"])
        lines = "".join(call.args[0] for call in stderr.write.call_args_list if call.args)
        self.assertEqual(lines, "bridge_outdated: server contract differs; update the checkout\n")

    def test_actual_worker_block_and_trickle_total_deadline(self):
        for code in ("import time;time.sleep(20)", "import os,time\nwhile True: os.write(1,b' ');time.sleep(.02)"):
            result = anyio.run(self.fixture, code)
            self.assertEqual(result, {"ok": False, "error": "worker_deadline"})

    def test_caps_and_error_redaction(self):
        for code, expected in (("import os;os.write(1,b'x'*300000)", "response_too_large"),
                               ("print('{\"ok\":false,\"error\":\"private-path-secret\"}')", "worker_failed"),
                               ("print('{\"ok\":true,\"result\":{},\"ok\":false}')", "worker_failed")):
            result = anyio.run(self.fixture, code)
            self.assertEqual(result, {"ok": False, "error": expected})

    def test_success_and_filtered_environment(self):
        with patch.dict(os.environ, {"MCP_SECRET_FIXTURE": "never-pass", "HTTPS_PROXY": "http://secret"}):
            result = anyio.run(self.fixture, "import os,json;print(json.dumps({'ok':True,'result':{'secret':os.getenv('MCP_SECRET_FIXTURE'),'proxy':os.getenv('HTTPS_PROXY')}}))")
        self.assertEqual(result, {"ok": True, "result": {"secret": None, "proxy": None}})

    def test_cancellation_kills_and_reaps(self):
        async def run(): await self.fixture("import time;time.sleep(20)", cancel=True)
        anyio.run(run)

    def test_grandchild_retaining_pipe_cannot_extend_deadline(self):
        # Child remains the owned unreaped process-group leader; descendant
        # inherits stdout so only killing the entire owned group ends this case.
        result = anyio.run(self.fixture, "import subprocess,sys\nsubprocess.Popen([sys.executable,'-c','import time;time.sleep(20)'])")
        self.assertEqual(result, {"ok": False, "error": "worker_deadline"})

    def test_unsafe_sigchld_rejected_before_spawn(self):
        old = signal.signal(signal.SIGCHLD, signal.SIG_IGN)
        try:
            supervisor = bridge.Supervisor("/unused/profile", SimpleNamespace(fingerprint="a" * 64))
            with patch.object(bridge.subprocess, "Popen") as spawn:
                result = anyio.run(supervisor.call, "local_status", {})
                spawn.assert_not_called()
            self.assertEqual(result["error"], "worker_ownership_lost")
        finally: signal.signal(signal.SIGCHLD, old)

    def test_postlaunch_setup_failure_reaps_owned_child(self):
        async def run():
            real_popen = subprocess.Popen
            children = []
            def spawn(_argv, **kwargs):
                process = real_popen([sys.executable, "-I", "-c", "import time;time.sleep(20)"], **kwargs)
                children.append(process)
                return process
            supervisor = bridge.Supervisor("/unused/profile", SimpleNamespace(fingerprint="a" * 64))
            with patch.object(bridge.subprocess, "Popen", side_effect=spawn), patch.object(bridge.os, "set_blocking", side_effect=OSError("private-path-secret")):
                self.assertEqual(await supervisor.call("local_status", {}), {"ok": False, "error": "worker_failed"})
            for process in children:
                with self.assertRaises(ChildProcessError): os.waitpid(process.pid, os.WNOHANG)
        anyio.run(run)

    def test_admission_bounds_duplicate_aliases_and_notification_flood(self):
        admission = bridge.Admission()
        self.assertTrue(admission.admit({"id": 1, "method": "tools/list"}))
        for value in ("1", "01", "+1", " 1 "):
            with self.assertRaises(ValueError): admission.admit({"id": value, "method": "tools/list"})
        for _ in range(1000): self.assertFalse(admission.admit({"method": "unknown/notification"}))
        self.assertTrue(admission.admit({"method": "notifications/cancelled", "params": {"requestId": "01"}}))
        for _ in range(1000):
            self.assertFalse(admission.admit({"method": "notifications/cancelled", "params": {"requestId": 1}}))
        for value in range(2, bridge.MAX_PENDING + 1): admission.admit({"id": value})
        with self.assertRaises(ValueError): admission.admit({"id": 99})
        admission.written({"id": "01"})
        self.assertTrue(admission.admit({"id": 99}))

    def test_output_backpressure_has_total_timeout_and_does_not_release_slot(self):
        async def run():
            read_fd, write_fd = os.pipe()
            try:
                os.set_blocking(write_fd, False)
                while True:
                    try: os.write(write_fd, b"x" * 4096)
                    except BlockingIOError: break
                admission = bridge.Admission(); admission.admit({"id": 1})
                output = bridge.BoundedOutput(write_fd, admission)
                with patch.object(bridge, "OUTPUT_SECONDS", .1), self.assertRaises(TimeoutError):
                    await output.write('{"jsonrpc":"2.0","id":1,"result":{}}\n')
                self.assertEqual(admission.pending, {"1"})
            finally: os.close(read_fd); os.close(write_fd)
        anyio.run(run)


if __name__ == "__main__": unittest.main()
