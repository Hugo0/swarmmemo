"""Client.work_create and `work create` on a disposable local board (set
SWARMMEMO_TEST_BINARY); never a public service."""
import contextlib
import io
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch
import urllib.error
import urllib.request

import swarmmemo as memo


@unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY"), "set SWARMMEMO_TEST_BINARY for a real board")
class LiveWorkCreateTests(unittest.TestCase):
    def setUp(self):
        folder = tempfile.TemporaryDirectory()
        self.addCleanup(folder.cleanup)
        self.work = Path(folder.name)
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        self.env = {**os.environ, "DATA_DIR": str(self.work / "data"), "LISTEN_ADDR": f"127.0.0.1:{port}", "ALLOW_INSECURE_LOCAL": "true",
                    "ALLOWANCE_LEDGER": "on"}
        self.server = subprocess.Popen([os.environ["SWARMMEMO_TEST_BINARY"], "serve"], env=self.env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(self.stop)
        self.origin = f"http://127.0.0.1:{port}"
        for _ in range(200):
            try:
                with urllib.request.urlopen(self.origin + "/health", timeout=1): break
            except OSError:
                if self.server.poll() is not None: self.fail("server exited during startup")
                time.sleep(0.05)
        self.keys = {}
        for name in ("requester", "reviewer"):
            self.keys[name] = self.work / f"{name}.json"
            memo.keygen(self.keys[name])
            self.api(name).command("post", room="lobby", text=f"{name} is here", request_id=memo.uuid.uuid4().hex)
        self.api("reviewer").command("agent.register", handle="review-bot")
        self.api("requester").command("agent.register", handle="requester-bot")

    def stop(self):
        self.server.terminate()
        self.server.wait(timeout=10)

    def api(self, who):
        return memo.Client(self.origin, memo.load_key(self.keys[who]))

    def fp(self, who):
        return memo.hashlib.sha256(memo.public_bytes(memo.load_key(self.keys[who]))).hexdigest()

    def operator(self, *argv):
        return subprocess.run([os.environ["SWARMMEMO_TEST_BINARY"], *argv], env=self.env, check=True, capture_output=True, text=True).stdout

    def fund_board(self):
        """Give the board a credit budget for the operator to grant from."""
        params = json.loads(self.operator("params", "show", "allowance"))["body"]
        params["resources"]["credit"].update(budget=100_000, spend_ceiling=100_000, inbound_cap=100_000, grant_share_ppm=1_000_000)
        (self.work / "allowance.json").write_text(json.dumps(params))
        self.operator("params", "set", "allowance", str(self.work / "allowance.json"), "--reason", "test credit")

    def grant(self, who, credits):
        """Operator-granted credit, which is transferable, so it can be held as a reward."""
        self.fund_board()
        self.operator("allowance", "grant", self.fp(who), "credit", str(credits), "--reason", "test")

    def request(self, text):
        posted = self.api("requester").command("post", room="lobby", kind="request", text=text, request_id=memo.uuid.uuid4().hex)
        return posted["receipt"]["id"]

    def read(self, message_id):
        with urllib.request.urlopen(f"{self.origin}/api/work/{message_id}", timeout=5) as response:
            return json.loads(response.read())["data"]["work"]

    def test_rewarded_work_with_a_named_reviewer(self):
        self.grant("requester", 1000)
        message_id = self.request("Review my Go patch")
        self.api("requester").work_create(message_id, reward=300, reviewer="review-bot", reviewer_fee=20,
                                          reward_note="+ thanks", title="Review my Go patch", capabilities=["code-review"])
        work = self.read(message_id)
        self.assertEqual((work["state"], work["title"], work["reward"]["amount"], work["reward"]["state"]), ("open", "Review my Go patch", 300, "held"))
        self.assertEqual((work["reviewer"]["id"], work["reviewer_fee"]["amount"]), (self.fp("reviewer"), 20))
        self.assertEqual(work["reward_note"], "+ thanks")

    def test_cli_creates_unpaid_work(self):
        message_id = self.request("Write a haiku")
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = memo.main(["--url", self.origin, "--key", str(self.keys["requester"]), "work", "create", message_id,
                              "--title", "Write a haiku", "--capability", "writing", "--ttl", "86400"])
        self.assertEqual(code, 0, out.getvalue())
        work = self.read(message_id)
        self.assertEqual((work["state"], work["title"], work["capabilities"]), ("open", "Write a haiku", ["writing"]))
        self.assertNotIn("reward", work)

    def test_server_errors_come_through(self):
        message_id = self.request("Paid, but no credit to hold")
        with self.assertRaises(memo.APIError) as raised:
            self.api("requester").work_create(message_id, title=" ", capabilities=[])
        self.assertEqual(raised.exception.code, "invalid_work_data")
        with self.assertRaises(memo.APIError):  # no transferable credit: refused, nothing created
            self.api("requester").work_create(message_id, reward=300, title="Paid", capabilities=[])
        with self.assertRaises(urllib.error.HTTPError) as missing:
            self.read(message_id)
        self.assertEqual(missing.exception.code, 404)


class WorkListKindsTest(unittest.TestCase):
    def test_cli_takes_every_kind_the_server_lists(self):
        # works.list kind: the eight effective states, rewarded and earn (tali b9502e26), closed and all (C606).
        import swarmmemo
        parser = swarmmemo.build_parser()
        for kind in swarmmemo.WORK_LIST_KINDS:
            with self.subTest(kind=kind):
                self.assertEqual(parser.parse_args(["work", "list", "--kind", kind]).kind, kind)
        self.assertEqual(len(swarmmemo.WORK_LIST_KINDS), 13)


class ReviewRelationshipsTest(unittest.TestCase):
    def test_cli_takes_every_relationship_the_server_takes(self):
        import json, pathlib, swarmmemo
        server = json.loads((pathlib.Path(__file__).resolve().parents[1] / "contract.json").read_text())["enums"]["review_relationships"]
        self.assertEqual(swarmmemo.REVIEW_RELATIONSHIPS, server)
        parser = swarmmemo.build_parser()
        for value in server:
            with self.subTest(value=value):
                self.assertEqual(parser.parse_args(["review", "x402:GET https://x.example/y", "good", "t", "--relationship", value]).relationship, value)


class WorkAcceptTierTest(unittest.TestCase):
    def test_accept_names_a_standing_tier(self):
        import json, swarmmemo
        client = swarmmemo.Client("https://example.org", swarmmemo.crypto()[0].generate())
        sent = []
        client.command = lambda operation, **fields: sent.append((operation, fields)) or {"ok": True}
        client.work_accept("a" * 32, 1, generation="b" * 32, result_sha256="c" * 64, tier="bug+fix")
        operation, fields = sent[0]
        self.assertEqual(operation, "work.accept")
        self.assertEqual(json.loads(fields["data"])["tier"], "bug+fix")
        args = swarmmemo.build_parser().parse_args(["work", "accept", "a" * 32, "1", "--tier", "bug+fix"])
        self.assertEqual(args.tier, "bug+fix")


class StandingCreateTest(unittest.TestCase):
    """C311: a standing task's create builds the data testdata/work_standing_create.json
    names, from the CLI and from work_create; internal/board runs that data on the store."""
    vector = json.loads((Path(__file__).resolve().parents[2] / "testdata/work_standing_create.json").read_text())

    def test_cli_builds_the_vector(self):
        v = self.vector
        args = memo.build_parser().parse_args(["--key", "k.json", "work", "create", "a" * 32, "--title", v["title"], "--capability", *v["capabilities"], *v["argv"]])
        self.assertEqual(args.tiers, [{"name": "bug", "credits": 300, "usdc": "0.20"}, {"name": "bug+fix", "credits": 500, "usdc": "0.40"}])
        sent = []

        def fake(self, path, body=None):
            sent.append(body)
            return {"ok": True}
        key = memo.crypto()[0].from_private_bytes(bytes(32))
        with patch.object(memo, "load_key", return_value=key), patch.object(memo.Client, "_request", fake), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(memo.main(["--key", "k.json", "work", "create", "a" * 32, "--title", v["title"], "--capability", *v["capabilities"],
                                        "--generation", v["generation"], *v["argv"]]), 0)
        self.assertEqual([(b["operation"], b["data"]) for b in sent], [("work.create", v["data"])])

    def test_work_create_builds_the_vector(self):
        v = self.vector
        client = memo.Client("https://example.org", memo.crypto()[0].generate())
        sent = []
        client.command = lambda operation, **fields: sent.append((operation, fields)) or {"ok": True}
        client.work_create("a" * 32, generation=v["generation"], title=v["title"], capabilities=v["capabilities"], seats=5, per_agent_day=1, day_max=5,
                           tiers=["bug:300:0.20", {"name": "bug+fix", "credits": 500, "usdc": "0.40"}], usdc_per_agent_day="0.40", usdc_day_max="2.00", results_max=20, per_agent_max=1)
        self.assertEqual(sent[0][1]["data"], v["data"])
        self.assertEqual(memo.WORK_STANDING_CREATE, ("day_max", "per_agent_day", "per_agent_max", "results_max", "seats", "tiers", "usdc_day_max", "usdc_per_agent_day"))

    def test_vector_tiers_are_refused(self):
        # C331: the same strings clients/javascript refuses, e.g. a fourth ":" part.
        for tier in self.vector["tiers_refused"]:
            with self.subTest(tier=tier), self.assertRaises(ValueError):
                memo.standing_tier(tier)

    def test_bad_standing_fields_are_refused_before_sending(self):
        build = lambda **f: memo.work_create_data("t", "g", **f)
        for fields in ({"seats": 0, "reward": 5}, {"seats": memo.LIMITS["work_standing_seats_maximum"] + 1}, {"day_max": 3},
                       {"seats": 2, "per_agent_day": 3}, {"seats": 2, "tiers": ["a:1"] * 4}, {"seats": 2, "tiers": ["Bad:1"]},
                       {"seats": 2, "tiers": ["bug:0"]}, {"seats": 2, "tiers": ["bug"]}, {"seats": 2, "nonsense": 1},
                       {"seats": 2, "results_max": 1}, {"seats": 2, "results_max": memo.LIMITS["work_standing_results_maximum"] + 1},
                       {"seats": 2, "results_max": "3"}, {"results_max": 3}, {"seats": 2, "per_agent_max": 0},
                       {"seats": 2, "results_max": 3, "per_agent_max": 4}, {"per_agent_max": 1}):
            with self.subTest(fields=fields), self.assertRaises(ValueError):
                build(**fields)
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
            memo.build_parser().parse_args(["work", "create", "a" * 32, "--title", "t", "--tier", "bug:x"])


class WorkCancelTest(unittest.TestCase):
    """C346: the requester closes a standing task early with work.cancel, from the
    client and the CLI, signing its reason and the work's generation."""

    def test_client_and_cli_sign_work_cancel(self):
        client = memo.Client("https://example.org", memo.crypto()[0].generate())
        sent = []
        client.command = lambda operation, **fields: sent.append((operation, fields)) or {"ok": True}
        client.work_cancel("a" * 32, "closed: enough results", generation="b" * 32, request_id="r-1")
        self.assertEqual(sent, [("work.cancel", {"message_id": "a" * 32, "reason": "closed: enough results",
                                                 "data": '{"schema":1,"generation":"' + "b" * 32 + '"}', "request_id": "r-1"})])
        args = memo.build_parser().parse_args(["work", "cancel", "a" * 32, "closed"])
        self.assertEqual((args.work_action, args.message_id, args.reason), ("cancel", "a" * 32, "closed"))


class ListOfTest(unittest.TestCase):
    def test_a_lone_string_is_one_item(self):
        import swarmmemo
        self.assertEqual(swarmmemo.list_of("call:" + "b" * 32), ["call:" + "b" * 32])
        self.assertEqual(swarmmemo.list_of(("a", "b")), ["a", "b"])


class StandingResultMarkTest(unittest.TestCase):
    def test_a_standing_result_names_its_own_task(self):
        import swarmmemo
        rid, root = "a" * 32, "b" * 32
        ok = {"type": "message", "id": rid, "reply_to": root,
              "work": {"result_of": rid, "title": "Bug bounty", "state": "submitted", "url": "/work/" + rid}}
        swarmmemo.check_work(ok)
        plain = {"type": "message", "id": rid, "reply_to": root,
                 "work": {"result_of": root, "title": "Task", "state": "submitted", "url": "/work/" + root}}
        swarmmemo.check_work(plain)
        bad = {"type": "message", "id": rid, "reply_to": root,
               "work": {"result_of": "c" * 32, "title": "x", "state": "submitted", "url": "/work/" + "c" * 32}}
        with self.assertRaises(ValueError):
            swarmmemo.check_work(bad)


if __name__ == "__main__":
    unittest.main()

