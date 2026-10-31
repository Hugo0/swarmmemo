# SPDX-License-Identifier: Apache-2.0
"""C377: every board data field is sendable from each Python wrapper and each CLI
subcommand of its operation, derived from the contract (CONTRACT["operations"][op]["data"],
generated from the board's DataSpec).

Each test sends every field of each operation through the wrapper (or the CLI) to a
recording transport and checks it in the signed data. A field the board adds fails here
until the surface sends it or the field is in that surface's exclusion map with a reason.
No surface may send a field the contract does not know or marks removed."""
import contextlib
import hashlib
import inspect
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import swarmmemo as memo

GENERATION, MESSAGE, AGENT, TX = "a" * 32, "b" * 32, "c" * 64, "0x" + "d" * 64
WALLET_KEY = bytes(range(1, 33))
PATTERN_SAMPLES = {"work_id": "e" * 32, "message_id": "e" * 32, "fingerprint": "f" * 64, "tx_hash": TX, "standing_tier": "bug"}
FIELD_SAMPLES = {
    "checks": [{"property": "tests", "state": "pass"}],
    "tiers": [{"name": "bug", "credits": 5}],
    "capabilities": ["review"],
    "ids": [MESSAGE],
    "payout_address": "0x" + "ab" * 20,
    "review": {"subject": "site:https://example.org/", "verdict": "good"},
}
DATA_OPERATIONS = sorted(name for name, op in memo.CONTRACT["operations"].items() if op.get("data") is not None)


def sample(operation, name):
    """A value of the field's contract type (its pattern's or enum's where it names one)."""
    field = memo.data_spec(operation)["fields"][name]
    if name in FIELD_SAMPLES: return FIELD_SAMPLES[name]
    kind = field["type"].split("|")[0]
    if field.get("enum"):
        first = memo.ENUMS[field["enum"]][0]
        return [first] if kind == "array" else first
    if kind == "string": return PATTERN_SAMPLES.get(field.get("pattern"), "x1")
    return {"int": 2, "number": 1.5, "bool": True, "object": {"k": "v"}, "array": ["x1"]}[kind]


class Recorder:
    """Stands in for Client._request: records each command and answers like the board."""
    def __init__(self):
        self.sent = []

    def __call__(self, client, path, body=None):
        operation = (body or {}).get("operation")
        if body is not None: self.sent.append(body)
        if path == "/api/changes?after=-1": return {"ok": True, "generation": GENERATION}
        if operation == "work.get":
            return {"ok": True, "data": {"work": {"id": MESSAGE, "service_generation": GENERATION, "fence": 3, "worker_subject": "agent:" + AGENT}}}
        if operation == "agent.get": return {"ok": True, "agent": {"id": AGENT}}
        if operation == "subject.resolve": return {"ok": True, "data": {"subject": {"id": "s" * 26, "uri": "site:https://example.org/"}}}
        if operation == "gate.challenge":
            data = json.loads(body["data"])
            challenge = "g" * 32
            return {"ok": True, "data": {"challenge": challenge, "pow": {"prefix": "p", "bits": 1},
                                         "statement": f"swarmmemo-gate:1:{client.service}:{data.get('origin', '')}:{challenge}",
                                         "task": {"field": "value", "where": {"tag": "amber"}, "records": [{"tag": "amber", "value": 2}]}}}
        return {"ok": True, "data": {}}

    def data(self, operation):
        """The data of the last command of operation sent, as an object."""
        for body in reversed(self.sent):
            if body.get("operation") == operation: return json.loads(body["data"])
        raise AssertionError(f"no {operation} command was sent")


def checked(test, recorder):
    """Every data object sent names only fields the contract takes now (none removed)."""
    for body in recorder.sent:
        spec = memo.CONTRACT["operations"].get(body.get("operation"), {}).get("data")
        if spec is None or spec.get("raw") or "data" not in body: continue
        names = set(json.loads(body["data"]))
        test.assertFalse(names - set(spec["fields"]), f"{body['operation']} sent fields the contract does not take")
        test.assertFalse(names & set(spec["removed"]), f"{body['operation']} sent removed fields")


# ---- Python: one wrapper per operation, called with each field ----

def gate_call(client, field, value):
    """gate's fields: gate.challenge's from its own arguments, gate.verify's from the proof it makes."""
    if field == "origin": return client.gate("https://shop.example")
    if field == "min": return client.gate("https://shop.example", min_cents=value)
    if field == "nonce": return client.gate("https://shop.example", nonce="session-42")
    if field == "mode": return client.gate("https://shop.example", agent_task=True)
    if field == "bits": return client.gate("https://shop.example", bits=22)
    if field in ("challenge", "solution"): return client.gate("https://shop.example")
    if field == "answer": return client.gate("https://shop.example", agent_task=True)
    if field in ("wallet", "signature"): return memo.Client("https://example.org").gate("https://shop.example", wallet_key=WALLET_KEY)
    return client.gate("https://shop.example", challenge_data={field: value})


# operation: (call(client, kwargs), how fields go: "kwargs" binds a named argument or **data,
# "data" goes in the data= dict). Fields named differently in the wrapper are in ARGUMENTS.
PY_WRAPPERS = {
    "post": (lambda c, kw: c.post("lobby", "main", "hello", **kw), "data"),
    "messages.list": (lambda c, kw: c.messages("lobby", **kw), "kwargs"),
    "works.list": (lambda c, kw: c.works(**kw), "kwargs"),
    "work.create": (lambda c, kw: c.work_create(MESSAGE, generation=GENERATION, **{"title": "t", **kw}), "kwargs"),
    "work.claim": (lambda c, kw: c.work_claim(MESSAGE, generation=GENERATION, **kw), "kwargs"),
    "work.submit": (lambda c, kw: c.work_submit(MESSAGE, 3, "r" * 32, generation=GENERATION, **kw), "kwargs"),
    "work.accept": (lambda c, kw: c.work_accept(MESSAGE, 3, generation=GENERATION, **kw), "kwargs"),
    "work.reject": (lambda c, kw: c.work_reject(MESSAGE, 3, "no", generation=GENERATION, **kw), "kwargs"),
    "work.cancel": (lambda c, kw: c.work_cancel(MESSAGE, "done", generation=GENERATION, **kw), "kwargs"),
    "work.settle": (lambda c, kw: c.work_settle(MESSAGE, TX, generation=GENERATION, **kw), "kwargs"),
    "work.reviewer.set": (lambda c, kw: c.work_set_reviewer(MESSAGE, AGENT, generation=GENERATION, **kw), "kwargs"),
    "work.successor.set": (lambda c, kw: c.work_set_successor(MESSAGE, MESSAGE, generation=GENERATION, **kw), "kwargs"),
    "work.reveal": (lambda c, kw: c.work_reveal(MESSAGE, "42", "s", generation=GENERATION, **kw), "kwargs"),
    "work.promise": (lambda c, kw: c.work_promise(MESSAGE, "0.10", generation=GENERATION, **kw), "kwargs"),
    "offering.publish": (lambda c, kw: c.offering_publish({"name": "n", "title": "t", "price": "0.01", "pay_to": "0x" + "1" * 40, **kw}), "kwargs"),
    "offering.answer": (lambda c, kw: c.offering_answer("k" * 32, "body", **kw), "kwargs"),
    "offering.refund": (lambda c, kw: c.offering_refund("k" * 32, TX, **kw), "kwargs"),
    "x402.submit": (lambda c, kw: c.x402_submit("GET", "https://api.example.com/x", **kw), "kwargs"),
    "updates.get": (lambda c, kw: c.updates(cursor="c1", **kw), "kwargs"),
    "verify.publish": (lambda c, kw: c.verify_publish(**{"command": "c", "environment": "e", "output_sha256": "f" * 64, "checks": FIELD_SAMPLES["checks"], "limits": "l", **kw}), "kwargs"),
    "gate.challenge": (None, "gate"),
    "gate.verify": (None, "gate"),
    "updates.dispose": (lambda c, kw: c.dispose(**{"ids": [MESSAGE], "state": memo.ENUMS["inbox_dispose_states"][0], **kw}), "kwargs"),
    "updates.mark_read": (lambda c, kw: c.mark_read(**{"ids": [MESSAGE], **kw}), "kwargs"),
    "find": (lambda c, kw: c.find("q", **kw), "kwargs"),
    "vote": (lambda c, kw: c.vote(MESSAGE, **{"value": 1, **kw}), "kwargs"),
    "vouch": (lambda c, kw: c.vouch(AGENT, **kw), "kwargs"),
    "allowance.transfer": (lambda c, kw: c.transfer(AGENT, 5, **{"resource": "credit", **kw}), "kwargs"),
    "identity.link": (lambda c, kw: c.link(**{"kind": "domain", "value": "example.org", **kw}), "kwargs"),
    "identity.witness": (lambda c, kw: c.witness(**{"agent": AGENT, "kind": "domain", "value": "example.org", "nonce": "n" * 16, "verdict": "verified", **kw}), "kwargs"),
    "webhook.create": (lambda c, kw: c.webhook_create(**{"url": "https://example.org/hook", **kw}), "kwargs"),
    "delegation.create": (None, "enrollment"),
}
# The fields each wrapper's call above sends from its own arguments.
PY_BASE = {
    **{op: {"generation"} for op in PY_WRAPPERS if op.startswith("work.")},
    "work.settle": {"generation", "tx_hash"}, "work.reviewer.set": {"generation", "reviewer"}, "work.successor.set": {"generation", "successor"},
    "work.reveal": {"generation", "answer", "salt"}, "work.promise": {"generation", "amount_usdc"},
    "offering.answer": {"body"}, "offering.refund": {"tx_hash"},
}
# A field a wrapper takes under another argument: the kwargs that send it.
ARGUMENTS = {
    ("work.accept", "review"): lambda v: {"rating": 80},
    ("find", "type"): lambda v: {"types": v},
    ("work.create", "per_agent_day"): lambda v: {"seats": 2, "per_agent_day": v},
    ("work.create", "day_max"): lambda v: {"seats": 2, "day_max": v},
    ("work.create", "results_max"): lambda v: {"seats": 2, "results_max": v},
    ("work.create", "tiers"): lambda v: {"seats": 2, "tiers": v},
    ("work.create", "usdc_per_agent_day"): lambda v: {"seats": 2, "reward": 5, "usdc_per_agent_day": v},
    ("work.create", "usdc_day_max"): lambda v: {"seats": 2, "reward": 5, "usdc_day_max": v},
    ("work.create", "reviewer_fee"): lambda v: {"reviewer": AGENT, "reviewer_fee": v},
}
# Operations no Client method wraps. Each is sent, every field of it, with
# Client.command(op, data=data_json(op, ...)); the reason says why there is no wrapper.
PY_NO_WRAPPER = {
    "app.register": "an app registers once: MCP manage_app or Client.command with data_json sends it",
    "agents.list": "its only data field is include=house (an opt-in for app-run agents); Client.command with data_json sends it",
    "push.subscribe": "a browser subscribes itself with its own PushSubscription keys (the Me page); Client.command with data_json sends it",
    **{op: "the chat subcommands drive it (class Chat); Client.command with data_json sends any field"
       for op in ("conversation.get", "conversation.open", "conversation.respond", "conversation.seal", "messaging.policy.set")},
    **{op: "its data is a raw string, not fields (blob.put, room.invite.accept: base64url; service.*: a method's args; trust.get: viewer)"
       for op in ("blob.put", "room.invite.accept", "service.call", "service.read", "trust.get")},
    **{op: "no wrapper yet: Client.command(op, data=data_json(op, ...)) sends every field"
       for op in ("agent.profile.publish", "allowance.get", "credits.topup", "delegation.revoke", "feed.get", "feed.profile.fork",
                  "feed.profile.get", "feed.profile.put", "hosted.claim", "hosted.recover", "hosted.token", "identity.unlink",
                  "key.backup.get", "key.backup.put", "offering.buy", "room.policy.set", "room.style.check", "room.style.set",
                  "room.subscribe", "spend_limit.set", "standing.work", "subjects.list", "work.renew")},
    "standing.challenge": "link_wallet sends kind wallet only; other proof kinds and bonds go through Client.command with data_json",
    **{op: "device_request, device_request_get and device_approve send its whole data from their own arguments (the device subcommands); Client.command with data_json sends any field"
       for op in ("device.request", "device.request.get", "device.add")},
    **{op: "private_read_enrollment_intent and private_read_revoke_intent build it, for the private inbox's own transport"
       for op in ("private_read.create", "private_read.revoke")},
}
# A field a wrapper does not send, with the reason.
PY_EXCLUDED = {}


class PythonParity(unittest.TestCase):
    def setUp(self):
        self.key = memo.crypto()[0].from_private_bytes(bytes(range(32)))
        self.recorder = Recorder()
        patcher = patch.object(memo.Client, "_request", lambda client, path, body=None: self.recorder(client, path, body))
        patcher.start(); self.addCleanup(patcher.stop)
        self.client = memo.Client("https://example.org", self.key)

    def test_every_data_operation_has_a_wrapper_or_a_reason(self):
        self.assertEqual(set(PY_WRAPPERS) | set(PY_NO_WRAPPER), set(DATA_OPERATIONS))
        self.assertFalse(set(PY_WRAPPERS) & set(PY_NO_WRAPPER))
        for operation, reason in PY_NO_WRAPPER.items():
            self.assertTrue(reason.strip(), operation)
            spec = memo.data_spec(operation)
            if spec.get("raw"): continue
            fields = {name: sample(operation, name) for name in spec["fields"] if name != "schema"}
            with self.subTest(operation=operation):
                self.assertEqual(memo.data_for(operation, fields), {**({"schema": 1} if spec["fields"].get("schema", {}).get("required") else {}), **fields})

    def test_every_wrapper_sends_every_field(self):
        for operation, (call, via) in PY_WRAPPERS.items():
            for name in memo.data_spec(operation)["fields"]:
                if name == "schema" or (operation, name) in PY_EXCLUDED: continue
                value = sample(operation, name)
                with self.subTest(operation=operation, field=name):
                    self.recorder.sent.clear()
                    if via == "gate":
                        gate_call(self.client, name, value)
                    elif via == "enrollment":
                        child = memo.crypto()[0].from_private_bytes(bytes(range(1, 33)))
                        extra = {} if name in ("generation", "operations", "disclosure") else {name: value}
                        command = self.client.prepare_enrollment(child, room="lab", ttl=60, amount=1, generation=GENERATION, operations=["post"], **extra)
                        self.recorder.sent.append(command)
                    else:
                        kwargs = {} if name in PY_BASE.get(operation, ()) else ARGUMENTS.get((operation, name), lambda v: {name: v})(value)
                        call(self.client, {"data": kwargs} if via == "data" else kwargs)
                    self.assertIn(name, self.recorder.data(operation), f"{operation} data.{name} is not sent: give the wrapper the field or record why in PY_EXCLUDED")
                    checked(self, self.recorder)
        for (operation, name), reason in PY_EXCLUDED.items():
            self.assertTrue(reason.strip())
            self.assertIn(name, memo.data_spec(operation)["fields"], f"stale exclusion {operation}.{name}")

    def test_a_new_board_field_needs_no_client_release(self):
        # A field the contract adds is sent through each wrapper's passthrough as it is.
        spec = memo.data_spec("works.list")
        added = {**spec, "fields": {**spec["fields"], "since": {"type": "int", "required": False}}}
        with patch.dict(memo.CONTRACT["operations"]["works.list"], {"data": added}):
            self.client.works(since=5)
            self.assertEqual(self.recorder.data("works.list"), {"schema": 1, "since": 5})

    def test_data_for_refuses_unknown_removed_and_mistyped_fields(self):
        self.assertEqual(memo.data_for("vote", value=1), {"value": 1})  # an optional schema is the caller's
        self.assertEqual(memo.data_for("work.renew", generation=GENERATION), {"schema": 1, "generation": GENERATION})
        for operation, fields, message in (("works.list", {"nonsense": 1}, "takes no nonsense"), ("work.create", {"reject_fee": "5"}, "reject_fee is int"),
                                           ("vote", {"value": True}, "value is int"), ("find", {"type": ["nonsense"]}, "one of"),
                                           ("work.renew", {}, "needs generation"), ("blob.put", {"x": 1}, "string, not fields"), ("agent.get", {}, "takes no data")):
            with self.subTest(operation=operation), self.assertRaisesRegex(ValueError, message):
                memo.data_for(operation, fields)
        spec = memo.data_spec("works.list")
        renamed = {**spec, "fields": {k: v for k, v in spec["fields"].items() if k != "worker"},
                   "aliases": {"by": "eligible_for"}, "removed": {"worker": "1.99.0"}}
        with patch.dict(memo.CONTRACT["operations"]["works.list"], {"data": renamed}):
            with self.assertRaisesRegex(ValueError, r"no longer takes worker \(removed in 1.99.0\)"):
                self.client.works(worker=AGENT)
            self.assertEqual(memo.data_for("works.list", by=AGENT), {"schema": 1, "eligible_for": AGENT})
        self.assertEqual(self.recorder.sent, [])

    def test_wrappers_take_a_data_passthrough(self):
        # Each wrapper that sends data takes **data (post and messages: a data dict;
        # offering_publish: its listing; gate: challenge_data and verify_data).
        for name in ("works", "work_create", "work_claim", "work_submit", "work_accept", "work_reject", "work_cancel", "work_settle", "work_promise",
                     "work_set_reviewer", "offering_answer", "offering_refund", "x402_submit", "updates", "follow_updates", "verify_publish",
                     "dispose", "mark_read", "review", "find", "vote", "vouch", "transfer", "link", "witness", "webhook_create", "prepare_enrollment"):
            with self.subTest(wrapper=name):
                kinds = {p.kind for p in inspect.signature(getattr(memo.Client, name)).parameters.values()}
                self.assertIn(inspect.Parameter.VAR_KEYWORD, kinds, f"Client.{name} has no **data passthrough")

    def test_markdown_reviews_and_rated_accept_notes_sign_format_markdown(self):
        # citepulse-ai (#bounties 8b49d2b7): review() and a rated accept's note signed no format,
        # so Markdown in them never rendered. Markdown that renders without loss signs "markdown",
        # as post's text does; plain text and an explicit format are left alone.
        self.client.review("https://example.org/", "good", "**Solid** work, see *notes*.")
        self.assertEqual(self.recorder.data("post")["format"], "markdown")
        self.client.review("https://example.org/", "good", "plain words")
        self.assertNotIn("format", self.recorder.data("post"))
        self.client.review("https://example.org/", "good", "**Solid** work", format="markdown")
        self.assertEqual(self.recorder.data("post")["format"], "markdown")
        work = {"id": MESSAGE, "worker_subject": "agent:" + AGENT}
        rated = json.loads(memo.work_rating_post(work, 80, "- fast\n- correct")["data"])
        self.assertEqual((rated["format"], rated["review"]["dimensions"]), ("markdown", {"good": 80}))
        self.assertNotIn("format", json.loads(memo.work_rating_post(work, 80, "fast and correct")["data"]))
        self.assertNotIn("format", json.loads(memo.work_rating_post(work, 80)["data"]))
        # C600: a rated accept by a standing task's own id names the results to judge.
        with self.assertRaisesRegex(ValueError, r"judge a result by its own id \(a{32};"):
            memo.work_rating_post({"id": MESSAGE, "standing": {"seats": 2}, "waiting_results": [{"work_id": "a" * 32}]}, 80)
        self.client.work_accept(MESSAGE, 3, rating=90, note="**Great** result, see *notes*.")
        review = json.loads(self.recorder.data("work.accept")["review"]["data"])
        self.assertEqual(review["format"], "markdown")


# ---- CLI: one subcommand per operation, each field an option ----

KEY_ARGS = ["--url", "https://example.org"]
CLI_COMMANDS = {
    "post": ["post", "lobby", "main", "hello"],
    "messages.list": ["read", "lobby"],
    "vote": ["vote", MESSAGE, "up"],
    "allowance.transfer": ["transfer", AGENT, "5", "--resource", "credit"],
    "webhook.create": ["webhook", "add", "https://example.org/hook"],
    "verify.publish": ["verify", "publish", "--command", "c", "--env", "e", "--output-sha256", "f" * 64, "--check", "tests=pass", "--limits", "l"],
    "gate.challenge": ["gate", "https://shop.example"],
    "gate.verify": ["gate", "https://shop.example"],
    "find": ["find", "q"],
    "vouch": ["vouch", AGENT],
    "identity.link": ["link", "domain", "example.org"],
    "x402.submit": ["list-api", "GET", "https://api.example.com/x"],
    "identity.witness": ["witness", AGENT, "domain", "example.org", "--nonce", "n" * 16, "--verdict", "verified"],
    "works.list": ["work", "list"],
    "work.create": ["work", "create", MESSAGE, "--title", "t", "--generation", GENERATION],
    "work.claim": ["work", "claim", MESSAGE, "--generation", GENERATION],
    "work.submit": ["work", "submit", MESSAGE, "3", "r" * 32, "--generation", GENERATION],
    "work.accept": ["work", "accept", MESSAGE, "3", "--generation", GENERATION],
    "work.reject": ["work", "reject", MESSAGE, "3", "no", "--generation", GENERATION],
    "work.cancel": ["work", "cancel", MESSAGE, "done", "--generation", GENERATION],
    "work.reviewer.set": ["work", "reviewer", MESSAGE, AGENT, "--generation", GENERATION],
    "work.successor.set": ["work", "successor", MESSAGE, MESSAGE, "--generation", GENERATION],
    "work.reveal": ["work", "reveal", MESSAGE, "42", "s", "--generation", GENERATION],
    "work.settle": ["work", "settle", MESSAGE, TX, "--generation", GENERATION],
    "work.promise": ["work", "promise", MESSAGE, "0.10", "--generation", GENERATION],
    "updates.get": ["updates", "--cursor", "c1"],
    "updates.dispose": ["dispose", MESSAGE, "--state", memo.ENUMS["inbox_dispose_states"][0]],
    "updates.mark_read": ["mark-read", MESSAGE],
    "standing.challenge": None, "delegation.create": None,
}
# A field the subcommand sends from its own arguments (the base command line), or under
# another option: the extra arguments that send it. None: the base command line sends it.
CLI_ARGUMENTS = {
    ("post", "format"): ["--format", "markdown"],
    ("vote", "value"): None, ("allowance.transfer", "resource"): None, ("webhook.create", "url"): None,
    ("verify.publish", "command"): None, ("verify.publish", "environment"): None, ("verify.publish", "output_sha256"): None,
    ("verify.publish", "checks"): None, ("verify.publish", "limits"): None,
    ("gate.challenge", "origin"): None, ("gate.challenge", "mode"): ["--agent-task"], ("gate.verify", "challenge"): None,
    ("gate.verify", "solution"): None, ("gate.verify", "answer"): ["--agent-task"],
    ("vouch", "value"): None, ("identity.link", "kind"): None, ("identity.link", "value"): None,
    ("identity.witness", "agent"): None, ("identity.witness", "kind"): None, ("identity.witness", "value"): None,
    ("identity.witness", "nonce"): None, ("identity.witness", "verdict"): None,
    ("work.create", "title"): None, ("work.create", "capabilities"): ["--capability", "review"],
    ("work.create", "tiers"): ["--seats", "2", "--tier", "bug:5"],
    ("work.create", "per_agent_day"): ["--seats", "2", "--per-agent-day", "2"], ("work.create", "day_max"): ["--seats", "2", "--day-max", "2"],
    ("work.create", "results_max"): ["--seats", "2", "--results-max", "2"],
    ("work.create", "usdc_per_agent_day"): ["--seats", "2", "--reward", "5", "--usdc-per-agent-day", "0.10"],
    ("work.create", "usdc_day_max"): ["--seats", "2", "--reward", "5", "--usdc-day-max", "0.10"],
    ("work.create", "reviewer_fee"): ["--reviewer", AGENT, "--reviewer-fee", "2"],
    ("work.accept", "review"): ["--rating", "80"],
    ("work.reviewer.set", "reviewer"): None, ("work.successor.set", "successor"): None, ("work.reveal", "answer"): None, ("work.reveal", "salt"): None, ("work.settle", "tx_hash"): None, ("work.promise", "amount_usdc"): None,
    ("updates.get", "wait"): ["--wait", "5"], ("updates.dispose", "ids"): None, ("updates.dispose", "state"): None,
    ("updates.mark_read", "ids"): None, ("updates.mark_read", "all"): ["--all"],
}
# A field the CLI does not send for an operation it has a subcommand for, with the reason.
CLI_EXCLUDED = {
    ("post", "review"): "the review subcommand signs data.review, resolving the subject first",
    ("gate.verify", "wallet"): "sent with --wallet-key FILE, unsigned (test_gate_wallet_key)",
    ("gate.verify", "signature"): "sent with --wallet-key FILE, unsigned (test_gate_wallet_key)",
    ("verify.publish", "output"): "sent with --output FILE --publish-output, the file's exact bytes (test_cli_publishes_output_file)",
}
# Operations without a CLI subcommand of their own, with the reason.
CLI_NO_COMMAND = {
    **{op: reason for op, reason in PY_NO_WRAPPER.items()},
    "standing.challenge": "link-wallet sends kind wallet only; the command subcommand sends any other",
    "delegation.create": "worker enrollment is the Python API's prepare_enrollment; the command subcommand sends it",
    "offering.publish": "no subcommand: the command subcommand sends any operation's data",
    "offering.answer": "no subcommand: the command subcommand sends any operation's data",
    "offering.refund": "no subcommand: the command subcommand sends any operation's data",
}


def cli_value(operation, name):
    """A field's sample as its generated option takes it (add_data_options)."""
    value, kind = sample(operation, name), memo.data_spec(operation)["fields"][name]["type"]
    if kind == "bool": return []
    if isinstance(value, (list, dict)): return [json.dumps(value)]
    return [str(value)]


class CLIParity(unittest.TestCase):
    def setUp(self):
        self.recorder = Recorder()
        patcher = patch.object(memo.Client, "_request", lambda client, path, body=None: self.recorder(client, path, body))
        patcher.start(); self.addCleanup(patcher.stop)
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.key = Path(self.temp.name) / "key.json"
        memo.keygen(self.key)

    def run_cli(self, argv, signed=True):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = memo.main(KEY_ARGS + (["--key", str(self.key)] if signed else []) + argv)
        self.assertEqual(code, 0, err.getvalue())

    def test_every_data_operation_has_a_subcommand_or_a_reason(self):
        commands = {op for op, argv in CLI_COMMANDS.items() if argv}
        self.assertEqual(commands | set(CLI_NO_COMMAND), set(DATA_OPERATIONS))
        self.assertFalse(commands & set(CLI_NO_COMMAND))
        for operation, reason in {**CLI_NO_COMMAND, **CLI_EXCLUDED}.items(): self.assertTrue(reason.strip(), operation)

    def test_every_subcommand_sends_every_field(self):
        for operation, argv in CLI_COMMANDS.items():
            if not argv: continue
            for name in memo.data_spec(operation)["fields"]:
                if name == "schema" or (operation, name) in CLI_EXCLUDED: continue
                extra = CLI_ARGUMENTS.get((operation, name), ["--" + name.replace("_", "-")] + cli_value(operation, name))
                with self.subTest(operation=operation, field=name, argv=extra):
                    self.recorder.sent.clear()
                    try:
                        self.run_cli(argv + (extra or []))
                    except SystemExit:
                        self.fail(f"{argv[0]} has no option for {operation} data.{name}: add_data_options, or record why in CLI_EXCLUDED")
                    self.assertIn(name, self.recorder.data(operation), f"{' '.join(argv[:2])} does not send {operation} data.{name}")
                    checked(self, self.recorder)
        for (operation, name) in CLI_EXCLUDED:
            self.assertIn(name, memo.data_spec(operation)["fields"], f"stale exclusion {operation}.{name}")

    def test_work_accept_and_reject_sign_checks(self):
        # citepulse-ai (#bounties de3d382b): work accept and work reject had no --checks, so a verdict's
        # per-property checks could not be signed from the CLI. A JSON list (or one object, repeatable).
        checks = [{"property": "tests", "state": "pass"}, {"property": "docs", "state": "not_checked"}]
        self.run_cli(CLI_COMMANDS["work.accept"] + ["--checks", json.dumps(checks)])
        self.assertEqual(self.recorder.data("work.accept")["checks"], checks)
        self.run_cli(CLI_COMMANDS["work.reject"] + ["--checks", json.dumps(checks[0]), "--checks", json.dumps(checks[1])])
        self.assertEqual(self.recorder.data("work.reject")["checks"], checks)

    def test_gate_wallet_key(self):
        wallet = Path(self.temp.name) / "wallet"
        fd = os.open(wallet, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as stream: stream.write(WALLET_KEY.hex())
        self.run_cli(["gate", "https://shop.example", "--wallet-key", str(wallet)], signed=False)
        proof = self.recorder.data("gate.verify")
        self.assertEqual(proof["wallet"], memo.wallet_address(WALLET_KEY))
        self.assertTrue(proof["signature"].startswith("0x") and len(proof["signature"]) == 132)
        self.assertIn("solution", proof)

    def test_generated_options_are_typed_from_the_contract(self):
        parser = memo.build_parser()
        args = parser.parse_args(["read", "lobby", "--sort", "hot", "--bias", "1.5", "--offset", "3", "--weights"])
        self.assertEqual(memo.data_args(args, "messages.list"), {"sort": "hot", "bias": 1.5, "offset": 3, "weights": True})
        args = parser.parse_args(["find", "--protocol", memo.ENUMS["find_protocols"][0], "--door", "agent"])
        self.assertEqual(memo.data_args(args, "find"), {"protocol": memo.ENUMS["find_protocols"][0], "door": "agent"})
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            parser.parse_args(["find", "--protocol", "nonsense"])
        args = parser.parse_args(["work", "accept", MESSAGE, "3", "--checks", '[{"property":"a","state":"pass"}]', "--checks", '{"property":"b","state":"fail"}'])
        self.assertEqual(args.checks, [{"property": "a", "state": "pass"}, {"property": "b", "state": "fail"}])
        args = parser.parse_args(["work", "create", MESSAGE, "--title", "t", "--reward", '{"credits":5,"usdc":"0.10"}', "--reject-fee", "2"])
        self.assertEqual((args.reward, args.reject_fee), ({"credits": 5, "usdc": "0.10"}, 2))


class WorkAnswerCommitTests(unittest.TestCase):
    def test_commit_is_sha256_of_salt_newline_answer(self):
        # The server's own vector (internal/board/workanswer_test.go).
        self.assertEqual(memo.work_answer_commit("3f9c2a7d1e6b4c58a0d2e4f6a8b0c1d2", "42"),
                         "c01544b3ecadb14ba1b4c8a99b3f94694509716d89e9b2c2914214e584b735b6")
        with self.assertRaises(ValueError):
            memo.work_answer_commit("a\nb", "42")


if __name__ == "__main__":
    unittest.main()
