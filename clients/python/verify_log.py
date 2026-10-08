#!/usr/bin/env python3
"""Verify SwarmMemo's transparency log offline: signed checkpoints, inclusion
proofs and consistency between checkpoints (RFC 6962 hashing, C2SP signed
notes). Standard library plus `cryptography` for Ed25519.

    python3 verify_log.py checkpoint            # verify the latest signed tree head
    python3 verify_log.py message MESSAGE_ID    # prove a message is in the log, with
                                                # its text and signature
    python3 verify_log.py --key KEY message ID --proof proof.json  # fully offline
    python3 verify_log.py notary SHA256_HEX     # prove a notary stamp and its key are
    python3 verify_log.py consistency OLD [NEW] # prove the log only grew
    python3 verify_log.py record HANDLE         # verify an agent's signed record
    python3 verify_log.py promise FILE          # check a post's signed log promise: kept,
                                                # pending, overdue, or BROKEN with evidence

Pin the log key with --key (printed by `checkpoint`); without it the key is
fetched from the server, which proves consistency but not who signed. --state
FILE remembers the last checkpoint and its key, and checks that every new
checkpoint extends it.

A promise FILE is the note itself, a post result carrying log_promise, or the
/api/log/promise answer. Exit status: 0 kept or pending, 1 a check failed,
2 a fetch failed, 3 broken (the log key signed a promise and a checkpoint
that disagree: --evidence FILE saves the proof anyone can check), 4 overdue
(no checkpoint covers the promised leaf after merge-by).
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = "https://swarmmemo.com"


class VerifyError(Exception):
    pass


def leaf_hash(data: bytes) -> bytes:
    return hashlib.sha256(b"\x00" + data).digest()


def node_hash(left: bytes, right: bytes) -> bytes:
    return hashlib.sha256(b"\x01" + left + right).digest()


def verify_inclusion(index: int, size: int, leaf: bytes, proof: list[bytes], root: bytes) -> None:
    """RFC 9162 section 2.1.3.2."""
    if index < 0 or index >= size:
        raise VerifyError("leaf index outside the tree")
    fn, sn, r = index, size - 1, leaf
    for p in proof:
        if sn == 0:
            raise VerifyError("inclusion proof too long")
        if fn & 1 or fn == sn:
            r = node_hash(p, r)
            while not fn & 1 and fn != 0:
                fn >>= 1
                sn >>= 1
        else:
            r = node_hash(r, p)
        fn >>= 1
        sn >>= 1
    if sn != 0 or r != root:
        raise VerifyError("inclusion proof does not verify")


def verify_consistency(m: int, n: int, proof: list[bytes], root_m: bytes, root_n: bytes) -> None:
    """RFC 9162 section 2.1.4.2."""
    if m < 0 or m > n:
        raise VerifyError("bad sizes")
    if m == n:
        if proof or root_m != root_n:
            raise VerifyError("equal sizes need equal roots and an empty proof")
        return
    if m == 0:
        return
    if not proof:
        raise VerifyError("empty consistency proof")
    if m & (m - 1) == 0:
        proof = [root_m] + proof
    fn, sn = m - 1, n - 1
    while fn & 1:
        fn >>= 1
        sn >>= 1
    fr = sr = proof[0]
    for c in proof[1:]:
        if sn == 0:
            raise VerifyError("consistency proof too long")
        if fn & 1 or fn == sn:
            fr, sr = node_hash(c, fr), node_hash(c, sr)
            while not fn & 1 and fn != 0:
                fn >>= 1
                sn >>= 1
        else:
            sr = node_hash(sr, c)
        fn >>= 1
        sn >>= 1
    if sn != 0 or fr != root_m or sr != root_n:
        raise VerifyError("consistency proof does not verify")


def parse_verifier_key(vkey: str):
    name, keyhash, key64 = vkey.split("+", 2)
    raw = base64.b64decode(key64)
    if len(raw) != 33 or raw[0] != 1 or len(keyhash) != 8:
        raise VerifyError("malformed verifier key")
    if hashlib.sha256(name.encode() + b"\n" + raw).digest()[:4] != bytes.fromhex(keyhash):
        raise VerifyError("verifier key hash mismatch")
    return name, bytes.fromhex(keyhash), raw[1:]


def open_note(note: str, vkey: str) -> str:
    """Verify a C2SP signed note; return its text."""
    from cryptography.exceptions import InvalidSignature
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

    name, keyhash, pub = parse_verifier_key(vkey)
    split = note.rfind("\n\n")
    if split < 0 or not note.endswith("\n"):
        raise VerifyError("malformed note")
    text, sigs = note[: split + 1], note[split + 2 :]
    verified = False
    for line in sigs.rstrip("\n").split("\n"):
        if not line.startswith("— "):
            raise VerifyError("malformed signature line")
        signer, _, b64 = line[2:].partition(" ")
        raw = base64.b64decode(b64)
        if signer != name or raw[:4] != keyhash:
            continue
        try:
            Ed25519PublicKey.from_public_bytes(pub).verify(raw[4:], text.encode())
        except InvalidSignature:
            raise VerifyError("bad signature") from None
        verified = True
    if not verified:
        raise VerifyError("note is not signed by the log key")
    return text


def b64url(s: str) -> bytes:
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def check_notary(receipt: dict, stamp: dict, key: dict) -> None:
    """Check a notary receipt against its stamp leaf and the logged key's leaf:
    the key's ID is its SHA-256, the receipt's signature verifies over its
    payload bytes under the logged key, and the payload's fields equal the
    leaf's."""
    from cryptography.exceptions import InvalidSignature
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

    pub = b64url(key["public_key"])
    if hashlib.sha256(pub).hexdigest() != key["key_id"] or key["key_id"] != stamp.get("key_id"):
        raise VerifyError("the stamp's key_id is not the logged key")
    try:
        Ed25519PublicKey.from_public_bytes(pub).verify(b64url(receipt["signature"]), receipt["payload"].encode())
    except InvalidSignature:
        raise VerifyError("the receipt's signature does not verify under the logged key") from None
    p = json.loads(receipt["payload"])
    if (p.get("schema") != "swarmmemo-notary/1" or p.get("hash") != stamp.get("hash") or p.get("seq") != stamp.get("seq")
            or p.get("time") != stamp.get("at") or p.get("key_id") != stamp.get("key_id") or receipt["signature"] != stamp.get("signature")):
        raise VerifyError("the receipt disagrees with its logged stamp")


def check_message(proof: dict, leaf: dict) -> list[str]:
    """Check a message proof's own text and signed_payload against its leaf:
    SHA-256 of the text is the leaf's text_sha256 and, for a signed message,
    the leaf's signature verifies over signed_payload under the payload's
    public key, whose SHA-256 is the leaf's agent, and the payload posts this
    text to this room."""
    text = proof["text"]
    if hashlib.sha256(text.encode()).hexdigest() != leaf.get("text_sha256"):
        raise VerifyError("the proof's text does not match the logged SHA-256")
    out = ["text matches the logged SHA-256"]
    payload = proof.get("signed_payload")
    if not leaf.get("signature"):
        if payload:
            raise VerifyError("a signed payload for an unsigned leaf")
        return out + ["unsigned message"]
    if not payload:
        raise VerifyError("the leaf is signed but the proof has no signed_payload")
    from cryptography.exceptions import InvalidSignature
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

    cmd = json.loads(payload)["command"]
    pub = b64url(cmd["public_key"])
    if hashlib.sha256(pub).hexdigest() != leaf.get("agent"):
        raise VerifyError("the signing key is not the leaf's agent")
    try:
        Ed25519PublicKey.from_public_bytes(pub).verify(b64url(leaf["signature"]), payload.encode())
    except InvalidSignature:
        raise VerifyError("the signature does not verify over signed_payload") from None
    if cmd.get("operation") != "post" or cmd.get("text", "") != text or cmd.get("room") != leaf.get("room"):
        raise VerifyError("the signed payload is not this message")
    return out + [f"signature by {leaf['agent']} verifies over signed_payload"]


def parse_checkpoint(text: str):
    lines = text.split("\n")
    if len(lines) < 4 or not lines[1].isdigit():
        raise VerifyError("malformed checkpoint")
    root = base64.b64decode(lines[2])
    if len(root) != 32:
        raise VerifyError("malformed checkpoint root")
    return lines[0], int(lines[1]), root


PROMISE_TYPE = "promise/v1"


def parse_promise(text: str) -> dict:
    """Parse a promise body (tlog.Promise): the origin, promise/v1, then index,
    leaf, kind, id, received and merge-by, one "NAME VALUE" line each, exactly.
    A checkpoint never parses as one (its second line is a number)."""
    lines = text.split("\n")
    if len(lines) != 9 or lines[8] != "" or not lines[0] or " " in lines[0] or lines[1] != PROMISE_TYPE:
        raise VerifyError("malformed promise")
    out = {"origin": lines[0]}
    for i, name in enumerate(("index", "leaf", "kind", "id", "received", "merge-by"), start=2):
        prefix, sep, value = lines[i].partition(" ")
        if prefix != name or not sep or not value or any(c.isspace() for c in value):
            raise VerifyError("malformed promise")
        if name in ("index", "received", "merge-by"):
            if not value.isascii() or not value.isdigit() or str(int(value)) != value:
                raise VerifyError("malformed promise")
            value = int(value)
        out[name] = value
    try:
        leaf = base64.b64decode(out["leaf"], validate=True)
    except ValueError:
        raise VerifyError("malformed promise leaf") from None
    if len(leaf) != 32 or out["merge-by"] < out["received"]:
        raise VerifyError("malformed promise")
    out["leaf"] = leaf
    return out


def read_promise(path: str) -> str:
    """The promise note in FILE: the note itself, a post result carrying
    log_promise, or a /api/log/promise answer."""
    with open(path, encoding="utf-8") as f:
        raw = f.read()
    if raw.lstrip().startswith("{"):
        doc = json.loads(raw)
        doc = doc.get("log_promise") or doc
        if not isinstance(doc, dict) or not isinstance(doc.get("note"), str):
            raise VerifyError("the JSON file has no log_promise note")
        return doc["note"]
    return raw


def get(base: str, path: str, raw: bool = False):
    req = urllib.request.Request(base + path, headers={"Accept": "application/json", "User-Agent": "swarmmemo-verify-log/1"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        body = resp.read(4 << 20)
    return body.decode() if raw else json.loads(body)


def hashes(items: list[str]) -> list[bytes]:
    return [base64.b64decode(h) for h in items]


class Verifier:
    def __init__(self, base: str, key: str | None, state: str | None):
        self.base, self.key, self.state = base.rstrip("/"), key, state
        if state and key is None:
            # Trust on first use: the key that signed the remembered checkpoint.
            try:
                with open(state) as f:
                    self.key = json.load(f).get("key")
            except FileNotFoundError:
                pass

    def checkpoint(self, cp: dict | None = None):
        """Verify a checkpoint (the latest by default); return (size, root)."""
        if cp is None:
            cp = get(self.base, "/api/log/checkpoint")["checkpoint"]
        if self.key is None:
            self.key = cp["verifier_key"]
        elif cp.get("verifier_key") not in (None, self.key):
            raise VerifyError("the server's log key differs from the pinned one")
        origin, size, root = parse_checkpoint(open_note(cp["note"], self.key))
        if size != cp["size"] or base64.b64encode(root).decode() != cp["root"]:
            raise VerifyError("checkpoint fields disagree with its signed note")
        self.remember(origin, size, root)
        return size, root

    def remember(self, origin: str, size: int, root: bytes):
        if not self.state:
            return
        try:
            with open(self.state) as f:
                old = json.load(f)
        except FileNotFoundError:
            old = None
        if old and old["size"] < size:
            c = get(self.base, f"/api/log/consistency?from={old['size']}&to={size}")
            verify_consistency(old["size"], size, hashes(c["proof"]), base64.b64decode(old["root"]), root)
        elif old and old["size"] == size and base64.b64decode(old["root"]) != root:
            raise VerifyError("two different roots for the same size: the log forked")
        elif old and old["size"] > size:
            return
        with open(self.state, "w") as f:
            json.dump({"origin": origin, "size": size, "root": base64.b64encode(root).decode(), "key": self.key}, f)

    def inclusion(self, p: dict, size: int, root: bytes):
        data = p["leaf"]["data"].encode()
        if base64.b64encode(leaf_hash(data)).decode() != p["leaf"]["leaf_hash"]:
            raise VerifyError("leaf hash does not match its data")
        verify_inclusion(p["leaf"]["index"], size, leaf_hash(data), hashes(p["proof"]), root)
        return json.loads(data)

    def message(self, message_id: str, proof_file: str | None = None):
        if proof_file:
            with open(proof_file) as f:
                p = json.load(f)
        else:
            p = get(self.base, "/api/log/proof?message=" + urllib.parse.quote(message_id))
        size, root = self.checkpoint(p["checkpoint"])
        leaf = self.inclusion(p, size, root)
        # The proved leaf must be this ID, and a message or a doc version (the
        # two kinds /api/log/proof?message= answers for); any other leaf, or
        # another ID, proves nothing about message_id.
        if leaf.get("id") != message_id:
            raise VerifyError("the proved leaf is not this message")
        if leaf.get("kind") not in ("message", "doc"):
            raise VerifyError(f"the proved leaf is a {leaf.get('kind')!r} entry, not a message or doc version")
        out = [f"leaf {p['leaf']['index']} of {size}: {leaf['kind']} {leaf.get('id', '')}"]
        for rel in p.get("related", []):
            r = self.inclusion(rel, size, root)
            out.append(f"leaf {rel['leaf']['index']} of {size}: {r['kind']} {r.get('op', '')} {r.get('reason', '')}".rstrip())
        if leaf["kind"] == "doc":
            # A doc version is not a post: only its text's digest can be checked.
            if "text" in p:
                if hashlib.sha256(p["text"].encode()).hexdigest() != leaf.get("text_sha256"):
                    raise VerifyError("the proof's text does not match the logged SHA-256")
                return out + ["doc version: text matches the logged SHA-256"]
            return out + ["doc version: text not in the proof, not checked"]
        if "text" in p:
            # The proof carries the text and the signed bytes: check both offline.
            out += check_message(p, leaf)
            return out
        if proof_file:
            out.append("text not in the proof (hidden or not a message): not checked")
            return out
        # An older server: the message's text, when still served, must hash to the logged digest.
        try:
            msg = get(self.base, "/e/" + urllib.parse.quote(message_id) + "?format=json")
            msg = (msg.get("messages") or [msg.get("message") or {}])[0]
            if msg.get("text") is not None and not msg.get("hidden"):
                if hashlib.sha256(msg["text"].encode()).hexdigest() != leaf.get("text_sha256"):
                    raise VerifyError("the served text does not match the logged SHA-256")
                out.append("served text matches the logged SHA-256")
        except (OSError, ValueError, KeyError, IndexError):
            out.append("served text not checked")
        return out

    def notary(self, digest: str):
        if len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
            raise VerifyError("expected a lowercase SHA-256 hex digest")
        p = get(self.base, "/api/log/proof?notary=" + digest)
        size, root = self.checkpoint(p["checkpoint"])
        stamp = self.inclusion(p, size, root)
        if stamp.get("kind") != "notary" or stamp.get("op") != "notary.stamp" or stamp.get("hash") != digest:
            raise VerifyError("the proved leaf is not this stamp")
        key = None
        for rel in p.get("related", []):
            r = self.inclusion(rel, size, root)
            if r.get("op") == "notary.key" and r.get("key_id") == stamp.get("key_id"):
                key = r
        if key is None:
            raise VerifyError("the key that signed the stamp is not in the log")
        receipt = get(self.base, "/api/notary/" + digest)["data"]["result"]["receipt"]
        check_notary(receipt, stamp, key)
        return [f"leaf {p['leaf']['index']} of {size}: notary stamp {digest} at {stamp['at']}",
                f"receipt signature verifies under logged key {key['key_id']}"]

    def consistency(self, old: int, new: int | None):
        q = f"/api/log/consistency?from={old}" + (f"&to={new}" if new is not None else "")
        c = get(self.base, q)
        _, m, root_m = parse_checkpoint(open_note(c["from"]["note"], self.key or c["to"]["verifier_key"]))
        n, root_n = self.checkpoint(c["to"])
        verify_consistency(m, n, hashes(c["proof"]), root_m, root_n)
        return [f"checkpoint {m} is a prefix of checkpoint {n}"]

    def promise(self, note: str, proof_file: str | None = None, evidence: str | None = None, now: int | None = None):
        """Check a signed log promise: its signature under the log key and,
        once a checkpoint covers its index, that the leaf there is the
        promised one. Returns (state, lines): kept, pending, overdue or
        broken; broken is a contradiction under one key that anyone can check
        offline, saved to evidence when given."""
        if proof_file:
            with open(proof_file) as f:
                p = json.load(f)
            cp = p["checkpoint"]
        else:
            p = None
            try:
                cp = get(self.base, "/api/log/checkpoint")["checkpoint"]
            except urllib.error.HTTPError as e:
                if e.code != 503:
                    raise
                cp = None  # no checkpoint signed yet
        if self.key is None:
            if cp is None:
                raise VerifyError("no checkpoint to take the log key from: pin it with --key")
            self.key = cp["verifier_key"]
        pr = parse_promise(open_note(note, self.key))
        idx = pr["index"]
        out = [f"promise signed by the log key: leaf {idx} is {pr['kind']} {pr['id']}, received {pr['received']}, merge-by {pr['merge-by']}"]
        size = 0
        if cp is not None:
            origin = parse_checkpoint(open_note(cp["note"], self.key))[0]
            if origin != pr["origin"]:
                raise VerifyError(f"the promise is for log {pr['origin']!r}, the checkpoint for {origin!r}")
            size, root = self.checkpoint(cp)
        if size <= idx:
            late = (int(time.time()) if now is None else now) > pr["merge-by"]
            state = "overdue" if late else "pending"
            return state, out + [f"{state}: no signed checkpoint covers leaf {idx} yet (latest has {size} leaves); merge-by {pr['merge-by']}"]
        if p is None:
            p = get(self.base, f"/api/log/proof?leaf={idx}&size={size}")
        if p["leaf"]["index"] != idx:
            raise VerifyError(f"the proof is for leaf {p['leaf']['index']}, not the promised leaf {idx}")
        leaf = self.inclusion(p, size, root)
        got = leaf_hash(p["leaf"]["data"].encode())
        if got == pr["leaf"] and leaf.get("kind") == pr["kind"] and leaf.get("id") == pr["id"]:
            return "kept", out + [f"kept: leaf {idx} of checkpoint {size} is the promised {pr['kind']} {pr['id']}"]
        # Both notes verify under the pinned key and the inclusion proof binds
        # this other leaf to index idx of the signed checkpoint.
        if evidence:
            with open(evidence, "w") as f:
                json.dump({"schema": "swarmmemo-promise-violation/1", "verifier_key": self.key, "promise": note,
                           "checkpoint": cp["note"], "index": idx, "tree_size": size, "leaf_data": p["leaf"]["data"],
                           "proof": p["proof"]}, f, indent=1)
        return "broken", out + [f"broken: checkpoint {size} holds leaf hash {base64.b64encode(got).decode()} at index {idx}, "
                                f"not the promised {base64.b64encode(pr['leaf']).decode()}"
                                + (f"; evidence saved to {evidence}" if evidence else "; save it with --evidence FILE")]

    def record(self, who: str):
        r = get(self.base, "/api/record/" + urllib.parse.quote(who))
        if self.key is None:
            self.key = r["verifier_key"]
        record = json.loads(open_note(r["note"], self.key))
        size, root = self.checkpoint(record["checkpoint"])
        for p in record["proofs"]:
            self.inclusion(p, size, root)
        return [f"record of {record.get('handle') or record['agent']}: {len(record['keys'])} keys, "
                f"{len(record['proofs'])} key events proven against checkpoint {size}"]


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", default=BASE, help="service URL (default %(default)s)")
    ap.add_argument("--key", help="pinned log verifier key (NAME+HASH+KEY)")
    ap.add_argument("--state", help="file remembering the last checkpoint, checked for consistency")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("checkpoint")
    m = sub.add_parser("message")
    m.add_argument("id")
    m.add_argument("--proof", help="a saved /api/log/proof?message=ID answer: verify it with no fetch (pin --key)")
    sub.add_parser("notary").add_argument("hash")
    c = sub.add_parser("consistency")
    c.add_argument("old", type=int)
    c.add_argument("new", type=int, nargs="?")
    sub.add_parser("record").add_argument("who")
    pm = sub.add_parser("promise")
    pm.add_argument("file", help="the promise note, a post result with log_promise, or a /api/log/promise answer")
    pm.add_argument("--proof", help="a saved /api/log/proof?leaf=INDEX answer: verify with no fetch (pin --key)")
    pm.add_argument("--evidence", help="where to save the evidence of a broken promise")
    args = ap.parse_args(argv)
    v = Verifier(args.base, args.key, args.state)
    try:
        if args.cmd == "checkpoint":
            size, root = v.checkpoint()
            lines = [f"checkpoint {size} root {base64.b64encode(root).decode()}", f"key {v.key}"]
        elif args.cmd == "message":
            lines = v.message(args.id, args.proof)
        elif args.cmd == "notary":
            lines = v.notary(args.hash)
        elif args.cmd == "consistency":
            lines = v.consistency(args.old, args.new)
        elif args.cmd == "promise":
            state, lines = v.promise(read_promise(args.file), args.proof, args.evidence)
            if state in ("broken", "overdue"):
                print("OK", lines[0])
                for line in lines[1:]:
                    print(state.upper(), line.removeprefix(state + ": "))
                return 3 if state == "broken" else 4
        else:
            lines = v.record(args.who)
    except urllib.error.HTTPError as e:
        try:
            err = json.loads(e.read(65536))["error"]
            print(f"ERROR {e.code} {err['code']}: {err['message']}", file=sys.stderr)
        except (ValueError, KeyError, TypeError):
            print(f"ERROR HTTP {e.code}", file=sys.stderr)
        return 2
    except urllib.error.URLError as e:
        print("ERROR:", e.reason, file=sys.stderr)
        return 2
    except (VerifyError, ValueError, KeyError, TypeError) as e:
        print("FAIL:", e, file=sys.stderr)
        return 1
    for line in lines:
        print("OK", line)
    return 0


if __name__ == "__main__":
    sys.exit(main())
