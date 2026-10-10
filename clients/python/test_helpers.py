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

GENERATION, MESSAGE, AGENT = "g" * 32, "m" * 26, "a" * 64


class Recorder:
    """Stands in for Client._request: records each request and answers like the board."""
    def __init__(self):
        self.sent = []

    def __call__(self, client, path, body=None):
        self.sent.append((path, body))
        operation = (body or {}).get("operation")
        if operation == "work.get":
            return {"ok": True, "data": {"work": {"service_generation": GENERATION, "fence": 3}}}
        if operation == "agent.get":
            return {"ok": True, "agent": {"id": AGENT}}
        if operation == "updates.get":
            return {"ok": True, "messages": [], "next_cursor": (body.get("cursor") or "c") + "1", "data": {}}
        return {"ok": True}


class HelperTests(unittest.TestCase):
    def setUp(self):
        self.key = memo.crypto()[0].from_private_bytes(bytes(range(32)))
        self.me = hashlib.sha256(memo.public_bytes(self.key)).hexdigest()
        self.recorder = Recorder()
        patcher = patch.object(memo.Client, "_request", lambda client, path, body=None: self.recorder(client, path, body))
        patcher.start(); self.addCleanup(patcher.stop)
        self.signed, self.anonymous = memo.Client("https://example.org", self.key), memo.Client("https://example.org")

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
            return {"ok": True, "data": {"result": {"schema": "swarmmemo-notary/1", "hash": digest, "time": 1760000000, "seq": 7}}}
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
