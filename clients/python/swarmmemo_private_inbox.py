#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Private-room metadata continuity. Only explicit network methods sign reads.

Consumer names are bookkeeping, not access-control principals. This module has
no CLI, public export, body cache, network callback hook, or mutation transport.
"""
from __future__ import annotations

from contextlib import contextmanager
import fcntl
import hashlib
import os
from pathlib import Path
import re
import sqlite3
import stat
import time

import swarmmemo_private_transport as transport

PrivateInboxError = transport.PrivateInboxError
MAX_EVENTS = 10000
MAX_NOTIFICATIONS = 50000
MAX_CONSUMERS = 16
MAX_BYTES = 64 * 1024 * 1024
APPLICATION_ID = 0x534D5049
FROZEN = frozenset({"unavailable", "reauth_required", "resync_required"})

SCHEMA = """
CREATE TABLE binding(value BLOB NOT NULL,consent INTEGER NOT NULL CHECK(consent=1));
CREATE TABLE checkpoint(id INTEGER PRIMARY KEY CHECK(id=1),phase TEXT NOT NULL DEFAULT 'new',
 generation TEXT NOT NULL DEFAULT '',cursor TEXT NOT NULL DEFAULT '',last_sequence INTEGER NOT NULL DEFAULT 0,
 resync INTEGER NOT NULL DEFAULT 0,round INTEGER NOT NULL DEFAULT 0,step TEXT NOT NULL DEFAULT '',
 retained_after INTEGER NOT NULL DEFAULT 0,check_counter INTEGER NOT NULL DEFAULT 0,
 last_success INTEGER,last_error TEXT);
INSERT INTO checkpoint(id) VALUES(1);
CREATE TABLE events(number INTEGER PRIMARY KEY,id TEXT UNIQUE NOT NULL,immutable BLOB NOT NULL,
 metadata BLOB NOT NULL,digest TEXT NOT NULL,state TEXT NOT NULL,latched INTEGER NOT NULL DEFAULT 0,
 checked INTEGER NOT NULL DEFAULT 0,observed_at INTEGER NOT NULL,seen_round INTEGER NOT NULL DEFAULT 0,
 notified_digest TEXT,current_notification INTEGER);
CREATE TABLE notifications(id INTEGER PRIMARY KEY,event_id TEXT NOT NULL REFERENCES events(id),
 kind TEXT NOT NULL,digest TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX notifications_event ON notifications(event_id,id);
CREATE TABLE consumers(id TEXT PRIMARY KEY);
CREATE TABLE acknowledgements(consumer TEXT NOT NULL REFERENCES consumers(id),
 notification INTEGER NOT NULL REFERENCES notifications(id),digest TEXT NOT NULL,accepted_at INTEGER NOT NULL,
 PRIMARY KEY(consumer,notification));
PRAGMA user_version=1;
"""


def fail(code):
    raise PrivateInboxError(code)


def matches(value, pattern):
    return isinstance(value, str) and re.fullmatch(pattern, value) is not None


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def private_fd(path, flags):
    fd = os.open(path, flags | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        os.close(fd)
        fail("private_regular_file_required")
    return fd


class PrivateRoomInbox:
    def __init__(self, path, binding):
        try:
            raw = Path(path)
        except (TypeError, ValueError, OSError):
            fail("unsafe_path")
        if ".." in raw.parts or "\x00" in str(raw):
            fail("unsafe_path")
        self.path = raw.absolute()
        self._binding = transport.encode(transport.binding_validated(binding))

    @property
    def binding(self):
        return transport.strict_json(self._binding)

    @contextmanager
    def database(self, *, create=False, write=False):
        db = None
        try:
            parent = self.path.parent
            for part in (self.path, *self.path.parents):
                if part.is_symlink():
                    fail("unsafe_path")
            if not parent.exists():
                if create:
                    fail("private_directory_required")
                yield None
                return
            info = parent.stat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                fail("private_directory_required")
            if not os.path.lexists(self.path) and not create:
                yield None
                return
            fd = private_fd(str(self.path) + ".lock", os.O_RDWR | os.O_CREAT)
            with os.fdopen(fd, "a") as lock:
                try:
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    fail("inbox_busy")
                for suffix in ("-journal", "-wal", "-shm"):
                    name = str(self.path) + suffix
                    if os.path.lexists(name):
                        os.close(private_fd(name, os.O_RDONLY))
                recovery = os.path.lexists(str(self.path) + "-journal")
                fd = private_fd(self.path, (os.O_RDWR if write or recovery else os.O_RDONLY)
                                | (os.O_CREAT if create else 0))
                os.close(fd)
                db = sqlite3.connect(self.path.as_uri() + ("?mode=rw" if write or recovery else "?mode=ro"),
                                     uri=True, timeout=1)
                db.row_factory = sqlite3.Row
                db.execute("PRAGMA foreign_keys=ON")
                if write or recovery:
                    db.execute("PRAGMA journal_mode=DELETE")
                    db.execute("PRAGMA synchronous=EXTRA")
                version = db.execute("PRAGMA user_version").fetchone()[0]
                application = db.execute("PRAGMA application_id").fetchone()[0]
                if version == 0 and application == 0 and create:
                    if db.execute("SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").fetchone()[0]:
                        fail("binding_mismatch")
                    db.executescript("BEGIN IMMEDIATE;\n" + SCHEMA)
                    with db:
                        db.execute("PRAGMA application_id=" + str(APPLICATION_ID))
                        db.execute("INSERT INTO binding VALUES(?,1)", (self._binding,))
                elif version != 1 or application != APPLICATION_ID:
                    fail("unsupported_private_inbox")
                rows = db.execute("SELECT value,consent FROM binding").fetchall()
                if len(rows) != 1 or bytes(rows[0][0]) != self._binding or rows[0][1] != 1:
                    fail("binding_mismatch")
                if write:
                    db.execute("PRAGMA max_page_count=" + str(128 * 1024 * 1024 // db.execute("PRAGMA page_size").fetchone()[0]))
                    # Retry directory durability even if a previous create failed
                    # after committing its binding but before syncing its entry.
                    fd = private_fd(self.path, os.O_RDONLY)
                    try:
                        os.fsync(fd)
                    finally:
                        os.close(fd)
                    fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
                    try:
                        os.fsync(fd)
                    finally:
                        os.close(fd)
                else:
                    db.execute("PRAGMA query_only=ON")
                try:
                    yield db
                finally:
                    db.close()
                    db = None
        except (sqlite3.Error, OSError):
            fail("storage_error")
        finally:
            if db is not None:
                db.close()

    @staticmethod
    def require(db):
        if db is None:
            fail("inbox_not_created")

    @staticmethod
    def checkpoint(db):
        row = db.execute("SELECT * FROM checkpoint WHERE id=1").fetchone()
        if row is None:
            fail("catalog_integrity_error")
        return row

    def create(self, *, consent_private_metadata=False):
        if consent_private_metadata is not True:
            fail("private_metadata_consent_required")
        with self.database(create=True, write=True) as db:
            return self._status(db)

    def _status(self, db, requests=0):
        if db is None:
            return {"phase": "not_created", "network_requests": 0, "freshness": "offline_metadata_only"}
        state = self.checkpoint(db)
        return {"phase": state["phase"], "resync_active": bool(state["resync"]),
                "resync_step": state["step"], "last_success": state["last_success"],
                "last_error": state["last_error"], "events": dict(db.execute("SELECT state,count(*) FROM events GROUP BY state")),
                "notifications": db.execute("SELECT count(*) FROM notifications").fetchone()[0],
                "consumers": db.execute("SELECT count(*) FROM consumers").fetchone()[0],
                "network_requests": requests, "freshness": "offline_metadata_only"}

    def status(self):
        with self.database() as db:
            return self._status(db)

    @staticmethod
    def consumer_known(db, consumer):
        if not matches(consumer, r"[A-Za-z0-9._-]{1,64}"):
            fail("invalid_consumer")
        if not db.execute("SELECT 1 FROM consumers WHERE id=?", (consumer,)).fetchone():
            fail("unknown_consumer")

    def add_consumer(self, consumer):
        if not matches(consumer, r"[A-Za-z0-9._-]{1,64}"):
            fail("invalid_consumer")
        with self.database(write=True) as db:
            self.require(db)
            if db.execute("SELECT 1 FROM consumers WHERE id=?", (consumer,)).fetchone():
                return {"created": False}
            if db.execute("SELECT count(*) FROM consumers").fetchone()[0] >= MAX_CONSUMERS:
                fail("consumer_limit")
            with db:
                db.execute("INSERT INTO consumers VALUES(?)", (consumer,))
                self.capacity(db)
            return {"created": True}

    @staticmethod
    def capacity(db):
        count = db.execute("SELECT count(*) FROM events").fetchone()[0]
        consumers = db.execute("SELECT count(*) FROM consumers").fetchone()[0]
        notifications = db.execute("SELECT count(*) FROM notifications").fetchone()[0]
        acknowledgements = db.execute("SELECT count(*) FROM acknowledgements").fetchone()[0]
        # Live -> unavailable -> tombstoned needs two future notifications.
        # A pending resync promotion needs its own currently reserved notification.
        future = db.execute("SELECT coalesce(sum(CASE state WHEN 'live' THEN 2 WHEN 'unavailable' THEN 1 ELSE 0 END),0) FROM events WHERE latched=0").fetchone()[0]
        promotions = db.execute("SELECT count(*) FROM events WHERE state='live' AND (notified_digest IS NULL OR notified_digest<>digest)").fetchone()[0]
        future += promotions
        pending_acks = db.execute("SELECT count(*) FROM consumers c CROSS JOIN events e WHERE e.current_notification IS NOT NULL AND NOT EXISTS(SELECT 1 FROM acknowledgements a WHERE a.consumer=c.id AND a.notification=e.current_notification)").fetchone()[0]
        used = db.execute("SELECT coalesce(sum(length(metadata)+length(immutable)+512),0) FROM events").fetchone()[0]
        used += 4096 + 256 * (notifications + future) + 128 * (acknowledgements + pending_acks + future * consumers)
        if count > MAX_EVENTS:
            fail("event_capacity")
        if notifications + future > MAX_NOTIFICATIONS:
            fail("notification_capacity")
        if used > MAX_BYTES:
            fail("catalog_capacity")

    @staticmethod
    def eligible(row, phase):
        return (row["notification_id"] == row["current_notification"]
                and row["notification_digest"] == row["digest"]
                and (row["state"] in ("unavailable", "tombstoned") or phase == "ready"))

    def _notification(self, db, consumer, notification_id):
        self.consumer_known(db, consumer)
        if type(notification_id) is not int or notification_id < 1:
            fail("invalid_notification")
        row = db.execute("SELECT n.id notification_id,n.digest notification_digest,n.kind,e.* FROM notifications n JOIN events e ON e.id=n.event_id WHERE n.id=?", (notification_id,)).fetchone()
        if row is None:
            fail("unknown_notification")
        if not self.eligible(row, self.checkpoint(db)["phase"]):
            fail("notification_superseded_or_unreconciled")
        self.verify_row(row)
        return row

    @staticmethod
    def verify_row(row):
        raw = bytes(row["metadata"])
        if transport.encode(transport.strict_json(raw)) != raw or sha(raw) != row["digest"]:
            fail("catalog_integrity_error")

    def pending(self, consumer, limit=20):
        if type(limit) is not int or not 1 <= limit <= 100:
            fail("invalid_limit")
        with self.database() as db:
            self.require(db)
            self.consumer_known(db, consumer)
            phase = self.checkpoint(db)["phase"]
            rows = db.execute("SELECT n.id notification_id,n.digest notification_digest,n.kind,e.* FROM notifications n JOIN events e ON e.id=n.event_id WHERE n.id=e.current_notification AND NOT EXISTS(SELECT 1 FROM acknowledgements a WHERE a.consumer=? AND a.notification=n.id) AND (e.state IN ('unavailable','tombstoned') OR ?='ready') ORDER BY n.id LIMIT ?", (consumer, phase, limit)).fetchall()
            result = []
            for row in rows:
                self.verify_row(row)
                if not self.eligible(row, phase):
                    fail("catalog_integrity_error")
                result.append({"notification_id": row["notification_id"], "message_id": row["id"],
                               "kind": row["kind"], "state": row["state"], "snapshot_digest": row["digest"],
                               "freshness": "offline_metadata_only"})
            return result

    def ack(self, consumer, notification_id, returned_digest):
        if type(notification_id) is not int or notification_id < 1 or not matches(returned_digest, r"[a-f0-9]{64}"):
            fail("invalid_acknowledgement")
        with self.database(write=True) as db:
            self.require(db)
            self.consumer_known(db, consumer)
            prior = db.execute("SELECT digest FROM acknowledgements WHERE consumer=? AND notification=?", (consumer, notification_id)).fetchone()
            if prior:
                if prior[0] != returned_digest:
                    fail("ack_digest_mismatch")
                return {"acknowledged": True, "duplicate": True}
            row = self._notification(db, consumer, notification_id)
            if row["digest"] != returned_digest:
                fail("ack_digest_mismatch")
            with db:
                db.execute("INSERT INTO acknowledgements VALUES(?,?,?,?)", (consumer, notification_id, returned_digest, int(time.time())))
                self.capacity(db)
            return {"acknowledged": True, "duplicate": False}

    @staticmethod
    def _notify(db, message_id, kind):
        row = db.execute("SELECT * FROM events WHERE id=?", (message_id,)).fetchone()
        if row["notified_digest"] == row["digest"]:
            return
        notification = db.execute("INSERT INTO notifications(event_id,kind,digest,created_at) VALUES(?,?,?,?)", (message_id, kind, row["digest"], int(time.time()))).lastrowid
        db.execute("UPDATE events SET current_notification=?,notified_digest=digest WHERE id=?", (notification, message_id))

    @staticmethod
    def _checked(db, message_id, round_number=0):
        db.execute("UPDATE checkpoint SET check_counter=check_counter+1 WHERE id=1")
        db.execute("UPDATE events SET checked=(SELECT check_counter FROM checkpoint),observed_at=?,seen_round=CASE WHEN ?>0 THEN ? ELSE seen_round END WHERE id=?", (int(time.time()), round_number, round_number, message_id))

    def _receive(self, db, event, *, staged=False, round_number=0):
        transport.validate_private_event(event, self.binding)
        old = db.execute("SELECT * FROM events WHERE id=?", (event["id"],)).fetchone()
        pinned = transport.immutable(event)
        if old:
            self.verify_row(old)
            original = transport.strict_json(old["immutable"])
            compare = dict(original)
            if event["type"] == "tombstone":
                compare.pop("attachments", None)
            elif old["latched"] and "attachments" not in original:
                # A first-seen tombstone never disclosed attachment identity.
                # An unhide cannot teach it new material or undo the latch.
                pinned.pop("attachments", None)
            if compare != pinned:
                fail("source_identity_changed")
            if old["latched"]:
                self._checked(db, event["id"], round_number)
                return
        projection = transport.encode(transport.metadata(event))
        value = transport.digest(event)
        if sha(projection) != value:
            fail("catalog_integrity_error")
        state = "tombstoned" if event["type"] == "tombstone" else "live"
        if old is None:
            db.execute("INSERT INTO events(id,immutable,metadata,digest,state,latched,observed_at) VALUES(?,?,?,?,?,?,?)", (event["id"], transport.encode(pinned), projection, value, state, int(state == "tombstoned"), int(time.time())))
        else:
            db.execute("UPDATE events SET metadata=?,digest=?,state=?,latched=? WHERE id=?", (projection, value, state, int(state == "tombstoned"), event["id"]))
            if staged and state == "live" and old["digest"] != value:
                # Observed A -> B -> A during a blocked resync must not revive
                # an old A notification/ack. Keep historical evidence, but break
                # the current notification link at the first divergence.
                db.execute("UPDATE events SET current_notification=NULL,notified_digest=NULL WHERE id=?", (event["id"],))
        self._checked(db, event["id"], round_number)
        if not staged or state == "tombstoned":
            self._notify(db, event["id"], "removal" if state == "tombstoned" else ("received" if old is None else "correction"))
        self.capacity(db)

    def _unavailable(self, db, message_id, round_number=0):
        row = db.execute("SELECT * FROM events WHERE id=?", (message_id,)).fetchone()
        if row is None:
            fail("unknown_event")
        self.verify_row(row)
        if not row["latched"]:
            original = transport.strict_json(row["immutable"])
            raw = transport.encode({"state": "unavailable", "id": message_id, "sha256": original["sha256"]})
            db.execute("UPDATE events SET metadata=?,digest=?,state='unavailable' WHERE id=?", (raw, sha(raw), message_id))
            self._notify(db, message_id, "removal")
        self._checked(db, message_id, round_number)
        self.capacity(db)

    @staticmethod
    def _record_error(db, code):
        phase = {"scope_unavailable": "unavailable", "reauth_required": "reauth_required",
                 "cursor_reset": "resync_required", "source_identity_changed": "resync_required"}.get(code)
        with db:
            if phase:
                db.execute("UPDATE checkpoint SET phase=?,resync=0,step='',last_error=? WHERE id=1", (phase, code))
            else:
                db.execute("UPDATE checkpoint SET last_error=? WHERE id=1", (code,))

    def _page(self, db, result, cursor, *, single_id=None, fence=False):
        if (not isinstance(result, dict) or set(result) != {"messages", "next_cursor", "generation"}
                or not matches(result["generation"], r"[a-f0-9]{32}")
                or not isinstance(result["next_cursor"], str) or len(result["next_cursor"]) > 4096
                or not isinstance(result["messages"], list) or len(result["messages"]) > (1 if single_id else 100)):
            fail("invalid_response")
        state = self.checkpoint(db)
        if state["generation"] and result["generation"] != state["generation"]:
            fail("cursor_reset")
        records = result["messages"]
        if single_id and (len(records) != 1 or records[0].get("id") != single_id):
            fail("invalid_response")
        previous = 0 if single_id or fence else state["last_sequence"]
        seen = set()
        for event in records:
            transport.validate_private_event(event, self.binding)
            if event["id"] in seen or event["sequence"] <= previous:
                fail("source_no_progress")
            seen.add(event["id"])
            previous = event["sequence"]
        if not single_id:
            if records and result["next_cursor"] == cursor:
                fail("source_no_progress")
            if not records and cursor not in ("", "start") and result["next_cursor"] != cursor:
                fail("source_no_progress")
        return records

    @staticmethod
    def _budget(max_requests, deadline_seconds):
        if type(max_requests) is not int or not 5 <= max_requests <= 20:
            fail("request_budget_limit")
        if type(deadline_seconds) not in (int, float) or not 0 < deadline_seconds <= 30:
            fail("deadline_limit")
        return time.monotonic() + deadline_seconds

    @staticmethod
    def _time(deadline):
        if time.monotonic() >= deadline:
            fail("deadline_exceeded")

    def _recheck(self, db, session, message_id, *, staged=False, round_number=0):
        result = session.event(message_id)
        if result is None:
            with db:
                self._unavailable(db, message_id, round_number)
            # A missing event is not proof of membership loss. A fresh room
            # check can detect a scope denial; an unobserved re-add is unknowable.
            session.room()
        else:
            records = self._page(db, result, "", single_id=message_id)
            with db:
                self._receive(db, records[0], staged=staged, round_number=round_number)

    def poll(self, *, key_path, max_requests=10, deadline_seconds=30):
        return self._collect(key_path, max_requests, deadline_seconds, resync=False)

    def resync(self, *, key_path, max_requests=10, deadline_seconds=30):
        return self._collect(key_path, max_requests, deadline_seconds, resync=True)

    def _collect(self, key_path, max_requests, deadline_seconds, *, resync):
        deadline = self._budget(max_requests, deadline_seconds)
        with self.database(write=True) as db:
            self.require(db)
            state = self.checkpoint(db)
            if not resync and (state["phase"] in FROZEN or state["resync"]):
                fail(state["phase"] if state["phase"] in FROZEN else "resync_in_progress")
            if resync and not state["resync"]:
                with db:
                    db.execute("UPDATE checkpoint SET phase='reconciling',resync=1,round=round+1,step='traverse',generation='',cursor=?,last_sequence=0,retained_after=0,last_error=NULL WHERE id=1", ("start" if self.binding["start_mode"] == "history" else "",))
            session = transport.ReadSession(self.binding, key_path, max_requests=max_requests, deadline_seconds=max(0.001, deadline-time.monotonic()))
            try:
                session.capabilities()
                session.room()
                if resync:
                    self._resync(db, session, max_requests, deadline)
                else:
                    # Reserve one page and a possible room check after a 404.
                    rows = db.execute("SELECT id FROM events WHERE latched=0 ORDER BY checked,number LIMIT ?", (max_requests,)).fetchall()
                    for row in rows:
                        if session.requests + 3 > max_requests:
                            break
                        self._recheck(db, session, row["id"])
                        self._time(deadline)
                    if session.requests >= max_requests:
                        fail("request_budget")
                    state = self.checkpoint(db)
                    cursor = state["cursor"] or ("start" if self.binding["start_mode"] == "history" else "")
                    result = session.messages(cursor)
                    records = self._page(db, result, cursor)
                    self._time(deadline)
                    with db:
                        for event in records:
                            self._receive(db, event)
                            self._time(deadline)
                        db.execute("UPDATE checkpoint SET generation=?,cursor=?,last_sequence=?,phase='ready',last_success=?,last_error=NULL WHERE id=1", (result["generation"], result["next_cursor"], records[-1]["sequence"] if records else state["last_sequence"], int(time.time())))
            except PrivateInboxError as exc:
                db.rollback()
                self._record_error(db, exc.code)
            return self._status(db, session.requests)

    def _resync(self, db, session, max_requests, deadline):
        while session.requests < max_requests:
            self._time(deadline)
            state = self.checkpoint(db)
            if state["step"] == "traverse":
                result = session.messages(state["cursor"])
                records = self._page(db, result, state["cursor"])
                self._time(deadline)
                with db:
                    for event in records:
                        self._receive(db, event, staged=True, round_number=state["round"])
                        self._time(deadline)
                    db.execute("UPDATE checkpoint SET generation=?,cursor=?,last_sequence=?,step=? WHERE id=1", (result["generation"], result["next_cursor"], records[-1]["sequence"] if records else state["last_sequence"], "traverse" if records else "retained"))
            elif state["step"] == "retained":
                row = db.execute("SELECT number,id FROM events WHERE latched=0 AND seen_round<>? AND number>? ORDER BY number LIMIT 1", (state["round"], state["retained_after"])).fetchone()
                if row is None:
                    with db:
                        db.execute("UPDATE checkpoint SET step='fence' WHERE id=1")
                    continue
                if session.requests + 2 > max_requests:
                    break
                self._recheck(db, session, row["id"], staged=True, round_number=state["round"])
                with db:
                    db.execute("UPDATE checkpoint SET retained_after=? WHERE id=1", (row["number"],))
            elif state["step"] == "fence":
                result = session.messages(state["cursor"], limit=10 if self.binding["schema"] == 2 else 1)
                self._page(db, result, state["cursor"], fence=True)
                self._time(deadline)
                with db:
                    for row in db.execute("SELECT e.id,EXISTS(SELECT 1 FROM notifications n WHERE n.event_id=e.id) AS history FROM events e WHERE state='live'").fetchall():
                        self._notify(db, row["id"], "correction" if row["history"] else "received")
                        self._time(deadline)
                    self.capacity(db)
                    db.execute("UPDATE checkpoint SET phase='ready',resync=0,step='',last_success=?,last_error=NULL WHERE id=1", (int(time.time()),))
                return
            else:
                fail("catalog_integrity_error")
        fail("request_budget")

    def read_current(self, consumer, notification_id, *, key_path,
                     disclose_private_body=False, max_requests=10, deadline_seconds=30):
        if disclose_private_body is not True:
            fail("private_body_consent_required")
        deadline = self._budget(max_requests, deadline_seconds)
        with self.database(write=True) as db:
            self.require(db)
            row = self._notification(db, consumer, notification_id)
            if row["state"] != "live" or self.checkpoint(db)["phase"] != "ready":
                fail("body_unavailable")
            session = transport.ReadSession(self.binding, key_path, max_requests=max_requests, deadline_seconds=max(0.001, deadline-time.monotonic()))
            try:
                session.capabilities()
                session.room()
                result = session.event(row["id"])
                if result is None:
                    with db:
                        self._unavailable(db, row["id"])
                    session.room()
                    fail("body_unavailable")
                records = self._page(db, result, "", single_id=row["id"])
                self._time(deadline)
                with db:
                    self._receive(db, records[0])
                current = db.execute("SELECT * FROM events WHERE id=?", (row["id"],)).fetchone()
                if current["state"] != "live" or current["digest"] != row["digest"]:
                    fail("notification_superseded")
                self._time(deadline)
                return {"notification_id": notification_id, "snapshot_digest": current["digest"],
                        "message": records[0], "observed_at": current["observed_at"],
                        "freshness": "online_revalidated_not_continuous", "untrusted_content": True,
                        "network_requests": session.requests}
            except PrivateInboxError as exc:
                db.rollback()
                self._record_error(db, exc.code)
                raise
