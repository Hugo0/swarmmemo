"""Tests for scripts/trust/recompute.py: python3 -m unittest scripts/trust/test_recompute.py"""

import copy
import io
import json
import os
import sys
import tempfile
import unittest
from contextlib import redirect_stdout, redirect_stderr
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import recompute  # noqa: E402

ROOT = Path(__file__).resolve().parents[2]
EXPORT = ROOT / "internal/board/testdata/endorsement_export.jsonl"


def golden_dir():
    return Path(os.environ.get("TRUST_GOLDEN_DIR", ROOT / "internal/trust/testdata"))


class Ed25519(unittest.TestCase):
    # RFC 8032 §7.1, tests 1–3.
    VECTORS = [
        ("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a", "",
         "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"),
        ("3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c", "72",
         "92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00"),
        ("fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025", "af82",
         "6291d657deec24024827e69c3abe01a30ce548a284743a445e3680d7db5ac3ac18ff9b538d16f290ae67f760984dc6594a7c15e9716ed28dc027beceea1ec40a"),
    ]

    def test_rfc8032_vectors(self):
        for public, message, signature in self.VECTORS:
            pk, msg, sig = bytes.fromhex(public), bytes.fromhex(message), bytes.fromhex(signature)
            self.assertTrue(recompute.ed25519_verify(pk, msg, sig))
            self.assertFalse(recompute.ed25519_verify(pk, msg + b"x", sig))
            bad = bytearray(sig)
            bad[5] ^= 1
            self.assertFalse(recompute.ed25519_verify(pk, msg, bytes(bad)))
            # S ≥ L is refused, as Go refuses it.
            big = sig[:32] + (int.from_bytes(sig[32:], "little") + recompute._L).to_bytes(32, "little")
            self.assertFalse(recompute.ed25519_verify(pk, msg, big))
        self.assertFalse(recompute.ed25519_verify(b"\x00" * 31, b"", b"\x00" * 64))


class ExportVerification(unittest.TestCase):
    def lines(self):
        return EXPORT.read_text().splitlines()

    def test_go_export_verifies_offline(self):
        results = list(recompute.read_export(self.lines(), "swarmmemo.com"))
        problems = [p for _, p in results]
        self.assertEqual(problems, ["unsigned", None, None, None, None, None])
        legacy = results[0][0]
        self.assertEqual((legacy["type"], legacy["seq"], legacy["signature"]), ("legacy_vote", 0, None))

    def test_every_change_breaks_a_record(self):
        records = [json.loads(line) for line in self.lines()]
        vote, vouch = records[1], records[3]
        mutations = [
            (vote, lambda r: r.update(value=-1)),
            (vote, lambda r: r.update(message_id="0" * 32)),
            (vote, lambda r: r.update(type="vouch")),
            (vote, lambda r: r.update(signed_payload=r["signed_payload"].replace("n-c1", "n-c9"))),
            (vote, lambda r: r.update(public_key=vouch["public_key"])),
            (vouch, lambda r: r.update(sponsor=False)),
            (vouch, lambda r: r.update(value=0)),
            (vouch, lambda r: r.update(extra=1)),
            (vouch, lambda r: r.update(signature=r["signature"][:-2] + "AA")),
        ]
        for original, mutate in mutations:
            r = copy.deepcopy(original)
            mutate(r)
            self.assertIsNotNone(recompute.verify_record(r, "swarmmemo.com"), r)
        self.assertIsNotNone(recompute.verify_record(vote, "other.example"))
        self.assertIsNone(recompute.verify_record(vote, "swarmmemo.com"))

    def test_cli_verify_and_endorsements(self):
        out = io.StringIO()
        with redirect_stdout(out):
            self.assertEqual(recompute.main(["verify", str(EXPORT)]), 0)
        self.assertEqual(json.loads(out.getvalue()), {"failed": 0, "unsigned": 1, "verified": 5})
        out = io.StringIO()
        with redirect_stdout(out):
            self.assertEqual(recompute.main(["endorsements", str(EXPORT)]), 0)
        inputs = [json.loads(line) for line in out.getvalue().splitlines()]
        self.assertEqual([r["kind"] for r in inputs], ["legacy_vote", "vote", "vote", "vouch", "vouch", "vouch"])
        self.assertNotIn("seq", inputs[0])  # seq 0 is omitted, as the reference writes it
        self.assertNotIn("value", inputs[-1])  # a withdrawn vouch has value 0
        self.assertTrue(inputs[3]["sponsor"])

    def test_cli_refuses_a_tampered_export(self):
        lines = self.lines()
        record = json.loads(lines[2])
        record["value"] = 1
        lines[2] = json.dumps(record)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "export.jsonl"
            path.write_text("\n".join(lines) + "\n")
            err = io.StringIO()
            with redirect_stdout(io.StringIO()), redirect_stderr(err):
                self.assertEqual(recompute.main(["endorsements", str(path)]), 1)
            self.assertIn("seq 2", err.getvalue())

    def test_unsigned_vote_is_weight_zero_input(self):
        record = json.loads(self.lines()[1])
        record["signature"] = None
        self.assertEqual(recompute.verify_record(record, "swarmmemo.com"), "unsigned")
        self.assertEqual(recompute.trust_input(record)["kind"], "unsigned")


class Helpers(unittest.TestCase):
    def test_go_integer_semantics(self):
        self.assertEqual(recompute.gdiv(7, 2), 3)
        self.assertEqual(recompute.gdiv(-7, 2), -3)
        self.assertEqual(recompute.day_of(86399), 0)
        self.assertEqual(recompute.day_of(-1), -1)
        self.assertEqual(recompute.day_of(-86400), -1)
        self.assertEqual(recompute.day_of(-86401), -2)

    def test_curves(self):
        c = recompute.Curves()
        self.assertEqual(c.decay(951695, 0), 1_000_000)
        self.assertEqual(c.decay(951695, 1), 951695)
        self.assertEqual(c.decay(951695, 2), 951695 * 951695 // 1_000_000)
        self.assertEqual(c.decay(951695, 5000), c.decay(951695, 3650))
        self.assertEqual(c.ramp(951695, 1), 1_000_000 - 951695)

    def test_domain_root(self):
        suffixes = {"co.uk", "github.io"}
        self.assertEqual(recompute.domain_root("shop.bob.co.uk", suffixes), "domain:bob.co.uk")
        self.assertEqual(recompute.domain_root("Bob.CO.UK.", suffixes), "domain:bob.co.uk")
        self.assertEqual(recompute.domain_root("a.b.example.com", suffixes), "domain:example.com")
        self.assertEqual(recompute.domain_root("me.github.io", suffixes), "domain:me.github.io")
        self.assertEqual(recompute.domain_root("localhost", suffixes), "domain:localhost")

    def test_canonical_json(self):
        self.assertEqual(recompute.canonical({"b": 1, "a": ["<&>", "é", " "]}), '{"a":["<&>","é","\\u2028"],"b":1}')
        self.assertEqual(recompute.record_key({"type": "proof", "account": "a", "kind": "url", "link_value": "https://x/?a&b"}),
                         '{"type":"proof","account":"a","kind":"url","link_value":"https://x/?a\\u0026b"}')

    def test_union_find(self):
        u = recompute.UnionFind()
        u.union("b", "domain:x")
        u.union("a", "domain:x")
        self.assertEqual(u.find("a"), u.find("b"))
        self.assertEqual(u.find("domain:x"), "a")
        self.assertEqual(u.find("z"), "z")

    def test_network_is_a_max_flow(self):
        # s→a (5), s→b (5), a→t (3), b→t (4), a→b (2): max flow 7.
        net = recompute.Network(4, 10_000, 0)
        s, a, b, t = range(4)
        for u, v, c in [(s, a, 5), (s, b, 5), (a, t, 3), (b, t, 4), (a, b, 2)]:
            net.add(u, v, c)
        net.maxflow(s, t)
        self.assertEqual(net.flow_on(0) + net.flow_on(2), 7)
        tight = recompute.Network(4, 3, 0)
        tight.add(s, a, 1)
        with self.assertRaises(recompute.WorkExceeded):
            tight.maxflow(s, a)

    def test_strict_snapshot(self):
        with self.assertRaises(recompute.InputError):
            recompute.read_snapshot(['{"type":"meta","as_of":1,"extra":1}'])
        with self.assertRaises(recompute.InputError):
            recompute.read_snapshot(['{"type":"nope"}'])
        with self.assertRaises(recompute.InputError):
            recompute.read_snapshot(['{"type":"meta","as_of":"1"}'])
        with self.assertRaises(recompute.InputError):
            recompute.read_snapshot(['{"type":"meta","as_of":1}'])  # no params


class GoldenFixture(unittest.TestCase):
    """The Go reference's golden fixtures (internal/trust/testdata): the same
    inputs must give byte-identical output. TRUST_GOLDEN_DIR points elsewhere.
    golden is parameter version 1 (no standing); golden_standing_v2 is version
    2 (phase 1A, frozen); golden_standing is version 3 (the simulation's
    fixes)."""

    NAMES = ("golden", "golden_standing_v2", "golden_standing")

    def fixture(self, name):
        directory = golden_dir()
        inputs, output = directory / f"{name}_inputs.jsonl", directory / f"{name}_output.json"
        if not inputs.exists():
            self.skipTest(f"no golden fixture {name} at {directory}")
        return inputs, output

    def test_matches_reference(self):
        for name in self.NAMES:
            inputs, output = self.fixture(name)
            with inputs.open(encoding="utf-8") as f:
                snap = recompute.read_snapshot(f)
            got = recompute.canonical(recompute.compute(snap))
            self.assertEqual(got, output.read_text(encoding="utf-8").rstrip("\n"), name)
            self.assertEqual("standing" in json.loads(got), name.startswith("golden_standing"))

    def test_record_order_does_not_matter(self):
        for name in self.NAMES:
            inputs, output = self.fixture(name)
            lines = inputs.read_text(encoding="utf-8").splitlines()
            snap = recompute.read_snapshot(list(reversed(lines)))
            self.assertEqual(recompute.canonical(recompute.compute(snap)), output.read_text(encoding="utf-8").rstrip("\n"), name)


class Standing(unittest.TestCase):
    def test_vote_weight(self):
        self.assertEqual(recompute.vote_weight_ppm(0, 250000, 500), 0)
        self.assertEqual(recompute.vote_weight_ppm(125, 250000, 500), 625000)
        self.assertEqual(recompute.vote_weight_ppm(500, 250000, 500), 1_000_000)
        self.assertEqual(recompute.vote_weight_ppm(10**9, 250000, 500), 1_000_000)
        self.assertEqual(recompute.vote_weight_ppm(1, 250000, 500), 250000 + 750000 * 44721 // 1_000_000)
        # Version 3 gates v0 at v_floor_cents: below it no weight, so 500 keys
        # of 1 cent weigh less than one key of 500 cents.
        self.assertEqual(recompute.vote_weight_ppm(49, 250000, 500, 50), 0)
        self.assertEqual(recompute.vote_weight_ppm(50, 250000, 500, 50), 250000 + 750000 * 316227 // 1_000_000)
        self.assertLess(500 * recompute.vote_weight_ppm(1, 250000, 500, 50), recompute.vote_weight_ppm(500, 250000, 500, 50))

    def test_mul_div(self):
        self.assertEqual(recompute.mul_div(7, 3, 2), 10)
        self.assertEqual(recompute.mul_div(0, 3, 2), 0)
        self.assertEqual(recompute.mul_div(1 << 62, 1 << 40, 3), 2**63 - 1)

    def test_standing_params_are_checked(self):
        lines = (golden_dir() / "golden_standing_inputs.jsonl").read_text(encoding="utf-8").splitlines()
        params = next(json.loads(line) for line in lines if '"type":"params"' in line)
        bad = copy.deepcopy(params)
        bad["body"]["standing"]["extra"] = 1
        with self.assertRaises(recompute.InputError):
            recompute.read_snapshot([json.dumps(bad)])
        bad = copy.deepcopy(params)
        bad["body"]["standing"]["mode"] = "on"
        with self.assertRaises(recompute.InputError):
            recompute.read_snapshot([json.dumps(bad)])
        # Version 3's fields are optional (omitted when zero), never required.
        self.assertEqual(params["body"]["standing"]["rule"], 1)
        lean = copy.deepcopy(params)
        for k in recompute.STANDING_KEYS_V3:
            del lean["body"]["standing"][k]
        recompute.check_params(lean["body"])
        bad = copy.deepcopy(params)
        del bad["body"]["standing"]["pass_ppm"]
        with self.assertRaises(recompute.InputError):
            recompute.read_snapshot([json.dumps(bad)])


if __name__ == "__main__":
    unittest.main()
