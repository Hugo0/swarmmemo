#!/usr/bin/env python3
"""Private local mutation outbox. Default/status never contact the service."""
from __future__ import annotations

import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import stat
import sys
import time
from urllib.parse import urlsplit

import swarmmemo as memo

MUTATIONS = {
    "post": "room page text kind reply_to to handle visibility attachments",
    "room.create": "room visibility members",
    "room.member.add": "room target", "room.member.remove": "room target",
    "agent.register": "handle", "agent.rotate": "target",
    "credit.transfer": "target amount", "report": "message_id reason",
    "lease.acquire": "room target ttl", "lease.release": "room target amount",
    "blob.put": "room data filename media_type ttl visibility",
    "blob.delete": "message_id target reason",
    "agent.profile.publish": "data ttl", "agent.profile.remove": "",
    "work.create": "message_id data ttl", "work.claim": "message_id data ttl",
    "work.renew": "message_id data amount ttl", "work.submit": "message_id data amount target",
    "work.accept": "message_id data amount", "work.reject": "message_id data amount reason",
    "work.cancel": "message_id data reason",
    "delegation.create": "room target ttl amount data", "delegation.revoke": "target data",
    "private_read.create": "room target ttl data", "private_read.revoke": "room target data",
}
MAX_INTENT = 1406300
MAX_RECEIPT = 131072
DEFAULT_BYTES = 64 * 1024 * 1024
MAX_ROWS = 10000
TARGETED_MUTATIONS = frozenset({"post", "work.claim", "work.renew", "work.submit"})
SAFE_CODES = {"stale_signature", "idempotency_conflict", "key_rotated", "invalid_signature",
              "quota_exceeded", "rate_limited", "room_not_found", "forbidden", "not_found",
              "invalid_command", "unexpected_field", "field_limit", "envelope_too_large",
              "attachment_gone", "insufficient_credits", "lease_busy", "quota_exhausted", "global_quota_exhausted"}
SAFE_CODES.update({"work_generation_mismatch", "work_state_conflict", "work_fence_mismatch",
                   "work_forbidden", "work_exists", "work_renew_not_extended", "work_fence_exhausted",
                   "invalid_work_data", "invalid_work_root", "invalid_work_result", "invalid_ttl", "invalid_reason"})
SAFE_CODES.update({"invalid_delegation_context", "delegation_required", "delegation_not_found", "delegation_context_mismatch",
                   "delegation_inactive", "delegation_forbidden", "delegation_scope_mismatch", "delegation_quota_exhausted",
                   "delegation_exists", "delegation_limit", "delegation_already_revoked", "invalid_delegation_data",
                   "invalid_delegation_proof", "delegation_generation_mismatch"})
SAFE_CODES.update({"invalid_private_read_context", "invalid_private_read_data", "invalid_private_read_proof",
                   "private_read_exists", "private_read_limit", "private_read_generation_mismatch",
                   "private_read_epoch_mismatch", "private_read_already_revoked", "private_read_rate_limited",
                   "private_read_response_limit", "invalid_limit"})
DELEGATION_LOCAL_CODES = {"invalid_delegation_context", "delegation_required", "delegation_context_mismatch", "delegation_key_mismatch",
                          "delegation_client_binding_mismatch", "delegation_forbidden", "delegation_scope_mismatch",
                          "delegation_public_post_required", "delegation_generation_mismatch", "delegation_signed_envelope_required"}


class OutboxError(ValueError):
    """Only fixed, nonsensitive error codes are emitted."""


def encoded(value, *, sorted_keys=False):
    return json.dumps(value, ensure_ascii=False, sort_keys=sorted_keys, allow_nan=False).encode("utf-8")


def strict_json(raw):
    def pairs(values):
        result = {}
        for key, value in values:
            if key in result: raise OutboxError("duplicate_json_field")
            result[key] = value
        return result
    def constant(_): raise OutboxError("invalid_json_number")
    try:
        return json.loads(raw, object_pairs_hook=pairs, parse_constant=constant)
    except (ValueError, UnicodeError) as exc:
        if isinstance(exc, OutboxError): raise
        raise OutboxError("invalid_json") from None


def sha(raw): return hashlib.sha256(raw).hexdigest()


def matching_text(actual, expected):
    return isinstance(actual, str) and bool(actual) and actual == expected


def validate_ack(command, result, service=memo.SERVICE):
    if not isinstance(result, dict) or result.get("ok") is not True:
        raise OutboxError("invalid_acknowledgement")
    operation = command["operation"]
    data = result.get("data", {})
    if not isinstance(data, dict): raise OutboxError("invalid_acknowledgement")
    valid = False
    if operation == "post":
        receipt = result.get("receipt", {})
        valid = (isinstance(receipt, dict) and isinstance(receipt.get("id"), str) and bool(receipt["id"])
                 and receipt.get("sha256") == sha(command.get("text", "").encode())
                 and type(receipt.get("accepted_at")) is int)
    elif operation == "room.create":
        valid = matching_text(data.get("room"), command.get("room")) and data.get("visibility") == (command.get("visibility") or "public")
    elif operation in ("room.member.add", "room.member.remove"):
        valid = all(matching_text(data.get(k), command.get(v)) for k, v in (("room", "room"), ("member", "target"), ("operation", "operation")))
    elif operation == "agent.register":
        valid = data.get("agent_id") == sha(memo.unb64(command["public_key"])) and data.get("handle") == command.get("handle", "").lower()
    elif operation == "agent.rotate":
        valid = (data.get("agent_id") == sha(memo.unb64(command["target"]))
                 and data.get("predecessor") == sha(memo.unb64(command["public_key"])))
    elif operation == "credit.transfer":
        valid = (matching_text(data.get("recipient"), command.get("target")) and type(data.get("transferred_bytes")) is int
                 and data["transferred_bytes"] > 0 and data["transferred_bytes"] == command.get("amount"))
    elif operation == "report":
        valid = isinstance(data.get("report_id"), str) and bool(data["report_id"]) and data.get("status") == "pending_review"
    elif operation == "lease.acquire":
        valid = (matching_text(data.get("room"), command.get("room")) and matching_text(data.get("target"), command.get("target"))
                 and data.get("holder") == sha(memo.unb64(command["public_key"]))
                 and type(data.get("fence")) is int and data["fence"] > 0 and type(data.get("expires_at")) is int and data["expires_at"] > 0)
    elif operation == "lease.release":
        valid = data.get("released") is True and type(data.get("fence")) is int and data["fence"] > 0 and data["fence"] == command.get("amount")
    elif operation == "agent.profile.publish":
        valid = (data.get("published") is True and data.get("agent_id") == sha(memo.unb64(command["public_key"]))
                 and type(data.get("published_at")) is int and type(data.get("expires_at")) is int
                 and data["expires_at"] - data["published_at"] == (command.get("ttl") or 604800))
    elif operation == "agent.profile.remove":
        valid = data.get("removed") is True and data.get("agent_id") == sha(memo.unb64(command["public_key"]))
    elif operation in ("delegation.create", "delegation.revoke"):
        if set(result) != {"ok", "data"} or set(data) != {"ack"}: raise OutboxError("invalid_acknowledgement")
        ack = data.get("ack")
        intent = strict_json(command.get("data", ""))
        creating = operation == "delegation.create"
        target = sha(memo.unb64(command["target"])) if creating else command.get("target")
        fields = {"grant_id", "child_id", "generation", "service_id", "state", "accepted_at", "expires_at", "ceiling_bytes"}
        if isinstance(ack, dict) and set(ack) == fields and isinstance(intent, dict):
            valid = (ack["grant_id"] == target and ack["child_id"] == target and isinstance(target, str)
                     and bool(re.fullmatch(r"[a-f0-9]{64}", target)) and ack["service_id"] == service
                     and ack["state"] == ("active" if creating else "revoked")
                     and matching_text(ack["generation"], intent.get("generation"))
                     and bool(re.fullmatch(r"[a-f0-9]{32}", ack["generation"]))
                     and all(type(ack[f]) is int and 0 < ack[f] < 2**63 for f in ("accepted_at", "expires_at", "ceiling_bytes")))
            if valid and creating:
                valid = (ack["expires_at"] - ack["accepted_at"] == command.get("ttl") and ack["ceiling_bytes"] == command.get("amount"))
    elif operation in ("private_read.create", "private_read.revoke"):
        if set(result) != {"ok", "data"} or set(data) != {"ack"} or len(encoded(result)) + 1 > 1024:
            raise OutboxError("invalid_acknowledgement")
        ack = data.get("ack")
        intent = strict_json(command.get("data", ""))
        creating = operation == "private_read.create"
        target = sha(memo.unb64(command["target"])) if creating else command.get("target")
        fields = set("type schema grant_id child_id room service_id generation grant_generation state accepted_at expires_at historical_acknowledgement".split())
        if isinstance(ack, dict) and set(ack) == fields and isinstance(intent, dict):
            valid = (ack["type"] == "private-room-read-grant-ack" and type(ack["schema"]) is int and ack["schema"] == 1
                     and isinstance(target, str) and bool(re.fullmatch(r"[a-f0-9]{64}", target))
                     and ack["grant_id"] == target and ack["child_id"] == target
                     and matching_text(ack["room"], command.get("room")) and ack["service_id"] == service
                     and matching_text(ack["generation"], intent.get("generation"))
                     and bool(re.fullmatch(r"[a-f0-9]{32}", ack["generation"]))
                     and isinstance(ack["grant_generation"], str) and bool(re.fullmatch(r"[a-f0-9]{32}", ack["grant_generation"]))
                     and ack["state"] == ("active" if creating else "revoked") and ack["historical_acknowledgement"] is True
                     and all(type(ack[k]) is int and 0 < ack[k] < 2**63 for k in ("accepted_at", "expires_at")))
            if valid and creating:
                valid = (ack["grant_generation"] == ack["generation"]
                         and ack["expires_at"] - ack["accepted_at"] == command.get("ttl", 86400))
    elif operation.startswith("work."):
        if set(result) != {"ok", "data"} or set(data) != {"ack"}:
            raise OutboxError("invalid_acknowledgement")
        ack = data.get("ack")
        intent = strict_json(command.get("data", ""))
        expected = {"work.create": "open", "work.claim": "claimed", "work.renew": "claimed",
                    "work.submit": "submitted", "work.accept": "accepted", "work.reject": "open",
                    "work.cancel": "cancelled"}.get(operation)
        fields = {"work_id", "state", "fence", "generation", "service_id", "accepted_at", "deadline", "claim_expires_at"}
        if isinstance(ack, dict) and set(ack) == fields and isinstance(intent, dict):
            valid = (matching_text(ack["work_id"], command.get("message_id")) and ack["state"] == expected
                     and ack["service_id"] == service and matching_text(ack["generation"], intent.get("generation"))
                     and isinstance(ack["generation"], str) and bool(re.fullmatch(r"[a-f0-9]{32}", ack["generation"]))
                     and all(type(ack[f]) is int and 0 <= ack[f] < 2**63 for f in ("fence", "accepted_at", "deadline", "claim_expires_at"))
                     and 0 < ack["accepted_at"] < ack["deadline"]
                     and ack["claim_expires_at"] <= ack["deadline"])
            if valid:
                if operation == "work.create":
                    valid = (ack["fence"] == 0 and ack["claim_expires_at"] == 0
                             and ack["deadline"] - ack["accepted_at"] == (command.get("ttl") or 604800))
                elif operation in ("work.claim", "work.renew"):
                    valid = (ack["fence"] > 0 and ack["claim_expires_at"] - ack["accepted_at"] == command.get("ttl")
                             and (operation == "work.claim" or ack["fence"] == command.get("amount")))
                elif operation in ("work.submit", "work.accept", "work.reject"):
                    valid = ack["fence"] == command.get("amount", 0)
                    if operation == "work.reject": valid = valid and ack["claim_expires_at"] == 0
                    else: valid = valid and ack["fence"] > 0
                    if operation == "work.submit": valid = valid and ack["claim_expires_at"] > ack["accepted_at"]
    elif operation in ("blob.put", "blob.delete"):
        blob = data.get("blob", {})
        if isinstance(blob, dict):
            if operation == "blob.put":
                body = memo.unb64(command.get("data", ""))
                valid = (isinstance(blob.get("id"), str) and bool(blob["id"]) and matching_text(blob.get("room"), command.get("room"))
                         and blob.get("sha256") == sha(body) and type(blob.get("size")) is int and blob["size"] == len(body))
            else: valid = matching_text(blob.get("id"), command.get("message_id") or command.get("target")) and blob.get("deleted") is True
    if not valid: raise OutboxError("invalid_acknowledgement")


def intent_bytes(intent_id, intent):
    if not isinstance(intent_id, str) or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", intent_id):
        raise OutboxError("invalid_intent_id")
    if not isinstance(intent, dict) or intent.get("operation") not in MUTATIONS:
        raise OutboxError("unsupported_mutation")
    allowed = set(MUTATIONS[intent["operation"]].split()) | {"operation", "delegation"}
    if set(intent) - allowed: raise OutboxError("unsupported_intent_field")
    if intent["operation"] in ("private_read.create", "private_read.revoke"):
        if "delegation" in intent: raise OutboxError("unsupported_intent_field")
        try:
            data = memo.strict_json(intent.get("data", ""))
            create = intent["operation"] == "private_read.create"
            expected = {"schema", "generation", "access_epoch", "disclosure"} if create else {"schema", "generation"}
            if not isinstance(data, dict) or set(data) != expected or type(data["schema"]) is not int or data["schema"] != 1:
                raise ValueError()
            if create:
                if data["disclosure"] != "private": raise ValueError()
                memo.private_read_enrollment_intent(intent.get("target"), room=intent.get("room"), generation=data["generation"],
                                                    access_epoch=data["access_epoch"], ttl=intent.get("ttl"))
            else:
                memo.private_read_revoke_intent(intent.get("target"), room=intent.get("room"), generation=data["generation"])
        except (ValueError, TypeError, KeyError): raise OutboxError("invalid_private_read_data") from None
    for field, value in intent.items():
        if field == "delegation":
            try: memo.delegation_context(value)
            except ValueError: raise OutboxError("invalid_delegation_context") from None
            if intent["operation"] not in memo.DELEGATED_OPERATIONS: raise OutboxError("delegation_forbidden")
            if intent["operation"] == "post" and (intent.get("visibility") != "public" or intent.get("handle") or intent.get("attachments")):
                raise OutboxError("delegation_public_post_required")
        elif field in ("amount", "ttl"):
            if type(value) is not int or not 0 <= value < 2**63: raise OutboxError("invalid_integer_field")
        elif field in ("members", "attachments"):
            if not isinstance(value, list) or len(value) > (8 if field == "attachments" else 100) or any(not isinstance(v, str) for v in value):
                raise OutboxError("invalid_list_field")
        elif not isinstance(value, str): raise OutboxError("invalid_string_field")
    raw = encoded(intent, sorted_keys=True)
    if len(raw) > MAX_INTENT: raise OutboxError("intent_byte_limit")
    if intent["operation"] == "blob.put":
        try: data = memo.unb64(intent.get("data", ""))
        except ValueError: raise OutboxError("invalid_blob_encoding") from None
        if len(data) > 1024 * 1024: raise OutboxError("blob_byte_limit")
    return raw


def private_fd(path, flags):
    fd = os.open(path, flags | getattr(os, "O_NOFOLLOW", 0), 0o600)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        os.close(fd)
        raise OutboxError("private_regular_file_required")
    return fd


SCHEMA = """
CREATE TABLE binding(service TEXT NOT NULL,base_url TEXT NOT NULL,public_key TEXT NOT NULL,delegation TEXT NOT NULL DEFAULT '');
CREATE TABLE queue(sequence INTEGER PRIMARY KEY,id TEXT UNIQUE NOT NULL,intent BLOB NOT NULL,
 intent_sha256 TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'queued',envelope BLOB,envelope_sha256 TEXT,
 response BLOB,created_at INTEGER NOT NULL,prepared_at INTEGER,acknowledged_at INTEGER,
 attempts INTEGER NOT NULL DEFAULT 0,last_attempt_at INTEGER,last_error TEXT,http_status INTEGER);
PRAGMA user_version=2;
"""


class Outbox:
    def __init__(self, path, base_url, public_key, service=memo.SERVICE, max_bytes=DEFAULT_BYTES, *, delegation=None):
        parsed = urlsplit(base_url)
        if (parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password
                or parsed.path not in ("", "/") or parsed.query or parsed.fragment
                or any(ord(c) <= 32 or ord(c) == 127 for c in base_url)):
            raise OutboxError("origin_required")
        if parsed.scheme != "https" and parsed.hostname not in ("localhost", "127.0.0.1", "::1"):
            raise OutboxError("https_required")
        try:
            if public_key and len(memo.unb64(public_key)) != 32: raise ValueError()
            parsed.port
        except ValueError: raise OutboxError("invalid_binding") from None
        if not isinstance(service, str) or not re.fullmatch(r"[A-Za-z0-9.-]{1,128}", service):
            raise OutboxError("invalid_service")
        if type(max_bytes) is not int or not 4 * 1024 * 1024 <= max_bytes <= DEFAULT_BYTES:
            raise OutboxError("invalid_queue_limit")
        self.path = Path(path).absolute()
        self.binding = (service, base_url.rstrip("/"), public_key)
        self._delegation = None
        if delegation is not None:
            try: self._delegation = tuple(memo.delegation_context(delegation).values())
            except ValueError: raise OutboxError("invalid_delegation_context") from None
            if not public_key or sha(memo.unb64(public_key)) != self._delegation[1]: raise OutboxError("delegation_key_mismatch")
        self.max_bytes = max_bytes

    @property
    def delegation(self):
        return dict(zip(("schema", "grant_id", "generation"), self._delegation)) if self._delegation is not None else None

    def check_delegation(self, intent):
        if ("delegation" in intent) != (self.delegation is not None) or intent.get("delegation") != self.delegation:
            raise OutboxError("delegation_binding_mismatch")

    @contextmanager
    def database(self, *, create=False, write=False):
        parent = self.path.parent
        if not parent.exists():
            if create: raise OutboxError("private_directory_required")
            yield None
            return
        info = parent.stat()
        if info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise OutboxError("private_directory_required")
        if not self.path.exists() and not create:
            yield None
            return
        lock_fd = private_fd(str(self.path) + ".lock", os.O_RDWR | os.O_CREAT)
        with os.fdopen(lock_fd, "a") as lock:
            # Even status can need local rollback recovery after a crash. Hold
            # the exclusive application lock while deciding/opening that path.
            try: fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError: raise OutboxError("outbox_busy") from None
            recovery = Path(str(self.path) + "-journal").exists()
            fd = private_fd(self.path, (os.O_RDWR if write or recovery else os.O_RDONLY) | (os.O_CREAT if create else 0))
            os.close(fd)
            uri = self.path.as_uri() + ("?mode=rw" if write or recovery else "?mode=ro")
            db = sqlite3.connect(uri, uri=True, timeout=5)
            db.row_factory = sqlite3.Row
            try:
                if write:
                    db.execute("PRAGMA journal_mode=DELETE")
                    db.execute("PRAGMA synchronous=EXTRA")
                    db.execute("PRAGMA secure_delete=ON")
                version = db.execute("PRAGMA user_version").fetchone()[0]
                if not write: db.execute("PRAGMA query_only=ON")
                if version == 0 and create:
                    # Schema and binding are one transaction: a crash during
                    # first creation cannot leave an unbound queue.
                    db.executescript("BEGIN IMMEDIATE;\n" + SCHEMA)
                    with db: db.execute("INSERT INTO binding VALUES(?,?,?,?)", (*self.binding, encoded(self.delegation).decode() if self.delegation is not None else ""))
                    version = 2
                    directory_fd = os.open(parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
                    try: os.fsync(directory_fd)
                    finally: os.close(directory_fd)
                elif version not in (1, 2): raise OutboxError("unsupported_outbox_version")
                binding = db.execute("SELECT * FROM binding").fetchall()
                expected = self.binding if version == 1 else (*self.binding, encoded(self.delegation).decode() if self.delegation is not None else "")
                if len(binding) != 1 or tuple(binding[0]) != expected or (version == 1 and self.delegation is not None):
                    raise OutboxError("outbox_binding_mismatch")
                if version == 1 and write:
                    db.execute("BEGIN IMMEDIATE")
                    with db:
                        db.execute("ALTER TABLE binding ADD COLUMN delegation TEXT NOT NULL DEFAULT ''")
                        db.execute("PRAGMA user_version=2")
                if write:
                    page_size = db.execute("PRAGMA page_size").fetchone()[0]
                    db.execute("PRAGMA max_page_count=" + str((2 * self.max_bytes) // page_size))
                yield db
            finally: db.close()

    def check_capacity(self, db, extra=0):
        used = db.execute("SELECT coalesce(sum(length(intent)+coalesce(length(envelope),0)+coalesce(length(response),0)),0) FROM queue").fetchone()[0]
        if used + extra > self.max_bytes: raise OutboxError("queue_byte_limit")

    @staticmethod
    def summary(row):
        return {key: row[key] for key in ("id", "state", "intent_sha256", "envelope_sha256", "created_at",
                    "prepared_at", "acknowledged_at", "attempts", "last_attempt_at", "last_error", "http_status")}

    def enqueue(self, intent_id, intent):
        raw = intent_bytes(intent_id, intent)
        self.check_delegation(intent)
        if not self.binding[2] and intent["operation"] != "post": raise OutboxError("anonymous_post_only")
        with self.database(create=True, write=True) as db:
            old = db.execute("SELECT * FROM queue WHERE id=?", (intent_id,)).fetchone()
            if old:
                if bytes(old["intent"]) != raw: raise OutboxError("intent_id_conflict")
                return self.summary(old)
            if db.execute("SELECT count(*) FROM queue").fetchone()[0] >= MAX_ROWS: raise OutboxError("queue_item_limit")
            # Reserve enough logical capacity for this intent's signed envelope
            # and bounded acknowledgement; do not fill the queue so it cannot send.
            reserved = db.execute("SELECT coalesce(sum(length(intent)+coalesce(length(envelope),length(intent)+4096)+coalesce(length(response),?)),0) FROM queue", (MAX_RECEIPT,)).fetchone()[0]
            if reserved + 2 * len(raw) + 4096 + MAX_RECEIPT > self.max_bytes: raise OutboxError("queue_byte_limit")
            with db:
                db.execute("INSERT INTO queue(id,intent,intent_sha256,created_at) VALUES(?,?,?,?)",
                           (intent_id, raw, sha(raw), int(time.time())))
            return self.summary(db.execute("SELECT * FROM queue WHERE id=?", (intent_id,)).fetchone())

    def status(self):
        with self.database() as db:
            if db is None: return {"counts": {}, "items": [], "network_requests": 0}
            counts = dict(db.execute("SELECT state,count(*) FROM queue GROUP BY state").fetchall())
            rows = db.execute("SELECT * FROM queue ORDER BY sequence DESC LIMIT 100").fetchall()
            return {"counts": counts, "items": [self.summary(r) for r in rows], "network_requests": 0}

    def inspect(self, intent_id, *, sensitive=False):
        with self.database() as db:
            row = None if db is None else db.execute("SELECT * FROM queue WHERE id=?", (intent_id,)).fetchone()
            if row is None: raise OutboxError("unknown_intent")
            result = self.summary(row)
            if sensitive:
                for key in ("intent", "envelope", "response"):
                    result[key] = strict_json(row[key]) if row[key] is not None else None
            return result

    def verify_envelope(self, row):
        if not isinstance(row["envelope"], bytes) or not isinstance(row["intent"], bytes):
            raise OutboxError("outbox_integrity_error")
        raw = bytes(row["envelope"])
        if sha(raw) != row["envelope_sha256"] or sha(bytes(row["intent"])) != row["intent_sha256"]:
            raise OutboxError("outbox_integrity_error")
        command = strict_json(raw)
        if encoded(command) != raw: raise OutboxError("envelope_wire_mismatch")
        expected = {**strict_json(row["intent"]), "request_id": row["id"]}
        self.check_delegation(expected)
        if not self.binding[2]:
            if command != expected: raise OutboxError("envelope_intent_mismatch")
            return command
        fields = {key: value for key, value in command.items() if key not in ("public_key", "signature", "timestamp", "nonce", "proof")}
        if fields != expected or command.get("public_key") != self.binding[2]: raise OutboxError("envelope_intent_mismatch")
        try:
            _, public, _ = memo.crypto()
            payload = memo.canonical(command, self.binding[0])
            public.from_public_bytes(memo.unb64(self.binding[2])).verify(memo.unb64(command["signature"]), payload)
            if command["operation"] in ("agent.rotate", "delegation.create", "private_read.create"):
                public.from_public_bytes(memo.unb64(command["target"])).verify(memo.unb64(command["proof"]), payload)
        except Exception: raise OutboxError("envelope_signature_invalid") from None
        return command

    def flush(self, client, limit=10, *, rotation_key=None, target_key=None, retry_blocked=False):
        return self._flush(client, limit, rotation_key=rotation_key, target_key=target_key, retry_blocked=retry_blocked)

    def deliver(self, client, intent_id, intent_sha256, *, retry_blocked=False):
        """Deliver only the exact FIFO head, or return its historical acknowledgement.

        Selection, integrity checks, preparation and send share one queue lock.
        This deliberately narrow API never enrolls or rotates keys and never sends
        another queued intent. A historical acknowledgement needs no live grant.
        """
        if not isinstance(intent_id, str) or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", intent_id):
            raise OutboxError("invalid_intent_id")
        if not isinstance(intent_sha256, str) or not re.fullmatch(r"[a-f0-9]{64}", intent_sha256):
            raise OutboxError("invalid_intent_digest")
        if type(retry_blocked) is not bool: raise OutboxError("invalid_retry_blocked")
        return self._flush(client, 1, retry_blocked=retry_blocked, target=(intent_id, intent_sha256))[0]

    def _target_summary(self, row):
        result = self.summary(row)
        if row["state"] != "acknowledged": return result
        command = self.verify_envelope(row)
        if not isinstance(row["response"], bytes) or len(row["response"]) > MAX_RECEIPT:
            raise OutboxError("outbox_integrity_error")
        response = strict_json(row["response"])
        validate_ack(command, response, self.binding[0])
        result["response"] = self._target_response(command, response)
        return result

    @staticmethod
    def _target_response(command, response):
        # Never echo arbitrary response extras: a hostile host may attach message
        # text, keys, URLs or tool instructions to an otherwise valid receipt.
        if command["operation"] == "post":
            saved = response["receipt"]
            receipt = {key: saved[key] for key in ("id", "sha256", "accepted_at")}
            if "cursor" in saved:
                if not isinstance(saved["cursor"], str): raise OutboxError("invalid_acknowledgement")
                receipt["cursor"] = saved["cursor"]
            if "duplicate" in saved:
                if type(saved["duplicate"]) is not bool: raise OutboxError("invalid_acknowledgement")
                receipt["duplicate"] = saved["duplicate"]
            return {"ok": True, "receipt": receipt}
        else:
            return {"ok": True, "data": {"ack": response["data"]["ack"]}}

    def _flush(self, client, limit, *, rotation_key=None, target_key=None, retry_blocked=False, target=None):
        if type(limit) is not int or not 1 <= limit <= 20: raise OutboxError("batch_limit")
        client_public = memo.b64(memo.public_bytes(client.key)) if client.key is not None else ""
        if (client.service, client.base_url, client_public) != self.binding:
            raise OutboxError("client_binding_mismatch")
        if getattr(client, "delegation", None) != self.delegation: raise OutboxError("delegation_binding_mismatch")
        if rotation_key is not None and target_key is not None: raise OutboxError("ambiguous_target_key")
        if client.save_request is not None or not 0 < client.timeout <= 30: raise OutboxError("unsupported_client_configuration")
        results = []
        with self.database(write=True) as db:
            if db is None:
                if target is not None: raise OutboxError("unknown_intent")
                return results
            selected = None
            if target is not None:
                selected = db.execute("SELECT * FROM queue WHERE id=?", (target[0],)).fetchone()
                if selected is None: raise OutboxError("unknown_intent")
                if selected["intent_sha256"] != target[1]: raise OutboxError("intent_digest_mismatch")
                if not isinstance(selected["intent"], bytes) or sha(selected["intent"]) != target[1]:
                    raise OutboxError("outbox_integrity_error")
                intent = strict_json(selected["intent"])
                if intent_bytes(selected["id"], intent) != selected["intent"]: raise OutboxError("outbox_integrity_error")
                self.check_delegation(intent)
                if intent["operation"] not in TARGETED_MUTATIONS: raise OutboxError("unsupported_targeted_mutation")
                if selected["state"] == "acknowledged": return [self._target_summary(selected)]
                first = db.execute("SELECT id FROM queue WHERE state<>'acknowledged' ORDER BY sequence LIMIT 1").fetchone()
                if first is None or first["id"] != target[0]: raise OutboxError("outbox_not_head")
                if selected["state"] not in ("queued", "prepared", "unresolved", "blocked"):
                    raise OutboxError("outbox_integrity_error")
                # A retry must never invent a replacement envelope after local
                # corruption or an earlier failure before preparation completed.
                if selected["envelope"] is None and selected["state"] != "queued":
                    if selected["state"] == "blocked" and not retry_blocked: return [self.summary(selected)]
                    raise OutboxError("outbox_integrity_error")
            rotations = db.execute("SELECT intent FROM queue WHERE state='acknowledged'").fetchall()
            if any(strict_json(r["intent"])["operation"] == "agent.rotate" for r in rotations):
                raise OutboxError("outbox_rotation_completed")
            states = ("queued", "prepared", "unresolved", "blocked")
            rows = [selected] if selected is not None else db.execute("SELECT * FROM queue WHERE state IN (" + ",".join("?" for _ in states) + ") ORDER BY sequence LIMIT ?", (*states, limit)).fetchall()
            for row in rows:
                if row["state"] == "blocked" and not retry_blocked:
                    results.append(self.summary(row))
                    break
                try:
                    if row["envelope"] is None:
                        if not isinstance(row["intent"], bytes): raise OutboxError("outbox_integrity_error")
                        raw_intent = bytes(row["intent"])
                        intent = strict_json(raw_intent)
                        self.check_delegation(intent)
                        if sha(raw_intent) != row["intent_sha256"] or intent_bytes(row["id"], intent) != raw_intent:
                            raise OutboxError("outbox_integrity_error")
                        command = client.prepare(**intent, request_id=row["id"])
                        if intent["operation"] in ("agent.rotate", "delegation.create", "private_read.create"):
                            proof_key = target_key if target_key is not None else (rotation_key if intent["operation"] == "agent.rotate" else None)
                            if proof_key is None or memo.b64(memo.public_bytes(proof_key)) != intent.get("target"):
                                raise OutboxError("rotation_key_required" if intent["operation"] == "agent.rotate" else "target_key_required")
                            command["proof"] = memo.b64(proof_key.sign(memo.canonical(command, client.service)))
                        envelope = encoded(command)
                        self.check_capacity(db, len(envelope))
                        # EXTRA synchronous commit completes BEFORE any network call.
                        with db:
                            db.execute("UPDATE queue SET envelope=?,envelope_sha256=?,prepared_at=?,state='prepared' WHERE id=?",
                                       (envelope, sha(envelope), int(time.time()), row["id"]))
                        row = db.execute("SELECT * FROM queue WHERE id=?", (row["id"],)).fetchone()
                    command = self.verify_envelope(row)
                    with db:
                        db.execute("UPDATE queue SET attempts=attempts+1,last_attempt_at=?,state='unresolved',last_error=NULL,http_status=NULL WHERE id=?", (int(time.time()), row["id"]))
                    response = client.send(command)
                    validate_ack(command, response, client.service)
                    if target is not None: self._target_response(command, response)
                    receipt = encoded(response)
                    if len(receipt) > MAX_RECEIPT: raise OutboxError("acknowledgement_byte_limit")
                    self.check_capacity(db, len(receipt))
                    with db:
                        db.execute("UPDATE queue SET response=?,acknowledged_at=?,state='acknowledged',last_error=NULL,http_status=NULL WHERE id=?",
                                   (receipt, int(time.time()), row["id"]))
                except Exception as exc:
                    db.rollback()
                    state, code, status = "unresolved", "delivery_uncertain", None
                    if isinstance(exc, memo.APIError):
                        status = exc.status if type(exc.status) is int else None
                        code = exc.code if isinstance(exc.code, str) and exc.code in SAFE_CODES else "http_error"
                        if status is not None and 400 <= status < 500 and status not in (408, 429): state = "blocked"
                    elif isinstance(exc, OutboxError):
                        code = str(exc)
                        if code not in ("invalid_acknowledgement", "acknowledgement_byte_limit"): state = "blocked"
                    elif isinstance(exc, ValueError) and str(exc) in DELEGATION_LOCAL_CODES:
                        state, code = "blocked", str(exc)
                    with db:
                        db.execute("UPDATE queue SET state=?,last_error=?,http_status=? WHERE id=?", (state, code, status, row["id"]))
                saved = db.execute("SELECT * FROM queue WHERE id=?", (row["id"],)).fetchone()
                results.append(self._target_summary(saved) if target is not None else self.summary(saved))
                # Rotation is a barrier even when its response was lost: later
                # old-key work must not race a possibly accepted key transition.
                if results[-1]["state"] != "acknowledged" or strict_json(row["intent"])["operation"] == "agent.rotate": break
        return results


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", required=True, type=Path)
    parser.add_argument("--url", default="https://swarmmemo.com")
    parser.add_argument("--service", default=memo.SERVICE)
    parser.add_argument("--public-key", required=True)
    parser.add_argument("--delegation-context", help="explicit original schema/grant_id/generation JSON; never refreshed automatically")
    sub = parser.add_subparsers(dest="action")
    sub.add_parser("status")
    enqueue = sub.add_parser("enqueue"); enqueue.add_argument("--id", required=True); enqueue.add_argument("--intent", type=Path, required=True)
    flush = sub.add_parser("flush"); flush.add_argument("--key", type=Path)
    flush.add_argument("--rotation-key", type=Path); flush.add_argument("--limit", type=int, default=10)
    flush.add_argument("--target-key", type=Path, help="local possession-proof key for enrollment or rotation; not uploaded")
    flush.add_argument("--grant-room"); flush.add_argument("--grant-operation", action="append", default=[])
    flush.add_argument("--retry-blocked", action="store_true")
    deliver = sub.add_parser("deliver", help="deliver one exact FIFO intent, or return its saved acknowledgement")
    deliver.add_argument("--id", required=True); deliver.add_argument("--intent-sha256", required=True)
    deliver.add_argument("--key", type=Path)
    deliver.add_argument("--grant-room"); deliver.add_argument("--grant-operation", action="append", default=[])
    inspect = sub.add_parser("inspect"); inspect.add_argument("--id", required=True); inspect.add_argument("--sensitive", action="store_true")
    args = parser.parse_args(argv)
    try:
        context = memo.delegation_context(strict_json(args.delegation_context)) if args.delegation_context is not None else None
        queue = Outbox(args.db, args.url, args.public_key, args.service, delegation=context)
        if args.action == "enqueue":
            fd = private_fd(args.intent, os.O_RDONLY)
            with os.fdopen(fd, "rb") as stream: raw = stream.read(MAX_INTENT + 1)
            if len(raw) > MAX_INTENT: raise OutboxError("intent_byte_limit")
            result = queue.enqueue(args.id, strict_json(raw))
        elif args.action in ("flush", "deliver"):
            if args.action == "deliver" and context is None and (args.grant_room is not None or args.grant_operation):
                raise OutboxError("delegation_required")
            client = memo.Client(args.url, memo.load_key(args.key) if args.key else None, timeout=10, service=args.service)
            if context is not None:
                client = memo.DelegatedClient(args.url, client.key, grant_id=context["grant_id"], generation=context["generation"],
                                             room=args.grant_room, operations=args.grant_operation, timeout=10, service=args.service)
            if args.action == "deliver":
                result = queue.deliver(client, args.id, args.intent_sha256)
            else:
                rotation_key = memo.load_key(args.rotation_key) if args.rotation_key else None
                target_key = memo.load_key(args.target_key) if args.target_key else None
                result = {"items": queue.flush(client, args.limit, rotation_key=rotation_key, target_key=target_key, retry_blocked=args.retry_blocked)}
        elif args.action == "inspect": result = queue.inspect(args.id, sensitive=args.sensitive)
        else: result = queue.status()
        print(json.dumps(result, ensure_ascii=False))
        if args.action == "deliver": return 0 if result.get("state") == "acknowledged" else 1
        return 1 if isinstance(result, dict) and any(item.get("state") != "acknowledged" for item in result.get("items", [])) and args.action == "flush" else 0
    except Exception as exc:
        print(json.dumps({"error": str(exc) if isinstance(exc, OutboxError) else "outbox_operation_failed"}), file=sys.stderr)
        return 1


if __name__ == "__main__": raise SystemExit(main())
