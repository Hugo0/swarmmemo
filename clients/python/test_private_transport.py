"""Disposable private transport tests; never contact a public service."""
from copy import deepcopy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import inspect
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch
import urllib.request

import swarmmemo as memo
import swarmmemo_private_transport as p


def binding(key, origin="https://example.invalid"):
    return {"schema": 1, "type": "private-room-inbox", "origin": origin,
            "service_id": "swarmmemo.com", "room": "secret-a", "reader_public_key": memo.b64(memo.public_bytes(key)),
            "start_mode": "history", "storage": "metadata-only", "offline_bodies": "deny"}


def event(key, *, text="PRIVATE BODY CANARY café 🌍", visibility=None, attachments=None):
    command = {"operation": "post", "room": "secret-a", "text": text}
    if visibility is not None: command["visibility"] = visibility
    if attachments: command["attachments"] = [a["id"] for a in attachments]
    command = memo.sign(command, key)
    result = {"id": "event1", "sequence": 1, "room": "secret-a", "page": "main", "text": text, "kind": "note",
              "author": p.sha(memo.public_bytes(key)), "public_key": command["public_key"],
              "signature": command["signature"], "signed_payload": memo.canonical(command).decode(),
              "created_at": 1788566400, "sha256": p.sha(text.encode()), "hidden": False,
              "type": "message", "visibility": "private", "archive_eligible": False}
    if attachments: result["attachments"] = attachments
    return result


def tombstone(value):
    return {**value, "type": "tombstone", "hidden": True, "text": "", "signature": "", "signed_payload": "", "attachments": []}


class ValidationTests(unittest.TestCase):
    def setUp(self):
        self.key = memo.crypto()[0].generate()
        self.binding = binding(self.key)

    def test_explicit_and_legacy_private_signatures(self):
        for visibility in (None, "private"):
            value = event(self.key, visibility=visibility)
            self.assertIs(p.validate_private_event(value, self.binding), value)
        with self.assertRaisesRegex(p.PrivateInboxError, "signed_event_field_mismatch"):
            p.validate_private_event(event(self.key, visibility="public"), self.binding)

    def test_via_is_a_channel_token(self):
        for via in ("ui", "command", "c64"):
            value = {**event(self.key), "via": via}
            self.assertIs(p.validate_private_event(value, self.binding), value)
        for via in ("", "UI", "a" * 17, 1):
            with self.subTest(via=via), self.assertRaisesRegex(p.PrivateInboxError, "invalid_event_metadata"):
                p.validate_private_event({**event(self.key), "via": via}, self.binding)

    def test_private_restrictions_and_tampered_signature_projection(self):
        original = event(self.key)
        cases = [{"visibility": "public"}, {"archive_eligible": True}, {"room": "secret-b"},
                 {"author": "anonymous"}, {"delegation_id": "a" * 64}, {"text": "tamper"},
                 {"signature": memo.b64(b"x" * 64)}, {"public_key": memo.b64(b"x" * 32)},
                 {"to": "a" * 64}, {"kind": "request"}, {"sequence": True}, {"created_at": -1}]
        for update in cases:
            with self.subTest(update=list(update)):
                with self.assertRaises(p.PrivateInboxError): p.validate_private_event({**original, **update}, self.binding)
        wrong = {**self.binding, "service_id": "elsewhere.invalid"}
        with self.assertRaisesRegex(p.PrivateInboxError, "signature_service_mismatch"):
            p.validate_private_event(original, wrong)

    def test_metadata_minimization_and_attachment_identity(self):
        attachment = {"id": "blob1", "room": "secret-a", "filename": "PRIVATE FILENAME CANARY",
                      "media_type": "PRIVATE MEDIA CANARY", "sha256": "a" * 64, "size": 12,
                      "created_at": 1, "expires_at": 100, "deleted": False, "expired": False}
        value = event(self.key, attachments=[attachment])
        value.update(handle="PRIVATE HANDLE CANARY", reason="PRIVATE REASON CANARY")
        p.validate_private_event(value, self.binding)
        encoded = p.encode(p.metadata(value))
        for forbidden in (value["text"], value["signature"], value["signed_payload"], attachment["filename"],
                          attachment["media_type"], value["handle"], value["reason"]):
            self.assertNotIn(forbidden.encode(), encoded)
        self.assertEqual(p.digest(value), p.sha(encoded))
        changed = deepcopy(value); changed["attachments"][0]["deleted"] = True
        self.assertNotEqual(p.digest(value), p.digest(changed))
        self.assertEqual(p.immutable(value), p.immutable(changed))
        hidden = tombstone(value)
        p.validate_private_event(hidden, self.binding)
        self.assertNotIn("attachments", p.metadata(hidden))
        self.assertNotIn("attachments", p.immutable(hidden))

    def test_tombstone_never_has_body_or_proof(self):
        original = event(self.key)
        hidden = tombstone(original)
        p.validate_private_event(hidden, self.binding)
        for field in ("text", "signature", "signed_payload"):
            with self.subTest(field=field), self.assertRaisesRegex(p.PrivateInboxError, "tombstone_contains_payload"):
                p.validate_private_event({**hidden, field: original[field]}, self.binding)

    def test_binding_exact_origin_and_mode(self):
        p.binding_validated(self.binding)
        for change in ({"origin": "https://example.invalid?"}, {"origin": "https://example.invalid#"},
                       {"origin": "https://user@example.invalid"}, {"origin": "http://example.invalid"},
                       {"origin": "https://example.invalid/"}, {"room": ""}, {"room": ["secret-a"]},
                       {"reader_public_key": ""}, {"offline_bodies": "allow"}, {"storage": "plaintext"}, {"schema": True}):
            with self.subTest(change=change), self.assertRaises(p.PrivateInboxError):
                p.binding_validated({**self.binding, **change})

    def test_duplicate_fields_and_nonfinite(self):
        for raw in ('{"ok":true,"ok":false}', '{"number":NaN}', b'"\xff"'):
            with self.assertRaises(p.PrivateInboxError): p.strict_json(raw)


class WireTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="swarmmemo-private-wire-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.key_path = self.root / "reader.json"
        memo.keygen(self.key_path)
        self.key = memo.load_key(self.key_path)
        self.calls = []
        self.mode = "normal"
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def handle_read(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                fixture.calls.append((self.command, self.path, body))
                if fixture.mode == "stall":
                    time.sleep(2)
                    return
                if fixture.mode == "redirect":
                    self.send_response(307); self.send_header("Location", fixture.origin + "/unapproved")
                    self.send_header("Content-Type", "application/json"); self.end_headers()
                    self.wfile.write(b'{"error":{"code":"redirect"}}'); return
                if self.path == "/capabilities":
                    result = {"service_id": "swarmmemo.com", "private_reads": {"message_get_room_filter": True},
                              "public_corrections": {"message_read_generation": True}}
                else:
                    command = p.strict_json(body)
                    if command["operation"] == "room.get":
                        result = {"ok": True, "room": {"name": "secret-a", "visibility": "private"}}
                    else:
                        result = {"ok": True, "messages": [event(fixture.key)], "generation": "a" * 32, "next_cursor": "opaque"}
                raw = p.encode(result)
                if fixture.mode == "duplicate": raw = b'{"ok":true,"ok":false}'
                self.send_response(200); self.send_header("Content-Type", "application/json")
                if fixture.mode == "oversize": self.send_header("Content-Length", str(p.MAX_RESPONSE + 1))
                elif fixture.mode == "truncated": self.send_header("Content-Length", str(len(raw) + 8))
                else: self.send_header("Content-Length", str(len(raw)))
                if fixture.mode == "compressed": self.send_header("Content-Encoding", "gzip")
                if fixture.mode == "duplicate_length": self.send_header("Content-Length", "999")
                if fixture.mode == "transfer_with_length": self.send_header("Transfer-Encoding", "chunked")
                self.end_headers()
                try: self.wfile.write(raw)
                except (BrokenPipeError, ConnectionResetError): pass
            do_GET = handle_read
            do_POST = handle_read

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.origin = "http://127.0.0.1:" + str(self.server.server_port)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop_server)
        self.binding = binding(self.key, self.origin)

    def stop_server(self):
        self.server.shutdown(); self.server.server_close(); self.thread.join(timeout=3)

    def session(self, **kw): return p.ReadSession(self.binding, self.key_path, **kw)

    def test_fresh_scoped_signed_reads_without_proxy_or_bytecode(self):
        with patch.dict(os.environ, {"HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1",
                                    "ALL_PROXY": "http://127.0.0.1:1", "NO_PROXY": "", "no_proxy": ""}):
            session = self.session()
            session.capabilities(); session.room()
            first = session.event("event1"); second = session.event("event1")
        self.assertEqual(first["messages"][0]["text"], second["messages"][0]["text"])
        self.assertEqual(session.requests, 4)
        signed = []
        for method, path, body in self.calls:
            if method == "GET": self.assertEqual(path, "/capabilities"); continue
            self.assertEqual((method, path), ("POST", "/v1/command"))
            command = p.strict_json(body); signed.append(command)
            self.assertEqual(command["room"], "secret-a")
            self.assertNotIn("delegation", command)
            memo.crypto()[1].from_public_bytes(memo.public_bytes(self.key)).verify(memo.unb64(command["signature"]), memo.canonical(command))
        self.assertNotEqual(signed[-1]["nonce"], signed[-2]["nonce"])

    def test_response_errors_are_fixed_and_no_redirect_follow(self):
        for mode in ("redirect", "duplicate", "oversize", "compressed", "truncated", "duplicate_length", "transfer_with_length"):
            self.mode = mode; self.calls.clear()
            with self.subTest(mode=mode), self.assertRaises(p.PrivateInboxError): self.session().capabilities()
            self.assertEqual(len(self.calls), 1)
            self.assertEqual(self.calls[0][1], "/capabilities")

    def test_wrong_key_symlink_fifo_and_permissions(self):
        alternate = self.root / "other.json"; memo.keygen(alternate)
        for path in (alternate,):
            session = p.ReadSession(self.binding, path); session.capabilities()
            with self.assertRaisesRegex(p.PrivateInboxError, "reader_key_mismatch"): session.room()
        symlink = self.root / "linked"; symlink.symlink_to(self.key_path)
        with self.assertRaisesRegex(p.PrivateInboxError, "symlink_not_allowed"): p.ReadSession(self.binding, symlink)
        fifo = self.root / "fifo"; os.mkfifo(fifo, 0o600)
        session = p.ReadSession(self.binding, fifo); session.capabilities()
        with self.assertRaisesRegex(p.PrivateInboxError, "private_regular_file_required"): session.room()
        self.key_path.chmod(0o644)
        session = self.session(); session.capabilities()
        with self.assertRaisesRegex(p.PrivateInboxError, "private_regular_file_required"): session.room()

    def test_deadline_and_request_budget(self):
        session = self.session(max_requests=1); session.capabilities()
        with self.assertRaisesRegex(p.PrivateInboxError, "request_budget"): session.room()
        self.mode = "stall"
        start = time.monotonic()
        with self.assertRaisesRegex(p.PrivateInboxError, "deadline_exceeded"):
            self.session(deadline_seconds=0.7).capabilities()
        self.assertLess(time.monotonic() - start, 1.3)
        for deadline in (float("nan"), float("inf"), True, 0, 31):
            with self.assertRaises(p.PrivateInboxError): self.session(deadline_seconds=deadline)

    def test_worker_requires_actual_parent_before_accepting_key_path(self):
        result = subprocess.run([sys.executable, "-I", "-B", p.__file__, "_fetch", "1"],
                                input=b"PRIVATE KEY PATH SHOULD NEVER BE PARSED", capture_output=True, timeout=3)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout + result.stderr, b"")
        self.assertEqual(self.calls, [])

    def test_interrupt_at_spawn_assignment_reaps_owned_worker(self):
        original, spawned, pipes = subprocess.Popen, [], []
        def interrupt_after_spawn(*args, **kwargs):
            process = original(*args, **kwargs)
            spawned.append(process)
            pipes.append((process.stdin, process.stdout))
            # This fixture has an HTTP server thread. Target the caller so the
            # signal is pending at its masked spawn-assignment boundary, rather
            # than delivered to another thread and observed at an arbitrary point.
            signal.pthread_kill(threading.get_ident(), signal.SIGINT)
            return process
        with patch.object(p.subprocess, "Popen", interrupt_after_spawn):
            with self.assertRaises(KeyboardInterrupt): self.session().capabilities()
        self.assertEqual(len(spawned), 1)
        self.assertIsNotNone(spawned[0].poll())
        self.assertTrue(all(stream.closed for stream in pipes[0]))
        self.assertEqual(self.calls, [])

    def test_repeated_interrupts_during_cleanup_do_not_orphan(self):
        original, spawned = subprocess.Popen, []
        def instrument(*args, **kwargs):
            process = original(*args, **kwargs)
            spawned.append(process)
            kill = process.kill
            def interrupt_then_kill():
                os.kill(os.getpid(), signal.SIGINT)
                os.kill(os.getpid(), signal.SIGINT)
                kill()
            process.kill = interrupt_then_kill
            return process
        self.mode = "stall"
        with patch.object(p.subprocess, "Popen", instrument):
            with self.assertRaises(KeyboardInterrupt): self.session(deadline_seconds=0.7).capabilities()
        self.assertIsNotNone(spawned[0].poll())
        self.assertTrue(spawned[0].stdout.closed)

    def test_second_interrupt_at_cleanup_entry_is_deferred_until_reaped(self):
        original, spawned, pipes = subprocess.Popen, [], []
        lines, start = inspect.getsourcelines(p.ReadSession._fetch_owned)
        cleanup_line = [start + index for index, line in enumerate(lines)
                        if "oldmask = signal.pthread_sigmask(signal.SIG_BLOCK, signals)" in line][-1]
        injected = []

        def instrument(*args, **kwargs):
            process = original(*args, **kwargs)
            spawned.append(process)
            pipes.append((process.stdin, process.stdout))
            signal.pthread_kill(threading.get_ident(), signal.SIGINT)
            return process

        def trace(frame, action, _arg):
            if (frame.f_code is p.ReadSession._fetch_owned.__code__ and action == "line"
                    and frame.f_lineno == cleanup_line):
                injected.append(True)
                signal.pthread_kill(threading.get_ident(), signal.SIGINT)
            return trace

        old_trace = sys.gettrace()
        try:
            sys.settrace(trace)
            with patch.object(p.subprocess, "Popen", instrument):
                with self.assertRaises(KeyboardInterrupt): self.session().capabilities()
            self.assertEqual(injected, [True])
            self.assertIsNotNone(spawned[0].poll())
            # Assert the actual pipe objects, even if the implementation has
            # already closed stdin and deliberately replaced its attribute.
            self.assertTrue(all(stream.closed for stream in pipes[0]))
            self.assertEqual(self.calls, [])
        finally:
            sys.settrace(old_trace)
            for child in spawned:
                if child.poll() is None: child.kill()
                child.wait(timeout=2)
                for stream in (child.stdin, child.stdout):
                    if stream is not None and not stream.closed: stream.close()

    def test_custom_ignored_and_masked_signals_keep_caller_semantics(self):
        original, spawned, delivered = subprocess.Popen, [], []
        prior_handler = signal.getsignal(signal.SIGTERM)
        prior_mask = signal.pthread_sigmask(signal.SIG_BLOCK, [])

        def handler(number, _frame):
            delivered.append((number, spawned[-1].poll()))

        def instrument(*args, **kwargs):
            process = original(*args, **kwargs)
            spawned.append(process)
            # Target this thread: the fixture's HTTP thread has an independent
            # mask and could otherwise receive a process-directed blocked signal.
            signal.pthread_kill(threading.get_ident(), signal.SIGTERM)
            return process

        try:
            signal.signal(signal.SIGTERM, handler)
            with patch.object(p.subprocess, "Popen", instrument):
                with self.assertRaisesRegex(p.PrivateInboxError, "interrupted"):
                    self.session().capabilities()
            self.assertEqual(len(delivered), 1)
            self.assertIsNotNone(delivered[0][1])
            self.assertIs(signal.getsignal(signal.SIGTERM), handler)
            self.assertEqual(signal.pthread_sigmask(signal.SIG_BLOCK, []), prior_mask)

            delivered.clear()
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            with patch.object(p.subprocess, "Popen", instrument): self.session().capabilities()
            self.assertEqual(signal.getsignal(signal.SIGTERM), signal.SIG_IGN)

            signal.signal(signal.SIGTERM, handler)
            signal.pthread_sigmask(signal.SIG_BLOCK, [signal.SIGTERM])
            with patch.object(p.subprocess, "Popen", instrument): self.session().capabilities()
            self.assertEqual(delivered, [])
            self.assertIn(signal.SIGTERM, signal.pthread_sigmask(signal.SIG_BLOCK, []))
            signal.pthread_sigmask(signal.SIG_SETMASK, prior_mask)
            self.assertEqual(len(delivered), 1)
            self.assertIsNotNone(delivered[0][1])
        finally:
            signal.signal(signal.SIGTERM, prior_handler)
            signal.pthread_sigmask(signal.SIG_SETMASK, prior_mask)
            for child in spawned:
                if child.poll() is None: child.kill()
                child.wait(timeout=2)
                for stream in (child.stdin, child.stdout):
                    if stream is not None and not stream.closed: stream.close()

    def test_network_sessions_require_main_thread_before_spawn(self):
        existing = self.session()
        errors = []
        def attempt():
            for action in (self.session, existing.capabilities):
                try: action()
                except p.PrivateInboxError as error: errors.append(error.code)
        with patch.object(p.subprocess, "Popen") as spawn:
            thread = threading.Thread(target=attempt)
            thread.start(); thread.join(timeout=2)
            self.assertFalse(thread.is_alive())
            spawn.assert_not_called()
        self.assertEqual(errors, ["worker_main_thread_required"] * 2)
        self.assertEqual(self.calls, [])

    def test_signal_during_handler_restoration_restores_remaining_handlers(self):
        numbers = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)
        originals = {number: signal.getsignal(number) for number in numbers}
        original_mask = signal.pthread_sigmask(signal.SIG_BLOCK, [])
        real_signal, real_spawn = signal.signal, subprocess.Popen
        spawned, interrupted = [], []

        def custom_term(_number, _frame): pass

        def instrument_spawn(*args, **kwargs):
            child = real_spawn(*args, **kwargs)
            spawned.append(child)
            return child

        def instrument_restore(number, handler):
            result = real_signal(number, handler)
            if number == signal.SIGINT and handler is originals[number] and not interrupted:
                interrupted.append(True)
                # The main thread is masked here. A real signal delivered to
                # another thread still schedules its Python handler on main.
                signal.pthread_kill(self.thread.ident, signal.SIGINT)
                time.sleep(0.02)
            return result

        try:
            real_signal(signal.SIGTERM, custom_term)
            real_signal(signal.SIGHUP, signal.SIG_IGN)
            expected = {number: signal.getsignal(number) for number in numbers}
            with patch.object(p.signal, "signal", instrument_restore), patch.object(p.subprocess, "Popen", instrument_spawn):
                with self.assertRaises(KeyboardInterrupt): self.session().capabilities()
            self.assertEqual(interrupted, [True])
            for number in numbers:
                self.assertEqual(signal.getsignal(number), expected[number])
            self.assertEqual(signal.pthread_sigmask(signal.SIG_BLOCK, []), original_mask)
            self.assertEqual(len(spawned), 1)
            self.assertIsNotNone(spawned[0].poll())
            self.assertTrue(spawned[0].stdout.closed)
        finally:
            for number in numbers: real_signal(number, originals[number])
            signal.pthread_sigmask(signal.SIG_SETMASK, original_mask)
            for child in spawned:
                if child.poll() is None: child.kill()
                child.wait(timeout=2)
                for stream in (child.stdin, child.stdout):
                    if stream is not None and not stream.closed: stream.close()

    def test_disappeared_private_key_directory_has_fixed_error(self):
        directory = self.root / "key-directory"
        directory.mkdir(mode=0o700)
        session = p.ReadSession(self.binding, directory / "not-loaded.json")
        session.capabilities()
        directory.rmdir()
        with self.assertRaisesRegex(p.PrivateInboxError, "^private_path_unavailable$"):
            session.room()

    def test_parent_sigkill_stops_signing_worker(self):
        # The killed caller's child may briefly be an adopted, nonexecuting
        # zombie. PDEATHSIG guarantees no surviving signer, not init's reaping.
        code = '''import json, os, subprocess, sys
sys.path.insert(0, sys.argv[1])
import swarmmemo_private_transport as p
original = subprocess.Popen
def spawn(*args, **kwargs):
    child = original(*args, **kwargs)
    print(child.pid, flush=True)
    return child
p.subprocess.Popen = spawn
session = p.ReadSession(json.loads(sys.argv[2]), sys.argv[3])
session.capabilities()
'''
        self.mode = "stall"
        parent = subprocess.Popen([sys.executable, "-I", "-B", "-c", code, str(Path(p.__file__).parent),
                                   json.dumps(self.binding), str(self.key_path)],
                                  stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        try:
            import select
            self.assertTrue(select.select([parent.stdout], [], [], 3)[0])
            child = int(parent.stdout.readline())
            for _ in range(100):
                if self.calls: break
                time.sleep(0.01)
            self.assertTrue(self.calls)
            parent.kill(); parent.wait(timeout=3)
            for _ in range(100):
                status = Path(f"/proc/{child}/stat")
                if not status.exists() or status.read_text().split(")", 1)[1].strip().startswith("Z "): break
                time.sleep(0.01)
            else:
                os.kill(child, signal.SIGKILL)
                self.fail("signing worker survived parent SIGKILL")
        finally:
            if parent.poll() is None: parent.kill(); parent.wait(timeout=3)
            parent.stdout.close()


@unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY"), "requires explicit disposable Go binary")
class ActualGoTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="swarmmemo-private-go-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.data = self.root / "data"; self.data.mkdir(mode=0o700)
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0)); self.port = s.getsockname()[1]
        self.origin = "http://127.0.0.1:" + str(self.port)
        self.env = {**os.environ, "DATA_DIR": str(self.data), "LISTEN_ADDR": "127.0.0.1:" + str(self.port),
                    "PUBLIC_URL": self.origin, "ALLOW_INSECURE_LOCAL": "true"}
        self.server = subprocess.Popen([os.environ["SWARMMEMO_TEST_BINARY"], "serve"], env=self.env,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(self.stop)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        for _ in range(100):
            try:
                with opener.open(self.origin + "/health", timeout=1): break
            except OSError: time.sleep(0.02)
        else: self.fail("Go fixture did not start")
        self.key_path = self.root / "reader.json"; reader = memo.keygen(self.key_path)
        self.key = memo.load_key(self.key_path)
        self.reader = memo.Client(self.origin, self.key)
        self.reader.command("agent.register")
        self.owner = memo.Client(self.origin, memo.crypto()[0].generate())
        self.reader_id = reader["id"]
        self.binding = binding(self.key, self.origin)
        for room in ("secret-a", "secret-b"):
            self.owner.command("room.create", room=room, visibility="private", members=[self.reader_id])
        self.a = self.owner.command("post", room="secret-a", text="ACTUAL PRIVATE A CANARY")["receipt"]["id"]
        self.b = self.owner.command("post", room="secret-b", text="ACTUAL PRIVATE B CANARY")["receipt"]["id"]

    def stop(self):
        self.server.terminate()
        try: self.server.wait(timeout=3)
        except subprocess.TimeoutExpired: self.server.kill(); self.server.wait(timeout=3)

    def session(self):
        result = p.ReadSession(self.binding, self.key_path)
        result.capabilities()
        return result

    def test_actual_scope_generation_membership_and_tombstone(self):
        session = self.session(); session.room()
        page = session.messages("start")
        self.assertEqual([e["id"] for e in page["messages"]], [self.a])
        self.assertEqual(session.event(self.a)["generation"], page["generation"])
        self.assertIsNone(session.event(self.b))
        self.owner.command("room.member.remove", room="secret-a", target=self.reader_id)
        with self.assertRaisesRegex(p.PrivateInboxError, "scope_unavailable"): self.session().room()
        self.owner.command("room.member.add", room="secret-a", target=self.reader_id)
        result = subprocess.run([os.environ["SWARMMEMO_TEST_BINARY"], "moderate", self.a, "hide", "private fixture"],
                                env=self.env, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0)
        hidden = self.session().event(self.a)["messages"][0]
        self.assertEqual(hidden["type"], "tombstone")
        self.assertEqual(hidden["text"], "")


if __name__ == "__main__": unittest.main()
