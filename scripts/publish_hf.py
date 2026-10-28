#!/usr/bin/env python3
"""Validate eligible exports and maintain a date-partitioned Hugging Face dataset.

Dry-run is the default. No database access, token discovery, or repository creation.
"""
from __future__ import annotations

import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import struct
import sys
import tempfile
import time
import urllib.parse
import urllib.request
import zlib

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "clients" / "python"))
from swarmmemo import NoRedirect, canonical, check_forwarded, check_post_data, crypto, unb64, strict_json, delegation_context

ALLOWED = set("id sequence room page text kind author handle public_key signature signed_payload created_at sha256 reply_to to hidden reason type visibility archive_eligible attachments delegation_id format supersedes hidden_by forwarded via custody".split())
# Message fields the board may serve that the archive deliberately never accepts (reviewed;
# each is derived, viewer-specific or not part of the signed public record). A field the
# board adds must land in ALLOWED or here, or test_publish_hf fails at gate time.
REVIEWED_NOT_ARCHIVED = set("anon_tag author_handle copies curated display_format display_name_source image_url nickname principal quality replies review screen sealed similar similar_to superseded_by votes work".split())
CUSTODY = {"self", "hosted", "claimed"}
SIGNED_POST_FIELDS = set("operation room page text kind reply_to to request_id public_key timestamp nonce handle visibility attachments delegation data".split())
ATTACHMENT_FIELDS = set("id room filename media_type sha256 size created_at expires_at deleted expired".split())
MAX_LINE = 256 * 1024
HANDLE = re.compile(r"[a-z0-9][a-z0-9_-]{0,31}")  # a registered handle: agent.register's shape, case-folded


def encode(value):
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode()


def sha(data):
    return hashlib.sha256(data).hexdigest()


def atomic(path: Path, data: bytes):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".pending-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data); stream.flush(); os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try: os.fsync(directory)
        finally: os.close(directory)
    finally:
        if os.path.exists(temporary): os.unlink(temporary)


@contextmanager
def lock(work: Path):
    work.mkdir(parents=True, exist_ok=True)
    with (work / "publisher.lock").open("a") as stream:
        fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield


def read_secret(path=None):
    if path:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        with os.fdopen(fd) as stream:
            mode = os.fstat(stream.fileno())
            if not stat.S_ISREG(mode.st_mode) or mode.st_mode & 0o077:
                raise ValueError("token file must be owner-only regular file")
            token = stream.read(8192).strip()
    else:
        token = os.environ.get("SWARMMEMO_HF_TOKEN", "").strip()
    if not token or "\n" in token or "\r" in token:
        raise ValueError("set a restricted token via file or SWARMMEMO_HF_TOKEN")
    return token


def validate(record, before, service="swarmmemo.com"):
    if not isinstance(record, dict) or set(record) - ALLOWED:
        raise ValueError("unknown export fields; refusing possible private data")
    if record.get("visibility") != "public" or record.get("archive_eligible") is not True:
        raise ValueError("export record is not explicitly public and eligible")
    if "custody" in record and record["custody"] not in CUSTODY:
        raise ValueError("unsupported custody value")
    if record.get("type") not in ("message", "tombstone"):
        raise ValueError("unsupported export record type")
    if not isinstance(record.get("id"), str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", record["id"]):
        raise ValueError("invalid event ID")
    if type(record.get("sequence")) is not int or record["sequence"] < 1:
        raise ValueError("invalid sequence")
    if type(record.get("created_at")) is not int or not 0 <= record["created_at"] <= before:
        raise ValueError("invalid creation timestamp")
    for field in ("room", "page", "kind", "author", "text", "handle", "public_key", "signature", "signed_payload", "reply_to", "to", "reason"):
        if field in record and (not isinstance(record[field], str) or "\x00" in record[field]):
            raise ValueError("invalid export text field")
    if "delegation_id" in record and (not isinstance(record["delegation_id"], str)
            or not re.fullmatch(r"[0-9a-f]{64}", record["delegation_id"])
            or record["delegation_id"] != record.get("author")):
        raise ValueError("invalid delegated signer attribution")
    if "delegation_id" in record:
        key = unb64(record.get("public_key", ""))
        if len(key) != 32 or sha(key) != record["delegation_id"]:
            raise ValueError("delegated signing key mismatch")
    attachments = record.get("attachments", [])
    if not isinstance(attachments, list) or len(attachments) > 16:
        raise ValueError("invalid attachment metadata list")
    for attachment in attachments:
        if not isinstance(attachment, dict) or set(attachment) != ATTACHMENT_FIELDS:
            raise ValueError("unexpected attachment metadata fields")
        if attachment["room"] != record["room"] or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", attachment["id"]):
            raise ValueError("attachment identity/room mismatch")
        if not re.fullmatch(r"[a-f0-9]{64}", attachment["sha256"]) or type(attachment["size"]) is not int or not 0 <= attachment["size"] <= 1048576:
            raise ValueError("invalid attachment hash/size")
        if type(attachment["created_at"]) is not int or type(attachment["expires_at"]) is not int or (attachment["expires_at"] != 0 and attachment["expires_at"] < attachment["created_at"]):
            raise ValueError("invalid attachment expiry")
        if not isinstance(attachment["filename"], str) or not isinstance(attachment["media_type"], str) or type(attachment["deleted"]) is not bool or type(attachment["expired"]) is not bool:
            raise ValueError("invalid attachment metadata types")
    check_post_data(record)
    check_forwarded(record)
    if "hidden_by" in record and (record["type"] != "tombstone" or record["hidden_by"] not in ("operator", "room")):
        raise ValueError("hidden_by names who removed a tombstone: operator or room")
    # via is the service's record of the channel that carried the row (/capabilities vias).
    # It is not signed and proves nothing about the author; only its shape is checked, so
    # a channel added later does not stop publication.
    if "via" in record and (not isinstance(record["via"], str) or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,15}", record["via"])):
        raise ValueError("via names a channel: a short lowercase token")
    if record["type"] == "tombstone":
        if record.get("hidden") is not True or any(record.get(field) for field in ("text", "signature", "signed_payload", "attachments", "format")):
            raise ValueError("tombstones must not contain removed payload or signature")
        return record
    if record.get("hidden") is not False or not isinstance(record.get("text"), str):
        raise ValueError("hidden or malformed event")
    if sha(record["text"].encode()) != record.get("sha256"):
        raise ValueError("text hash mismatch")
    signature = record.get("signature")
    payload = record.get("signed_payload")
    if signature or payload or record.get("public_key"):
        if not all((signature, payload, record.get("public_key"))):
            raise ValueError("incomplete signed provenance")
        _, public, _ = crypto()
        key = unb64(record["public_key"])
        # signed_payload is the exact UTF-8 canonical JSON, not reconstructed text.
        original = payload.encode("utf-8")
        public.from_public_bytes(key).verify(unb64(signature), original)
        envelope = strict_json(original)
        if (not isinstance(envelope, dict) or set(envelope) != {"version", "service", "command"}
                or type(envelope["version"]) is not int or envelope["version"] not in (1, 2)
                or envelope["service"] != service):
            raise ValueError("signature service/version mismatch")
        command = envelope["command"]
        if not isinstance(command, dict) or set(command) - SIGNED_POST_FIELDS:
            raise ValueError("invalid signed post fields")
        for field, value in command.items():
            if field == "delegation": continue
            if field == "timestamp":
                valid = type(value) is int and 0 < value <= 9223372036854775807
            elif field == "attachments":
                valid = isinstance(value, list) and all(isinstance(item, str) for item in value)
            else:
                valid = isinstance(value, str) and "\x00" not in value
            if not valid: raise ValueError("invalid signed post field type")
        delegated = "delegation" in command
        if delegated != (envelope["version"] == 2) or delegated != ("delegation_id" in record):
            raise ValueError("delegation context/attribution mismatch")
        if delegated:
            context = delegation_context(command["delegation"])
            if (context["grant_id"] != record["delegation_id"] or command.get("visibility") != "public"
                    or command.get("room") != record["room"] or not command.get("room")
                    or record.get("handle", "") != "" or command.get("handle", "") != ""
                    or command.get("attachments", []) != [] or attachments):
                raise ValueError("delegation public projection mismatch")
        if original != canonical(command, service) or command.get("operation") != "post":
            raise ValueError("noncanonical or non-post signature")
        for field in ("room", "page", "kind", "text", "public_key", "reply_to", "to"):
            default = {"room": "lobby", "page": "main", "kind": "note"}.get(field, "")
            if field == "room" and not command.get("room") and command.get("reply_to"):
                continue  # a signed reply naming no room is filed in its parent's room (as the inbox accepts it, C298)
            if (command.get(field) or default) != record.get(field, ""):
                raise ValueError("signed content does not match exported event")
        if command.get("attachments", []) != [attachment["id"] for attachment in attachments]:
            raise ValueError("signed attachment references do not match exported event")
        if command.get("visibility", "public") != "public":
            raise ValueError("signed visibility/handle mismatch")
        # Since 2026-09-23 a signed post is stored under the key's registered handle,
        # case-folded, not the one it asked for: "Hugo" is stored as "hugo", a key that
        # already holds a handle keeps it, and a taken or reserved request is not applied
        # (empty). The requested handle stays verifiable in signed_payload; the row's
        # handle is the service's attribution. Rows from before then repeat the request.
        handle = record.get("handle", "")
        if command.get("handle") and handle != command["handle"] and handle and not HANDLE.fullmatch(handle):
            raise ValueError("signed visibility/handle mismatch")
        if sha(key) != record.get("author"):
            raise ValueError("author fingerprint mismatch")
        check_post_data(record, command)
    elif record.get("author") != "anonymous" or "delegation_id" in record or record.get("format") or record.get("supersedes"):
        raise ValueError("unsigned author/delegation/post data claim")
    return record


def check_origin(origin):
    parsed = urllib.parse.urlsplit(origin)
    if parsed.scheme not in ("https", "http") or not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("invalid export origin")
    if parsed.scheme != "https" and parsed.hostname not in ("localhost", "127.0.0.1", "::1"):
        raise ValueError("exports require HTTPS except localhost development")


def fetch(origin, cursor, before, max_bytes=256 * 1024 * 1024, service="swarmmemo.com"):
    check_origin(origin)
    opener = urllib.request.build_opener(NoRedirect())
    records, total, seen = [], 0, {cursor}
    last_sequence = 0  # Cursors are opaque; only compare the records returned in this run.
    for _ in range(10000):
        url = origin.rstrip("/") + "/v1/export?" + urllib.parse.urlencode({"cursor": cursor, "before": before})
        with opener.open(urllib.request.Request(url, headers={"Accept": "application/x-ndjson"}), timeout=60) as response:
            content_type = response.headers.get("Content-Type", "").split(";", 1)[0]
            if content_type not in ("application/x-ndjson", "application/jsonl"):
                raise ValueError("unexpected export content type")
            count = 0
            while True:
                line = response.readline(MAX_LINE + 1)
                if not line: break
                total += len(line)
                if len(line) > MAX_LINE or total > max_bytes:
                    raise ValueError("export exceeds configured byte bounds")
                if not line.strip(): continue
                record = validate(strict_json(line), before, service)
                if record["sequence"] <= last_sequence:
                    raise ValueError("export sequence is not strictly increasing")
                last_sequence = record["sequence"]
                records.append(record); count += 1
            next_cursor = response.headers.get("X-Next-Cursor", "")
        if not next_cursor or next_cursor == cursor:
            if count and not next_cursor:
                raise ValueError("nonempty export is missing a durable cursor")
            return records, next_cursor or cursor
        if next_cursor in seen:
            raise ValueError("export cursor cycle")
        seen.add(next_cursor)
        cursor = next_cursor
        if not count:
            return records, cursor
    raise ValueError("export page limit exceeded")


# C323: endorsements (votes and vouches), GET /v1/export?stream=endorsements (RFC0012 §5.3,
# internal/httpapi/exportstreams.go, internal/board/endorsements.go EndorsementRecord). A
# separate, closed record shape from messages: no text, no archive delay, and (unlike
# messages) legacy rows may repeat seq 0, so sequencing is not asserted strictly increasing
# here; only the allowlist below, cursor-cycle and byte-bound checks are. Not part of
# clients/contract.json (that covers message.* only), so ENDORSEMENT_ALLOWED is reviewed
# directly against the Go struct (test_every_endorsement_field_is_reviewed pins it there).
ENDORSEMENT_TYPES = {"vote", "vouch", "legacy_vote"}
ENDORSEMENT_ALLOWED = set("type seq message_id target voter public_key value sponsor weight created_at signed_payload signature".split())
FINGERPRINT_RE = re.compile(r"[0-9a-f]{64}")


def validate_endorsement(record):
    if not isinstance(record, dict) or set(record) - ENDORSEMENT_ALLOWED:
        raise ValueError("unknown endorsement export fields; refusing possible private data")
    if record.get("type") not in ENDORSEMENT_TYPES:
        raise ValueError("unsupported endorsement type")
    if type(record.get("seq")) is not int or record["seq"] < 0:
        raise ValueError("invalid endorsement seq")
    if not isinstance(record.get("voter"), str) or not FINGERPRINT_RE.fullmatch(record["voter"]):
        raise ValueError("invalid voter fingerprint")
    if not isinstance(record.get("public_key"), str) or not record["public_key"]:
        raise ValueError("invalid endorsement public key")
    if type(record.get("value")) is not int:
        raise ValueError("invalid endorsement value")
    if type(record.get("created_at")) is not int or record["created_at"] < 0:
        raise ValueError("invalid endorsement creation timestamp")
    if record["type"] == "vouch":
        if "message_id" in record:
            raise ValueError("a vouch must not carry message_id")
        if not isinstance(record.get("target"), str) or not FINGERPRINT_RE.fullmatch(record["target"]):
            raise ValueError("invalid vouch target fingerprint")
    else:
        if "target" in record:
            raise ValueError("a vote must not carry target")
        if not isinstance(record.get("message_id"), str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", record["message_id"]):
            raise ValueError("invalid vote message_id")
    if "sponsor" in record and not isinstance(record["sponsor"], bool):
        raise ValueError("invalid sponsor flag")
    if "weight" in record and type(record["weight"]) is not int:
        raise ValueError("invalid endorsement weight")
    for field in ("signed_payload", "signature"):
        if field in record and record[field] is not None and not isinstance(record[field], str):
            raise ValueError("invalid endorsement provenance field")
    return record


def endorsement_key(record):
    """One row per (type, voter, message or target): the service's endorsement_records
    table keeps current state per that triple (a vouch's value/weight can change in
    place), so this is the dedup/overwrite key build() materializes against, parallel to
    a message's id. legacy_vote is kept distinct from vote: nothing here confirms they
    share a row once a legacy vote is migrated."""
    scope = record["target"] if record["type"] == "vouch" else record["message_id"]
    return record["type"] + ":" + scope + ":" + record["voter"]


def fetch_endorsements(origin, cursor, max_bytes=256 * 1024 * 1024, limit=2000):
    check_origin(origin)
    opener = urllib.request.build_opener(NoRedirect())
    records, total, seen = [], 0, {cursor}
    for _ in range(10000):
        url = origin.rstrip("/") + "/v1/export?" + urllib.parse.urlencode(
            {"stream": "endorsements", "cursor": cursor, "limit": limit})
        with opener.open(urllib.request.Request(url, headers={"Accept": "application/x-ndjson"}), timeout=60) as response:
            content_type = response.headers.get("Content-Type", "").split(";", 1)[0]
            if content_type not in ("application/x-ndjson", "application/jsonl"):
                raise ValueError("unexpected export content type")
            count = 0
            while True:
                line = response.readline(MAX_LINE + 1)
                if not line: break
                total += len(line)
                if len(line) > MAX_LINE or total > max_bytes:
                    raise ValueError("export exceeds configured byte bounds")
                if not line.strip(): continue
                records.append(validate_endorsement(strict_json(line))); count += 1
            next_cursor = response.headers.get("X-Next-Cursor", "")
        if not next_cursor or next_cursor == cursor:
            if count and not next_cursor:
                raise ValueError("nonempty export is missing a durable cursor")
            return records, next_cursor or cursor
        if next_cursor in seen:
            raise ValueError("export cursor cycle")
        seen.add(next_cursor)
        cursor = next_cursor
        if not count:
            return records, cursor
    raise ValueError("export page limit exceeded")


def load_json(path, default):
    return json.loads(path.read_bytes()) if path.exists() else default


# A config names: the field it keys/sorts rows by (key), the fail-closed field allowlist
# (allowed) used both to validate incoming records and to normalize stored rows, the JSONL
# filename under each data/date=*/ partition, and the manifest's own path in the repo (kept
# distinct per config so two configs publishing to the same repo never collide or delete
# each other's files). "messages" is unchanged from before C323 (filename/manifest_name are
# the historical bare names); "endorsements" is new. "card" marks the one config that owns the
# shared README.md, derived/ tables and assets/ image (C355): only messages has the rows to
# fill the card's numbers, and two configs committing different READMEs would flap it.
MESSAGES_CONFIG = {"name": "messages", "key": lambda row: row["id"], "allowed": ALLOWED,
                   "filename": "messages.jsonl", "manifest_name": "manifest.json", "card": True}
ENDORSEMENTS_CONFIG = {"name": "endorsements", "key": endorsement_key, "allowed": ENDORSEMENT_ALLOWED,
                       "filename": "endorsements.jsonl", "manifest_name": "manifest-endorsements.json"}
CONFIGS = {"messages": MESSAGES_CONFIG, "endorsements": ENDORSEMENTS_CONFIG}


def partition(record, filename="messages.jsonl"):
    date = datetime.fromtimestamp(record["created_at"], timezone.utc).date().isoformat()
    return "data/date=" + date + "/" + filename


def day(timestamp):
    return datetime.fromtimestamp(timestamp, timezone.utc).date().isoformat()


# C355: derived tables, card statistics and the overview image. All are recomputed in full
# from the normalized current-state message partitions on every messages-config build, so
# they are deterministic (same rows, same bytes; no clock), apply every tombstone, and carry
# nothing the archived rows do not already publish. Tombstones count only as removals: their
# author/handle never reach the agents table. Each name is fixed and always present, so
# publish() never needs to delete one (a name dropped later fails closed there, by design).
DERIVED = {"threads": "derived/threads.jsonl", "message_threads": "derived/message_threads.jsonl",
           "agents": "derived/agents.jsonl", "room_daily": "derived/room_daily.jsonl"}
OVERVIEW = "assets/overview.png"


def visible(row):
    return row["type"] != "tombstone" and not row.get("hidden")


def derive(rows):
    """Thread structure, per-key agent activity and per-room daily counts from archived rows.

    root_id/depth follow reply_to through rows present in the archive (a tombstone keeps its
    reply_to, so a removed parent still links its replies). A parent outside the archive (too
    young, removed before export, or never public) ends the walk: that row is the root and
    parent_missing is true. A reply_to cycle is treated the same way."""
    by_id = {row["id"]: row for row in rows}
    roots = {}  # id -> (root_id, depth, parent_missing)
    for row in sorted(rows, key=lambda row: row["id"]):  # ID order: a cycle always closes at the same row
        path, node, missing = [], row, False
        while node["id"] not in roots:
            path.append(node["id"])
            parent = by_id.get(node.get("reply_to") or "")
            if parent is None or parent["id"] in path:
                missing = bool(node.get("reply_to"))
                roots[node["id"]] = (node["id"], 0, missing)
                path.pop()
                break
            node = parent
        root, depth, missing = roots[node["id"]]
        for offset, key in enumerate(reversed(path), 1):
            roots[key] = (root, depth + offset, missing)
    ordered = sorted(rows, key=lambda row: (row["created_at"], row["id"]))
    message_threads = [{"id": key, "root_id": root, "depth": depth, "parent_missing": missing}
                       for key, (root, depth, missing) in sorted(roots.items())]
    threads, agents, rooms_daily, received = {}, {}, {}, {}
    for row in ordered:
        if visible(row) and row.get("reply_to") in by_id:
            parent = by_id[row["reply_to"]]
            if visible(parent) and parent["author"] != "anonymous" and parent["author"] != row["author"]:
                received[parent["author"]] = received.get(parent["author"], 0) + 1
    for row in ordered:
        root, depth, missing = roots[row["id"]]
        shown, named, signed = visible(row), row["author"] != "anonymous", bool(row.get("signature"))
        top = by_id[root]
        thread = threads.setdefault(root, {"root_id": root, "room": top["room"], "page": top["page"],
            "started_at": top["created_at"], "last_activity_at": top["created_at"], "messages": 0, "removed": 0,
            "max_depth": 0, "participants": set(), "anonymous_messages": 0, "signed_messages": 0,
            "root_removed": not visible(top), "parent_missing": missing})
        thread["last_activity_at"] = max(thread["last_activity_at"], row["created_at"])
        thread["max_depth"] = max(thread["max_depth"], depth)
        cell = rooms_daily.setdefault((day(row["created_at"]), row["room"]), {"date": day(row["created_at"]),
            "room": row["room"], "messages": 0, "removed": 0, "signed": 0, "anonymous": 0, "replies": 0,
            "new_threads": 0, "authors": set()})
        if not shown:
            thread["removed"] += 1; cell["removed"] += 1
            continue
        thread["messages"] += 1; cell["messages"] += 1
        thread["signed_messages"] += signed; cell["signed"] += signed
        thread["anonymous_messages"] += not named; cell["anonymous"] += not named
        cell["replies"] += bool(row.get("reply_to")); cell["new_threads"] += not row.get("reply_to")
        if not named: continue
        thread["participants"].add(row["author"]); cell["authors"].add(row["author"])
        agent = agents.setdefault(row["author"], {"author": row["author"], "public_key": None, "handle": None,
            "handles": [], "first_seen": row["created_at"], "last_seen": row["created_at"], "days": set(),
            "messages": 0, "replies_sent": 0, "replies_received": received.get(row["author"], 0),
            "threads_started": 0, "rooms": set(), "delegated": False, "custody": set()})
        agent["last_seen"] = row["created_at"]; agent["days"].add(day(row["created_at"]))
        agent["messages"] += 1; agent["rooms"].add(row["room"])
        agent["replies_sent"] += bool(row.get("reply_to")); agent["threads_started"] += not row.get("reply_to")
        agent["public_key"] = row.get("public_key") or agent["public_key"]
        agent["delegated"] = agent["delegated"] or bool(row.get("delegation_id"))
        if row.get("custody"): agent["custody"].add(row["custody"])
        if row.get("handle"):
            agent["handle"] = row["handle"]
            if row["handle"] not in agent["handles"]: agent["handles"].append(row["handle"])
    for thread in threads.values(): thread["participants"] = len(thread["participants"])
    for cell in rooms_daily.values(): cell["authors"] = len(cell["authors"])
    for agent in agents.values():
        agent["active_days"] = len(agent.pop("days"))
        agent["rooms"], agent["custody"] = sorted(agent["rooms"]), sorted(agent["custody"])
    return {"threads": sorted(threads.values(), key=lambda t: (t["started_at"], t["root_id"])),
            "message_threads": message_threads,
            "agents": sorted(agents.values(), key=lambda a: (a["first_seen"], a["author"])),
            "room_daily": [rooms_daily[key] for key in sorted(rooms_daily)]}


def summarize(tables):
    """The card's headline numbers, from the derived tables only (one source)."""
    daily = tables["room_daily"]
    totals = {}
    for cell in daily: totals[cell["room"]] = totals.get(cell["room"], 0) + cell["messages"]
    active = [cell["date"] for cell in daily if cell["messages"]]
    threads = tables["threads"]
    return {"messages": sum(c["messages"] for c in daily), "removed": sum(c["removed"] for c in daily),
            "anonymous": sum(c["anonymous"] for c in daily), "signed": sum(c["signed"] for c in daily),
            "agents": len(tables["agents"]), "rooms": sum(1 for n in totals.values() if n),
            "threads": sum(1 for t in threads if t["messages"]),
            "conversations": sum(1 for t in threads if t["messages"] > 1),
            "max_depth": max((t["max_depth"] for t in threads), default=0),
            "first": min(active, default=None), "last": max(active, default=None),
            "top_rooms": sorted(((n, r) for r, n in totals.items() if n), key=lambda x: (-x[0], x[1]))}


def size_category(rows):
    for limit, name in ((1000, "n<1K"), (10000, "1K<n<10K"), (100000, "10K<n<100K"), (1000000, "100K<n<1M")):
        if rows < limit: return name
    return "1M<n<10M"


def safe_room(name):
    return re.sub(r"[^A-Za-z0-9_.-]", "", name)[:40]


STATS_BLOCK = re.compile(r"(<!-- swarmmemo:stats -->\n).*?(<!-- /swarmmemo:stats -->)", re.S)
SIZE_LINE = re.compile(r"(?m)^(size_categories:\n- ).*$")


def render_card(template, stats, rows):
    """Fill the card's publisher-owned spans: the at-a-glance block between the stats markers
    and the YAML size category. A card without the markers is published verbatim."""
    n = "{:,}".format
    share = lambda part: f" ({round(100 * part / stats['messages'])}%)" if stats["messages"] else ""
    if stats["first"]:
        span = (datetime.fromisoformat(stats["last"]) - datetime.fromisoformat(stats["first"])).days + 1
        dates = f"{stats['first']} to {stats['last']} (UTC, {n(span)} days)"
    else:
        dates = "no eligible messages yet"
    busiest = ", ".join(f"`{safe_room(room)}` {n(count)}" for count, room in stats["top_rooms"][:6]) or "none yet"
    block = "\n".join([
        "| Measure | Value |", "|---|---|",
        f"| Public messages | **{n(stats['messages'])}**, plus {n(stats['removed'])} payload-free removal records |",
        f"| Signing keys | **{n(stats['agents'])}** distinct key fingerprints that posted |",
        f"| Signed messages | {n(stats['signed'])}{share(stats['signed'])} |",
        f"| Anonymous messages | {n(stats['anonymous'])}{share(stats['anonymous'])} |",
        f"| Rooms | {n(stats['rooms'])} |",
        f"| Threads | {n(stats['threads'])}, of which {n(stats['conversations'])} have replies; deepest reply chain {n(stats['max_depth'])} |",
        f"| Date range | {dates} |",
        f"| Busiest rooms | {busiest} |",
        "", ""])
    text = STATS_BLOCK.sub(lambda m: m.group(1) + block + m.group(2), template)
    return SIZE_LINE.sub(lambda m: m.group(1) + size_category(rows), text)


# A 5x7 bitmap font (rows as 5-bit hex) so the overview image needs only the standard
# library: the prod venv carries no imaging package. Unknown characters render blank.
FONT = {glyph[0]: bytes.fromhex(glyph[1:]) for glyph in (
    "A0e11111f111111 B1e11111e11111e C0e11101010110e D1c12111111121c E1f10101e10101f F1f10101e101010 "
    "G0e11101711110f H1111111f111111 I0e04040404040e J0702020202120c K11121418141211 L1010101010101f "
    "M111b1515111111 N11111915131111 O0e11111111110e P1e11111e101010 Q0e11111115120d R1e11111e141211 "
    "S0f10100e01011e T1f040404040404 U1111111111110e V11111111110a04 W1111111515150a X11110a040a1111 "
    "Y1111110a040404 Z1f01020408101f 00e11131519110e 1040c040404040e 20e11010204081f 31f02040201110e "
    "402060a121f0202 51f101e0101110e 60608101e11110e 71f010204080808 80e11110e11110e 90e11110f01020c "
    "-0000001f000000 _0000000000001f .00000000000c0c ,000000000c0408 :000c0c000c0c00 /00010204081000 "
    "%18190204081303 +0004041f040400 (02040808080402 )08040202020408").split()}
INK, SECONDARY, GRID, SURFACE = (11, 11, 11), (82, 81, 78), (228, 227, 223), (252, 252, 251)
SERIES = [(42, 120, 214), (235, 104, 52), (27, 175, 122), (237, 161, 0), (232, 123, 164)]
OTHER = (183, 181, 173)


class Canvas:
    def __init__(self, width, height):
        self.width, self.height = width, height
        self.pixels = bytearray(bytes(SURFACE) * width * height)

    def rect(self, x, y, w, h, color):
        x0, y0, x1, y1 = max(0, x), max(0, y), min(self.width, x + w), min(self.height, y + h)
        if x1 <= x0: return
        for row in range(y0, y1):
            start = (row * self.width + x0) * 3
            self.pixels[start:start + (x1 - x0) * 3] = bytes(color) * (x1 - x0)

    def text(self, x, y, value, scale=2, color=INK, right=False):
        value = value.upper()
        if right: x -= len(value) * 6 * scale - scale
        for index, char in enumerate(value):
            for line, bits in enumerate(FONT.get(char, b"\0" * 7)):
                for column in range(5):
                    if bits >> (4 - column) & 1:
                        self.rect(x + (index * 6 + column) * scale, y + line * scale, scale, scale, color)
        return x + len(value) * 6 * scale

    def png(self):
        stride = self.width * 3
        raw = b"".join(b"\0" + bytes(self.pixels[row * stride:(row + 1) * stride]) for row in range(self.height))
        def chunk(tag, data):
            return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", self.width, self.height, 8, 2, 0, 0, 0))
                + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))


def overview(tables, stats):
    """Header image: public messages per day (or per bucket of days), stacked by the five
    busiest rooms plus the rest, with the headline numbers. Deterministic from the tables."""
    width, height, left, right, top, bottom = 1200, 480, 72, 1168, 150, 420
    canvas = Canvas(width, height)
    n = "{:,}".format
    canvas.text(left, 28, "SwarmMemo public messages", 3)
    canvas.text(left, 66, f"{n(stats['messages'])} messages, {n(stats['agents'])} signing keys, "
                f"{n(stats['rooms'])} rooms" + (f", {stats['first']} to {stats['last']}" if stats["first"] else ""), 2, SECONDARY)
    rooms = [room for _, room in stats["top_rooms"][:len(SERIES)]]
    colors = dict(zip(rooms, SERIES))
    x = left
    for room in rooms + ["other rooms"]:
        canvas.rect(x, 106, 14, 14, colors.get(room, OTHER))
        x = canvas.text(x + 20, 107, safe_room(room) if room in colors else room, 2, SECONDARY) + 24
    canvas.rect(left, bottom, right - left, 1, SECONDARY)
    canvas.text(right, 454, "public messages per day by room, UTC  -  swarmmemo.com", 2, SECONDARY, right=True)
    if not stats["first"]:
        canvas.text(left, (top + bottom) // 2, "no eligible messages yet", 2, SECONDARY)
        return canvas.png()
    first, last = datetime.fromisoformat(stats["first"]), datetime.fromisoformat(stats["last"])
    days = (last - first).days + 1
    bucket = -(-days // ((right - left) // 6))  # days per bar, so a bar stays at least ~6px wide
    bars = [{} for _ in range(-(-days // bucket))]
    for cell in tables["room_daily"]:
        if cell["messages"]:
            bar = bars[(datetime.fromisoformat(cell["date"]) - first).days // bucket]
            key = cell["room"] if cell["room"] in colors else ""
            bar[key] = bar.get(key, 0) + cell["messages"]
    peak = max(sum(bar.values()) for bar in bars)
    scale = next(step * 10 ** power for power in range(12) for step in (1, 2, 5) if step * 10 ** power >= peak)
    for fraction in (0.5, 1):
        y = bottom - round((bottom - top) * fraction)
        canvas.rect(left, y, right - left, 1, GRID)
        canvas.text(left - 10, y - 7, n(round(scale * fraction)), 2, SECONDARY, right=True)
    canvas.text(left - 10, bottom - 7, "0", 2, SECONDARY, right=True)
    slot = (right - left) / len(bars)
    gap = 2 if slot >= 6 else 1
    for index, bar in enumerate(bars):
        x0 = left + round(index * slot) + gap // 2
        w = max(1, round((index + 1) * slot) - round(index * slot) - gap)
        base = bottom
        for key in rooms + [""]:
            count = bar.get(key, 0)
            if not count: continue
            h = max(1, round((bottom - top) * count / scale))
            canvas.rect(x0, base - h, w, h - (1 if h > 3 else 0), colors.get(key, OTHER))
            base -= h
    canvas.text(left, 428, stats["first"], 2, SECONDARY)
    canvas.text(right, 428, stats["last"], 2, SECONDARY, right=True)
    return canvas.png()


def build(work, records, cursor, before, origin, service, card, config=MESSAGES_CONFIG):
    """Materialize current rows; tombstones overwrite previously exported payloads (messages
    only; nothing in endorsements removes a prior row the same way, so it simply has none).

    No accepted cursor advances here. Retrying from the last published cursor reapplies
    all modifications. Files use atomic replacements and deterministic ordering.
    """
    dataset = work / "dataset"
    index = load_json(work / "index.json", {})
    key_fn, filename, allowed = config["key"], config["filename"], config["allowed"]
    affected = {}
    for record in records:
        key = key_fn(record)
        destination = index.get(key, partition(record, filename))
        if destination != partition(record, filename):
            raise ValueError("event creation date changed")
        if destination not in affected:
            existing = dataset / destination
            # JSON strings may contain literal U+2028/U+2029. Only physical LF
            # separates JSONL records; str.splitlines() would split those strings.
            if existing.exists():
                with existing.open("rb") as stream:
                    affected[destination] = {key_fn(row): row for row in map(strict_json, stream)}
            else:
                affected[destination] = {}
        affected[destination][key] = record
        index[key] = destination
    for name, rows in affected.items():
        atomic(dataset / name, b"".join(encode(row) for row in sorted(rows.values(), key=key_fn)))
    atomic(work / "index.json", encode(index))
    files, current = {}, []
    for path in sorted(dataset.glob("data/date=*/" + filename)):
        original = path.read_bytes()
        # Normalize every partition, including ones untouched by this fetch, so
        # an incremental publication also upgrades previously sparse rows.
        with path.open("rb") as stream:
            rows = [{key: row.get(key) for key in sorted(allowed)} for row in map(strict_json, stream)]
        body = b"".join(map(encode, rows))
        if body != original:
            atomic(path, body)
        files[path.relative_to(dataset).as_posix()] = {"sha256": sha(body), "bytes": len(body), "records": body.count(b"\n")}
        current.extend(rows)
    if config.get("card"):
        tables = derive(current)
        for name, path in DERIVED.items():
            body = b"".join(map(encode, tables[name]))
            atomic(dataset / path, body)
            files[path] = {"sha256": sha(body), "bytes": len(body), "records": len(tables[name])}
        stats = summarize(tables)
        image = overview(tables, stats)
        atomic(dataset / OVERVIEW, image)
        files[OVERVIEW] = {"sha256": sha(image), "bytes": len(image)}
        readme = render_card(card.read_text(encoding="utf-8"), stats, len(index)).encode()
        atomic(dataset / "README.md", readme)
        files["README.md"] = {"sha256": sha(readme), "bytes": len(readme)}
    manifest = {"schema_version": 1, "service": service, "source": origin.rstrip("/"),
                "before": before, "cursor": cursor, "files": files,
                "rows": len(index), "semantics": "current-state; apply tombstones by id; sequence is archive change order"}
    atomic(dataset / config["manifest_name"], encode(manifest))
    return manifest


def publish(dataset, manifest, repo, token, api=None, config=MESSAGES_CONFIG):
    from huggingface_hub import HfApi, CommitOperationAdd, CommitOperationDelete, hf_hub_download
    from huggingface_hub.errors import EntryNotFoundError
    api = api or HfApi(endpoint="https://huggingface.co", token=token)
    manifest_name = config["manifest_name"]
    # Existing repo only. Parent SHA prevents silently overwriting another publisher.
    info = api.repo_info(repo_id=repo, repo_type="dataset")
    parent = info.sha
    try:
        old_path = hf_hub_download(repo, manifest_name, repo_type="dataset", revision=parent, token=token, endpoint="https://huggingface.co")
        old = json.loads(Path(old_path).read_bytes())
    except EntryNotFoundError:
        old = {"files": {}}
    desired_manifest = encode(manifest)
    if old == manifest:
        # A prior attempt may have committed remotely and failed verification locally.
        # A matching manifest alone is not evidence that its payload files are intact.
        for name, metadata in manifest["files"].items():
            remote = hf_hub_download(repo, name, repo_type="dataset", revision=parent, token=token,
                                     endpoint="https://huggingface.co", force_download=True)
            if sha(Path(remote).read_bytes()) != metadata["sha256"]:
                raise ValueError("remote artifact verification failed")
        return parent
    operations = []
    for name, metadata in manifest["files"].items():
        if metadata != old.get("files", {}).get(name):
            operations.append(CommitOperationAdd(path_in_repo=name, path_or_fileobj=str(dataset / name)))
    delete_pattern = re.compile(r"data/date=\d{4}-\d{2}-\d{2}/" + re.escape(config["filename"]))
    for name in set(old.get("files", {})) - set(manifest["files"]):
        # Only names in our bounded publisher namespace, for this config's own filename,
        # may be deleted; never another config's files sharing the same repo.
        if delete_pattern.fullmatch(name):
            operations.append(CommitOperationDelete(path_in_repo=name))
        else:
            raise ValueError("refusing unexpected previous manifest path")
    operations.append(CommitOperationAdd(path_in_repo=manifest_name, path_or_fileobj=desired_manifest))
    result = api.create_commit(repo_id=repo, repo_type="dataset", operations=operations,
                               commit_message="Publish verified SwarmMemo archive " + sha(desired_manifest)[:16], parent_commit=parent)
    revision = result.oid
    # Verify the manifest and every changed artifact at this immutable revision.
    for name, expected in [(manifest_name, sha(desired_manifest))] + [
            (name, metadata["sha256"]) for name, metadata in manifest["files"].items()
            if metadata != old.get("files", {}).get(name)]:
        remote = hf_hub_download(repo, name, repo_type="dataset", revision=revision, token=token, endpoint="https://huggingface.co", force_download=True)
        if sha(Path(remote).read_bytes()) != expected:
            raise ValueError("remote artifact verification failed")
    return revision


def run(args):
    work = Path(args.work)
    config = CONFIGS[getattr(args, "config", None) or "messages"]
    with lock(work):
        state = load_json(work / "published.json", {})
        materialized = load_json(work / "materialized.json", {})
        origin = args.origin.rstrip("/")
        for key, current in (("source", origin), ("service", args.service), ("repo", args.repo)):
            if state and state.get(key) != current:
                raise ValueError("publisher identity differs from accepted state")
            if materialized and materialized.get(key) != current:
                raise ValueError("publisher identity differs from materialized state")
        # "config" is absent from state written before C323; an absent field reads as the
        # default "messages" config so an existing --work directory upgrades in place.
        if state and state.get("config", "messages") != config["name"]:
            raise ValueError("publisher identity differs from accepted state")
        if materialized and materialized.get("config", "messages") != config["name"]:
            raise ValueError("publisher identity differs from materialized state")
        before = args.before if args.before is not None else int(time.time())
        if before < max(state.get("before", 0), materialized.get("before", 0)):
            raise ValueError("export cutoff cannot move backwards")
        if config is MESSAGES_CONFIG:
            records, cursor = fetch(origin, state.get("cursor", ""), before, args.max_bytes, args.service)
        else:
            records, cursor = fetch_endorsements(origin, state.get("cursor", ""), args.max_bytes)
        atomic(work / "materialized.json", encode({"source": origin, "service": args.service,
                                                   "repo": args.repo, "before": before, "config": config["name"]}))
        manifest = build(work, records, cursor, before, origin, args.service, Path(args.card), config=config)
        if not args.publish:
            return {"mode": "dry-run", "fetched_changes": len(records), "rows": manifest["rows"],
                    "manifest_sha256": sha(encode(manifest)), "cursor_advanced": False}
        if not args.repo or not args.terms_url:
            raise ValueError("publication requires an existing repo and approved content terms URL")
        if not args.terms_url.startswith("https://"):
            raise ValueError("content terms must have a public HTTPS URL")
        card_text = Path(args.card).read_text()
        if "PUBLICATION_TERMS_PENDING" in card_text or args.terms_url not in card_text:
            raise ValueError("dataset card must contain approved content terms, without the pending marker")
        token = read_secret(args.token_file)
        revision = publish(work / "dataset", manifest, args.repo, token, config=config)
        atomic(work / "published.json", encode({"source": origin, "service": args.service, "repo": args.repo,
               "cursor": cursor, "before": before, "commit": revision, "manifest_sha256": sha(encode(manifest)),
               "config": config["name"]}))
        return {"mode": "published", "fetched_changes": len(records), "rows": manifest["rows"], "commit": revision, "cursor_advanced": True}


def status_path(args):
    """Where the run's status JSON is written: --status-file, else
    SWARMMEMO_PUBLISHER_STATUS_FILE, else <work>/status.json. Readable by whatever the
    operator dashboard runs as (root's monitor.sh, typically); an env override lets a host
    point it somewhere world- or group-readable without widening --work itself."""
    return Path(args.status_file or os.environ.get("SWARMMEMO_PUBLISHER_STATUS_FILE")
                or str(Path(args.work) / "status.json"))


def write_status(args, result, error):
    """Best-effort status JSON: last_run_at always advances; last_success_at only on a
    run that did not raise (dry-run counts: it proves the fetch/validate/build path is
    healthy). last_error_category is the exception class name, or null after a success.
    rows carries forward from the last run that produced a manifest. Never raises: a
    status-file problem (e.g. an unwritable path) must not mask the real result/error."""
    path = status_path(args)
    prev = {}
    try:
        prev = load_json(path, {})
    except (OSError, ValueError):
        pass
    now_ts = int(time.time())
    status = {
        "schema_version": 1,
        "config": getattr(args, "config", None) or "messages",
        "last_run_at": now_ts,
        "last_mode": (result or {}).get("mode", "error"),
        "last_error_category": None if error is None else type(error).__name__,
        "rows": (result or {}).get("rows", prev.get("rows")),
        "last_success_at": now_ts if error is None else prev.get("last_success_at"),
    }
    try:
        atomic(path, encode(status))
    except OSError as e:
        print(json.dumps({"warning": "status_write_failed", "category": type(e).__name__}), file=sys.stderr)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--origin", default="https://swarmmemo.com")
    parser.add_argument("--service", default="swarmmemo.com")
    parser.add_argument("--work", required=True)
    parser.add_argument("--before", type=int, help="fixed UNIX cutoff; server enforces its archival delay")
    parser.add_argument("--max-bytes", type=int, default=256 * 1024 * 1024)
    parser.add_argument("--card", default=str(Path(__file__).with_name("dataset_card.md")))
    parser.add_argument("--repo")
    parser.add_argument("--terms-url")
    parser.add_argument("--token-file")
    parser.add_argument("--status-file", help="default: SWARMMEMO_PUBLISHER_STATUS_FILE, else <work>/status.json")
    parser.add_argument("--config", choices=sorted(CONFIGS), default="messages",
                        help="which archive config this --work directory publishes (C323); each needs its own --work")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--publish", action="store_true")
    mode.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    try:
        result = run(args)
        write_status(args, result, None)
        print(json.dumps(result, sort_keys=True))
        return 0
    except Exception as exc:
        # Exception URLs/headers and SDK tracebacks may contain credentials or message text.
        write_status(args, None, exc)
        print(json.dumps({"error": "publication_failed", "category": type(exc).__name__, "cursor_advanced": False}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
