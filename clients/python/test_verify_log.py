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


if __name__ == "__main__":
    unittest.main()
