#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Public-only local inbox. Only explicit poll/resync perform unsigned network reads."""
from __future__ import annotations

import os
import sys


def _bind_parent(expected):
    import ctypes
    import signal
    if sys.platform != "linux" or expected <= 1:
        raise ValueError("worker_parent_lost")
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.prctl(1, signal.SIGKILL, 0, 0, 0) != 0 or os.getppid() != expected:
        raise ValueError("worker_parent_lost")


# Arm before application imports or accepting input. The ordinary offline CLI
# does not require a parent binding, main thread or signal ownership.
if __name__ == "__main__" and sys.argv[1:2] == ["_fetch"]:
    try:
        if len(sys.argv) != 3: raise ValueError()
        _bind_parent(int(sys.argv[2]))
    except BaseException:
        sys.exit(1)

import argparse
import base64
from contextlib import contextmanager
import fcntl
import hashlib
import json
from pathlib import Path
import re
import select
import signal
import sqlite3
import stat
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

if __name__ == "__main__":
    sys.path.insert(0, str(Path(__file__).resolve().parent))
import swarmmemo as memo

MAX_RESPONSE = 1024 * 1024
MAX_IPC = 2 * MAX_RESPONSE
CLEANUP_SECONDS = 0.25
MAX_EVENT = 256 * 1024
MAX_BYTES = 64 * 1024 * 1024
BODY_BUDGET = 56 * 1024 * 1024
MAX_EVENTS = 10000
MAX_NOTIFICATIONS = 50000
MAX_CONSUMERS = 16
MAX_SENDER_RULES_PER_CONSUMER = 128
MAX_SENDER_RULES = 2048
SENDER_RULE_BYTES = 256
EVENT_FIELDS = set("id sequence room page text kind author handle public_key signature signed_payload created_at sha256 reply_to to hidden reason type visibility archive_eligible attachments delegation_id".split())
SIGNED_POST_FIELDS = set("operation room page text kind reply_to to request_id public_key timestamp nonce handle visibility attachments delegation".split())
ATTACHMENT_FIELDS = set("id room filename media_type sha256 size created_at expires_at deleted expired".split())
BINDING_FIELDS = {"version", "origin", "service_id", "recipient", "room", "visibility", "reader_public_key", "start_mode"}
SLUG = r"[a-z0-9][a-z0-9_-]{0,63}"
IDENTIFIER = r"[A-Za-z0-9_-]{1,128}"
HASH = r"[0-9a-f]{64}"
GENERATION = r"[0-9a-f]{32}"


class InboxError(ValueError): pass


def _require_worker_platform():
    if sys.platform != "linux" or signal.getsignal(signal.SIGCHLD) != signal.SIG_DFL:
        raise InboxError("worker_platform_required")
    if threading.current_thread() is not threading.main_thread():
        raise InboxError("worker_main_thread_required")


@contextmanager
def _cancellation_guard():
    """Preserve caller signals, deferring their semantics until child cleanup."""
    _require_worker_platform()
    numbers = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)
    originals = {number: signal.getsignal(number) for number in numbers}
    pending = set()

    def remember(number, _frame): pending.add(number)

    def check():
        if pending: raise InboxError("interrupted")

    initial_mask = signal.pthread_sigmask(signal.SIG_BLOCK, numbers)
    try:
        for number in numbers:
            if originals[number] != signal.SIG_IGN: signal.signal(number, remember)
        signal.pthread_sigmask(signal.SIG_SETMASK, initial_mask)
        yield check
    finally:
        signal.pthread_sigmask(signal.SIG_BLOCK, numbers)
        try:
            # A signal delivered to another unmasked thread can still schedule
            # a raising Python main-thread handler during this restoration.
            try: signal.signal(signal.SIGINT, originals[signal.SIGINT])
            finally:
                try: signal.signal(signal.SIGTERM, originals[signal.SIGTERM])
                finally: signal.signal(signal.SIGHUP, originals[signal.SIGHUP])
        finally:
            try:
                for number in numbers:
                    if number in pending: signal.pthread_kill(threading.get_ident(), number)
            finally: signal.pthread_sigmask(signal.SIG_SETMASK, initial_mask)


def encode(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()


def sha(value): return hashlib.sha256(value).hexdigest()


def strict_json(raw):
    def pairs(values):
        result = {}
        for key, value in values:
            if key in result: raise InboxError("duplicate_json_field")
            result[key] = value
        return result
    def bad(_): raise InboxError("invalid_json")
    try: return json.loads(raw, object_pairs_hook=pairs, parse_constant=bad)
    except (ValueError, UnicodeError) as exc:
        if isinstance(exc, InboxError): raise
        raise InboxError("invalid_json") from None


def matches(value, pattern): return isinstance(value, str) and re.fullmatch(pattern, value) is not None


def private_fd(path, flags):
    fd = os.open(path, flags | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        os.close(fd); raise InboxError("private_regular_file_required")
    return fd


def binding_validated(binding):
    if not isinstance(binding, dict) or set(binding) != BINDING_FIELDS or type(binding["version"]) is not int or binding["version"] != 1:
        raise InboxError("invalid_binding")
    if binding["visibility"] != "public" or binding["reader_public_key"] != "": raise InboxError("private_mode_not_supported")
    if not matches(binding["recipient"], HASH) or (binding["room"] != "" and not matches(binding["room"], SLUG)):
        raise InboxError("invalid_subscription")
    if binding["start_mode"] not in ("history", "recent") or not matches(binding["service_id"], r"[A-Za-z0-9.-]{1,128}"):
        raise InboxError("invalid_binding")
    try:
        origin = binding["origin"]
        parsed = urllib.parse.urlsplit(origin)
        if (not isinstance(origin, str) or parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username is not None
                or parsed.password is not None or parsed.path or parsed.query or parsed.fragment or parsed.port == 0
                or urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, "", "", "")) != origin
                or any(ord(c) <= 32 or ord(c) == 127 or c == "\\" for c in origin)):
            raise ValueError()
        if parsed.scheme != "https" and parsed.hostname not in ("localhost", "127.0.0.1", "::1"): raise ValueError()
    except (ValueError, TypeError): raise InboxError("invalid_origin") from None
    return strict_json(encode(binding))


def validate_event(event, binding, *, addressed=False, scoped=True):
    if not isinstance(event, dict) or set(event) - EVENT_FIELDS or len(encode(event)) > MAX_EVENT:
        raise InboxError("invalid_event_fields_or_size")
    required = {"id", "sequence", "room", "page", "text", "kind", "author", "created_at", "sha256", "hidden", "type", "visibility", "archive_eligible"}
    if not required <= set(event): raise InboxError("missing_event_field")
    if event["visibility"] != "public": raise InboxError("event_scope_mismatch")
    if scoped and binding["room"] and event["room"] != binding["room"]: raise InboxError("event_scope_mismatch")
    if not matches(event["id"], IDENTIFIER) or not all(matches(event[k], SLUG) for k in ("room", "page", "kind")):
        raise InboxError("invalid_message_identifier")
    if (type(event["sequence"]) is not int or event["sequence"] < 1 or type(event["created_at"]) is not int or event["created_at"] < 0
            or not matches(event["sha256"], HASH) or type(event["hidden"]) is not bool or type(event["archive_eligible"]) is not bool):
        raise InboxError("invalid_event_metadata")
    for field in ("text", "handle", "public_key", "signature", "signed_payload", "reply_to", "to", "reason"):
        if field in event and (not isinstance(event[field], str) or "\x00" in event[field]): raise InboxError("invalid_event_text")
    if event.get("to") and not matches(event["to"], HASH): raise InboxError("invalid_recipient")
    if addressed and not event.get("to"): raise InboxError("unaddressed_event")
    if event.get("reply_to") and not matches(event["reply_to"], IDENTIFIER): raise InboxError("invalid_reply")
    if "delegation_id" in event and (not matches(event["delegation_id"], HASH) or event["delegation_id"] != event["author"]):
        raise InboxError("invalid_delegation_attribution")
    if "delegation_id" in event:
        try:
            key = memo.unb64(event.get("public_key", ""))
            if len(key) != 32 or sha(key) != event["delegation_id"]: raise ValueError()
        except Exception: raise InboxError("invalid_delegation_attribution") from None
    attachments = event.get("attachments", [])
    if not isinstance(attachments, list) or len(attachments) > 8: raise InboxError("attachment_limit")
    seen = set()
    for item in attachments:
        if not isinstance(item, dict) or set(item) != ATTACHMENT_FIELDS: raise InboxError("invalid_attachment_fields")
        if not matches(item["id"], IDENTIFIER) or item["id"] in seen or item["room"] != event["room"]: raise InboxError("invalid_attachment_identity")
        seen.add(item["id"])
        if (not matches(item["sha256"], HASH) or type(item["size"]) is not int or not 0 <= item["size"] <= MAX_RESPONSE
                or type(item["created_at"]) is not int or type(item["expires_at"]) is not int or item["expires_at"] < item["created_at"]
                or type(item["deleted"]) is not bool or type(item["expired"]) is not bool
                or not isinstance(item["filename"], str) or not isinstance(item["media_type"], str)):
            raise InboxError("invalid_attachment_metadata")
    if event["type"] == "tombstone":
        if event["hidden"] is not True or any(event.get(k) for k in ("text", "signature", "signed_payload", "attachments")):
            raise InboxError("tombstone_contains_payload")
        if event["author"] != "anonymous" and not matches(event["author"], HASH): raise InboxError("invalid_author")
        return event
    if event["type"] != "message" or event["hidden"] is not False or sha(event["text"].encode()) != event["sha256"]:
        raise InboxError("event_hash_or_state_mismatch")
    signed = any(event.get(k) for k in ("public_key", "signature", "signed_payload"))
    if signed:
        if not all(event.get(k) for k in ("public_key", "signature", "signed_payload")): raise InboxError("incomplete_signature")
        try:
            payload = event["signed_payload"].encode()
            envelope = strict_json(payload)
            if (not isinstance(envelope, dict) or set(envelope) != {"version", "service", "command"}
                    or type(envelope["version"]) is not int or envelope["version"] not in (1, 2)
                    or envelope["service"] != binding["service_id"]):
                raise InboxError("signature_service_mismatch")
            command = envelope["command"]
            if not isinstance(command, dict) or set(command) - SIGNED_POST_FIELDS:
                raise InboxError("invalid_signed_post_fields")
            for field, value in command.items():
                if field == "delegation": continue
                if field == "timestamp":
                    valid = type(value) is int and 0 < value <= 9223372036854775807
                elif field == "attachments":
                    valid = isinstance(value, list) and all(isinstance(item, str) for item in value)
                else:
                    valid = isinstance(value, str) and "\x00" not in value
                if not valid: raise InboxError("invalid_signed_post_fields")
            delegated = "delegation" in command
            if delegated != (envelope["version"] == 2) or delegated != ("delegation_id" in event):
                raise InboxError("delegation_context_mismatch")
            if delegated:
                context = memo.delegation_context(command["delegation"])
                if (context["grant_id"] != event["delegation_id"] or command.get("visibility") != "public"
                        or command.get("room") != event["room"] or not command.get("room")
                        or event.get("handle", "") != "" or command.get("handle", "") != ""
                        or command.get("attachments", []) != [] or attachments):
                    raise InboxError("delegation_projection_mismatch")
            if payload != memo.canonical(command, binding["service_id"]) or command.get("operation") != "post": raise InboxError("noncanonical_signed_event")
            key = memo.unb64(event["public_key"])
            memo.crypto()[1].from_public_bytes(key).verify(memo.unb64(event["signature"]), payload)
            if sha(key) != event["author"]: raise InboxError("author_signature_mismatch")
            for field in ("room", "page", "kind", "text", "public_key", "reply_to", "to"):
                default = {"room": "lobby", "page": "main", "kind": "note"}.get(field, "")
                if (command.get(field) or default) != event.get(field, ""): raise InboxError("signed_event_field_mismatch")
            if command.get("handle") and command["handle"] != event.get("handle", ""):
                raise InboxError("signed_event_field_mismatch")
            if command.get("visibility", "public") != "public": raise InboxError("signed_event_field_mismatch")
            if command.get("attachments", []) != [item["id"] for item in attachments]: raise InboxError("signed_attachment_mismatch")
        except InboxError: raise
        except Exception: raise InboxError("invalid_event_signature") from None
    elif event["author"] != "anonymous" or "delegation_id" in event: raise InboxError("unsigned_author_claim")
    return event


def immutable(event):
    result = {k: event.get(k, "") for k in ("id", "room", "page", "kind", "author", "public_key", "created_at", "sha256", "reply_to", "to")}
    # Keep legacy local immutable bytes unchanged; bind new attribution only when
    # explicitly present. A correction may not change a signed event's grant.
    if "delegation_id" in event: result["delegation_id"] = event["delegation_id"]
    return result


def event_digest(event): return sha(encode({k: v for k, v in event.items() if k != "sequence"}))


def fetch_worker():
    """Fresh process, no inherited DB connection/key; bounded stdout protocol."""
    try:
        request = strict_json(sys.stdin.buffer.read(8193))
        if set(request) != {"url"} or len(encode(request)) > 8192: raise InboxError("invalid_fetch_request")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), memo.NoRedirect())
        req = urllib.request.Request(request["url"], headers={"Accept": "application/json", "Accept-Encoding": "identity"}, method="GET")
        try: response = opener.open(req, timeout=5)
        except urllib.error.HTTPError as error: response = error
        with response:
            if sum(len(k) + len(v) for k, v in response.headers.items()) > 65536: raise InboxError("header_limit")
            if response.headers.get("Content-Encoding", "identity").lower() != "identity": raise InboxError("compressed_response")
            if response.headers.get_content_type() != "application/json": raise InboxError("response_content_type")
            length = response.headers.get("Content-Length")
            if length is not None and (not re.fullmatch(r"[0-9]+", length) or int(length) > MAX_RESPONSE): raise InboxError("response_byte_limit")
            chunks, size = [], 0
            while True:
                chunk = response.read1(min(65536, MAX_RESPONSE + 1 - size))
                if not chunk: break
                size += len(chunk)
                if size > MAX_RESPONSE: raise InboxError("response_byte_limit")
                chunks.append(chunk)
            if length is not None and int(length) != size: raise InboxError("truncated_response")
            result = {"status": response.status, "body": base64.b64encode(b"".join(chunks)).decode()}
    except Exception as error:
        result = {"error": str(error) if isinstance(error, InboxError) else "network_failed"}
    sys.stdout.buffer.write(encode(result))


SCHEMA = """
CREATE TABLE binding(value BLOB NOT NULL);
CREATE TABLE checkpoint(id INTEGER PRIMARY KEY CHECK(id=1),phase TEXT NOT NULL DEFAULT 'new',generation TEXT NOT NULL DEFAULT '',
 cursor TEXT NOT NULL DEFAULT '',after INTEGER NOT NULL DEFAULT 0,event_empty INTEGER NOT NULL DEFAULT 0,
 resync INTEGER NOT NULL DEFAULT 0,round INTEGER NOT NULL DEFAULT 0,revalidate_after INTEGER NOT NULL DEFAULT 0,
 last_success INTEGER,last_error TEXT);
INSERT INTO checkpoint(id) VALUES(1);
CREATE TABLE events(number INTEGER PRIMARY KEY,id TEXT UNIQUE NOT NULL,immutable BLOB NOT NULL,snapshot BLOB,digest TEXT NOT NULL,
 state TEXT NOT NULL,latched INTEGER NOT NULL DEFAULT 0,first_observed INTEGER NOT NULL,last_observed INTEGER NOT NULL,
 revalidated_at INTEGER NOT NULL,notified_digest TEXT,current_notification INTEGER,seen_round INTEGER NOT NULL DEFAULT 0);
CREATE TABLE notifications(id INTEGER PRIMARY KEY,event_id TEXT NOT NULL REFERENCES events(id),kind TEXT NOT NULL,digest TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE consumers(id TEXT PRIMARY KEY);
CREATE TABLE acknowledgements(consumer TEXT NOT NULL REFERENCES consumers(id),notification INTEGER NOT NULL REFERENCES notifications(id),
 digest TEXT NOT NULL,acknowledged_at INTEGER NOT NULL,PRIMARY KEY(consumer,notification));
PRAGMA user_version=1;
"""

SENDER_SCHEMA = """CREATE TABLE sender_mutes(consumer TEXT NOT NULL REFERENCES consumers(id),
 signer TEXT NOT NULL CHECK(typeof(signer)='text' AND length(signer)=64 AND signer NOT GLOB '*[^0-9a-f]*'),
 PRIMARY KEY(consumer,signer))"""


class Inbox:
    def __init__(self, path, binding):
        self.path = Path(path).absolute()
        self.binding = binding_validated(binding)

    @contextmanager
    def database(self, *, create=False, write=False):
        parent = self.path.parent
        if not parent.exists():
            if create: raise InboxError("private_directory_required")
            yield None; return
        info = parent.stat()
        if info.st_uid != os.getuid() or info.st_mode & 0o077: raise InboxError("private_directory_required")
        if not self.path.exists() and not create:
            yield None; return
        lock_fd = private_fd(str(self.path) + ".lock", os.O_RDWR | os.O_CREAT)
        with os.fdopen(lock_fd, "a") as lock:
            try: fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError: raise InboxError("inbox_busy") from None
            for suffix in ("-journal", "-wal", "-shm"):
                sidecar = str(self.path) + suffix
                if os.path.lexists(sidecar):
                    sidecar_fd = private_fd(sidecar, os.O_RDONLY)
                    os.close(sidecar_fd)
            recovery = os.path.lexists(str(self.path) + "-journal")
            fd = private_fd(self.path, (os.O_RDWR if write or recovery else os.O_RDONLY) | (os.O_CREAT if create else 0))
            os.close(fd)
            db = sqlite3.connect(self.path.as_uri() + ("?mode=rw" if write or recovery else "?mode=ro"), uri=True, timeout=1)
            db.row_factory = sqlite3.Row
            try:
                db.execute("PRAGMA foreign_keys=ON")
                if write:
                    db.execute("PRAGMA journal_mode=DELETE"); db.execute("PRAGMA synchronous=EXTRA"); db.execute("PRAGMA secure_delete=ON")
                version = db.execute("PRAGMA user_version").fetchone()[0]
                if not write: db.execute("PRAGMA query_only=ON")
                if version == 0 and create:
                    db.executescript("BEGIN IMMEDIATE;\n" + SCHEMA)
                    with db: db.execute("INSERT INTO binding VALUES(?)", (encode(self.binding),))
                    directory_fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
                    try: os.fsync(directory_fd)
                    finally: os.close(directory_fd)
                elif version not in (1, 2): raise InboxError("unsupported_inbox_version")
                rows = db.execute("SELECT value FROM binding").fetchall()
                if len(rows) != 1 or bytes(rows[0][0]) != encode(self.binding): raise InboxError("binding_mismatch")
                if version == 2: self.validate_sender_policy(db)
                if write:
                    db.execute("PRAGMA max_page_count=" + str(2 * MAX_BYTES // db.execute("PRAGMA page_size").fetchone()[0]))
                yield db
            finally: db.close()

    def create(self, allow_private_storage=False):
        if allow_private_storage: raise InboxError("private_mode_not_supported")
        with self.database(create=True, write=True): pass
        return self.status()

    @staticmethod
    def require(db):
        if db is None: raise InboxError("inbox_not_created")

    def add_consumer(self, consumer):
        if not matches(consumer, r"[A-Za-z0-9._-]{1,64}"): raise InboxError("invalid_consumer")
        with self.database(write=True) as db:
            self.require(db)
            if db.execute("SELECT 1 FROM consumers WHERE id=?", (consumer,)).fetchone(): return {"created": False}
            if db.execute("SELECT count(*) FROM consumers").fetchone()[0] >= MAX_CONSUMERS: raise InboxError("consumer_limit")
            with db: db.execute("INSERT INTO consumers VALUES(?)", (consumer,))
        return {"created": True}

    @staticmethod
    def consumer_known(db, consumer):
        if not db.execute("SELECT 1 FROM consumers WHERE id=?", (consumer,)).fetchone(): raise InboxError("unknown_consumer")

    @staticmethod
    def eligible(row, phase):
        return (row["notification_id"] == row["current_notification"] and row["digest"] == row["notification_digest"]
                and (row["state"] in ("tombstoned", "unavailable") or (row["state"] == "ready" and phase == "ready")))

    @staticmethod
    def validate_sender_policy(db):
        try:
            table = db.execute("SELECT sql FROM sqlite_master WHERE type='table' AND name='sender_mutes'").fetchone()
            if table is None or table[0] != SENDER_SCHEMA: raise InboxError("invalid_sender_policy")
            if db.execute("SELECT 1 FROM sender_mutes WHERE typeof(consumer)!='text' OR length(consumer) NOT BETWEEN 1 AND 64 OR typeof(signer)!='text' OR length(signer)!=64 LIMIT 1").fetchone():
                raise InboxError("invalid_sender_policy")
            rows = db.execute("SELECT m.consumer,m.signer,c.id known FROM sender_mutes m LEFT JOIN consumers c ON c.id=m.consumer ORDER BY m.consumer,m.signer LIMIT ?", (MAX_SENDER_RULES + 1,)).fetchall()
            counts = {}
            if len(rows) > MAX_SENDER_RULES: raise InboxError("invalid_sender_policy")
            for row in rows:
                consumer, signer = row["consumer"], row["signer"]
                if not matches(consumer, r"[A-Za-z0-9._-]{1,64}") or not matches(signer, HASH) or row["known"] != consumer:
                    raise InboxError("invalid_sender_policy")
                counts[consumer] = counts.get(consumer, 0) + 1
                if counts[consumer] > MAX_SENDER_RULES_PER_CONSUMER: raise InboxError("invalid_sender_policy")
        except sqlite3.Error: raise InboxError("invalid_sender_policy") from None

    @staticmethod
    def sender_rules(db, consumer):
        if db.execute("PRAGMA user_version").fetchone()[0] == 1: return set()
        return {row[0] for row in db.execute("SELECT signer FROM sender_mutes WHERE consumer=?", (consumer,))}

    def sender_controls_enable(self):
        with self.database(write=True) as db:
            self.require(db)
            if db.execute("PRAGMA user_version").fetchone()[0] == 2: return {"enabled": True, "migrated": False}
            with db:
                db.execute("BEGIN IMMEDIATE")
                db.execute(SENDER_SCHEMA)
                db.execute("PRAGMA user_version=2")
        return {"enabled": True, "migrated": True}

    def sender_mute(self, consumer, signer):
        return self._sender_rule(consumer, signer, True)

    def sender_unmute(self, consumer, signer):
        return self._sender_rule(consumer, signer, False)

    def _sender_rule(self, consumer, signer, muted):
        if not matches(consumer, r"[A-Za-z0-9._-]{1,64}"): raise InboxError("invalid_consumer")
        if not matches(signer, HASH): raise InboxError("invalid_signer")
        with self.database(write=True) as db:
            self.require(db); self.consumer_known(db, consumer)
            if db.execute("PRAGMA user_version").fetchone()[0] != 2: raise InboxError("sender_controls_not_enabled")
            exists = signer in self.sender_rules(db, consumer)
            if exists == muted: return {"muted": muted, "changed": False}
            if muted:
                if (db.execute("SELECT count(*) FROM sender_mutes WHERE consumer=?", (consumer,)).fetchone()[0] >= MAX_SENDER_RULES_PER_CONSUMER
                        or db.execute("SELECT count(*) FROM sender_mutes").fetchone()[0] >= MAX_SENDER_RULES):
                    raise InboxError("sender_rule_limit")
                reserve = 512 * db.execute("SELECT count(*) FROM events WHERE latched=0").fetchone()[0]
                if self.usage(db) + SENDER_RULE_BYTES + reserve > MAX_BYTES: raise InboxError("catalog_capacity")
            with db:
                if muted: db.execute("INSERT INTO sender_mutes VALUES(?,?)", (consumer, signer))
                else: db.execute("DELETE FROM sender_mutes WHERE consumer=? AND signer=?", (consumer, signer))
        return {"muted": muted, "changed": True}

    def sender_mutes(self, consumer):
        if not matches(consumer, r"[A-Za-z0-9._-]{1,64}"): raise InboxError("invalid_consumer")
        with self.database() as db:
            self.require(db); self.consumer_known(db, consumer)
            if db.execute("PRAGMA user_version").fetchone()[0] != 2: raise InboxError("sender_controls_not_enabled")
            return {"signers": sorted(self.sender_rules(db, consumer)), "network_requests": 0}

    def metadata_signer(self, row):
        # Match only the complete immutable metadata previously admitted by signature
        # validation. No body, source prose, handle, or parent-identity lookup occurs.
        try:
            raw = row["immutable"]
            if not isinstance(raw, bytes) or len(raw) > MAX_EVENT: raise ValueError()
            data = strict_json(raw)
            fields = {"id", "room", "page", "kind", "author", "public_key", "created_at", "sha256", "reply_to", "to"}
            if not isinstance(data, dict) or set(data) not in (fields, fields | {"delegation_id"}) or encode(data) != raw: raise ValueError()
            if data["id"] != row["message_id"] or not matches(data["id"], IDENTIFIER): raise ValueError()
            if not all(matches(data[k], SLUG) for k in ("room", "page", "kind")): raise ValueError()
            if self.binding["room"] and data["room"] != self.binding["room"]: raise ValueError()
            if type(data["created_at"]) is not int or data["created_at"] < 0 or not matches(data["sha256"], HASH): raise ValueError()
            for field, pattern in (("reply_to", IDENTIFIER), ("to", HASH)):
                if data[field] != "" and not matches(data[field], pattern): raise ValueError()
            if not isinstance(data["public_key"], str): raise ValueError()
            if data["public_key"]:
                key = memo.unb64(data["public_key"])
                if len(key) != 32 or not matches(data["author"], HASH) or sha(key) != data["author"]: raise ValueError()
            elif data["author"] != "anonymous": raise ValueError()
            if "delegation_id" in data and (not data["public_key"] or data["delegation_id"] != data["author"]): raise ValueError()
            return data["author"] if data["public_key"] else None
        except (InboxError, ValueError, TypeError, KeyError): raise InboxError("cache_integrity_error") from None

    def status(self):
        with self.database() as db:
            if db is None: return {"phase": "not_created", "network_requests": 0}
            state = db.execute("SELECT * FROM checkpoint").fetchone()
            result = {"phase": state["phase"], "resync_active": bool(state["resync"]), "last_success": state["last_success"],
                    "last_error": state["last_error"], "event_page_empty": bool(state["event_empty"]),
                    "events": dict(db.execute("SELECT state,count(*) FROM events GROUP BY state").fetchall()),
                    "notifications": db.execute("SELECT count(*) FROM notifications").fetchone()[0], "network_requests": 0}
            if db.execute("PRAGMA user_version").fetchone()[0] == 2:
                result.update(local_schema=2, sender_mute_rules=db.execute("SELECT count(*) FROM sender_mutes").fetchone()[0])
            return result

    def pending(self, consumer, limit=20, *, include_muted=False):
        if type(limit) is not int or not 1 <= limit <= 100: raise InboxError("invalid_limit")
        if type(include_muted) is not bool: raise InboxError("invalid_include_muted")
        with self.database() as db:
            self.require(db); self.consumer_known(db, consumer)
            phase = db.execute("SELECT phase FROM checkpoint").fetchone()[0]
            rules = self.sender_rules(db, consumer)
            if rules:
                # SQLite may scan the bounded 50k notification history; only the
                # <=10k current eligible metadata rows reach this streaming loop.
                rows = db.execute("SELECT n.id notification_id,n.kind,n.digest notification_digest,e.id event_id,e.digest,e.state,CASE WHEN length(e.immutable)<=? THEN e.immutable ELSE NULL END immutable FROM notifications n JOIN events e ON e.id=n.event_id LEFT JOIN acknowledgements a ON a.notification=n.id AND a.consumer=? WHERE a.notification IS NULL AND n.id=e.current_notification AND n.digest=e.digest AND (e.state IN ('tombstoned','unavailable') OR (e.state='ready' AND ?='ready')) ORDER BY n.id", (MAX_EVENT, consumer, phase))
                result = []
                for count, row in enumerate(rows, 1):
                    if count > MAX_EVENTS: raise InboxError("cache_integrity_error")
                    muted = row["state"] == "ready" and self.metadata_signer(row) in rules
                    if muted and not include_muted: continue
                    item = {"notification_id": row["notification_id"], "message_id": row["message_id"], "kind": row["kind"], "state": row["state"], "snapshot_digest": row["digest"]}
                    if muted: item["muted"] = True
                    result.append(item)
                    if len(result) == limit: break
                return result
            rows = db.execute("SELECT n.id notification_id,n.kind,n.digest notification_digest,e.id event_id,e.digest,e.state FROM notifications n JOIN events e ON e.id=n.event_id LEFT JOIN acknowledgements a ON a.notification=n.id AND a.consumer=? WHERE a.notification IS NULL AND n.id=e.current_notification AND n.digest=e.digest AND (e.state IN ('tombstoned','unavailable') OR (e.state='ready' AND ?='ready')) ORDER BY n.id LIMIT ?", (consumer, phase, limit)).fetchall()
            return [{"notification_id": r["notification_id"], "message_id": r["message_id"], "kind": r["kind"], "state": r["state"], "snapshot_digest": r["digest"]} for r in rows]

    def notification(self, db, consumer, notification_id, *, metadata_only=False):
        self.consumer_known(db, consumer)
        if type(notification_id) is not int or notification_id < 1: raise InboxError("invalid_notification")
        fields = f"e.id,e.id message_id,CASE WHEN length(e.immutable)<={MAX_EVENT} THEN e.immutable ELSE NULL END immutable,e.digest,e.state,e.current_notification,e.last_observed,e.revalidated_at" if metadata_only else "e.*"
        row = db.execute("SELECT n.id notification_id,n.kind,n.digest notification_digest," + fields + " FROM notifications n JOIN events e ON e.id=n.event_id WHERE n.id=?", (notification_id,)).fetchone()
        if row is None: raise InboxError("unknown_notification")
        phase = db.execute("SELECT phase FROM checkpoint").fetchone()[0]
        if not self.eligible(row, phase): raise InboxError("notification_superseded_or_unreconciled")
        return row

    def read(self, consumer, notification_id, sensitive=False):
        with self.database() as db:
            self.require(db)
            rules = self.sender_rules(db, consumer)
            row = self.notification(db, consumer, notification_id, metadata_only=bool(rules))
            result = {"notification_id": notification_id, "message_id": row["id"], "kind": row["kind"], "state": row["state"], "snapshot_digest": row["digest"]}
            if rules:
                muted = row["state"] == "ready" and self.metadata_signer(row) in rules
                if muted:
                    if sensitive: raise InboxError("sender_muted")
                    result["muted"] = True
            if sensitive:
                state = db.execute("SELECT last_success FROM checkpoint").fetchone()
                raw = db.execute("SELECT snapshot FROM events WHERE id=?", (row["id"],)).fetchone()[0] if rules else row["snapshot"]
                snapshot = strict_json(raw) if raw is not None else None
                if row["state"] == "ready" and (snapshot is None or event_digest(snapshot) != row["digest"]): raise InboxError("cache_integrity_error")
                result.update(snapshot=snapshot,
                              observed_at=row["last_observed"], revalidated_at=row["revalidated_at"], corrections_observed_at=state[0],
                              correction_coverage="observed_public_corrections_not_live", stale=state[0] is None or int(time.time()) - state[0] > 60,
                              untrusted_content=True)
            return result

    def ack(self, consumer, notification_id, snapshot_digest):
        if type(notification_id) is not int or notification_id < 1 or not matches(snapshot_digest, HASH): raise InboxError("invalid_acknowledgement")
        with self.database(write=True) as db:
            self.require(db); self.consumer_known(db, consumer)
            old = db.execute("SELECT digest FROM acknowledgements WHERE consumer=? AND notification=?", (consumer, notification_id)).fetchone()
            if old:
                if old[0] != snapshot_digest: raise InboxError("ack_digest_mismatch")
                return {"acknowledged": True, "duplicate": True}
            row = self.notification(db, consumer, notification_id)
            if snapshot_digest != row["digest"]: raise InboxError("ack_digest_mismatch")
            reserve = 512 * db.execute("SELECT count(*) FROM events WHERE latched=0").fetchone()[0]
            if self.usage(db) + 128 + reserve > MAX_BYTES: raise InboxError("catalog_capacity")
            with db: db.execute("INSERT INTO acknowledgements VALUES(?,?,?,?)", (consumer, notification_id, snapshot_digest, int(time.time())))
        return {"acknowledged": True, "duplicate": False}

    @staticmethod
    def usage(db):
        return (db.execute("SELECT coalesce(sum(coalesce(length(snapshot),0)+length(immutable)+256),0) FROM events").fetchone()[0]
                + 256 * db.execute("SELECT count(*) FROM notifications").fetchone()[0]
                + 128 * db.execute("SELECT count(*) FROM acknowledgements").fetchone()[0]
                + (SENDER_RULE_BYTES * db.execute("SELECT count(*) FROM sender_mutes").fetchone()[0] if db.execute("PRAGMA user_version").fetchone()[0] == 2 else 0))

    @staticmethod
    def notify(db, row, kind, now):
        if row["notified_digest"] == row["digest"]: return
        if db.execute("SELECT count(*) FROM notifications").fetchone()[0] >= MAX_NOTIFICATIONS: raise InboxError("notification_capacity")
        cursor = db.execute("INSERT INTO notifications(event_id,kind,digest,created_at) VALUES(?,?,?,?)", (row["id"], kind, row["digest"], now))
        db.execute("UPDATE events SET notified_digest=?,current_notification=? WHERE id=?", (row["digest"], cursor.lastrowid, row["id"]))

    def remove(self, db, message_id, state, now):
        row = db.execute("SELECT * FROM events WHERE id=?", (message_id,)).fetchone()
        if row is None or (row["latched"] and state != "tombstoned"): return
        evidence = strict_json(row["immutable"])
        digest = sha(encode({"id": message_id, "state": state, "sha256": evidence["sha256"]}))
        db.execute("UPDATE events SET snapshot=NULL,digest=?,state=?,latched=max(latched,?),last_observed=?,revalidated_at=? WHERE id=?",
                   (digest, state, int(state == "tombstoned"), now, now, message_id))
        self.notify(db, db.execute("SELECT * FROM events WHERE id=?", (message_id,)).fetchone(), "removal", now)

    def receive(self, db, event, now, round_number):
        old = db.execute("SELECT * FROM events WHERE id=?", (event["id"],)).fetchone()
        metadata = encode(immutable(event))
        if old and bytes(old["immutable"]) != metadata: raise InboxError("source_identity_changed")
        if old and old["latched"]:
            db.execute("UPDATE events SET seen_round=? WHERE id=?", (round_number, event["id"]))
            return
        if old is None:
            if db.execute("SELECT count(*) FROM events").fetchone()[0] >= MAX_EVENTS: raise InboxError("event_capacity")
            if (db.execute("SELECT count(*) FROM notifications").fetchone()[0]
                    + db.execute("SELECT count(*) FROM events WHERE latched=0").fetchone()[0] >= MAX_NOTIFICATIONS):
                raise InboxError("notification_capacity")
            db.execute("INSERT INTO events(id,immutable,digest,state,first_observed,last_observed,revalidated_at,seen_round) VALUES(?,?,'','staged',?,?,?,?)",
                       (event["id"], metadata, now, now, now, round_number))
        if event["type"] == "tombstone":
            self.remove(db, event["id"], "tombstoned", now)
        else:
            db.execute("UPDATE events SET snapshot=?,digest=?,state='staged',last_observed=?,revalidated_at=?,seen_round=? WHERE id=?",
                       (encode(event), event_digest(event), now, now, round_number, event["id"]))
            if self.usage(db) > BODY_BUDGET: raise InboxError("catalog_capacity")

    def promote(self, db, now):
        staged = db.execute("SELECT * FROM events WHERE state='staged'").fetchall()
        new_notifications = sum(r["notified_digest"] != r["digest"] for r in staged)
        live_count = db.execute("SELECT count(*) FROM events WHERE latched=0").fetchone()[0]
        existing = db.execute("SELECT count(*) FROM notifications").fetchone()[0]
        if existing + new_notifications + live_count > MAX_NOTIFICATIONS or self.usage(db) + 256 * new_notifications > BODY_BUDGET:
            raise InboxError("notification_capacity")
        for row in staged:
            self.notify(db, row, "received" if row["notified_digest"] is None else "correction", now)
        db.execute("UPDATE events SET state='ready' WHERE state='staged'")
        db.execute("UPDATE checkpoint SET phase='ready',resync=0,last_success=?,last_error=CASE WHEN last_error IN ('catalog_capacity','event_capacity','notification_capacity') THEN last_error ELSE NULL END", (now,))

    def _fetch(self, path, deadline):
        with _cancellation_guard() as check_cancelled:
            result = self._fetch_owned(path, deadline, check_cancelled)
            check_cancelled()
            return result

    def _fetch_owned(self, path, deadline, check_cancelled):
        check_cancelled()
        if deadline - time.monotonic() <= CLEANUP_SECONDS: raise InboxError("deadline_exceeded")
        request = encode({"url": self.binding["origin"] + path})
        if len(request) > 8192: raise InboxError("invalid_fetch_request")
        environment = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8"}
        worker, complete = None, False
        numbers = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)
        try:
            # Establish ownership before pending terminal signals are observed.
            mask = signal.pthread_sigmask(signal.SIG_BLOCK, numbers)
            try:
                worker = subprocess.Popen([sys.executable, "-I", "-B", str(Path(__file__).resolve()), "_fetch", str(os.getpid())],
                                          stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                          close_fds=True, start_new_session=True, env=environment)
            finally: signal.pthread_sigmask(signal.SIG_SETMASK, mask)
            os.set_blocking(worker.stdin.fileno(), False)
            os.set_blocking(worker.stdout.fileno(), False)
            output, sent = bytearray(), 0
            while True:
                check_cancelled()
                remaining = deadline - time.monotonic() - CLEANUP_SECONDS
                if remaining <= 0: raise InboxError("deadline_exceeded")
                readable, writable, _ = select.select([worker.stdout], [worker.stdin] if worker.stdin else [], [], min(remaining, 0.05))
                if writable:
                    try: sent += os.write(worker.stdin.fileno(), request[sent:])
                    except BlockingIOError: pass
                    if sent == len(request): worker.stdin.close(); worker.stdin = None
                if readable:
                    try: chunk = os.read(worker.stdout.fileno(), min(65536, MAX_IPC + 1 - len(output)))
                    except BlockingIOError: continue
                    if chunk == b"": break
                    output.extend(chunk)
                    if len(output) > MAX_IPC: raise InboxError("response_byte_limit")
            raw, complete = bytes(output), True
        finally:
            if worker is not None:
                mask = signal.pthread_sigmask(signal.SIG_BLOCK, numbers)
                try:
                    if complete:
                        try: worker.wait(timeout=max(0, min(0.05, deadline - time.monotonic())))
                        except subprocess.TimeoutExpired: pass
                    if worker.poll() is None: worker.kill()
                    try: worker.wait(timeout=max(0, deadline - time.monotonic()))
                    except subprocess.TimeoutExpired: raise InboxError("worker_cleanup_failed") from None
                finally:
                    try:
                        for stream in (worker.stdin, worker.stdout):
                            if stream is not None: stream.close()
                    finally: signal.pthread_sigmask(signal.SIG_SETMASK, mask)
        if time.monotonic() >= deadline: raise InboxError("deadline_exceeded")
        if len(raw) > MAX_IPC or worker.returncode != 0: raise InboxError("fetch_worker_failed")
        result = strict_json(raw)
        if not isinstance(result, dict): raise InboxError("fetch_worker_failed")
        if "error" in result:
            allowed = {"network_failed", "header_limit", "compressed_response", "response_content_type", "response_byte_limit", "truncated_response"}
            raise InboxError(result["error"] if isinstance(result["error"], str) and result["error"] in allowed else "fetch_worker_failed")
        if set(result) != {"status", "body"} or type(result["status"]) is not int: raise InboxError("fetch_worker_failed")
        try: body = base64.b64decode(result["body"], validate=True)
        except Exception: raise InboxError("fetch_worker_failed") from None
        if len(body) > MAX_RESPONSE: raise InboxError("response_byte_limit")
        parsed = strict_json(body)
        if time.monotonic() >= deadline: raise InboxError("deadline_exceeded")
        return result["status"], parsed

    def check_client(self, client):
        if client is None: return
        if (type(client) is not memo.Client or client.key is not None or client.save_request is not None
                or client.base_url != self.binding["origin"] or client.service != self.binding["service_id"]):
            raise InboxError("public_unsigned_client_required")
        # Never call client callbacks/openers: all network reads use our fixed,
        # keyless worker, even when a same-binding Client was supplied.

    def poll(self, client=None, max_requests=10, deadline_seconds=30):
        return self._poll(client, max_requests, deadline_seconds, resync=False)

    def resync(self, client=None, max_requests=10, deadline_seconds=30):
        return self._poll(client, max_requests, deadline_seconds, resync=True)

    def _poll(self, client, max_requests, deadline_seconds, resync):
        _require_worker_platform()
        self.check_client(client)
        if type(max_requests) is not int or not 1 <= max_requests <= 20 or type(deadline_seconds) not in (int, float) or not 0 < deadline_seconds <= 30:
            raise InboxError("poll_budget_limit")
        deadline, requests = time.monotonic() + deadline_seconds, 0
        def budget():
            if time.monotonic() >= deadline: raise InboxError("deadline_exceeded")
        def fetch(path):
            nonlocal requests
            budget()
            if requests >= max_requests: raise InboxError("request_budget")
            requests += 1
            status, result = self._fetch(path, deadline)
            budget()
            if not isinstance(result, dict): raise InboxError("invalid_response")
            if status == 409 and isinstance(result.get("error"), dict) and result["error"].get("code") == "cursor_reset": raise InboxError("cursor_reset")
            if status not in (200, 404): raise InboxError("source_http_error")
            return status, result
        def change_page(after, generation=""):
            query = {"after": after}
            if generation: query["generation"] = generation
            status, result = fetch("/api/changes?" + urllib.parse.urlencode(query))
            if (status != 200 or set(result) != {"ok", "messages", "after", "generation", "service_id"} or result["ok"] is not True
                    or not matches(result["generation"], GENERATION) or result["service_id"] != self.binding["service_id"]
                    or type(result["after"]) is not int or result["after"] < 0): raise InboxError("invalid_changes_response")
            if generation and result["generation"] != generation: raise InboxError("cursor_reset")
            records = result["messages"]
            if records is None: records = []
            if not isinstance(records, list) or len(records) > 100: raise InboxError("event_page_limit")
            for record in records: validate_event(record, self.binding, scoped=False)
            if after == -1:
                if records: raise InboxError("invalid_bootstrap")
            elif result["after"] < after or (records and result["after"] == after) or (not records and result["after"] != after):
                raise InboxError("correction_no_progress")
            budget()
            return records, result["after"], result["generation"]
        def event_page(path, generation, single=False):
            status, result = fetch(path)
            if status == 404:
                if single: return None, None
                raise InboxError("source_http_error")
            if (set(result) - {"ok", "messages", "next_cursor", "generation"} or result.get("ok") is not True
                    or not matches(result.get("generation"), GENERATION)): raise InboxError("invalid_event_response")
            if result["generation"] != generation: raise InboxError("cursor_reset")
            records = result.get("messages", [])
            if not isinstance(records, list) or len(records) > (1 if single else 100): raise InboxError("event_page_limit")
            if single and len(records) != 1: raise InboxError("invalid_event_response")
            ids = set()
            for record in records:
                if single and isinstance(record, dict) and record.get("visibility") != "public":
                    if set(record) - EVENT_FIELDS or not matches(record.get("id"), IDENTIFIER): raise InboxError("invalid_event_response")
                    continue  # The caller purges this known ID; no private payload is admitted.
                validate_event(record, self.binding, addressed=True, scoped=not single)
                if record["id"] in ids: raise InboxError("duplicate_event")
                ids.add(record["id"])
            cursor = result.get("next_cursor", "")
            if not isinstance(cursor, str) or len(cursor) > 4096 or (records and not cursor): raise InboxError("invalid_event_cursor")
            budget()
            return records, cursor
        with self.database(write=True) as db:
            self.require(db)
            state = db.execute("SELECT * FROM checkpoint").fetchone()
            if state["phase"] == "resync_required" and not resync: raise InboxError("resync_required")
            if state["resync"] and not resync: raise InboxError("resync_in_progress")
            if resync and not state["resync"]:
                with db:
                    db.execute("UPDATE checkpoint SET phase='new',generation='',cursor='',after=0,event_empty=0,resync=1,round=round+1,revalidate_after=0,last_error=NULL")
                    db.execute("UPDATE events SET state='staged' WHERE latched=0 AND snapshot IS NOT NULL")
            try:
                state = db.execute("SELECT * FROM checkpoint").fetchone()
                if state["phase"] == "new":
                    _records, after, generation = change_page(-1)
                    cursor = "start" if self.binding["start_mode"] == "history" else ""
                    with db: db.execute("UPDATE checkpoint SET generation=?,after=?,cursor=?,phase='events'", (generation, after, cursor))
                # Normal polls collect one source page, then catch up corrections.
                # Explicit resync instead traverses to empty, revalidates retained
                # IDs absent from that traversal, and only then exposes bodies.
                while True:
                    state = db.execute("SELECT * FROM checkpoint").fetchone()
                    if state["phase"] in ("messages", "ready"):
                        query = {"to": self.binding["recipient"], "limit": 100}
                        if self.binding["room"]: query["room"] = self.binding["room"]
                        if state["cursor"]: query["cursor"] = state["cursor"]
                        records, cursor = event_page("/api/messages?" + urllib.parse.urlencode(query), state["generation"])
                        if records and cursor == state["cursor"]: raise InboxError("event_no_progress")
                        if not records and cursor and cursor != state["cursor"] and state["cursor"] not in ("", "start"):
                            raise InboxError("event_no_progress")
                        now = int(time.time())
                        try:
                            with db:
                                for record in records: self.receive(db, record, now, state["round"])
                                phase = "messages" if state["resync"] and records else ("revalidate" if state["resync"] else "corrections")
                                db.execute("UPDATE checkpoint SET cursor=?,event_empty=?,phase=?,last_error=NULL", (cursor or state["cursor"], int(not records), phase))
                        except InboxError as error:
                            if str(error) not in ("catalog_capacity", "event_capacity", "notification_capacity"): raise
                            if state["resync"]:
                                # Never jump from an incomplete resync traversal
                                # to promotion. Still attempt bounded urgent purges.
                                position = state["after"]
                                while True:
                                    changes, next_position, _gen = change_page(position, state["generation"])
                                    for change in changes:
                                        if change["type"] == "tombstone":
                                            with db: self.remove(db, change["id"], "tombstoned", now)
                                    with db:
                                        for change in changes:
                                            if db.execute("SELECT 1 FROM events WHERE id=?", (change["id"],)).fetchone(): self.receive(db, change, now, state["round"])
                                        db.execute("UPDATE checkpoint SET after=?", (next_position,))
                                    position = next_position
                                    if not changes: break
                                raise
                            with db: db.execute("UPDATE checkpoint SET phase='corrections',last_error=?", (str(error),))
                            # Drain purges without advancing a rejected event page.
                        continue
                    if state["phase"] == "revalidate":
                        old = db.execute("SELECT * FROM events WHERE number>? AND seen_round<>? AND latched=0 ORDER BY number LIMIT 1", (state["revalidate_after"], state["round"])).fetchone()
                        if old is None:
                            with db: db.execute("UPDATE checkpoint SET phase='corrections'")
                            continue
                        records, _cursor = event_page("/e/" + urllib.parse.quote(old["id"], safe=""), state["generation"], single=True)
                        now = int(time.time())
                        with db:
                            if records is not None and records[0]["id"] != old["id"]: raise InboxError("message_identifier_mismatch")
                            if records is None or records[0].get("visibility") != "public" or (self.binding["room"] and records[0]["room"] != self.binding["room"]):
                                self.remove(db, old["id"], "unavailable", now)
                            else:
                                self.receive(db, records[0], now, state["round"])
                            db.execute("UPDATE checkpoint SET revalidate_after=?", (old["number"],))
                        continue
                    if state["phase"] == "corrections":
                        records, after, _generation = change_page(state["after"], state["generation"])
                        now = int(time.time())
                        # Known payload removals commit first, independent of any
                        # later attachment-update/admission capacity failure.
                        for record in records:
                            if record["type"] == "tombstone" and db.execute("SELECT 1 FROM events WHERE id=?", (record["id"],)).fetchone():
                                with db: self.remove(db, record["id"], "tombstoned", now)
                        with db:
                            for record in records:
                                if db.execute("SELECT 1 FROM events WHERE id=?", (record["id"],)).fetchone(): self.receive(db, record, now, state["round"])
                            db.execute("UPDATE checkpoint SET after=?", (after,))
                            if not records: self.promote(db, now)
                        if not records: break
                        continue
                    raise InboxError("invalid_checkpoint_state")
            except Exception as error:
                db.rollback()
                code = str(error) if isinstance(error, InboxError) else "poll_failed"
                with db:
                    if code in ("cursor_reset", "source_identity_changed"):
                        db.execute("UPDATE checkpoint SET phase='resync_required',resync=0,last_error=?", (code,))
                    else: db.execute("UPDATE checkpoint SET phase=CASE WHEN phase='ready' THEN 'events' ELSE phase END,last_error=?", (code,))
        result = self.status(); result["network_requests"] = requests
        return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", type=Path, required=True)
    parser.add_argument("--binding", type=Path, required=True, help="mode-600 immutable public binding JSON")
    sub = parser.add_subparsers(dest="action")
    sub.add_parser("create"); sub.add_parser("status")
    sub.add_parser("sender-controls-enable")
    for name in ("sender-mute", "sender-unmute", "sender-mutes"):
        action = sub.add_parser(name); action.add_argument("consumer")
        if name != "sender-mutes": action.add_argument("--signer", required=True)
    consumer = sub.add_parser("consumer-add"); consumer.add_argument("consumer")
    for name in ("poll", "resync"):
        action = sub.add_parser(name); action.add_argument("--max-requests", type=int, default=10); action.add_argument("--deadline", type=float, default=30)
    pending = sub.add_parser("pending"); pending.add_argument("consumer"); pending.add_argument("--limit", type=int, default=20); pending.add_argument("--include-muted", action="store_true")
    inspect = sub.add_parser("inspect"); inspect.add_argument("consumer"); inspect.add_argument("notification", type=int); inspect.add_argument("--sensitive", action="store_true")
    ack = sub.add_parser("ack"); ack.add_argument("consumer"); ack.add_argument("notification", type=int); ack.add_argument("digest")
    args = parser.parse_args(argv)
    try:
        with os.fdopen(private_fd(args.binding, os.O_RDONLY), "rb") as stream: raw = stream.read(8193)
        if len(raw) > 8192: raise InboxError("binding_byte_limit")
        inbox = Inbox(args.db, strict_json(raw))
        if args.action == "create": result = inbox.create()
        elif args.action == "sender-controls-enable": result = inbox.sender_controls_enable()
        elif args.action in ("sender-mute", "sender-unmute"): result = getattr(inbox, args.action.replace("-", "_"))(args.consumer, args.signer)
        elif args.action == "sender-mutes": result = inbox.sender_mutes(args.consumer)
        elif args.action == "consumer-add": result = inbox.add_consumer(args.consumer)
        elif args.action in ("poll", "resync"): result = getattr(inbox, args.action)(max_requests=args.max_requests, deadline_seconds=args.deadline)
        elif args.action == "pending": result = inbox.pending(args.consumer, args.limit, include_muted=args.include_muted)
        elif args.action == "inspect": result = inbox.read(args.consumer, args.notification, args.sensitive)
        elif args.action == "ack": result = inbox.ack(args.consumer, args.notification, args.digest)
        else: result = inbox.status()
        print(json.dumps(result, ensure_ascii=False))
        return 1 if args.action in ("poll", "resync") and result.get("last_error") not in (None, "request_budget") else 0
    except Exception as error:
        print(json.dumps({"error": str(error) if isinstance(error, InboxError) else "inbox_operation_failed"}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    if sys.argv[1:2] == ["_fetch"]: fetch_worker()
    else: raise SystemExit(main())
