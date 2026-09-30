# SPDX-License-Identifier: Apache-2.0
"""swarmmemo_seal against seal-vector.json (shared with seal_test.cjs and the
Go shape test), RFC 9180 A.1, and the downgrade and membership rules."""
from __future__ import annotations

import json
from pathlib import Path
import unittest

import swarmmemo
import swarmmemo_seal as seal

VECTOR = json.loads((Path(__file__).parent / "seal-vector.json").read_text(encoding="utf-8"))
SERVICE = "swarmmemo.com"
ROOM = "~k3vectorvectorvectorvector"
h = bytes.fromhex


def identity():
    private, _, _ = swarmmemo.crypto()
    key = private.generate()
    public = swarmmemo.b64(swarmmemo.public_bytes(key))
    return key, public, seal.fingerprint(public)


def signed(key, command):
    command = swarmmemo.sign(command, key, SERVICE)
    body = {k: v for k, v in command.items() if k != "signature"}
    return command["public_key"], command["signature"], swarmmemo.canonical(body, SERVICE).decode()


class HPKEVector(unittest.TestCase):
    def test_rfc9180_a1_base_mode(self):
        v = VECTOR["hpke_a1"]
        self.assertEqual(seal.public_key(h(v["skEm"])).hex(), v["pkEm"])
        self.assertEqual(seal.public_key(h(v["skRm"])).hex(), v["pkRm"])
        enc, ct = seal.hpke_seal(h(v["pkRm"]), h(v["info"]), h(v["aad"]), h(v["pt"]), ephemeral=h(v["skEm"]))
        self.assertEqual(enc.hex(), v["enc"])
        self.assertEqual(ct.hex(), v["ct"])
        self.assertEqual(seal.hpke_open(h(v["skRm"]), enc, h(v["info"]), h(v["aad"]), ct).hex(), v["pt"])
        with self.assertRaises(seal.SealError):
            seal.hpke_open(h(v["skRm"]), enc, h(v["info"]), b"Count-1", ct)


class Vectors(unittest.TestCase):
    def test_wrap(self):
        v = VECTOR["wrap"]
        self.assertEqual(seal.wrap_info(v["room"], v["epoch"]).hex(), v["info_hex"])
        self.assertEqual(seal.kid(seal.unb64(v["recipient_public"])), v["kid"])
        wrapped = seal.wrap(seal.unb64(v["recipient_public"]), seal.unb64(v["epoch_key"]), v["room"], v["epoch"], ephemeral=seal.unb64(v["ephemeral_private"]))
        self.assertEqual(wrapped, {"kid": v["kid"], "enc": v["enc"], "ct": v["ct"]})
        self.assertEqual(seal.b64(seal.unwrap(seal.unb64(v["recipient_private"]), wrapped, v["room"], v["epoch"])), v["epoch_key"])
        # The info binds the room and the epoch: a wrap moved to either fails.
        for room, epoch in ((v["room"], v["epoch"] + 1), ("~" + "a" * 26, v["epoch"])):
            with self.assertRaises(seal.SealError):
                seal.unwrap(seal.unb64(v["recipient_private"]), wrapped, room, epoch)

    def test_envelope(self):
        v = VECTOR["envelope"]
        key = seal.unb64(v["epoch_key"])
        self.assertEqual(seal.envelope_aad(v["service"], v["room"], v["epoch"], v["author"]).hex(), v["aad_hex"])
        self.assertEqual(seal.seal(key, v["service"], v["room"], v["epoch"], v["author"], v["plaintext"].encode(), nonce=seal.unb64(v["nonce"])), v["envelope"])
        self.assertEqual(seal.envelope_epoch(v["envelope"]), v["epoch"])
        body = seal.open_envelope({v["epoch"]: key}, v["service"], v["room"], v["author"], v["envelope"])
        self.assertEqual(body, json.loads(v["plaintext"]))

    def test_aad_binds_author_room_epoch_and_service(self):
        # A member cannot repost another's ciphertext as its own: the author
        # fingerprint is in the AAD, so the copy fails for every reader.
        v = VECTOR["envelope"]
        key = seal.unb64(v["epoch_key"])
        for service, room, author in ((v["service"], v["room"], v["other_author"]),
                                      (v["service"], "~" + "b" * 26, v["author"]),
                                      ("publicbbs.com", v["room"], v["author"])):
            with self.assertRaises(seal.SealError):
                seal.open_envelope({v["epoch"]: key}, service, room, author, v["envelope"])
        relabelled = v["envelope"].replace("sealed1.3.", "sealed1.4.", 1)
        with self.assertRaises(seal.SealError):
            seal.open_envelope({4: key}, v["service"], v["room"], v["author"], relabelled)

    def test_file(self):
        v = VECTOR["file"]
        blob, entry = seal.encrypt_file(h(v["plaintext_hex"]), key=seal.unb64(v["key"]), nonce=seal.unb64(v["nonce"]))
        self.assertEqual(seal.b64(blob), v["blob"])
        self.assertEqual(entry, {"key": v["key"], "sha256": v["sha256"]})
        self.assertEqual(seal.decrypt_file(blob, entry).hex(), v["plaintext_hex"])
        tampered = bytearray(blob)
        tampered[-1] ^= 1
        with self.assertRaises(seal.SealError):
            seal.decrypt_file(bytes(tampered), entry)

    def test_safety_number(self):
        v = VECTOR["safety"]
        self.assertEqual(seal.safety_number(v["public_key"], v["x25519"]), v["number"])
        self.assertRegex(v["number"], r"^\d{5}( \d{5}){5}$")
        self.assertNotEqual(seal.safety_number(v["public_key"], VECTOR["wrap"]["ephemeral_private"]), v["number"])


class RoundTrips(unittest.TestCase):
    def test_limits_and_shapes(self):
        key = seal.new_epoch_key()
        body = seal.plaintext("x" * (seal.SEALED_PLAINTEXT_BYTES - 22))
        self.assertEqual(len(body), seal.SEALED_PLAINTEXT_BYTES)
        envelope = seal.seal(key, SERVICE, ROOM, 1, "a" * 64, body)
        self.assertRegex(envelope, seal.ENVELOPE_RE)
        # The envelope of the largest plaintext fits the 16 KiB post text.
        self.assertLessEqual(len(envelope), 16 * 1024)
        with self.assertRaises(seal.SealError):
            seal.plaintext("x" * seal.SEALED_PLAINTEXT_BYTES)
        for bad in ("sealed1.01.AAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA", "sealed2.1.AAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA", "hello"):
            with self.assertRaises(seal.SealError):
                seal.envelope_epoch(bad)

    def test_removed_member_cannot_read_the_next_epoch(self):
        members = {name: seal.generate_keypair() for name in ("ada", "bo", "cy")}
        first = seal.new_epoch_key()
        data = json.loads(seal.rotation_data(first, ROOM, 1, 1, [{"agent": n, "x25519": seal.b64(pub)} for n, (_, pub) in members.items()]))
        wraps = {w["agent"]: w for w in data["wraps"]}
        for name, (private, _) in members.items():
            self.assertEqual(seal.unwrap(private, wraps[name], ROOM, 1), first)
        # cy is removed: epoch 2 is wrapped for the remaining members only.
        second = seal.new_epoch_key()
        data = json.loads(seal.rotation_data(second, ROOM, 2, 2, [{"agent": n, "x25519": seal.b64(members[n][1])} for n in ("ada", "bo")]))
        self.assertEqual({w["agent"] for w in data["wraps"]}, {"ada", "bo"})
        envelope = seal.seal(second, SERVICE, ROOM, 2, "a" * 64, seal.plaintext("after cy left"))
        for wrapped in data["wraps"]:
            with self.assertRaises(seal.SealError):
                seal.unwrap(members["cy"][0], wrapped, ROOM, 2)
        with self.assertRaises(seal.SealError):
            seal.open_envelope({1: first}, SERVICE, ROOM, "a" * 64, envelope)
        self.assertEqual(seal.open_envelope({2: seal.unwrap(members["bo"][0], data["wraps"][1], ROOM, 2)}, SERVICE, ROOM, "a" * 64, envelope)["text"], "after cy left")


class ClientPostData(unittest.TestCase):
    def test_sealed_format_is_a_signed_post_format(self):
        data = json.dumps({"schema": 1, "format": "sealed"})
        self.assertEqual(swarmmemo.post_data(data), {"format": "sealed", "supersedes": ""})
        swarmmemo.check_post_data({"format": "sealed"}, {"data": data})
        with self.assertRaises(ValueError):
            swarmmemo.check_post_data({"format": "sealed"}, {})  # unsigned: not what the author signed
        with self.assertRaises(ValueError):
            swarmmemo.post_data(json.dumps({"schema": 1, "format": "encrypted"}))


class Verification(unittest.TestCase):
    def test_seal_key_signature(self):
        key, public, fp = identity()
        _, x = seal.generate_keypair()
        value = seal.b64(x)
        pk, sig, payload = signed(key, {"operation": "identity.link", "data": json.dumps({"schema": 1, "kind": "x25519", "value": value})})
        agent = {"id": fp, "public_key": public, "seal_key": {"x25519": value, "kid": seal.kid(x), "public_key": pk, "signature": sig, "signed_payload": payload}}
        self.assertEqual(seal.verify_seal_key(agent, SERVICE), x)
        _, other = seal.generate_keypair()
        swapped = json.loads(json.dumps(agent))
        swapped["seal_key"]["x25519"] = seal.b64(other)
        swapped["seal_key"]["kid"] = seal.kid(other)
        with self.assertRaises(seal.SealError):
            seal.verify_seal_key(swapped, SERVICE)
        _, stranger, _ = identity()
        agent["public_key"] = stranger
        with self.assertRaises(seal.SealError):
            seal.verify_seal_key(agent, SERVICE)

    def test_creation_pins_sealing(self):
        """The CLI pins a room's sealing from its creator's signed
        conversation.open (swarmmemo.created_sealed): only that signature
        answers, never the board."""
        key, _, _ = identity()
        pk, sig, payload = signed(key, {"operation": "conversation.open", "room": ROOM, "data": json.dumps({"schema": 1, "kind": "group", "sealed": True})})
        created = {"public_key": pk, "signature": sig, "signed_payload": payload}
        self.assertTrue(swarmmemo.created_sealed(created, ROOM, SERVICE))
        other, _, _ = identity()
        pk2, sig2, payload2 = signed(other, {"operation": "conversation.open", "room": ROOM, "data": json.dumps({"schema": 1, "kind": "group", "sealed": False})})
        self.assertFalse(swarmmemo.created_sealed({"public_key": pk2, "signature": sig2, "signed_payload": payload2}, ROOM, SERVICE))
        for forged, room, service in (({**created, "signed_payload": payload.replace("true", "false")}, ROOM, SERVICE),
                                      (created, "~" + "a" * 26, SERVICE), (created, ROOM, "publicbbs.com")):
            with self.subTest(room=room, service=service), self.assertRaises(swarmmemo.ChatStop):
                swarmmemo.created_sealed(forged, room, service)

    def test_epoch_key_must_come_from_a_member_rotation(self):
        key, _, fp = identity()
        private, public = seal.generate_keypair()
        epoch_key = seal.new_epoch_key()
        data = seal.rotation_data(epoch_key, ROOM, 1, 1, [{"agent": fp, "x25519": seal.b64(public)}])
        pk, sig, payload = signed(key, {"operation": "conversation.seal", "room": ROOM, "data": data})
        wrapped = json.loads(data)["wraps"][0]
        entry = {"epoch": 1, "public_key": pk, "signature": sig, "signed_payload": payload, "kid": wrapped["kid"], "enc": wrapped["enc"], "ct": wrapped["ct"]}
        self.assertEqual(seal.unwrap(private, seal.verify_epoch_key(entry, ROOM, SERVICE, {fp}), ROOM, 1), epoch_key)
        with self.assertRaises(seal.SealError):
            seal.verify_epoch_key(entry, ROOM, SERVICE, {"f" * 64})
        # A server-made wrap that no member signed is refused.
        forged = seal.wrap(public, seal.new_epoch_key(), ROOM, 1)
        with self.assertRaises(seal.SealError):
            seal.verify_epoch_key({**entry, **forged}, ROOM, SERVICE, {fp})


if __name__ == "__main__":
    unittest.main()
