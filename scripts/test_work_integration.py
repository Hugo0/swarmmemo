"""Opt-in local work simulation: Go service, Python requester and fresh Node workers."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "clients/python"))
import swarmmemo as memo
import swarmmemo_outbox as outbox


@unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY") and shutil.which("node"), "requires explicit local Go binary and Node 22+")
class CrossRuntimeWorkTests(unittest.TestCase):
    def test_epoch_change_never_prepares_or_resigns_result_steps(self):
        for stop_at in ("after_prepare_blob", "after_accept_blob", "after_record_blob", "after_prepare_post", "after_prepare_submit"):
            with self.subTest(stop_at=stop_at), tempfile.TemporaryDirectory(prefix="swarmmemo-work-epoch-") as directory:
                local = Path(directory)
                with socket.socket() as reservation:
                    reservation.bind(("127.0.0.1", 0)); port = reservation.getsockname()[1]
                origin = "http://127.0.0.1:" + str(port)
                env = {**os.environ, "DATA_DIR": str(local / "server"), "LISTEN_ADDR": "127.0.0.1:" + str(port), "ALLOW_INSECURE_LOCAL": "true"}
                binary = os.environ["SWARMMEMO_TEST_BINARY"]
                process = None
                def start():
                    process = subprocess.Popen([binary, "serve"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    for _ in range(100):
                        try:
                            with urllib.request.urlopen(origin + "/health", timeout=1): return process
                        except OSError:
                            if process.poll() is not None: self.fail("fixture server exited")
                            time.sleep(.05)
                    process.terminate(); process.wait(timeout=5)
                    self.fail("fixture server startup failed")
                try:
                    process = start()
                    requester_path, worker_path = local / "requester.json", local / "worker.json"
                    memo.keygen(requester_path); memo.keygen(worker_path)
                    requester = memo.Client(origin, memo.load_key(requester_path)); reader = memo.Client(origin)
                    generation = reader._request("/api/changes?after=-1")["generation"]
                    work_id = requester.post("coordination-lab", "main", "Operator-run epoch recovery simulation; not independent adoption.", kind="simulation")["receipt"]["id"]
                    requester.command("work.create", message_id=work_id, ttl=3600,
                                      data=json.dumps({"schema": 1, "generation": generation, "title": "Simulation: cross-runtime UTF-8 accounting", "capabilities": []}))
                    def worker(action, failpoint=None, expected=0):
                        worker_env = dict(os.environ); worker_env.pop("SWARMMEMO_LAB_FAILPOINT", None)
                        if failpoint: worker_env["SWARMMEMO_LAB_FAILPOINT"] = failpoint
                        result = subprocess.run(["node", str(ROOT / "examples/coordination-lab/worker.mjs"), action, origin,
                                                 str(worker_path), str(local), work_id], env=worker_env, capture_output=True, timeout=30)
                        self.assertEqual(result.returncode, expected, result.stderr.decode())
                        return result
                    worker("claim"); worker("result", stop_at, 79)
                    state = local / work_id
                    saved = {path.name: path.read_bytes() for path in state.iterdir()}
                    messages_before = reader._request("/api/stats")["stats"]["simulation_messages"]
                    process.terminate(); process.wait(timeout=5)
                    rotated = subprocess.run([binary, "recover-generation", "--offline-confirmed"], env=env, capture_output=True, timeout=10)
                    self.assertEqual(rotated.returncode, 0, rotated.stderr.decode())
                    process = start()
                    self.assertNotEqual(reader._request("/api/changes?after=-1")["generation"], generation)
                    worker("retry-result", expected=1)
                    self.assertEqual({path.name: path.read_bytes() for path in state.iterdir()}, saved)
                    self.assertEqual(reader._request("/api/work/" + work_id)["data"]["work"]["state"], "recovery_required")
                    self.assertEqual(reader._request("/api/stats")["stats"]["simulation_messages"], messages_before)
                    if stop_at.endswith("blob"):
                        self.assertFalse((state / "post.json").exists())
                    elif stop_at == "after_prepare_submit":
                        command = json.loads(json.loads((state / "submit.json").read_bytes())["body"])
                        self.assertEqual(json.loads(command["data"])["generation"], generation)
                    history = reader._request("/api/work/" + work_id + "/history")["data"]["transitions"]
                    self.assertEqual([item["operation"] for item in history], ["work.create", "work.claim"])
                finally:
                    if process is not None and process.poll() is None:
                        process.terminate()
                        try: process.wait(timeout=5)
                        except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=5)

    def test_labeled_work_files_exact_retry_and_acceptance(self):
        with tempfile.TemporaryDirectory(prefix="swarmmemo-work-simulation-") as directory:
            local = Path(directory)
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0)); port = reservation.getsockname()[1]
            origin = "http://127.0.0.1:" + str(port)
            env = {**os.environ, "DATA_DIR": str(local / "server"), "LISTEN_ADDR": "127.0.0.1:" + str(port), "ALLOW_INSECURE_LOCAL": "true"}
            process = subprocess.Popen([os.environ["SWARMMEMO_TEST_BINARY"], "serve"], env=env,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                for _ in range(100):
                    try:
                        with urllib.request.urlopen(origin + "/health", timeout=1): break
                    except OSError:
                        if process.poll() is not None: self.fail("fixture server exited")
                        time.sleep(.05)
                else: self.fail("fixture server startup failed")
                requester_path, worker_path = local / "requester.json", local / "worker.json"
                requester_info = memo.keygen(requester_path); memo.keygen(worker_path)
                requester = memo.Client(origin, memo.load_key(requester_path))
                reader = memo.Client(origin)
                generation = reader._request("/api/changes?after=-1")["generation"]
                requester.command("agent.register", handle="sim-python-requester")
                root = requester.post("coordination-lab", "main", "Operator-run simulation: count the UTF-8 bytes and SHA-256 of this exact fixture. No payment, customer demand or independent adoption implied.\nHello, swarms — café 🌍\n", kind="simulation")["receipt"]["id"]
                create = {"operation": "work.create", "message_id": root, "ttl": 3600,
                          "data": json.dumps({"schema": 1, "generation": generation, "title": "Simulation: cross-runtime UTF-8 accounting", "capabilities": ["utf8", "protocol-testing"]})}
                queue = outbox.Outbox(local / "requester-outbox.sqlite", origin, requester_info["public_key"])
                queue.enqueue("create-simulation", create)
                self.assertEqual(queue.flush(requester)[0]["state"], "acknowledged")
                self.assertEqual(reader._request("/api/works")["data"]["works"], [])
                self.assertEqual(len(reader._request("/api/works?room=coordination-lab")["data"]["works"]), 1)

                def worker(action, failpoint=None, expected_exit=0, key_path=worker_path):
                    worker_env = dict(os.environ)
                    worker_env.pop("SWARMMEMO_LAB_FAILPOINT", None)
                    if failpoint: worker_env["SWARMMEMO_LAB_FAILPOINT"] = failpoint
                    result = subprocess.run(["node", str(ROOT / "examples/coordination-lab/worker.mjs"), action, origin,
                                             str(key_path), str(local), root], capture_output=True, timeout=30, env=worker_env)
                    self.assertEqual(result.returncode, expected_exit, result.stderr.decode())
                    return json.loads(result.stdout) if result.returncode == 0 else None
                state = local / root
                worker("claim", "after_accept_claim", 79)  # Accepted remotely; process dies before recording its response.
                envelope = (state / "claim.json").read_bytes()
                claim = worker("retry-claim")
                retried = worker("retry-claim")  # A fresh process reads the original durable command.
                self.assertEqual(claim, retried)
                self.assertEqual(envelope, (state / "claim.json").read_bytes())
                self.assertEqual(len(reader._request("/api/work/" + root + "/history")["data"]["transitions"]), 2)
                worker("retry-result", expected_exit=1)  # No intent means no implicit fresh computation.
                saved_envelopes = {"claim": envelope}
                for step in ("blob", "post", "submit"):
                    for position in ("after_prepare", "after_accept", "after_record"):
                        action = "result" if (step, position) == ("blob", "after_prepare") else "retry-result"
                        worker(action, position + "_" + step, 79)
                        saved_envelopes.setdefault(step, (state / (step + ".json")).read_bytes())
                        for name, expected in saved_envelopes.items():
                            self.assertEqual((state / (name + ".json")).read_bytes(), expected)
                result = worker("retry-result")
                self.assertEqual(result["acknowledgement_scope"], "historical_acceptance_not_current_claim")
                worker("result", expected_exit=1)  # Existing intent requires explicit retry, never a second result.
                worker("retry-result", expected_exit=1, key_path=requester_path)
                fixture = "Hello, swarms — café 🌍\n".encode()
                expected = {"schema": 1, "simulated": True, "task": "utf8-accounting-v1", "utf8_bytes": len(fixture),
                            "sha256": hashlib.sha256(fixture).hexdigest(), "base64url": memo.b64(fixture)}
                self.assertEqual(result["evidence"], expected)
                downloaded = local / "evidence.json"
                reader.download(result["attachment_id"], downloaded)
                self.assertEqual(json.loads(downloaded.read_bytes()), expected)
                inbox = reader.messages(to=requester_info["id"])["messages"]
                self.assertEqual([e["id"] for e in inbox], [result["result_id"]])
                queue.enqueue("accept-verified-simulation", {"operation": "work.accept", "message_id": root,
                              "amount": claim["data"]["ack"]["fence"], "data": json.dumps({"schema": 1, "generation": generation})})
                self.assertEqual(queue.flush(requester)[0]["state"], "acknowledged")
                work = reader._request("/api/work/" + root)["data"]["work"]
                self.assertEqual(work["state"], "accepted"); self.assertTrue(work["simulated"])
                self.assertEqual(worker("retry-result"), result)  # Terminal accepted work needs no current claim.
                for name, expected in saved_envelopes.items():
                    self.assertEqual((state / (name + ".json")).read_bytes(), expected)
                history = reader._request("/api/work/" + root + "/history")["data"]["transitions"]
                self.assertEqual([t["operation"] for t in history], ["work.create", "work.claim", "work.submit", "work.accept"])
                for transition in history:
                    memo.crypto()[1].from_public_bytes(memo.unb64(transition["public_key"])).verify(
                        memo.unb64(transition["signature"]), transition["signed_payload"].encode())
                stats = reader._request("/api/stats")["stats"]
                self.assertEqual(stats["simulation_messages"], 2)
                self.assertEqual(stats["native_messages"], 0)
                self.assertEqual(stats["native_posting_agents"], 0)
                # Durable terminal evidence is offline-readable: no fresh GET or
                # mutation is needed merely to report previous acceptance.
                process.terminate(); process.wait(timeout=5)
                self.assertEqual(worker("retry-result"), result)
            finally:
                process.terminate()
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=5)


if __name__ == "__main__": unittest.main()
