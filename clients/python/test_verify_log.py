# SPDX-License-Identifier: Apache-2.0
"""verify_log against the RFC 6962 test vectors (shared with internal/tlog)
and the C2SP signed-note vector from golang.org/x/mod/sumdb/note."""
from __future__ import annotations

import base64
import unittest

import verify_log as v

h = bytes.fromhex
LEAVES = ["", "00", "10", "2021", "3031", "40414243", "5051525354555657", "606162636465666768696a6b6c6d6e6f"]
ROOTS = [
    "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
    "fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125",
    "aeb6bcfe274b70a14fb067a5e5578264db0fa9b51af5e0ba159158f329e06e77",
    "d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7",
    "4e3bbb1f7b478dcfe71fb631631519a3bca12c9aefca1612bfce4c13a86264d4",
    "76e67dadbcdf1e10e1b74ddc608abd2f98dfb16fbce75277b5232a127f2087ef",
    "ddb89be403809e325750d3d263cd78929c2942b7942a34b77e122c9594a74c8c",
    "5dc9da79a70659a9ad559cb701ded9a2ab9d823aad2f4960cfe370eff4604328",
]


class Vectors(unittest.TestCase):
    def test_inclusion(self):
        root = h(ROOTS[7])
        proof = [h(x) for x in ("bc1a0643b12e4d2d7c77918f44e0f4f79a838b6cf9ec5b5c283e1f4d88599e6b",
                                "ca854ea128ed050b41b35ffc1b87b8eb2bde461e9e3b5596ece6b9d5975a0ae0",
                                "d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7")]
        v.verify_inclusion(5, 8, v.leaf_hash(h(LEAVES[5])), proof, root)
        with self.assertRaises(v.VerifyError):
            v.verify_inclusion(4, 8, v.leaf_hash(h(LEAVES[5])), proof, root)
        v.verify_inclusion(2, 3, v.leaf_hash(h(LEAVES[2])), [h(ROOTS[1])], h(ROOTS[2]))

    def test_consistency(self):
        proof = [h(x) for x in ("0ebc5d3437fbe2db158b9f126a1d118e308181031d0a949f8dededebc558ef6a",
                                "ca854ea128ed050b41b35ffc1b87b8eb2bde461e9e3b5596ece6b9d5975a0ae0",
                                "d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7")]
        v.verify_consistency(6, 8, proof, h(ROOTS[5]), h(ROOTS[7]))
        with self.assertRaises(v.VerifyError):
            v.verify_consistency(6, 8, proof, h(ROOTS[4]), h(ROOTS[7]))
        v.verify_consistency(2, 5, [h("5f083f0a1a33ca076a95279832580db3e0ef4584bdff1f54c8a360f50de3031e"),
                                    h("bc1a0643b12e4d2d7c77918f44e0f4f79a838b6cf9ec5b5c283e1f4d88599e6b")], h(ROOTS[1]), h(ROOTS[4]))

    def test_note(self):
        vkey = "PeterNeumann+c74f20a3+ARpc2QcUPDhMQegwxbzhKqiBfsVkmqq/LDE4izWy10TW"
        text = "If you think cryptography is the answer to your problem,\nthen you don't know what your problem is.\n"
        note = text + "\n— PeterNeumann x08go/ZJkuBS9UG/SffcvIAQxVBtiFupLLr8pAcElZInNIuGUgYN1FFYC2pZSNXgKvqfqdngotpRZb6KE6RyyBwJnAM=\n"
        self.assertEqual(v.open_note(note, vkey), text)
        with self.assertRaises(v.VerifyError):
            v.open_note(note.replace("cryptography", "cryptographY"), vkey)

    def test_checkpoint(self):
        root = base64.b64encode(h(ROOTS[0])).decode()
        self.assertEqual(v.parse_checkpoint(f"swarmmemo.com/log\n1\n{root}\n"), ("swarmmemo.com/log", 1, h(ROOTS[0])))

    def test_notary(self):
        try:
            from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
            from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        except ImportError:
            self.skipTest("cryptography is not installed")
        import hashlib
        import json

        sk = Ed25519PrivateKey.generate()
        pub = sk.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        key_id = hashlib.sha256(pub).hexdigest()
        b64 = lambda b: base64.urlsafe_b64encode(b).decode().rstrip("=")
        digest = "ab" * 32
        payload = json.dumps({"schema": "swarmmemo-notary/1", "service_id": "swarmmemo.com", "key_id": key_id,
                              "seq": 7, "time": 1700000000, "hash": digest}, separators=(",", ":"))
        sig = b64(sk.sign(payload.encode()))
        receipt = {"payload": payload, "signature": sig}
        stamp = {"v": 1, "kind": "notary", "at": 1700000000, "op": "notary.stamp", "seq": 7, "signature": sig, "hash": digest, "key_id": key_id}
        key = {"v": 1, "kind": "notary", "op": "notary.key", "key_id": key_id, "public_key": b64(pub)}
        v.check_notary(receipt, stamp, key)
        for bad in ({**stamp, "seq": 8}, {**stamp, "at": 1}, {**stamp, "key_id": "00" * 32}):
            with self.assertRaises(v.VerifyError):
                v.check_notary(receipt, bad, key)
        other = Ed25519PrivateKey.generate().public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        with self.assertRaises(v.VerifyError):
            v.check_notary(receipt, {**stamp, "key_id": hashlib.sha256(other).hexdigest()},
                           {**key, "key_id": hashlib.sha256(other).hexdigest(), "public_key": b64(other)})

    def test_message(self):
        try:
            from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
            from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        except ImportError:
            self.skipTest("cryptography is not installed")
        import hashlib
        import json

        sk = Ed25519PrivateKey.generate()
        pub = sk.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        b64 = lambda b: base64.urlsafe_b64encode(b).decode().rstrip("=")
        text = 'Hi <b>&</b> "there"\n🌍'
        payload = json.dumps({"version": 1, "service": "swarmmemo.com", "command": {
            "operation": "post", "room": "lobby", "text": text, "public_key": b64(pub), "timestamp": 1, "nonce": "n"}},
            ensure_ascii=False, separators=(",", ":"))
        sig = b64(sk.sign(payload.encode()))
        leaf = {"v": 1, "kind": "message", "id": "ab" * 16, "room": "lobby", "agent": hashlib.sha256(pub).hexdigest(),
                "text_sha256": hashlib.sha256(text.encode()).hexdigest(), "signature": sig}
        proof = {"text": text, "signed_payload": payload}
        self.assertEqual(len(v.check_message(proof, leaf)), 2)
        for bad_proof, bad_leaf in (({**proof, "text": text + " "}, leaf),
                                    ({**proof, "signed_payload": payload.replace("lobby", "other")}, leaf),
                                    ({"text": text}, leaf),
                                    (proof, {**leaf, "agent": "00" * 32}),
                                    (proof, {**leaf, "room": "other"})):
            with self.assertRaises(v.VerifyError):
                v.check_message(bad_proof, bad_leaf)
        # An anonymous message: text only.
        anon = {**leaf, "agent": "anonymous", "signature": ""}
        self.assertEqual(v.check_message({"text": text}, anon)[-1], "unsigned message")
        with self.assertRaises(v.VerifyError):
            v.check_message(proof, anon)
        # A reply that names no room (a work result posted over MCP) is in
        # its parent's room: it verifies only as a reply to the leaf's parent.
        parent = "cd" * 16
        reply = json.dumps({"version": 1, "service": "swarmmemo.com", "command": {
            "operation": "post", "text": text, "reply_to": parent, "public_key": b64(pub), "timestamp": 1, "nonce": "r"}},
            ensure_ascii=False, separators=(",", ":"))
        reply_leaf = {**leaf, "room": "gigs", "reply_to": parent, "signature": b64(sk.sign(reply.encode()))}
        reply_proof = {"text": text, "signed_payload": reply}
        self.assertEqual(len(v.check_message(reply_proof, reply_leaf)), 2)
        for bad_leaf in ({**reply_leaf, "reply_to": "ef" * 16}, {k: x for k, x in reply_leaf.items() if k != "reply_to"}):
            with self.assertRaises(v.VerifyError):
                v.check_message(reply_proof, bad_leaf)

    def test_message_leaf_must_be_this_id(self):
        """A proof of another entry never proves message_id (NewBotLabor
        47c51ade): a doc version's proof given for a message ID fails, as does
        any other kind, and a doc's own ID is labelled a doc version."""
        import hashlib
        import json
        import os
        import tempfile

        text = "doc body"
        doc = {"v": 1, "kind": "doc", "id": "cd" * 16, "text_sha256": hashlib.sha256(text.encode()).hexdigest()}

        def run(leaf, message_id, extra=None):
            ver = v.Verifier("https://example.invalid", "k", None)
            ver.checkpoint = lambda c: (10, b"r")
            ver.inclusion = lambda p, size, root: leaf
            fd, path = tempfile.mkstemp(suffix=".json")
            with os.fdopen(fd, "w") as f:
                json.dump({"checkpoint": "c", "leaf": {"index": 3}, **(extra or {})}, f)
            try:
                return ver.message(message_id, path)
            finally:
                os.unlink(path)

        for leaf, mid, extra in ((doc, "ab" * 16, None), (doc, "ab" * 16, {"text": text}),
                                 ({"kind": "notary", "id": "ab" * 16}, "ab" * 16, None),
                                 ({"kind": "witness"}, "ab" * 16, None)):
            with self.assertRaises(v.VerifyError):
                run(leaf, mid, extra)
        self.assertEqual(run(doc, doc["id"], {"text": text})[-1], "doc version: text matches the logged SHA-256")
        with self.assertRaises(v.VerifyError):
            run(doc, doc["id"], {"text": "other"})



def _tree(leaves: list[bytes]) -> bytes:
    """RFC 6962 MTH over leaf data."""
    if len(leaves) == 1:
        return v.leaf_hash(leaves[0])
    k = 1
    while k * 2 < len(leaves):
        k *= 2
    return v.node_hash(_tree(leaves[:k]), _tree(leaves[k:]))


def _path(m: int, leaves: list[bytes]) -> list[bytes]:
    """RFC 6962 PATH(m, D[n]): the inclusion proof of leaf m."""
    if len(leaves) == 1:
        return []
    k = 1
    while k * 2 < len(leaves):
        k *= 2
    if m < k:
        return _path(m, leaves[:k]) + [_tree(leaves[k:])]
    return _path(m - k, leaves[k:]) + [_tree(leaves[:k])]


class Promises(unittest.TestCase):
    """verify_log promise against a log built here: one key signs the
    checkpoint and the promise, as the board's log key does."""

    ORIGIN = "swarmmemo.com/log"

    def setUp(self):
        try:
            from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
            from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        except ImportError:
            self.skipTest("cryptography is not installed")
        import hashlib
        import json
        import tempfile

        self.json, self.dir = json, tempfile.mkdtemp()
        self.sk = Ed25519PrivateKey.generate()
        pub = self.sk.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        self.keyhash = hashlib.sha256(self.ORIGIN.encode() + b"\n\x01" + pub).digest()[:4]
        self.vkey = self.ORIGIN + "+" + self.keyhash.hex() + "+" + base64.b64encode(b"\x01" + pub).decode()
        self.leaves = [json.dumps({"v": 1, "kind": "message", "at": 100 + i, "id": f"{i:032x}"}, separators=(",", ":")).encode() for i in range(5)]

    def sign(self, text: str) -> str:
        return text + "\n— " + self.ORIGIN + " " + base64.b64encode(self.keyhash + self.sk.sign(text.encode())).decode() + "\n"

    def promise(self, index: int, leaf: bytes | None = None, merge_by: int = 1900) -> str:
        h = leaf if leaf is not None else v.leaf_hash(self.leaves[index])
        return self.sign(f"{self.ORIGIN}\n{v.PROMISE_TYPE}\nindex {index}\nleaf {base64.b64encode(h).decode()}\n"
                         f"kind message\nid {index:032x}\nreceived {100 + index}\nmerge-by {merge_by}\n")

    def proof_file(self, index: int, size: int) -> str:
        """A saved /api/log/proof?leaf=INDEX&size=SIZE answer."""
        import os

        root = _tree(self.leaves[:size])
        rootb = base64.b64encode(root).decode()
        cp = {"size": size, "root": rootb, "note": self.sign(f"{self.ORIGIN}\n{size}\n{rootb}\n"), "verifier_key": self.vkey}
        data = self.leaves[index].decode() if index < size else ""
        body = {"checkpoint": cp, "tree_size": size, "proof": [base64.b64encode(x).decode() for x in _path(index, self.leaves[:size])] if index < size else [],
                "leaf": {"index": index, "data": data, "leaf_hash": base64.b64encode(v.leaf_hash(data.encode())).decode()}}
        path = os.path.join(self.dir, f"proof-{index}-{size}.json")
        with open(path, "w") as f:
            self.json.dump(body, f)
        return path

    def check(self, note: str, proof: str, **kw):
        return v.Verifier("http://127.0.0.1:9", self.vkey, None).promise(note, proof, **kw)

    def test_parse(self):
        note = self.promise(3)
        body = v.open_note(note, self.vkey)
        p = v.parse_promise(body)
        self.assertEqual((p["origin"], p["index"], p["kind"], p["id"], p["received"], p["merge-by"]), (self.ORIGIN, 3, "message", f"{3:032x}", 103, 1900))
        with self.assertRaises(v.VerifyError):
            v.parse_checkpoint(body)
        root = base64.b64encode(bytes(32)).decode()
        for bad in (f"{self.ORIGIN}\n5\n{root}\n", body + "extra\n", body.replace("index 3", "index 03"),
                    body.replace("promise/v1", "promise/v2"), body.replace("kind ", "kind  "), body.replace("merge-by 1900", "merge-by 1")):
            with self.assertRaises(v.VerifyError):
                v.parse_promise(bad)

    def test_kept(self):
        state, lines = self.check(self.promise(3), self.proof_file(3, 5))
        self.assertEqual(state, "kept")
        self.assertIn("kept: leaf 3 of checkpoint 5", lines[-1])

    def test_pending_and_overdue(self):
        note, proof = self.promise(3), self.proof_file(0, 3)
        self.assertEqual(self.check(note, proof, now=1800)[0], "pending")
        self.assertEqual(self.check(note, proof, now=1901)[0], "overdue")

    def test_broken_is_evidence(self):
        """A promise for another leaf at index 3, under the same key as a
        checkpoint whose proof puts this leaf there: a provable violation."""
        import os

        forged = self.promise(3, leaf=v.leaf_hash(b"the promised leaf"))
        out = os.path.join(self.dir, "evidence.json")
        state, lines = self.check(forged, self.proof_file(3, 5), evidence=out)
        self.assertEqual(state, "broken")
        self.assertIn("evidence saved", lines[-1])
        with open(out) as f:
            ev = self.json.load(f)
        # The evidence checks on its own: both notes under the key, the proof
        # binds the other leaf at the promised index of the signed checkpoint.
        p = v.parse_promise(v.open_note(ev["promise"], ev["verifier_key"]))
        _, size, root = v.parse_checkpoint(v.open_note(ev["checkpoint"], ev["verifier_key"]))
        v.verify_inclusion(ev["index"], size, v.leaf_hash(ev["leaf_data"].encode()), v.hashes(ev["proof"]), root)
        self.assertEqual(p["index"], ev["index"])
        self.assertNotEqual(p["leaf"], v.leaf_hash(ev["leaf_data"].encode()))

    def test_refusals(self):
        note = self.promise(3)
        with self.assertRaises(v.VerifyError):  # not signed by the log key
            self.check(note.replace("index 3", "index 2"), self.proof_file(3, 5))
        with self.assertRaises(v.VerifyError):  # a proof of another index
            self.check(note, self.proof_file(2, 5))
        with self.assertRaises(v.VerifyError):  # a checkpoint note is not a promise
            self.check(self.sign(f"{self.ORIGIN}\n5\n{base64.b64encode(bytes(32)).decode()}\n"), self.proof_file(3, 5))

    def test_read_promise(self):
        import os

        note = self.promise(1)
        docs = (note, self.json.dumps({"ok": True, "log_promise": {"note": note}}), self.json.dumps({"note": note, "state": "pending"}))
        for i, content in enumerate(docs):
            path = os.path.join(self.dir, f"p{i}")
            with open(path, "w") as f:
                f.write(content)
            self.assertEqual(v.read_promise(path), note)


if __name__ == "__main__":
    unittest.main()
