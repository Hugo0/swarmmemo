#!/usr/bin/env python3
"""Guarded local reference projection. Dry-run by default; never board/HF writes."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import sqlite3
import stat
import sys
import tempfile
import time
from urllib.parse import urlsplit, unquote

import sync_sources as sync

MAX_BYTES = 8 * 1024 * 1024
MAX_POLICY = 1024 * 1024
MAX_INT = 9007199254740991
MAX_DEPTH, MAX_NODES = 32, 150000
CLOCK_TOLERANCE = 60
WHITE_SPACE = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
IPV4_DENY = tuple(map(ipaddress.ip_network, ("0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4")))
IPV6_DENY = tuple(map(ipaddress.ip_network, ("::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8")))
IPV6_EXCEPTIONS = tuple(map(ipaddress.ip_network, ("2001:1::1/128", "2001:1::2/128", "2001:3::/32", "2001:4:112::/48", "2001:20::/28", "2001:30::/28")))
ATTRIBUTION = {"jsonfeed-1.1": "source_declared", sync.CUTTLE_ADAPTER: "site_declared_human_agent_co_creation"}
TOP = frozenset("version state registry_sha256 suppression_sha256 generated_at valid_until sources references".split())
SOURCE = frozenset("id name feed_url adapter last_attempted_at last_successful_at status attribution_basis".split())
REFERENCE = frozenset("id source_id external_id url title excerpt excerpt_available excerpt_truncated authors source_published_at source_publication_timezone_known first_observed_at last_observed_at content_hash native_identity claimable_job hugging_face_eligible untrusted_content".split())
HEX = re.compile(r"[0-9a-f]{64}\Z")
UTC_DATE = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\Z", re.ASCII)
SOURCE_DATE = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})\Z", re.ASCII)
LEGACY_DATE = re.compile(r"\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\Z", re.ASCII)
ERRORS = frozenset("invalid_json invalid_integer invalid_projection invalid_policy invalid_suppression unsafe_path file_unavailable file_too_large publication_busy publication_guard_required publication_blocked publication_stale publication_changed source_unavailable source_binding_mismatch source_failed capacity_exceeded invalid_url invalid_date invalid_excerpt durability_uncertain publication_failed invalid_timeout publication_deadline publication_interrupted retention_uncertain worker_requires_single_thread worker_requires_default_sigchld".split())


class PublicationError(ValueError):
    def __init__(self, code):
        self.code = code if code in ERRORS else "publication_failed"
        super().__init__(self.code)


def fail(code): raise PublicationError(code)


def integer(value):
    if type(value) is not int or not 0 <= value <= MAX_INT: fail("invalid_integer")
    return value


def canonical(value):
    nodes = 0
    def check(item, depth=0):
        nonlocal nodes
        nodes += 1
        if depth > MAX_DEPTH or nodes > MAX_NODES: fail("invalid_json")
        if isinstance(item, dict):
            for k, v in item.items():
                if not isinstance(k, str): fail("invalid_json")
                k.encode("utf-8"); check(v, depth + 1)
        elif isinstance(item, list):
            for v in item: check(v, depth + 1)
        elif isinstance(item, str): item.encode("utf-8")
        elif type(item) is int: integer(item)
        elif item is None or type(item) is bool: pass
        else: fail("invalid_json")
    try:
        check(value)
        return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode("utf-8")
    except (UnicodeError, TypeError, ValueError, RecursionError) as exc:
        if isinstance(exc, PublicationError): raise
        fail("invalid_json")


def strict_json(raw, *, exact=False):
    def pairs(values):
        out = {}
        for k, v in values:
            if k in out: fail("invalid_json")
            out[k] = v
        return out
    def parse_int(v):
        if v == "-0": fail("invalid_integer")
        return integer(int(v))
    def bad(_): fail("invalid_integer")
    try:
        obj = json.loads(raw.decode("utf-8"), object_pairs_hook=pairs, parse_int=parse_int,
                         parse_float=bad, parse_constant=bad)
        encoded = canonical(obj)
        if exact and encoded != raw: fail("invalid_json")
        return obj
    except (UnicodeError, ValueError, RecursionError) as exc:
        if isinstance(exc, PublicationError): raise
        fail("invalid_json")


def sha(raw): return hashlib.sha256(raw).hexdigest()


def fields(value, expected, code="invalid_projection"):
    if not isinstance(value, dict) or set(value) != expected: fail(code)


def text(value, limit, *, empty=True):
    if not isinstance(value, str): fail("invalid_projection")
    try: size = len(value.encode("utf-8"))
    except UnicodeError: fail("invalid_projection")
    if size > limit or "\x00" in value or (not empty and not value): fail("invalid_projection")
    return value


def public_address(address):
    """Pinned literal-IP policy; never depends on Python's evolving is_global."""
    if isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped is not None:
        return public_address(address.ipv4_mapped)
    if isinstance(address, ipaddress.IPv4Address):
        if str(address) in ("192.0.0.9", "192.0.0.10"): return True
        return not any(address in prefix for prefix in IPV4_DENY)
    if any(address in prefix for prefix in IPV6_EXCEPTIONS): return True
    return not any(address in prefix for prefix in IPV6_DENY)


def public_url(value, *, empty=False):
    return _url(value, empty=empty, public=True)


def _url(value, *, empty=False, public):
    text(value, 4096, empty=empty)
    if value == "" and empty: return value
    try:
        parsed = urlsplit(value)
        if (parsed.scheme != "https" or not parsed.hostname or (public and "?" in value) or "#" in value
                or "@" in parsed.netloc or parsed.port not in (None, 443)
                or "\\" in value or any(ord(c) <= 32 or ord(c) == 127 for c in value)):
            fail("invalid_url")
        host = parsed.hostname.encode("idna").decode("ascii").lower()
        if not parsed.hostname.isascii(): fail("invalid_url")
        if host == "localhost" or host.endswith((".localhost", ".local", ".internal")) or "%" in host:
            fail("invalid_url")
        try: address = ipaddress.ip_address(host)
        except ValueError: address = None
        if address is not None and not public_address(address): fail("invalid_url")
        if address is None and (not re.fullmatch(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?", host)
                or len(host) > 253 or ".." in host
                or re.fullmatch(r"(?:[0-9]+|0x[0-9a-f]+)", host.split(".")[-1])): fail("invalid_url")
        if re.search(r"%(?![0-9A-Fa-f]{2})", parsed.path): fail("invalid_url")
        if not public: return value
        decoded = unquote(parsed.path, errors="strict")
        if (any(part in (".", "..") for part in decoded.split("/")) or "\\" in decoded
                or any(ord(c) < 32 or ord(c) == 127 for c in decoded)):
            fail("invalid_url")
        route = decoded.lower()
        if route.startswith("/v1/command") or any(route == p or route.startswith(p + "/") for p in ("/w", "/w64", "/c64")):
            fail("invalid_url")
        return value
    except (ValueError, UnicodeError): fail("invalid_url")


def permission_time(value):
    if not isinstance(value, str) or not UTC_DATE.fullmatch(value): fail("invalid_policy")
    try: return int(datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp())
    except ValueError: fail("invalid_policy")


def publication_time(value, adapter):
    text(value, 40)
    if not value:
        if adapter == sync.CUTTLE_ADAPTER: fail("invalid_date")
        return False
    if adapter == sync.CUTTLE_ADAPTER and LEGACY_DATE.fullmatch(value):
        try: datetime.strptime(value, "%Y-%m-%d %H:%M:%S")
        except ValueError: fail("invalid_date")
        return False
    if not SOURCE_DATE.fullmatch(value): fail("invalid_date")
    if value[-1:] != "Z" and (int(value[-5:-3]) > 23 or int(value[-2:]) > 59): fail("invalid_date")
    try: datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError: fail("invalid_date")
    return True


def reference_id(source_id, external_id):
    if not isinstance(source_id, str) or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,63}", source_id): fail("invalid_projection")
    text(external_id, 512, empty=False)
    return sha(canonical(["swarmmemo.reference.v1", source_id, external_id]))


def checked_path(path):
    path = Path(os.path.abspath(path))
    try:
        sync.trusted_publication_ancestors(path)
    except (OSError, sync.SyncError): fail("unsafe_path")
    return path


def read_file(path, maximum):
    path = checked_path(path)
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
        try:
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid not in (os.geteuid(), 0)
                    or info.st_mode & 0o022 or info.st_nlink != 1): fail("unsafe_path")
            if info.st_size > maximum: fail("file_too_large")
            chunks, size = [], 0
            while True:
                b = os.read(fd, min(65536, maximum + 1 - size))
                if not b: break
                chunks.append(b); size += len(b)
                if size > maximum: fail("file_too_large")
            return b"".join(chunks)
        finally: os.close(fd)
    except OSError: fail("file_unavailable")


def registry(raw):
    obj = strict_json(raw)
    if not isinstance(obj, dict) or type(obj.get("version")) is not int or obj["version"] != 1: fail("invalid_policy")
    try: normalized = sync.registry_from_bytes(raw)
    except (sync.SyncError, ValueError, TypeError, KeyError, AttributeError, UnicodeError): fail("invalid_policy")
    for source in normalized["sources"]:
        # Disabled sources may deliberately have no feed selected yet, as in
        # the distributed registry. These absent values never authorize output.
        if source.get("feed_url") is not None: _url(source["feed_url"], public=False)
        if source.get("terms_url"): _url(source["terms_url"], public=False)
        source.setdefault("name", "")
        for grant in source.get("permissions", {}).values():
            if grant["approved"]:
                _url(grant["evidence_url"], public=False)
                permission_time(grant["reviewed_at"]); permission_time(grant["expires_at"])
    return normalized


def suppression(raw):
    obj = strict_json(raw, exact=True)
    fields(obj, {"version", "ids"}, "invalid_suppression")
    ids = obj["ids"]
    if type(obj["version"]) is not int or obj["version"] != 1 or not isinstance(ids, list) or len(ids) > 10000:
        fail("invalid_suppression")
    if any(not isinstance(v, str) or not HEX.fullmatch(v) for v in ids) or ids != sorted(set(ids)):
        fail("invalid_suppression")
    return set(ids)


def policy_files(registry_path, suppression_path):
    r = read_file(registry_path, MAX_POLICY); p = read_file(suppression_path, MAX_POLICY)
    return r, p, registry(r), suppression(p)


def authorized(source, now):
    return sync.collectable(source, now) and sync.permitted(source, "public_archive", now)


def validate_snapshot(raw, registry_raw, suppression_raw, now):
    try: return _validate_snapshot(raw, registry_raw, suppression_raw, now)
    except PublicationError: raise
    except (ValueError, TypeError, KeyError, OverflowError, AttributeError): fail("invalid_projection")


def _validate_snapshot(raw, registry_raw, suppression_raw, now):
    integer(now)
    if len(raw) > MAX_BYTES: fail("capacity_exceeded")
    obj = strict_json(raw, exact=True)
    if obj == {"state": "blocked", "version": 1} and type(obj.get("version")) is int: fail("publication_blocked")
    fields(obj, TOP)
    if type(obj["version"]) is not int or obj["version"] != 1 or obj["state"] != "ready": fail("invalid_projection")
    if obj["registry_sha256"] != sha(registry_raw) or obj["suppression_sha256"] != sha(suppression_raw): fail("publication_changed")
    policy, suppressed = registry(registry_raw), suppression(suppression_raw)
    generation, until = integer(obj["generated_at"]), integer(obj["valid_until"])
    if generation > now + CLOCK_TOLERANCE or not generation < until <= generation + 900 or now >= until:
        fail("publication_stale")
    sources, refs = obj["sources"], obj["references"]
    if not isinstance(sources, list) or len(sources) > 50 or not isinstance(refs, list) or len(refs) > 1000:
        fail("capacity_exceeded")
    current = {v["id"]: v for v in policy["sources"]}
    by_id, successful, previous = {}, {}, ""
    for item in sources:
        fields(item, SOURCE)
        source_id = text(item["id"], 64, empty=False)
        if source_id <= previous or source_id not in current: fail("invalid_projection")
        previous = source_id; source = current[source_id]
        if not authorized(source, now): fail("source_unavailable")
        if (item["name"] != source["name"] or item["adapter"] != source["adapter"]
                or item["feed_url"] != source.get("feed_url") or item["attribution_basis"] != ATTRIBUTION[item["adapter"]]):
            fail("source_binding_mismatch")
        text(item["name"], 256); public_url(item["feed_url"])
        success, attempt = integer(item["last_successful_at"]), integer(item["last_attempted_at"])
        if item["status"] not in ("ok", "not_modified") or not 0 < success <= attempt <= generation + CLOCK_TOLERANCE:
            fail("source_unavailable")
        expiry = min(permission_time(source["permissions"][right]["expires_at"]) for right in ("automated_collection", "public_archive"))
        if until > min(success + 86400, expiry): fail("publication_stale")
        by_id[source_id] = source
        successful[source_id] = success
    seen, previous = set(), None
    for ref in refs:
        fields(ref, REFERENCE)
        source = by_id.get(ref["source_id"])
        if source is None: fail("invalid_projection")
        rid = reference_id(ref["source_id"], ref["external_id"])
        if ref["id"] != rid or rid in seen or rid in suppressed: fail("invalid_projection")
        seen.add(rid); public_url(ref["url"]); text(ref["title"], 512)
        if not ref["title"].strip(WHITE_SPACE): fail("invalid_projection")
        first, last = integer(ref["first_observed_at"]), integer(ref["last_observed_at"])
        if not 0 < first <= last <= successful[ref["source_id"]]: fail("invalid_projection")
        order = (-first, rid)
        if previous is not None and order <= previous: fail("invalid_projection")
        previous = order
        if not isinstance(ref["content_hash"], str) or not HEX.fullmatch(ref["content_hash"]): fail("invalid_projection")
        if any(ref[k] is not False for k in ("native_identity", "claimable_job", "hugging_face_eligible")) or ref["untrusted_content"] is not True:
            fail("invalid_projection")
        known = publication_time(ref["source_published_at"], source["adapter"])
        if ref["source_publication_timezone_known"] is not known: fail("invalid_date")
        for flag in ("excerpt_available", "excerpt_truncated"):
            if type(ref[flag]) is not bool: fail("invalid_projection")
        text(ref["excerpt"], 2048)
        if len(ref["excerpt"]) > 512: fail("invalid_excerpt")
        if not ref["excerpt_available"]:
            if ref["excerpt"] or ref["excerpt_truncated"]: fail("invalid_excerpt")
        else:
            if not sync.permitted(source, "full_text_storage", now): fail("source_unavailable")
            if until > permission_time(source["permissions"]["full_text_storage"]["expires_at"]): fail("publication_stale")
        if not isinstance(ref["authors"], list) or len(ref["authors"]) > 20: fail("invalid_projection")
        for author in ref["authors"]:
            fields(author, {"name", "url"}); text(author["name"], 512); public_url(author["url"], empty=True)
        if source["adapter"] == sync.CUTTLE_ADAPTER:
            if not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,255}", ref["external_id"]) or ref["url"] != sync.CUTTLE_ORIGIN + "/" + ref["external_id"] + "/":
                fail("invalid_url")
            expected = [{"name": "@0xCuttlefish", "url": sync.CUTTLE_ORIGIN + "/about/"}, {"name": "Trurl (Hermes Agent; site-declared co-creator)", "url": sync.CUTTLE_ORIGIN + "/llms.txt"}]
            if ref["authors"] != expected: fail("invalid_projection")
    return obj


def _excerpt(row, source, now):
    if not sync.permitted(source, "full_text_storage", now) or row["body"] is None:
        return "", False, False
    body = row["body_text"]
    if source["adapter"] == sync.CUTTLE_ADAPTER:
        prefix = "External co-created worklog index excerpt; not a native post or open job.\nSource series: "
        if not body.startswith(prefix): fail("invalid_excerpt")
        series, separator, excerpt = body[len(prefix):].partition("\n\n")
        if not separator or series not in sync.CUTTLE_SERIES or row["body_format"] != "text" or row["body"] != body:
            fail("invalid_excerpt")
        body = excerpt
    text(body, source["limits"]["item_bytes"])
    return body[:512], True, len(body) > 512


def _project(catalog_path, registry_raw, suppression_raw, now):
    """No network. Caller owns publication lock; catalog() supplies inner lock."""
    policy, suppressed = registry(registry_raw), suppression(suppression_raw)
    result = {"version": 1, "state": "ready", "registry_sha256": sha(registry_raw),
              "suppression_sha256": sha(suppression_raw), "generated_at": now, "valid_until": now + 900,
              "sources": [], "references": []}
    encoded_total = 1024
    with sync.catalog(catalog_path, policy["max_catalog_bytes"]) as db:
        db.execute("BEGIN")
        try:
            for source in sorted(policy["sources"], key=lambda s: s["id"]):
                if not authorized(source, now): continue
                public_url(source["feed_url"])
                record = db.execute("SELECT * FROM sources WHERE id=?", (source["id"],)).fetchone()
                if record is None: fail("source_unavailable")
                if record["feed_url"] != source["feed_url"] or record["adapter"] != source["adapter"]: fail("source_binding_mismatch")
                if record["status"] not in ("ok", "not_modified") or not record["last_success"]: fail("source_unavailable")
                result["sources"].append({"id": source["id"], "name": source["name"], "feed_url": source["feed_url"],
                    "adapter": source["adapter"], "last_attempted_at": record["last_checked"], "last_successful_at": record["last_success"],
                    "status": record["status"], "attribution_basis": ATTRIBUTION[source["adapter"]]})
                encoded_total += len(canonical(result["sources"][-1])) + 1
                result["valid_until"] = min(result["valid_until"], record["last_success"] + 86400,
                    *(permission_time(source["permissions"][right]["expires_at"]) for right in ("automated_collection", "public_archive")))
                rows = db.execute("SELECT * FROM items WHERE source_id=? AND deleted=0 LIMIT 1001", (source["id"],))
                for index, row in enumerate(rows):
                    if index == 1000: fail("capacity_exceeded")
                    rid = reference_id(source["id"], row["external_id"])
                    if rid in suppressed: continue
                    excerpt, available, truncated = _excerpt(row, source, now)
                    if available: result["valid_until"] = min(result["valid_until"], permission_time(source["permissions"]["full_text_storage"]["expires_at"]))
                    result["references"].append({"id": rid, "source_id": source["id"], "external_id": row["external_id"],
                        "url": row["external_url"], "title": row["title"], "excerpt": excerpt, "excerpt_available": available,
                        "excerpt_truncated": truncated, "authors": strict_json(row["authors_json"].encode()),
                        "source_published_at": row["published_at"], "source_publication_timezone_known": publication_time(row["published_at"], source["adapter"]),
                        "first_observed_at": row["first_observed"], "last_observed_at": row["last_observed"], "content_hash": row["content_hash"],
                        "native_identity": False, "claimable_job": False, "hugging_face_eligible": False, "untrusted_content": True})
                    if len(result["references"]) > 1000: fail("capacity_exceeded")
                    encoded_total += len(canonical(result["references"][-1])) + 1
                    if encoded_total > MAX_BYTES: fail("capacity_exceeded")
        finally: db.rollback()
    result["references"].sort(key=lambda r: (-r["first_observed_at"], r["id"]))
    raw = canonical(result)
    validate_snapshot(raw, registry_raw, suppression_raw, now)
    return raw


def _atomic(path, raw, mode=0o640):
    path = checked_path(path)
    temporary = None
    try:
        # Refuse replacing links/special files rather than following them.
        try:
            info = path.lstat()
            if (not stat.S_ISREG(info.st_mode) or info.st_uid not in (os.geteuid(), 0)
                    or info.st_nlink != 1 or info.st_mode & 0o022): fail("unsafe_path")
        except FileNotFoundError: pass
        fd, temporary = tempfile.mkstemp(prefix=".reference-", suffix=".tmp", dir=path.parent)
        try:
            os.fchmod(fd, mode)
            view = memoryview(raw)
            while view: view = view[os.write(fd, view):]
            os.fsync(fd)
        finally: os.close(fd)
        os.replace(temporary, path); temporary = None
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
        try: os.fsync(directory)
        finally: os.close(directory)
    except OSError: fail("durability_uncertain")
    finally:
        if temporary is not None:
            try: os.unlink(temporary)
            except OSError: pass


def _now(): return int(time.time())


def _preflight(catalog_path, registry_path, suppression_path, snapshot_path, collect):
    r, p, policy, _ = policy_files(registry_path, suppression_path)
    if not collect:
        validate_snapshot(read_file(snapshot_path, MAX_BYTES), r, p, _now())
    marker = Path(str(catalog_path) + ".publication-bound.json")
    try: existing = read_file(marker, 64)
    except PublicationError as exc:
        if exc.code != "file_unavailable" or marker.is_symlink(): raise
        try: marker.lstat()
        except FileNotFoundError: existing = None
        else: raise
    if existing is not None and existing != b'{"version":1}': fail("publication_guard_required")
    _atomic(marker, b'{"version":1}', 0o600)
    _atomic(snapshot_path, canonical({"version": 1, "state": "blocked"}))
    return [sha(r), sha(p)]


def _bound_policy(registry_path, suppression_path, expected):
    r, p, policy, suppressed = policy_files(registry_path, suppression_path)
    if [sha(r), sha(p)] != expected: fail("publication_changed")
    return r, p, policy, suppressed


def _collect(catalog_path, registry_path, suppression_path, expected):
    _, _, policy, _ = _bound_policy(registry_path, suppression_path, expected)
    for source in policy["sources"]:
        if sync.collectable(source, _now()): public_url(source["feed_url"])
    results = sync._synchronize(policy, catalog_path, fetcher=sync.fetch_feed)
    if any(row.get("status") not in ("ok", "not_modified", "permission_blocked") for row in results): fail("source_failed")
    return True


def _retention(catalog_path, registry_path):
    # Current policy, not the preflight policy: revocation wins during a run.
    policy = registry(read_file(registry_path, MAX_POLICY))
    sync._retention_only(policy, catalog_path, None)
    return True


def _finish(catalog_path, registry_path, suppression_path, snapshot_path, expected):
    r, p, _, _ = _bound_policy(registry_path, suppression_path, expected)
    now = _now()
    raw = _project(catalog_path, r, p, now)
    _bound_policy(registry_path, suppression_path, expected)
    obj = validate_snapshot(raw, r, p, _now())
    _atomic(snapshot_path, raw)
    return {"state": "ready", "snapshot_sha256": sha(raw), "generated_at": now,
            "valid_until": obj["valid_until"], "sources": len(obj["sources"]), "references": len(obj["references"])}


def _phase(task, deadline):
    """No child can create another supervisor. IPC contains only fixed metadata."""
    def guarded():
        try: return {"value": task()}
        except PublicationError as exc: return {"error": exc.code}
        except sync.SyncError as exc:
            return {"error": str(exc) if str(exc) in ERRORS else "publication_failed"}
        except (OSError, ValueError, TypeError, sqlite3.Error): return {"error": "publication_failed"}
    try: result = sync._supervised_call(guarded, deadline - sync.REAP_SECONDS, deadline)
    except sync.SyncError as exc:
        fail({"sync_deadline": "publication_deadline", "sync_interrupted": "publication_interrupted"}.get(str(exc), "publication_failed"))
    if not isinstance(result, dict): fail("publication_failed")
    if "error" in result: fail(result["error"])
    return result.get("value")


def _publish_locked(catalog_path, registry_path, suppression_path, snapshot_path, *, collect, total_timeout):
    deadline = time.monotonic() + total_timeout
    args = (catalog_path, registry_path, suppression_path, snapshot_path)
    expected = _phase(lambda: _preflight(*args, collect), min(deadline - 5, time.monotonic() + 3))
    try:
        if collect:
            try:
                _phase(lambda: _collect(catalog_path, registry_path, suppression_path, expected), deadline - 4)
            finally:
                try: _phase(lambda: _retention(catalog_path, registry_path), deadline - 2)
                except BaseException: fail("retention_uncertain")
        return _phase(lambda: _finish(*args, expected), deadline - 0.5)
    except BaseException:
        # A ready rename can precede a failed directory fsync or lost child
        # result. Bounded replacement is best effort, never a durability claim.
        try: _phase(lambda: _atomic(snapshot_path, canonical({"version": 1, "state": "blocked"})), deadline)
        except BaseException: pass
        raise


def publish(catalog_path, registry_path, suppression_path, snapshot_path, *, collect=False, total_timeout=60):
    """Explicit local write. Refresh cannot recover missing/blocked/stale state.

    collect=True is explicit full guarded collection + retention recovery. All
    disabled sources may produce an empty ready view, without network requests.
    No callback/fetcher/skip-guard option exists on this public API.
    """
    if type(collect) is not bool or type(total_timeout) is not int or not 10 <= total_timeout <= 300: fail("invalid_timeout")
    try: sync._supervisor_preconditions()
    except sync.SyncError as exc: fail(str(exc))
    paths = [checked_path(p) for p in (catalog_path, registry_path, suppression_path, snapshot_path)]
    if len(set(paths)) != 4 or any(str(paths[0]) + suffix in map(str, paths[1:]) for suffix in (".lock", ".publication.lock", ".publication-bound.json", "-wal", "-shm")):
        fail("unsafe_path")
    try:
        with sync.publication_lock(paths[0]):
            return _publish_locked(*paths, collect=collect, total_timeout=total_timeout)
    except PublicationError: raise
    except sync.SyncError as exc:
        raise PublicationError(str(exc) if str(exc) in ERRORS else "publication_failed") from None
    except (OSError, sqlite3.Error, ValueError, TypeError, KeyError): raise PublicationError("publication_failed") from None


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("catalog", "registry", "suppression", "snapshot"): parser.add_argument("--" + name, required=True)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--refresh", action="store_true"); mode.add_argument("--collect", action="store_true")
    parser.add_argument("--total-timeout", type=int, default=60)
    args = parser.parse_args(argv)
    try:
        if not args.refresh and not args.collect:
            policy_files(args.registry, args.suppression)
            result = {"mode": "dry_run", "network": False, "writes": False}
        else:
            result = publish(args.catalog, args.registry, args.suppression, args.snapshot,
                             collect=args.collect, total_timeout=args.total_timeout)
        print(canonical(result).decode()); return 0
    except PublicationError as exc:
        print(canonical({"error": exc.code}).decode()); return 1


if __name__ == "__main__": raise SystemExit(main())
