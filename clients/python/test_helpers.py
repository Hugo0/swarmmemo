"""The client helpers agents use most (C56): work, updates and the journal, shared
docs, tools and /call. Each sends the documented command, signed with a key."""
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
import urllib.error
import urllib.request
from unittest.mock import patch

import swarmmemo as memo

GENERATION, MESSAGE, AGENT, SUBJECT = "g" * 32, "m" * 26, "a" * 64, "s" * 26


class Recorder:
    """Stands in for Client._request: records each request and answers like the board."""
    def __init__(self):
        self.sent = []

    def __call__(self, client, path, body=None):
        self.sent.append((path, body))
        operation = (body or {}).get("operation")
        if operation == "work.get":
            return {"ok": True, "data": {"work": {"id": MESSAGE, "service_generation": GENERATION, "fence": 3, "worker_subject": "agent:" + AGENT}}}
        if operation == "agent.get":
            return {"ok": True, "agent": {"id": AGENT}}
        if operation == "updates.get":
            return {"ok": True, "messages": [], "next_cursor": (body.get("cursor") or "c") + "1", "data": {}}
        if operation == "subject.resolve":
            return {"ok": True, "data": {"subject": {"id": SUBJECT, "uri": "site:https://example.org/", "exists": False}}}
        return {"ok": True}


class HelperTests(unittest.TestCase):
    def setUp(self):
        self.key = memo.crypto()[0].from_private_bytes(bytes(range(32)))
        self.me = hashlib.sha256(memo.public_bytes(self.key)).hexdigest()
        self.recorder = Recorder()
        patcher = patch.object(memo.Client, "_request", lambda client, path, body=None: self.recorder(client, path, body))
        patcher.start(); self.addCleanup(patcher.stop)
        self.signed, self.anonymous = memo.Client("https://example.org", self.key), memo.Client("https://example.org")

    def test_base_url_accepts_only_http_origins(self):
        # A non-root pathname is not an origin. Accepting one silently turns
        # /v1/command into /docs/v1/command (vm2-defi-auditor, PR #8).
        for origin in ("https://example.org", "https://example.org/", "http://localhost:8080/"):
            with self.subTest(origin=origin):
                self.assertEqual(memo.Client(origin).base_url, origin.rstrip("/"))
        for nested in ("https://example.org/docs", "https://example.org/docs/", "https://example.org/api/v1", "http://localhost:8080/sandbox"):
            with self.subTest(nested=nested):
                with self.assertRaisesRegex(ValueError, "origin"):
                    memo.Client(nested)

    def test_docs_read_by_hash(self):
        # docs.read takes id or hash, like the JS client (arenaforge-lab, #bounties 10-17).
        sent = []
        client = memo.Client()
        client.service_call = lambda service, method, args, max_cost=None, request_id=None: sent.append((method, args)) or {"ok": True}
        client.docs_read(None, hash="a" * 64)
        client.docs_read("d" * 26, version=2)
        self.assertEqual(sent, [("read", {"hash": "a" * 64}), ("read", {"id": "d" * 26, "version": 2})])
        for bad in ({}, {"doc_id": "d" * 26, "hash": "a" * 64}):
            with self.subTest(args=bad), self.assertRaisesRegex(ValueError, "by id or by hash"):
                client.docs_read(**bad)

    def test_scoped_worker_carries_work_contract_fields(self):
        # The grant binds schema and generation; payout_address and result_sha256
        # may ride along on a delegated claim or submit (citepulse-ai, 10-17).
        g = "a" * 32
        grant = hashlib.sha256(memo.public_bytes(self.key)).hexdigest()
        worker = memo.DelegatedClient("https://example.org", self.key, grant_id=grant, generation=g, room="lab", operations=["work.claim"])
        claim = lambda data: worker.prepare("work.claim", message_id="b" * 32, ttl=60, data=json.dumps(data))
        claim({"schema": 1, "generation": g, "result_sha256": "d" * 64})
        for bad in ({"schema": 1, "generation": "c" * 32, "payout_address": "0x" + "1" * 40}, {"schema": 1, "generation": g, "other": 1}):
            with self.subTest(data=bad), self.assertRaisesRegex(ValueError, "delegation_generation_mismatch"):
                claim(bad)
        # C380: payout_address only when the grant pins it, and then exactly that address
        # (any case); the server enforces the same.
        with self.assertRaisesRegex(ValueError, "delegation_payout_unpinned"):
            claim({"schema": 1, "generation": g, "payout_address": "0x" + "1" * 40})
        payee = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"  # a valid EIP-55 address (C484)
        pinned = memo.DelegatedClient("https://example.org", self.key, grant_id=grant, generation=g, room="lab",
                                      operations=["work.claim", "work.submit"], payout_address=payee)
        for op in ("work.claim", "work.submit"):
            pinned.prepare(op, message_id="b" * 32, ttl=60, data=json.dumps({"schema": 1, "generation": g, "payout_address": payee.lower()}))
            pinned.prepare(op, message_id="b" * 32, ttl=60, data=json.dumps({"schema": 1, "generation": g}))
            with self.assertRaisesRegex(ValueError, "delegation_payout_mismatch"):
                pinned.prepare(op, message_id="b" * 32, ttl=60, data=json.dumps({"schema": 1, "generation": g, "payout_address": "0x" + "cd" * 20}))
        for bad in ("0x" + "1" * 39, "1" * 42, 7):
            with self.subTest(pin=bad), self.assertRaisesRegex(ValueError, "invalid_payout_address"):
                memo.DelegatedClient("https://example.org", self.key, grant_id=grant, generation=g, room="lab", operations=["work.claim"], payout_address=bad)

    def test_enrollment_pins_payout_address(self):
        # C380: the parent pins the worker's payout address in the signed grant data.
        parent = memo.Client("https://example.org", memo.crypto()[0].generate())
        enrollment = parent.prepare_enrollment(self.key, room="lab", ttl=60, amount=1, generation="a" * 32, operations=["work.claim"],
                                               payout_address="0x" + "ab" * 20)
        self.assertEqual(json.loads(enrollment["data"])["payout_address"], "0x" + "ab" * 20)
        self.assertNotIn("payout_address", json.loads(parent.prepare_enrollment(self.key, room="lab", ttl=60, amount=1, generation="a" * 32, operations=["work.claim"])["data"]))
        with self.assertRaisesRegex(ValueError, "invalid_payout_address"):
            parent.prepare_enrollment(self.key, room="lab", ttl=60, amount=1, generation="a" * 32, operations=["work.claim"], payout_address="0x12")

    def test_notary_url_shorthand_after_root_options(self):
        # `notary URL` expands after root options too (alex-vale, #bounties 10-17).
        for argv in (["notary", "https://example.com", "--help"],
                     ["--key", "missing.json", "notary", "https://example.com", "--help"],
                     ["--url=https://swarmmemo.com", "--home", "h", "notary", "https://example.com", "--help"]):
            with self.subTest(argv=argv):
                out = io.StringIO()
                with contextlib.redirect_stdout(out), self.assertRaises(SystemExit) as stop:
                    memo.main(argv)
                self.assertEqual(stop.exception.code, 0)
                self.assertIn("stamp-fetch", out.getvalue())

    def test_anonymous_messages_keep_an_explicit_zero(self):
        # 0 == False in Python: bias=0 (top ordering) was dropped from the GET
        # and the server read the default decay (chatgpt-aab, #bounties 10-17).
        client = memo.Client()
        client._request = lambda path, body=None: path
        for value, shown in ((0, "bias=0"), (0.0, "bias=0.0"), ("0", "bias=0"), (1.5, "bias=1.5")):
            with self.subTest(value=value):
                self.assertIn(shown, client.messages(sort="hot", bias=value))
        self.assertNotIn("bias", client.messages(sort="hot"))

    def test_unsigned_http_remote_origin_rejected_before_request(self):
        # Anonymous reads over plaintext HTTP can be observed or rewritten in
        # transit, so unsigned clients need HTTPS too (vm2, PR #10).
        for host in ("http://example.org", "http://example.org:8080"):
            with self.subTest(host=host):
                with self.assertRaisesRegex(memo.OriginError, "needs HTTPS"):
                    memo.Client(host)
        for host in ("http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"):
            with self.subTest(host=host):
                self.assertEqual(memo.Client(host).base_url, host)

    def test_one_transport_rule_plain_http_only_to_loopback(self):
        # C454: one rule for Python, JS and the MCP bridge (test_client.mjs, test_policy.py):
        # plain http:// only to a loopback host, signed or not, with no flag.
        key = memo.crypto()[0].generate()
        signed = memo.sign({"operation": "messages.list"}, key)
        for host in ("http://localhost", "http://localhost:8080", "http://127.0.0.1:8080", "http://127.0.0.2",
                     "http://127.255.255.254:9", "http://[::1]:8080", "http://[::ffff:7f00:1]", "http://[::ffff:127.0.0.1]"):
            with self.subTest(host=host):
                client = memo.Client(host, key=key)
                with patch.object(memo.Client, "_request", return_value={"ok": True}):
                    self.assertEqual(client.send(dict(signed), save_request=False), {"ok": True})
        for host in ("http://localhost.evil.com", "http://127.0.0.1.nip.io", "http://evil.com", "http://128.0.0.1",
                     "http://10.0.0.1", "http://0.0.0.0", "http://[::2]", "http://[::ffff:808:808]", "http://[::]",
                     "http://localhost.:8080", "http://127.1", "http://2130706433", "http://0x7f.0.0.1",
                     "http://[::1%25eth0]", "ftp://127.0.0.1"):
            with self.subTest(host=host):
                with self.assertRaisesRegex(memo.OriginError, "HTTP"):
                    memo.Client(host, key=key)
        for host in ("http://127.0.0.1@evil.com", "http://evil.com@127.0.0.1", "http://u:p@localhost"):
            with self.subTest(host=host), self.assertRaisesRegex(memo.OriginError, "credentials"):
                memo.Client(host, key=key)
        # A signed send rechecks the origin, naming HTTPS.
        client = memo.Client("https://example.org", key=key)
        client.base_url = "http://localhost.evil.com"
        with patch.object(memo.Client, "_request", side_effect=AssertionError("must not send")), \
                self.assertRaisesRegex(ValueError, "require HTTPS"):
            client.send(dict(signed), save_request=False)
        for host in ("localhost", "LOCALHOST", "127.0.0.1", "127.9.9.9", "::1", "::ffff:127.0.0.1", "::ffff:7f00:1"):
            self.assertTrue(memo.is_loopback_host(host), host)
        for host in ("", None, 127, "localhost.evil.com", "evil.localhost.com", "127.0.0.1.nip.io", "nip.io",
                     "128.0.0.1", "::2", "::ffff:8.8.8.8", "::1%eth0", "127.1", "2130706433", "[::1]"):
            self.assertFalse(memo.is_loopback_host(host), repr(host))

    def shown(self, body):
        """The command without its signature envelope, after checking that envelope."""
        body = dict(body)
        if "signature" in body:
            signature = body.pop("signature")
            self.assertEqual(body["public_key"], memo.b64(memo.public_bytes(self.key)))
            self.key.public_key().verify(memo.unb64(signature), memo.canonical(body))
        for field in ("public_key", "timestamp", "nonce"):
            body.pop(field, None)
        return body

    def last(self, signed=True):
        path, body = self.recorder.sent.pop()
        self.assertEqual(path, "/v1/command")
        self.assertEqual("signature" in body, signed)
        return self.shown(body)

    def test_work_mutations_save_final_request_after_generation_lookup(self):
        mutations = [
            ("work.claim", lambda c: c.work_claim(MESSAGE)),
            ("work.submit", lambda c: c.work_submit(MESSAGE, 3, MESSAGE)),
            ("work.accept", lambda c: c.work_accept(MESSAGE, 3)),
            ("work.reject", lambda c: c.work_reject(MESSAGE, 3, "needs revision")),
        ]
        for key in (None, self.key):
            for operation, invoke in mutations:
                with self.subTest(operation=operation, signed=key is not None), tempfile.TemporaryDirectory() as directory:
                    path = Path(directory) / "request.json"
                    client = memo.Client("https://example.org", key, save_request=path)
                    self.recorder.sent.clear()
                    invoke(client)
                    self.assertEqual([body["operation"] for _, body in self.recorder.sent], ["work.get", operation])
                    saved = json.loads(path.read_text())
                    self.assertEqual(saved, self.recorder.sent[-1][1])
                    self.assertEqual(self.shown(saved)["operation"], operation)
                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                    original = path.read_bytes()
                    self.recorder.sent.clear()
                    with self.assertRaises(FileExistsError):
                        invoke(client)
                    self.assertEqual(path.read_bytes(), original)
                    self.assertEqual([body["operation"] for _, body in self.recorder.sent], ["work.get"])

    def test_reviewer_handle_lookup_keeps_save_request_for_the_mutation(self):
        # 037f99127e54 d56c12fd (C337): resolving a reviewer handle is a
        # prerequisite agent.get; it must not consume the one-shot request file.
        mutations = [
            ("work.reviewer.set", lambda c, r: c.work_set_reviewer(MESSAGE, r, generation=GENERATION, request_id="c" * 32)),
            ("work.create", lambda c, r: c.work_create(MESSAGE, title="Test task", reviewer=r, generation=GENERATION, request_id="c" * 32)),
        ]
        for operation, invoke in mutations:
            for reviewer in ("reviewer", AGENT):
                with self.subTest(operation=operation, reviewer=reviewer), tempfile.TemporaryDirectory() as directory:
                    path = Path(directory) / "request.json"
                    client = memo.Client("https://example.org", self.key, save_request=path)
                    self.recorder.sent.clear()
                    invoke(client, reviewer)
                    lookups = [] if reviewer == AGENT else ["agent.get"]
                    self.assertEqual([body["operation"] for _, body in self.recorder.sent], lookups + [operation])
                    saved = json.loads(path.read_text())
                    self.assertEqual(saved, self.recorder.sent[-1][1])
                    self.assertEqual(json.loads(self.shown(saved)["data"])["reviewer"], AGENT)
                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                    self.recorder.sent.clear()
                    with self.assertRaises(FileExistsError):
                        invoke(client, reviewer)
                    self.assertEqual(saved, json.loads(path.read_text()))
                    self.assertEqual([body["operation"] for _, body in self.recorder.sent], lookups)

    def test_signed_messages_list_carries_sort_options_as_data(self):
        # C260: sort, bias, offset, scope and weights are no command fields
        # (canonical refuses them); a signed messages.list carries them as data.
        result = self.signed.messages(sort="new", scope="all")
        self.assertEqual(result, {"ok": True})
        sent = self.last()
        self.assertEqual(sent["operation"], "messages.list")
        self.assertEqual(json.loads(sent["data"]), {"sort": "new", "scope": "all"})
        self.assertFalse({"sort", "scope", "bias", "offset", "weights"} & sent.keys())
        self.signed.messages("lab", sort="top", bias="1.5", offset="20", weights=1)
        sent = self.last()
        self.assertEqual((sent["room"], json.loads(sent["data"])), ("lab", {"sort": "top", "bias": 1.5, "offset": 20, "weights": True}))
        self.signed.messages("lab")
        self.assertNotIn("data", self.last())
        with self.assertRaises(ValueError):
            self.signed.messages(sort="new", data="{}")
        # Unsigned reads keep them as query parameters the board converts.
        self.anonymous.messages(sort="new", scope="all")
        path, body = self.recorder.sent.pop()
        self.assertIsNone(body)
        self.assertIn("sort=new", path); self.assertIn("scope=all", path)

    def test_explicit_work_read_still_saves_request(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "read.json"
            client = memo.Client("https://example.org", self.key, save_request=path)
            client.work(MESSAGE)
            self.assertEqual(json.loads(path.read_text()), self.recorder.sent[-1][1])
            self.assertEqual(json.loads(path.read_text())["operation"], "work.get")

    def test_follow_explicit_cursor_overrides_file_and_saves_after_handling(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "cursor.json"
            memo.write_private(path, {"cursor": "old"})
            pages = self.signed.follow_updates(path, cursor="explicit")
            next(pages)
            self.assertEqual(self.last()["cursor"], "explicit")
            self.assertEqual(memo.read_private(path, {})["cursor"], "old")
            next(pages)
            self.assertEqual(self.last()["cursor"], "explicit1")
            self.assertEqual(memo.read_private(path, {})["cursor"], "explicit1")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            pages.close()

    def test_cli_memory_list_sends_after(self):
        sent = []
        with patch.object(memo.Client, "service_read", lambda client, service, method, data: sent.append((service, method, data)) or {"ok": True}), \
                contextlib.redirect_stdout(io.StringIO()):
            memo.main(["--url", "http://127.0.0.1:9", "memory", "list", "--cursor", "page2"])
        self.assertEqual(sent, [("memory", "list", {"after": "page2"})])

    def memory_put(self, *argv, stdin=""):
        sent = []
        def call(client, service, method, data, cost, request_id):
            sent.append((service, method, data, cost))
            return {"ok": True}
        err = io.StringIO()
        with patch.object(memo.Client, "service_call", call), patch("sys.stdin", io.StringIO(stdin)), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err):
            code = memo.main(["--url", "http://127.0.0.1:9", "memory", "put", *argv])
        return code, sent, err.getvalue()

    def test_cli_memory_put_dash_reads_stdin(self):
        value = "notes from the last session\nsecond line\n"
        code, sent, _ = self.memory_put("notes", "-", stdin=value)
        self.assertEqual(code, 0)
        self.assertEqual(sent, [("memory", "put", {"key": "notes", "value": value, "visibility": "private"},
                                 memo.memory_put_price("notes", value))])
        code, sent, _ = self.memory_put("notes", "literal", stdin="ignored")
        self.assertEqual(sent[0][2]["value"], "literal")

    def test_cli_memory_put_dash_empty_stdin_stores_nothing(self):
        code, sent, err = self.memory_put("notes", "-", stdin="")
        self.assertEqual((code, sent), (1, []))
        self.assertIn("stdin was empty", err)

    def test_cli_follow_passes_explicit_cursor(self):
        for follow in (False, True):
            with self.subTest(follow=follow):
                args = memo.build_parser().parse_args(
                    ["updates", "--agent", AGENT, "--cursor", "resume"]
                    + (["--follow"] if follow else []))
                sent = []
                def request(client, path, body=None):
                    if sent:
                        raise KeyboardInterrupt
                    sent.append(body)
                    return {"ok": True, "messages": [], "next_cursor": "next"}
                with patch.object(memo.Client, "_request", request), contextlib.redirect_stdout(io.StringIO()):
                    if follow:
                        with self.assertRaises(memo.ChatStop):
                            memo.run_helper(args, self.anonymous)
                    else:
                        memo.run_helper(args, self.anonymous)
                self.assertEqual(sent[0]["cursor"], "resume")

    def test_work_helpers(self):
        data = lambda extra="": f'{{"schema":1,"generation":"{GENERATION}"{extra}}}'
        self.anonymous.works(kind="rewarded", limit=5)
        self.assertEqual(self.last(False), {"operation": "works.list", "kind": "rewarded", "limit": 5})
        self.signed.works(kind="earn", eligible_for=AGENT)
        self.assertEqual(self.last(), {"operation": "works.list", "kind": "earn", "data": f'{{"schema":1,"eligible_for":"{AGENT}"}}'})
        self.anonymous.works(standing=MESSAGE)
        self.assertEqual(self.last(False), {"operation": "works.list", "data": f'{{"schema":1,"standing":"{MESSAGE}"}}'})
        self.anonymous.work(MESSAGE, agent=AGENT)
        self.assertEqual(self.last(False), {"operation": "work.get", "message_id": MESSAGE, "target": AGENT})
        self.signed.work_claim(MESSAGE, generation=GENERATION, request_id="c-1")
        self.assertEqual(self.last(), {"operation": "work.claim", "message_id": MESSAGE, "ttl": 3600, "data": data(), "request_id": "c-1"})
        self.signed.work_claim(MESSAGE, "r1", generation=GENERATION, result_sha256="f" * 64, request_id="c-2")
        self.assertEqual(self.last(), {"operation": "work.claim", "message_id": MESSAGE, "target": "r1", "request_id": "c-2",
                                       "data": data(f',"result_sha256":"{"f" * 64}"')})
        self.signed.work_submit(MESSAGE, 3, "r1", GENERATION, request_id="s-1")
        self.assertEqual(self.last(), {"operation": "work.submit", "message_id": MESSAGE, "amount": 3, "target": "r1", "data": data(), "request_id": "s-1"})
        self.signed.work_reject(MESSAGE, 3, "Breaks the build.", GENERATION, request_id="j-1")
        self.assertEqual(self.last(), {"operation": "work.reject", "message_id": MESSAGE, "amount": 3, "reason": "Breaks the build.",
                                       "data": data(), "request_id": "j-1"})
        # A verdict may sign what it checked, per property (C97).
        checks = [{"property": "integrity", "state": "pass", "subject_sha256": "a" * 64}, {"property": "conformance", "state": "not_checked"}]
        self.signed.work_reject(MESSAGE, 3, "Fails conformance.", GENERATION, request_id="j-2", checks=checks[1:])
        self.assertEqual(self.last()["data"], data(',"checks":[{"property":"conformance","state":"not_checked"}]'))
        self.signed.work_accept(MESSAGE, 3, GENERATION, "f" * 64, request_id="a-0", checks=checks)
        self.assertEqual(self.last(), {"operation": "work.accept", "message_id": MESSAGE, "amount": 3, "request_id": "a-0",
                                       "data": data(f',"result_sha256":"{"f" * 64}","checks":{json.dumps(checks, separators=(",", ":"))}')})
        # Left out, the generation is read once with work.get, then signed into the transition.
        self.signed.work_accept(MESSAGE, 3, request_id="a-1")
        self.assertEqual(self.last(), {"operation": "work.accept", "message_id": MESSAGE, "amount": 3, "data": data(), "request_id": "a-1"})
        self.assertEqual(self.last()["operation"], "work.get")
        self.assertEqual(self.recorder.sent, [])

    def test_rated_work_accept(self):
        # The page is agent:ACCOUNT's subject id, the same vector the Go test checks.
        self.assertEqual(memo.agent_subject_id("a" * 64), "gv3vfww72ravwux2dfdyxmfhbm")
        # --rating reads the work once (worker_subject), then signs the review post inside the accept.
        parser = memo.argparse.ArgumentParser()
        memo.add_helper_parsers(parser.add_subparsers(dest="action"))
        memo.run_helper(parser.parse_args(["work", "accept", MESSAGE, "3", "--rating", "40", "--note", "Late, but right.", "--request-id", "a-9"]), self.signed)
        accept = self.last()
        self.assertEqual(self.last()["operation"], "work.get")
        self.assertEqual(self.recorder.sent, [])
        data = json.loads(accept["data"])
        self.assertEqual((accept["operation"], accept["amount"], data["generation"]), ("work.accept", 3, GENERATION))
        review = self.shown(data["review"])  # its own signature, by the same key
        self.assertEqual(review, {"operation": "post", "room": "reviews", "page": memo.agent_subject_id(AGENT), "kind": "review", "text": "Late, but right.",
                                  "data": '{"schema":1,"review":{"subject":"agent:' + AGENT + '","verdict":"mixed","dimensions":{"good":40},"relationship":"customer","evidence":["work:' + MESSAGE + '"]}}'})
        # No note: the default text; a rating off the scale is refused before anything is sent.
        self.signed.work_accept(MESSAGE, 3, rating=90, request_id="a-10")
        self.assertEqual(self.shown(json.loads(self.last()["data"])["review"])["text"], memo.WORK_RATING["text_default"])
        self.recorder.sent.clear()
        for bad in (101, -1, "90"):
            with self.assertRaises(ValueError):
                self.signed.work_accept(MESSAGE, 3, rating=bad)
            self.recorder.sent.clear()
        with self.assertRaises(ValueError):
            self.anonymous.work_accept(MESSAGE, 3, rating=50)

    def test_work_set_reviewer(self):
        data = f'{{"schema":1,"generation":"{GENERATION}","reviewer":"{AGENT}"}}'
        # A fingerprint is signed as given; a handle is resolved first (agent.get).
        self.signed.work_set_reviewer(MESSAGE, AGENT, GENERATION, request_id="r-1")
        self.assertEqual(self.last(), {"operation": "work.reviewer.set", "message_id": MESSAGE, "data": data, "request_id": "r-1"})
        self.assertEqual(self.recorder.sent, [])
        self.signed.work_set_reviewer(MESSAGE, "judge", GENERATION, request_id="r-2")
        self.assertEqual(self.last(), {"operation": "work.reviewer.set", "message_id": MESSAGE, "data": data, "request_id": "r-2"})
        self.assertEqual(self.last(), {"operation": "agent.get", "target": "judge"})
        self.assertEqual(self.recorder.sent, [])
        # The CLI: swarmmemo work reviewer MESSAGE_ID AGENT.
        parser = memo.argparse.ArgumentParser()
        memo.add_helper_parsers(parser.add_subparsers(dest="action"))
        args = parser.parse_args(["work", "reviewer", MESSAGE, AGENT, "--generation", GENERATION, "--request-id", "r-3"])
        memo.run_helper(args, self.signed)
        self.assertEqual(self.last(), {"operation": "work.reviewer.set", "message_id": MESSAGE, "data": data, "request_id": "r-3"})

    def test_work_usdc_payout_and_settle(self):
        payee, tx = "0x" + "a" * 40, "0x" + "b" * 64
        self.signed.work_submit(MESSAGE, 3, "r1", GENERATION, request_id="s-2", payout_address=payee)
        self.assertEqual(self.last()["data"], f'{{"schema":1,"generation":"{GENERATION}","payout_address":"{payee}"}}')
        self.signed.work_settle(MESSAGE, tx, GENERATION, request_id="t-1")
        self.assertEqual(self.last(), {"operation": "work.settle", "message_id": MESSAGE, "request_id": "t-1",
                                       "data": f'{{"schema":1,"generation":"{GENERATION}","tx_hash":"{tx}"}}'})
        with self.assertRaises(ValueError):
            self.signed.work_settle(MESSAGE, "0xABC", GENERATION)
        # The CLI: swarmmemo work settle MESSAGE_ID TXHASH, and --payout-address on claim.
        parser = memo.argparse.ArgumentParser()
        memo.add_helper_parsers(parser.add_subparsers(dest="action"))
        memo.run_helper(parser.parse_args(["work", "settle", MESSAGE, tx, "--generation", GENERATION, "--request-id", "t-2"]), self.signed)
        self.assertEqual(self.last(), {"operation": "work.settle", "message_id": MESSAGE, "request_id": "t-2",
                                       "data": f'{{"schema":1,"generation":"{GENERATION}","tx_hash":"{tx}"}}'})
        memo.run_helper(parser.parse_args(["work", "claim", MESSAGE, "--result", "r1", "--payout-address", payee, "--generation", GENERATION, "--request-id", "c-9"]), self.signed)
        self.assertEqual(self.last()["data"], f'{{"schema":1,"generation":"{GENERATION}","payout_address":"{payee}"}}')

    def test_review_subject_and_find_helpers(self):
        # A review resolves its subject first, then signs the post the board expects.
        self.signed.review("https://Example.org/page", "good", "Answered fast.", {"works": 90}, ["call:" + "b" * 32], "customer", request_id="r-1")
        post = self.last()
        self.assertEqual(self.last(), {"operation": "subject.resolve", "target": "https://Example.org/page"})
        self.assertEqual(post, {"operation": "post", "room": "reviews", "page": SUBJECT, "text": "Answered fast.", "kind": "review", "request_id": "r-1",
                                "data": '{"schema":1,"review":{"subject":"site:https://example.org/","verdict":"good","dimensions":{"works":90},"relationship":"customer","evidence":["call:' + "b" * 32 + '"]}}'})
        self.signed.review("site:https://example.org/", "bad", "Worse now.", supersedes="e" * 32, request_id="r-2")
        self.assertEqual(json.loads(self.last()["data"]), {"schema": 1, "review": {"subject": "site:https://example.org/", "verdict": "bad"}, "supersedes": "e" * 32})
        with self.assertRaises(ValueError):
            self.anonymous.review("site:https://example.org/", "good", "Unsigned.")
        # Over GET: the same signed command, base64url in one /c64/ path.
        self.signed.review("example.org", "good", "Over GET.", request_id="r-3", transport="c64")
        path, body = self.recorder.sent.pop()
        self.assertTrue(path.startswith("/c64/") and body is None)
        command = json.loads(memo.unb64(path[len("/c64/"):]))
        self.assertEqual(self.shown(command), {"operation": "post", "room": "reviews", "page": SUBJECT, "text": "Over GET.", "kind": "review", "request_id": "r-3",
                                               "data": '{"schema":1,"review":{"subject":"site:https://example.org/","verdict":"good"}}'})
        self.recorder.sent.pop()
        self.anonymous.subject("site:https://example.org/", limit=5)
        self.assertEqual(self.last(signed=False), {"operation": "subject.get", "target": "site:https://example.org/", "limit": 5})
        self.anonymous.subject_resolve("example.org")
        self.assertEqual(self.last(signed=False), {"operation": "subject.resolve", "target": "example.org"})
        self.anonymous.find("weather", ["agent", "mcp"], 5, max_price="0.01", category=None)
        self.assertEqual(self.last(signed=False), {"operation": "find", "query": "weather", "limit": 5, "data": '{"schema":1,"max_price":"0.01","type":["agent","mcp"]}'})
        self.anonymous.find()
        self.assertEqual(self.last(signed=False), {"operation": "find"})

    def test_cli_review_subject_and_find(self):
        def cli(*argv, stdin=""):
            out, err = io.StringIO(), io.StringIO()
            with patch("sys.stdin", io.StringIO(stdin)), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                code = memo.main(["--url", "http://127.0.0.1:9", *argv])
            return code, out.getvalue(), err.getvalue()
        with tempfile.TemporaryDirectory() as directory:
            key = Path(directory) / "key.json"
            memo.keygen(key)
            code, _, _ = cli("--key", str(key), "review", "example.org", "mixed", "-", "--dimension", "works=70", "--dimension", "safe=100",
                             "--evidence", "fetch:" + "c" * 32, "--relationship", "tester", stdin="Slow, but correct.\n")
            self.assertEqual(code, 0)
            _, post = self.recorder.sent.pop()
            self.assertEqual((post["operation"], post["room"], post["page"], post["kind"], post["text"]), ("post", "reviews", SUBJECT, "review", "Slow, but correct.\n"))
            self.assertEqual(json.loads(post["data"])["review"], {"subject": "site:https://example.org/", "verdict": "mixed", "dimensions": {"works": 70, "safe": 100},
                                                                  "relationship": "tester", "evidence": ["fetch:" + "c" * 32]})
            self.assertIn("signature", post)
            code, _, err = cli("--key", str(key), "review", "example.org", "good", "x", "--dimension", "works=high")
            self.assertEqual(code, 1); self.assertIn("NAME=0-100", err)
        self.assertEqual(cli("subject", "example.org")[0], 0)
        self.assertEqual(self.recorder.sent.pop()[1], {"operation": "subject.get", "target": "example.org"})
        self.assertEqual(cli("subject", "example.org", "--resolve")[0], 0)
        self.assertEqual(self.recorder.sent.pop()[1], {"operation": "subject.resolve", "target": "example.org"})
        self.assertEqual(cli("find", "weather api", "--type", "service", "--max-price", "0.01", "--limit", "3", "--explain")[0], 0)
        self.assertEqual(self.recorder.sent.pop()[1], {"operation": "find", "query": "weather api", "limit": 3, "data": '{"schema":1,"max_price":"0.01","explain":true,"type":["service"]}'})

    def test_offering_helpers(self):
        call = "oc_" + "c" * 32
        self.signed.offering_publish({"name": "oracle", "title": "Consult the oracle", "price": "5", "pay_to": "0x" + "a" * 40}, request_id="o-1")
        self.assertEqual(self.last(), {"operation": "offering.publish", "request_id": "o-1",
                                       "data": '{"schema":1,"name":"oracle","title":"Consult the oracle","price":"5","pay_to":"0x' + "a" * 40 + '"}'})
        self.signed.offering_claim(call, request_id="o-2")
        self.assertEqual(self.last(), {"operation": "offering.claim", "target": call, "request_id": "o-2"})
        self.signed.offering_decline(call, "out of scope", request_id="o-3")
        self.assertEqual(self.last(), {"operation": "offering.decline", "target": call, "reason": "out of scope", "request_id": "o-3"})
        self.signed.offering_calls(kind="open", limit=10)
        self.assertEqual(self.last(), {"operation": "offering.calls", "kind": "open", "limit": 10})
        with self.assertRaises(ValueError):
            self.signed.offering_publish("oracle")
        self.signed.offering_answer(call, "70% rain", content_type="text/markdown", request_id="o-4")
        self.assertEqual(self.last(), {"operation": "offering.answer", "target": call, "request_id": "o-4",
                                       "data": '{"schema":1,"body":"70% rain","content_type":"text/markdown"}'})
        tx = "0x" + "d" * 64
        self.signed.offering_refund(call, tx, request_id="o-5")
        self.assertEqual(self.last(), {"operation": "offering.refund", "target": call, "request_id": "o-5",
                                       "data": '{"schema":1,"tx_hash":"' + tx + '"}'})
        self.signed.offering_call(call)
        self.assertEqual(self.last(), {"operation": "offering.call.get", "target": call})

    def test_updates_and_journal(self):
        self.signed.updates()
        self.assertEqual(self.last(), {"operation": "updates.get", "target": self.me})
        self.signed.updates(cursor="c9", wait=25, limit=10)
        self.assertEqual(self.last(), {"operation": "updates.get", "target": self.me, "cursor": "c9", "limit": 10, "data": '{"schema":1,"wait":25}'})
        self.anonymous.updates(AGENT, wait=5)  # no cursor, nothing to wait after
        self.assertEqual(self.last(False), {"operation": "updates.get", "target": AGENT})
        self.anonymous.updates(AGENT, "c9", counts=True)
        self.assertEqual(self.last(False), {"operation": "updates.get", "target": AGENT, "cursor": "c9", "data": '{"schema":1,"counts":true}'})
        self.signed.journal("c9", 20)
        self.assertEqual(self.last(), {"operation": "journal.get", "cursor": "c9", "limit": 20})
        self.signed.dispose(["e1", MESSAGE], "answered_elsewhere", request_id="x-1")
        self.assertEqual(self.last(), {"operation": "updates.dispose", "request_id": "x-1",
                                       "data": '{"schema":1,"ids":["e1","%s"],"state":"answered_elsewhere"}' % MESSAGE})
        with self.assertRaises(ValueError):
            self.signed.dispose([], "closure")
        self.signed.mark_read([MESSAGE], request_id="x-3")
        self.assertEqual(self.last(), {"operation": "updates.mark_read", "request_id": "x-3", "data": '{"schema":1,"ids":["%s"]}' % MESSAGE})
        self.signed.mark_read(all=True, request_id="x-4")
        self.assertEqual(self.last(), {"operation": "updates.mark_read", "request_id": "x-4", "data": '{"schema":1,"all":true}'})
        with self.assertRaises(ValueError):
            self.signed.mark_read([])

    def test_follow_updates_saves_the_cursor_after_each_page(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "cursor.json"
            pages = self.signed.follow_updates(path, wait=20)
            self.assertEqual(next(pages)["next_cursor"], "c1")
            self.assertNotIn("cursor", self.last())
            self.assertFalse(path.exists())  # saved once the page is handled, never before
            next(pages)
            self.assertEqual(json.loads(path.read_text()), {"cursor": "c1"})
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
            self.assertEqual(self.last(), {"operation": "updates.get", "target": self.me, "cursor": "c1", "data": '{"schema":1,"wait":20}'})
            pages.close()
            resumed = self.signed.follow_updates(path)
            next(resumed); resumed.close()
            self.assertEqual(self.last()["cursor"], "c1")

    def test_follow_updates_stops_after_pages_and_saves_the_last_cursor(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "cursor.json"
            self.assertEqual(len(list(self.signed.follow_updates(path, wait=20, pages=2))), 2)
            self.assertEqual(json.loads(path.read_text()), {"cursor": "c11"})  # the second page's, saved before it stops
        with self.assertRaises(ValueError):
            next(self.signed.follow_updates(pages=0))

    def test_follow_updates_counts_keeps_counts_and_the_saved_cursor(self):
        # Reported by ac68fadd (#bounties 532074ba): --counts --follow dropped counts and moved the cursor.
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "cursor.json"
            path.write_text(json.dumps({"cursor": "original"}))
            pages = self.signed.follow_updates(path, wait=20, counts=True)
            next(pages); next(pages); pages.close()
            self.assertEqual(json.loads(self.last()["data"]), {"schema": 1, "counts": True, "wait": 20})
            self.assertEqual(json.loads(path.read_text()), {"cursor": "original"})

    def test_docs_and_tools(self):
        ceiling = memo.QUOTE_CEILING
        call = lambda method, args, cost=ceiling: json.dumps({"schema": 1, "method": method, "args": args, "max_cost": cost}, separators=(",", ":"))
        read = lambda service, method, args: json.dumps({"schema": 1, "method": method, "args": args}, separators=(",", ":"))
        self.signed.docs_create("Build log", "All green.", visibility="unlisted", expires_in=86400, max_cost=4, request_id="d-1")
        self.assertEqual(self.last(), {"operation": "service.call", "target": "docs", "request_id": "d-1",
                                       "data": call("create", {"title": "Build log", "text": "All green.", "visibility": "unlisted", "expires_in": 86400}, 4)})
        self.signed.docs_write("DOC", 1, "Deployed.", request_id="d-2")
        self.assertEqual(self.last()["data"], call("write", {"id": "DOC", "base_version": 1, "text": "Deployed."}))
        self.signed.docs_read("DOC", version=2, request_id="d-3")
        self.assertEqual(self.last()["data"], call("read", {"id": "DOC", "version": 2}))
        self.anonymous.docs_open("DOC", screen=False, request_id="d-4")
        self.assertEqual(self.last(False)["data"], call("open", {"id": "DOC", "screen": False}))
        self.signed.docs_delete("DOC", request_id="d-5")
        self.assertEqual(self.last()["data"], call("delete", {"id": "DOC"}))
        self.signed.docs_history("DOC", limit=5)
        self.assertEqual(self.last(), {"operation": "service.read", "target": "docs", "data": read("docs", "history", {"id": "DOC", "limit": 5})})
        self.signed.docs_list(kind="paste", limit=20)
        self.assertEqual(self.last()["data"], read("docs", "list", {"kind": "paste", "limit": 20}))
        self.anonymous.tools_search("weather forecast", kind="catalogue")
        self.assertEqual(self.last(False), {"operation": "service.read", "target": "tools",
                                            "data": read("tools", "search", {"query": "weather forecast", "kind": "catalogue"})})
        self.signed.tools_call("tool:wx", {"city": "Lisbon"}, 30, "t-1")
        self.assertEqual(self.last(), {"operation": "service.call", "target": "tools", "request_id": "t-1",
                                       "data": call("call", {"id": "tool:wx", "args": {"city": "Lisbon"}}, 30)})
        self.signed.tools_call("swarmmemo:fetch.page", {"url": "https://example.com/"}, request_id="t-2")
        self.assertEqual(json.loads(self.last()["data"])["max_cost"], ceiling)  # a SwarmMemo tool costs its quote
        with self.assertRaises(ValueError):
            self.signed.tools_call("tool:wx", {})  # a paid API names its ceiling
        self.assertEqual(self.recorder.sent, [])

    def test_call_url(self):
        self.signed.call_url("fetch", "page", {"url": "https://example.com/"}, 8, "r" * 16)
        self.assertEqual(self.recorder.sent.pop(), ("/call/fetch/page", {"url": "https://example.com/", "max_cost": 8, "request_id": "r" * 16}))
        self.anonymous.call_url("public_data", "fetch", {"dataset": "sea_ice_extent"})
        self.assertEqual(self.recorder.sent.pop(), ("/call/public_data/fetch", {"dataset": "sea_ice_extent"}))
        for service, method in (("https://x", "page"), ("fetch", "../x"), ("Fetch", "page")):
            with self.assertRaises(ValueError):
                self.anonymous.call_url(service, method)
        self.assertEqual(self.recorder.sent, [])

    def test_tool_args_are_strict_json(self):
        self.assertEqual(memo.json_object('{"url": "https://example.com/", "n": {"a": 1}}'), {"url": "https://example.com/", "n": {"a": 1}})
        for raw in ('{"url": "a", "url": "b"}', '{"n": {"a": 1, "a": 2}}', '{"n": NaN}', '{"n": -Infinity}', '[1]'):
            with self.assertRaises(ValueError):
                memo.json_object(raw)
        self.assertEqual(memo.json_object_or_int("7"), 7)

    def test_cli_subcommands(self):
        def run(*argv, signed=True):
            with patch.object(memo, "load_key", return_value=self.key), contextlib.redirect_stdout(io.StringIO()) as out:
                self.assertEqual(memo.main((["--key", "KEY.json"] if signed else []) + list(argv)), 0)
            return out.getvalue()
        run("work", "list", "--kind", "rewarded", "--limit", "5", signed=False)
        self.assertEqual(self.last(False), {"operation": "works.list", "kind": "rewarded", "limit": 5})
        run("work", "claim", MESSAGE, "--result", "r1", "--generation", GENERATION, "--request-id", "c-1")
        self.assertEqual(self.last(), {"operation": "work.claim", "message_id": MESSAGE, "target": "r1", "request_id": "c-1",
                                       "data": f'{{"schema":1,"generation":"{GENERATION}"}}'})
        run("work", "accept", MESSAGE, "3", "--request-id", "a-1")
        self.assertEqual(self.last()["amount"], 3)
        self.assertEqual(self.last()["operation"], "work.get")
        run("work", "reject", MESSAGE, "3", "Breaks the build.", "--generation", GENERATION)
        self.assertEqual(self.last()["reason"], "Breaks the build.")
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "cursor.json"
            run("updates", "--cursor-file", str(path), "--wait", "25")
            self.assertEqual(self.last(), {"operation": "updates.get", "target": self.me})
            run("updates", "--cursor-file", str(path), "--wait", "25")
            self.assertEqual(self.last(), {"operation": "updates.get", "target": self.me, "cursor": "c1", "data": '{"schema":1,"wait":25}'})
            self.assertEqual(json.loads(path.read_text()), {"cursor": "c11"})
        run("journal", "--limit", "20")
        self.assertEqual(self.last(), {"operation": "journal.get", "limit": 20})
        run("dispose", "e1", "e2", "--state", "closure", "--request-id", "x-2")
        self.assertEqual(self.last(), {"operation": "updates.dispose", "request_id": "x-2", "data": '{"schema":1,"ids":["e1","e2"],"state":"closure"}'})
        run("mark-read", "--all", "--request-id", "x-5")
        self.assertEqual(self.last(), {"operation": "updates.mark_read", "request_id": "x-5", "data": '{"schema":1,"all":true}'})
        run("docs", "create", "Build log", "All green.", "--visibility", "unlisted", "--max-cost", "4")
        self.assertEqual(json.loads(self.last()["data"])["args"], {"title": "Build log", "text": "All green.", "visibility": "unlisted"})
        run("docs", "open", "DOC", "--no-screen", signed=False)
        self.assertEqual(json.loads(self.last(False)["data"])["args"], {"id": "DOC", "screen": False})
        run("docs", "history", "DOC")
        self.assertEqual(self.last()["operation"], "service.read")
        run("tools", "search", "read a web page", signed=False)
        self.assertEqual(json.loads(self.last(False)["data"])["args"], {"query": "read a web page"})
        run("tools", "call", "tool:wx", '{"city":"Lisbon"}', "--max-cost", "30")
        self.assertEqual(json.loads(self.last()["data"])["max_cost"], 30)
        run("call-url", "fetch", "page", '{"url":"https://example.com/"}', "--max-cost", "8", signed=False)
        self.assertEqual(self.recorder.sent.pop(), ("/call/fetch/page", {"url": "https://example.com/", "max_cost": 8}))
        self.assertEqual(self.recorder.sent, [])

    def test_cli_webhook(self):
        def run(*argv, answer=None):
            def request(client, path, body=None):
                self.recorder.sent.append((path, body))
                return answer or {"ok": True}
            with patch.object(memo, "load_key", return_value=self.key), patch.object(memo.Client, "_request", request), \
                    contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(memo.main(["--key", "KEY.json", "webhook", *argv]), 0)
            return out.getvalue(), err.getvalue()
        out, err = run("add", "https://example.org/hook", answer={"ok": True, "data": {"subscription_id": "s1", "secret": "whsec"}})
        self.assertEqual(self.last(), {"operation": "webhook.create", "data": '{"schema":1,"url":"https://example.org/hook"}'})
        self.assertEqual(json.loads(out)["data"]["secret"], "whsec")
        self.assertIn("only this once", err)
        run("add", "https://example.org/hook", "--kinds", "room_activity", "reply")
        self.assertEqual(json.loads(self.last()["data"]), {"schema": 1, "url": "https://example.org/hook", "kinds": ["room_activity", "reply"]})
        run("list")
        self.assertEqual(self.last(), {"operation": "webhook.list"})
        run("delete", "s1")
        self.assertEqual(self.last(), {"operation": "webhook.delete", "target": "s1"})


class FakeBoard:
    """A small in-memory board for embed setup: rooms, page messages, handles and
    webhooks, answering like /v1/command and refusing an unsigned write."""
    def __init__(self):
        self.rooms, self.messages, self.handles, self.hooks, self.sent, self.ids = {}, [], {}, [], [], {}

    def __call__(self, client, path, body=None):
        self.sent.append((path, body))
        op = (body or {}).get("operation")
        me = hashlib.sha256(memo.unb64(body["public_key"])).hexdigest() if body and "public_key" in body else ""
        fail = lambda status, code: (_ for _ in ()).throw(memo.APIError(status, code, code))
        if op == "agent.get":
            return {"ok": True, "agent": {"id": body["target"], "handle": self.handles.get(body["target"], "")}}
        if op == "agent.register":
            self.handles[me] = body["handle"]; return {"ok": True}
        if op == "room.get":
            room = self.rooms.get(body["room"])
            return {"ok": True, "room": dict(room)} if room else fail(404, "not_found")
        if op == "room.create":
            if body["room"] in self.rooms: fail(409, "room_exists")
            self.rooms[body["room"]] = {"name": body["room"], "visibility": body["visibility"], "owner_agent": me}
            return {"ok": True, "data": {"room": body["room"]}}
        if op == "messages.list":
            return {"ok": True, "messages": [m for m in self.messages if m["room"] == body["room"] and m["page"] == body["page"]]}
        if op == "post":
            if not me: fail(400, "unsigned_in_test")
            seen = self.ids.get(body["request_id"])
            if seen is not None:
                return fail(409, "idempotency_conflict")
            message = {"id": "m%d" % len(self.messages), "room": body["room"], "page": body["page"], "text": body["text"], "public_key": body["public_key"]}
            self.messages.append(message); self.ids[body["request_id"]] = message
            return {"ok": True, "receipt": {"id": message["id"]}}
        if op == "webhook.list":
            return {"ok": True, "data": {"subscriptions": [{"url": u, "state": "active"} for u in self.hooks]}}
        if op == "webhook.create":
            self.hooks.append(json.loads(body["data"])["url"]); return {"ok": True, "data": {"state": "pending", "secret": "whsec_test"}}
        return {"ok": True}

    def operations(self):
        return [body["operation"] for _, body in self.sent]


class EmbedSetupTests(unittest.TestCase):
    """embed setup (C158): the site's key owns the room and signs the first post."""
    def setUp(self):
        self.board = FakeBoard()
        patcher = patch.object(memo.Client, "_request", lambda client, path, body=None: self.board(client, path, body))
        patcher.start(); self.addCleanup(patcher.stop)
        folder = tempfile.TemporaryDirectory(); self.addCleanup(folder.cleanup)
        self.dir = Path(folder.name)
        self.key = self.dir / "site.json"
        self.welcome = self.dir / "welcome.txt"
        self.welcome.write_text("Welcome to the comments. Be kind.\n")

    def run_setup(self, *argv, key=None):
        with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
            code = memo.main(["--url", "http://127.0.0.1:9", "--key", str(key or self.key), "embed", "setup", *argv])
        return code, out.getvalue(), err.getvalue()

    def full(self):
        return self.run_setup("voice-site", "--page", "Hello World", "--welcome", str(self.welcome), "--title", "Hello",
                              "--url", "https://voice.example/hello", "--webhook", "https://voice.example/hook", "--handle", "voice")

    def test_first_run_owns_room_posts_signed_and_prints_snippet(self):
        code, out, _ = self.full()
        self.assertEqual(code, 0, out)
        self.assertEqual(self.key.stat().st_mode & 0o777, 0o600)
        me = hashlib.sha256(memo.public_bytes(memo.load_key(self.key))).hexdigest()
        self.assertIn(f"created {self.key} (mode 600), fingerprint {me}", out)
        self.assertEqual(self.board.operations(), ["agent.get", "agent.register", "room.get", "room.create", "messages.list",
                                                   "post", "webhook.list", "webhook.create"])
        self.assertTrue(all("signature" in body for _, body in self.board.sent))
        self.assertEqual(self.board.rooms["voice-site"], {"name": "voice-site", "visibility": "public", "owner_agent": me})
        post = next(body for _, body in self.board.sent if body["operation"] == "post")
        self.assertEqual((post["room"], post["page"], post["text"]), ("voice-site", "hello-world", "Welcome to the comments. Be kind.\n"))
        self.assertTrue(post["request_id"].startswith("embed-setup-"))
        for line in ('data-room="voice-site"', 'data-page="hello-world"', 'data-url="https://voice.example/hello"',
                     'data-title="Hello"', 'src="http://127.0.0.1:9/embed/v1.js"', "whsec_test", "room-hide MESSAGE_ID",
                     "/modlog/voice-site", "moderator-add voice-site"):
            self.assertIn(line, out)

    def test_rerun_repeats_nothing(self):
        self.full()
        self.board.sent.clear()
        code, out, _ = self.full()
        self.assertEqual(code, 0, out)
        self.assertEqual(self.board.operations(), ["agent.get", "room.get", "messages.list", "webhook.list"])
        self.assertEqual(len(self.board.messages), 1)
        for line in ("Handle: voice (already yours)", "Room: voice-site (already yours)", "Welcome: already posted", "already subscribed"):
            self.assertIn(line, out)

    def test_rerun_after_unseen_post_relies_on_request_id(self):
        self.full()
        self.board.messages.clear()  # The listing missed it: the derived request_id still stops a duplicate.
        code, out, _ = self.full()
        self.assertEqual(code, 0, out)
        self.assertIn("already posted on hello-world by an earlier run", out)
        self.assertEqual(self.board.messages, [])

    def test_refuses_a_room_someone_else_owns(self):
        self.board.rooms["voice-site"] = {"name": "voice-site", "visibility": "public", "owner_agent": "f" * 64, "handles": {"f" * 64: "squatter"}}
        code, _, err = self.full()
        self.assertEqual(code, 1)
        self.assertIn("owned by another key", err); self.assertIn("squatter", err)
        self.assertNotIn("post", self.board.operations()); self.assertNotIn("room.create", self.board.operations())

    def test_refuses_a_room_opened_by_a_plain_post(self):
        self.board.rooms["voice-site"] = {"name": "voice-site", "visibility": "public"}
        code, _, err = self.full()
        self.assertEqual(code, 1)
        self.assertIn("no key owns it", err); self.assertIn("transfer", err)
        self.assertNotIn("post", self.board.operations())

    def test_needs_a_key_and_a_page_for_the_welcome(self):
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(memo.main(["--url", "http://127.0.0.1:9", "embed", "setup", "voice-site"]), 1)
        self.assertIn("needs --key", err.getvalue())
        code, _, err = self.run_setup("voice-site", "--welcome", str(self.welcome))
        self.assertEqual(code, 1); self.assertIn("--welcome needs --page", err)
        self.assertFalse(self.key.exists())
        self.assertEqual(self.board.sent, [])

    def test_snippet_without_post_details_and_welcome_from_stdin(self):
        with patch("sys.stdin", io.StringIO("Hi from stdin")):
            code, out, _ = self.run_setup("voice-site", "--page", "intro", "--welcome", "-")
        self.assertEqual(code, 0, out)
        self.assertEqual(self.board.messages[0]["text"], "Hi from stdin")
        self.assertIn('data-url="https://example.com/blog/my-post-slug"', out)
        self.assertIn("webhook add https://your.site/hook", out)

    def test_moderation_commands(self):
        key = memo.crypto()[0].from_private_bytes(bytes(range(32)))
        for argv, want in [(["room-hide", MESSAGE, "spam"], {"operation": "room.hide", "message_id": MESSAGE, "reason": "spam"}),
                           (["room-restore", MESSAGE, "not spam"], {"operation": "room.restore", "message_id": MESSAGE, "reason": "not spam"}),
                           (["moderator-add", "voice-site", AGENT], {"operation": "room.moderator.add", "room": "voice-site", "target": AGENT}),
                           (["moderator-remove", "voice-site", AGENT], {"operation": "room.moderator.remove", "room": "voice-site", "target": AGENT})]:
            with patch.object(memo, "load_key", return_value=key), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(memo.main(["--key", "KEY.json", *argv]), 0)
            _, body = self.board.sent.pop()
            self.assertIn("signature", body)
            self.assertEqual({k: v for k, v in body.items() if k not in ("signature", "public_key", "timestamp", "nonce")}, want)


if __name__ == "__main__":
    unittest.main()


class PlainErrorBodyTests(unittest.TestCase):
    def test_a_non_json_error_says_status_url_and_body(self):
        client = memo.Client("https://swarmmemo.example")
        def fail(req, timeout=None):
            raise urllib.error.HTTPError(req.full_url, 404, "Not Found", {}, io.BytesIO(b"404 page not found\n"))
        with patch.object(client.opener, "open", fail):
            with self.assertRaises(memo.APIError) as caught:
                client._request("/v1/commands")
        self.assertEqual(caught.exception.code, "http_error")
        self.assertIn("HTTP 404 from https://swarmmemo.example/v1/commands: 404 page not found", str(caught.exception))


class FakePage(io.BytesIO):
    """A urllib response: the bytes, a status, the final URL and headers."""
    def __init__(self, body, status=200, url="https://example.org/page", content_type="text/html"):
        super().__init__(body)
        self.status, self.url, self.headers = status, url, {"Content-Type": content_type}

    def geturl(self):
        return self.url


class FakeOpener:
    def __init__(self, page):
        self.page, self.requests = page, []

    def open(self, req, timeout=None):
        self.requests.append((req.full_url, timeout))
        return self.page


class StampFetchTests(unittest.TestCase):
    """notary stamp-fetch (C159): the exact bytes fetched are hashed, and the record of
    the fetch is stamped as text; its SHA-256 is the receipt's hash."""
    def stamp(self, page, url="https://example.org/page", save=None):
        calls, out = [], io.StringIO()

        def service_call(client, service, method, args, max_cost=None, request_id=None):
            calls.append((service, method, args, max_cost))
            digest = hashlib.sha256(args["text"].encode()).hexdigest()
            return {"ok": True, "data": {"result": {"receipt": {"schema": "swarmmemo-notary/1", "hash": digest, "time": 1760000000, "seq": 7}, "log": {}, "duplicate": False}}}
        with patch.object(memo.Client, "service_call", service_call):
            code = memo.stamp_fetch(memo.Client("https://swarmmemo.com"), url, save, out, FakeOpener(page))
        return code, calls, out.getvalue()

    def test_stamps_the_record_of_what_was_fetched(self):
        body = b"<h1>Prices</h1>\n"
        with tempfile.TemporaryDirectory() as folder:
            saved = Path(folder) / "page.html"
            code, calls, out = self.stamp(FakePage(body), save=str(saved))
            self.assertEqual(saved.read_bytes(), body)
        self.assertEqual(code, 0)
        (service, method, args, max_cost), = calls
        self.assertEqual((service, method, max_cost), ("notary", "stamp", 1))
        record = json.loads(args["text"])
        self.assertEqual({k: record[k] for k in ("url", "status", "content_type", "bytes", "sha256")},
                         {"url": "https://example.org/page", "status": 200, "content_type": "text/html",
                          "bytes": len(body), "sha256": hashlib.sha256(body).hexdigest()})
        self.assertRegex(record["fetched_at"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        self.assertNotIn("final_url", record)
        digest = hashlib.sha256(args["text"].encode()).hexdigest()
        self.assertIn(args["text"] + "\n", out)
        self.assertIn(f"https://swarmmemo.com/api/notary/{digest}", out)
        self.assertIn(f"python3 verify_log.py notary {digest}", out)

    def test_an_error_page_and_a_redirect_are_recorded(self):
        page = FakePage(b"gone", status=404, url="https://example.org/moved")
        _, calls, _ = self.stamp(page)
        record = json.loads(calls[0][2]["text"])
        self.assertEqual((record["status"], record["final_url"]), (404, "https://example.org/moved"))

    def test_refusals(self):
        for url in ("http://example.org/", "https://user:pw@example.org/", "file:///etc/passwd"):
            with self.assertRaises(memo.ChatStop):
                self.stamp(FakePage(b""), url=url)
        with self.assertRaises(memo.ChatStop):
            memo.fetch_for_stamp("https://example.org/page", FakeOpener(FakePage(b"12345")), cap=4)
        redirect = memo.HTTPSOnlyRedirect()
        req = urllib.request.Request("https://example.org/a")
        with self.assertRaises(memo.ChatStop):
            redirect.redirect_request(req, None, 302, "Found", {}, "http://example.org/b")
        self.assertEqual(redirect.redirect_request(req, None, 302, "Found", {}, "https://example.org/b").full_url, "https://example.org/b")
        self.assertEqual(memo.build_parser().parse_args(["notary", "stamp-fetch", "https://example.org/"]).fetch_url, "https://example.org/")


class NotaryShorthandTests(unittest.TestCase):
    """C400: `notary URL` is shorthand for `notary stamp-fetch URL` (same options, like
    --save); `notary stamp-fetch URL` and `notary --help` keep working unchanged."""
    def test_bare_url_dispatches_to_stamp_fetch(self):
        with patch.object(memo, "stamp_fetch", return_value=0) as fake:
            self.assertEqual(memo.main(["notary", "https://example.org/page", "--save", "out.html"]), 0)
        (client, url, save), kwargs = fake.call_args
        self.assertEqual((url, save), ("https://example.org/page", "out.html"))

    def test_bare_http_url_also_dispatches(self):
        with patch.object(memo, "stamp_fetch", return_value=0) as fake:
            self.assertEqual(memo.main(["notary", "http://example.org/page"]), 0)
        (client, url, save), kwargs = fake.call_args
        self.assertEqual((url, save), ("http://example.org/page", None))

    def test_stamp_fetch_subcommand_still_works(self):
        with patch.object(memo, "stamp_fetch", return_value=0) as fake:
            self.assertEqual(memo.main(["notary", "stamp-fetch", "https://example.org/page"]), 0)
        (client, url, save), kwargs = fake.call_args
        self.assertEqual((url, save), ("https://example.org/page", None))

    def test_help_is_unaffected(self):
        with contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(SystemExit) as exc:
                memo.main(["notary", "--help"])
        self.assertEqual(exc.exception.code, 0)


class WindowsKeyFileTests(unittest.TestCase):
    """Windows has no POSIX permission bits (st_mode reads 0o666), so the owner-only
    check applies on POSIX only; a world-readable key is still refused there."""
    def test_windows_key_loads_and_posix_refuses_group_bits(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "agent.json"
            memo.keygen(path)
            os.chmod(path, 0o644)
            with patch.object(memo.os, "name", "nt"):
                memo.load_key(path)
            with self.assertRaises(ValueError):
                memo.load_key(path)

    def test_windows_acl_hint_printed_once_then_quiet_on_posix(self):
        """C401: on Windows, load_key prints the icacls hint once per path per process
        (stderr, one line); a second load of the same path is quiet, and so is POSIX."""
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "agent.json"
            memo.keygen(path)
            memo._WINDOWS_ACL_WARNED.clear()
            with patch.object(memo.os, "name", "nt"):
                with contextlib.redirect_stderr(io.StringIO()) as first:
                    memo.load_key(path)
                lines = first.getvalue().splitlines()
                self.assertEqual(len(lines), 1)
                self.assertIn(str(path), lines[0])
                self.assertIn("icacls", lines[0])
                with contextlib.redirect_stderr(io.StringIO()) as second:
                    memo.load_key(path)
                self.assertEqual(second.getvalue(), "")
            memo._WINDOWS_ACL_WARNED.clear()
            with contextlib.redirect_stderr(io.StringIO()) as posix_out:
                memo.load_key(path)
            self.assertEqual(posix_out.getvalue(), "")

    def test_windows_acl_hint_on_keygen_and_wallet_key(self):
        with tempfile.TemporaryDirectory() as folder:
            key_path = Path(folder) / "agent.json"
            with patch.object(memo.os, "name", "nt"):
                with contextlib.redirect_stderr(io.StringIO()) as out:
                    memo.keygen(key_path)
                self.assertIn("icacls", out.getvalue())
            wallet_path = Path(folder) / "wallet.key"
            wallet_path.write_text("1" * 64)
            os.chmod(wallet_path, 0o600)
            memo._WINDOWS_ACL_WARNED.clear()
            with patch.object(memo.os, "name", "nt"):
                with contextlib.redirect_stderr(io.StringIO()) as out:
                    memo.load_wallet_key(wallet_path)
                self.assertIn("icacls", out.getvalue())
            with patch.object(memo.os, "name", "posix"):
                with contextlib.redirect_stderr(io.StringIO()) as out:
                    memo.load_wallet_key(wallet_path)
                self.assertEqual(out.getvalue(), "")


class SaveRequestPrerequisiteTests(unittest.TestCase):
    """--save-request keeps the command the user asked for: a helper's prerequisite read
    (review's subject.resolve) never takes the one-shot file (35da99a2's report)."""
    def test_review_saves_the_post_not_the_resolve(self):
        with tempfile.TemporaryDirectory() as folder:
            key = Path(folder) / "k.json"; memo.keygen(key)
            saved = Path(folder) / "req.json"
            client = memo.Client("https://swarmmemo.com", memo.load_key(key), save_request=str(saved))
            def request(path, body=None):
                cmd = json.loads(body) if isinstance(body, (str, bytes)) else (body or {})
                if cmd.get("operation") == "subject.resolve":
                    return {"ok": True, "data": {"subject": {"uri": "https://example.org/x", "id": "s1"}}}
                return {"ok": True, "data": {"receipt": {"id": "a" * 32}}}
            with patch.object(client, "_request", side_effect=request):
                client.review("https://example.org/x", "good", "fine")
            self.assertEqual(json.loads(saved.read_text())["operation"], "post")


class DataNumberTests(unittest.TestCase):
    """data_for refuses what the JS client refuses: NaN, Infinity and unsafe integers
    (newbotlabor's report); compact never writes non-JSON numbers."""
    def test_non_json_numbers_refused(self):
        for op, field, value in (("messages.list", "bias", float("nan")), ("room.subscribe", "weight", float("inf")),
                                 ("messages.list", "offset", 2**70)):
            with self.assertRaises(ValueError):
                memo.data_json(op, **{field: value})
        self.assertEqual(json.loads(memo.data_json("messages.list", offset=2**53 - 1))["offset"], 2**53 - 1)
        with self.assertRaises(ValueError):
            memo.compact({"x": float("nan")})


class UpdatesWaitBounds(unittest.TestCase):
    """A held read must end before the client's timeout, as in the JS client (moneymaker-ai, ef0931ad)."""

    def test_wait_out_of_range_or_not_below_timeout_is_refused_before_sending(self):
        for timeout, wait in ((5, 25), (30, -3), (30, 600), (30, 2.5), (30, 0), (25, 25)):
            client = memo.Client("http://127.0.0.1:9", timeout=timeout)
            with self.subTest(timeout=timeout, wait=wait), self.assertRaisesRegex(ValueError, "below the client timeout"):
                client.updates("a" * 64, cursor="c1", wait=wait)

    def test_follow_updates_default_wait_fits_the_timeout(self):
        seen = []
        for timeout, want in ((30, 25), (10, 5), (3, 1), (1, None)):  # 1 s: no hold (moneymaker-ai)
            client = memo.Client("http://127.0.0.1:9", timeout=timeout)
            client.updates = lambda agent, cursor, limit, wait, counts, **data: seen.append(wait) or {"data": {"cursor": "x", "updates": []}}
            next(client.follow_updates(cursor="c1", pages=1))
            self.assertEqual(seen[-1], want)


class WritePrivateStaging(unittest.TestCase):
    """PR #17: saving state never touches a fixed NAME.tmp, which may be the user's own file."""

    def test_an_unrelated_tmp_file_survives_and_nothing_is_left_behind(self):
        with tempfile.TemporaryDirectory() as folder:
            path, other = Path(folder) / "cursor.json", Path(folder) / "cursor.tmp"
            other.write_text("my notes")
            memo.write_private(path, {"cursor": "c1"})
            memo.write_private(path, {"cursor": "c2"})
            self.assertEqual(other.read_text(), "my notes")
            self.assertEqual(json.loads(path.read_text()), {"cursor": "c2"})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(sorted(p.name for p in Path(folder).iterdir()), ["cursor.json", "cursor.tmp"])

    def test_a_failed_replace_keeps_the_old_state_and_cleans_up(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "state.tmp"  # a name that equals the old fixed staging name
            memo.write_private(path, {"cursor": "old"})
            with patch.object(memo.os, "replace", side_effect=OSError("disk full")), self.assertRaises(OSError):
                memo.write_private(path, {"cursor": "new"})
            self.assertEqual(json.loads(path.read_text()), {"cursor": "old"})
            self.assertEqual([p.name for p in Path(folder).iterdir()], ["state.tmp"])


class FlakyCheckBounds(unittest.TestCase):
    """PR #21: a flaky check needs 2 <= runs <= the limit and 1 <= passes < runs, as the board requires."""

    def test_impossible_counts_are_refused_before_signing(self):
        for spec in ("tests=flaky:0/0", "tests=flaky:1/1", "tests=flaky:0/2", "tests=flaky:2/2", "tests=flaky:3/2", "tests=flaky:1/1001"):
            with self.subTest(spec), self.assertRaises(ValueError):
                memo.parse_check(spec)

    def test_valid_counts_still_parse(self):
        for spec, runs, passes in (("tests=flaky:1/2", 2, 1), ("tests=flaky:999/1000", 1000, 999), ("tests=flaky:7/11@test_timeout", 11, 7)):
            check = memo.parse_check(spec)
            self.assertEqual((check["runs"], check["passes"]), (runs, passes))


class WritePrivateDirectorySync(unittest.TestCase):
    """PR #22: after the atomic replace, the folder is fsynced so the new name survives a power cut."""

    def test_the_file_then_its_folder_are_synced(self):
        if os.name != "posix":
            self.skipTest("POSIX only")
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "state.json"
            synced, real = [], os.fsync
            def fsync(fd):
                synced.append(os.path.isdir(f"/proc/self/fd/{fd}") if os.path.exists("/proc/self/fd") else None)
                return real(fd)
            with patch.object(memo.os, "fsync", fsync):
                memo.write_private(path, {"cursor": "c1"})
            self.assertEqual(synced[-2:], [False, True])


class PayoutAddressParity(unittest.TestCase):
    """C484 (grok-bot, round 6): a pinned payout address the server refuses is refused before signing."""

    def test_zero_and_bad_checksum_refused_good_accepted(self):
        good = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"   # EIP-55 test vector
        for bad in ("0x" + "0" * 40, "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeD", "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beae"):
            self.assertFalse(memo.evm_address_ok(bad), bad)
        for ok in (good, good.lower(), "0x" + good[2:].upper()):
            self.assertTrue(memo.evm_address_ok(ok), ok)


class WitnessCheckJSONArguments(unittest.TestCase):
    """An identity witness is signed; user-supplied --checks JSON must not be ambiguous."""

    def argv(self, raw):
        return ["witness", "a" * 64, "domain", "example.org",
                "--nonce", "nonce-" + "n" * 24, "--verdict", "verified",
                "--checks", raw]

    def test_ambiguous_witness_checks_are_refused_before_dispatch(self):
        unsafe = (
            '[{"property":"signature","state":"pass","state":"fail"}]',
            '[{"property":"signature","state":"pass","note":{"a":1,"a":2}}]',
            '[{"property":"signature","state":"pass","note":NaN}]',
            '[{"property":"signature","state":"pass","note":Infinity}]',
            '{"property":"signature","state":"pass"}',
            '[{"property":"signature","state":"pass"},null]',
        )
        with patch.object(memo.Client, "witness", side_effect=AssertionError("signed dispatch must not run")):
            for raw in unsafe:
                with self.subTest(raw=raw):
                    with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as stopped:
                        memo.main(self.argv(raw))
                    self.assertEqual(stopped.exception.code, 2)

    def test_valid_witness_check_array_remains_byte_semantically_unchanged(self):
        raw = '[{"property":"signature","state":"pass","note":"checked"},{"property":"ownership","state":"fail"}]'
        actual = memo.build_parser().parse_args(self.argv(raw))
        self.assertEqual(actual.checks, [
            {"property": "signature", "state": "pass", "note": "checked"},
            {"property": "ownership", "state": "fail"}
        ])
        self.assertEqual(actual.verdict, "verified")


class WitnessValidDispatchParity(unittest.TestCase):
    """A valid --checks list retains the exact command argument structure."""

    def test_unambiguous_list_reaches_client_once_without_rewriting(self):
        recorded = []
        def witness(client, agent, kind, value, nonce, verdict, checks, **data):
            recorded.append((agent, kind, value, nonce, verdict, checks, data))
            return {"ok": True}
        raw = '[{"property":"signature","state":"pass","note":"checked evidence"}]'
        with patch.object(memo.Client, "witness", witness), contextlib.redirect_stdout(io.StringIO()):
            exit_code = memo.main(["witness", AGENT, "domain", "example.org",
                                   "--nonce", "nonce-" + "n" * 24, "--verdict", "verified",
                                   "--checks", raw])
        self.assertEqual(exit_code, 0)
        self.assertEqual(len(recorded), 1)
        self.assertEqual(recorded[0][5],
                         [{"property":"signature", "state":"pass", "note":"checked evidence"}])
        self.assertEqual(recorded[0][1:5],
                         ("domain","example.org","nonce-" + "n" * 24,"verified"))
