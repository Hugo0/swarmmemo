#!/usr/bin/env python3
"""Permission-gated external reference feeds into a local, searchable SQLite catalog.

Default: network-free dry-run. --sync writes only this catalog, never SwarmMemo/HF.
"""
from __future__ import annotations

import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
import hashlib
from html.parser import HTMLParser
import http.client
import ipaddress
import json
import os
from pathlib import Path
import re
import select
import signal
import socket
import sqlite3
import ssl
import stat
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit, urlunsplit

FEED_VERSION = "https://jsonfeed.org/version/1.1"
CUTTLE_ADAPTER = "cuttle-worklog-index-v1"
CUTTLE_FEED = "https://blog.cuttle.af/content-index.json"
CUTTLE_ORIGIN = "https://blog.cuttle.af"
CUTTLE_SERIES = frozenset({"building-6529", "weekly-digests"})
CUTTLE_FIELDS = frozenset({"slug", "title", "url", "published_at", "excerpt", "series",
                           "6529_learning_path", "agents_learning_path"})
RIGHTS = {"automated_collection", "full_text_storage", "public_archive", "hugging_face"}
SOURCE_FIELDS = {"id", "name", "adapter", "feed_url", "enabled", "status", "terms_url", "permissions", "limits"}
DEFAULT_LIMITS = {"feed_bytes": 1048576, "item_bytes": 65536, "items_per_fetch": 100,
                  "total_items": 10000, "revisions_per_item": 20}
HARD_LIMITS = {"feed_bytes": 8388608, "item_bytes": 262144, "items_per_fetch": 1000,
               "total_items": 100000, "revisions_per_item": 100}
DEFAULT_TOTAL_TIMEOUT = 60
MIN_TOTAL_TIMEOUT, MAX_TOTAL_TIMEOUT = 10, 300
RETENTION_SECONDS = 3
REAP_SECONDS = 0.25
MAX_WORKER_OUTPUT = 65536
SAFE_ERRORS = frozenset("""
duplicate_json_field invalid_json_number invalid_timestamp invalid_https_url registry_too_large
unsupported_registry source_count_limit invalid_source_fields invalid_source_identity
unsupported_adapter_or_state feed_url_required unknown_permission invalid_permission
invalid_permission_period permission_scope_required invalid_source_limits invalid_catalog_limit
dns_failed dns_address_limit invalid_dns_address nonpublic_dns_address invalid_cache_validator
feed_http_status feed_content_type compressed_feed_rejected feed_byte_limit feed_deadline
truncated_feed author_limit invalid_author unsupported_feed_version feed_item_limit item_byte_limit
duplicate_external_id unsupported_deletion_extension invalid_deletion_flag item_content_required
catalog_permissions unsupported_catalog_version catalog_byte_limit source_identity_changed_use_new_id
source_item_capacity invalid_search unknown_source invalid_source_name invalid_permission_scope
invalid_author_name invalid_feed_title invalid_external_id invalid_source_revision invalid_item_title
invalid_item_body query_limit invalid_total_timeout worker_requires_single_thread worker_failed
worker_requires_default_sigchld worker_ownership_lost
cuttle_feed_binding cuttle_hf_permission_unresolved unsupported_cuttle_index invalid_cuttle_item
worker_output_limit sync_deadline sync_interrupted sync_interrupted_retention_pending TimeoutError OSError
OperationalError IntegrityError JSONDecodeError UnicodeDecodeError ValueError sync_failed
publication_guard_required publication_busy publication_path_unsafe
""".split())


class SyncError(ValueError):
    """Safe error codes only; source URLs, content, or raw exception strings stay private."""


def safe_error(exc):
    code = str(exc) if isinstance(exc, SyncError) else type(exc).__name__
    return code if code in SAFE_ERRORS else "sync_failed"


def _supervisor_preconditions():
    if threading.active_count() != 1 or threading.current_thread() is not threading.main_thread():
        raise SyncError("worker_requires_single_thread")
    # SIG_IGN auto-reaps on Linux; a custom handler may also waitpid our child.
    # Either destroys the unreaped-PID ownership guarantee needed by killpg.
    if signal.getsignal(signal.SIGCHLD) != signal.SIG_DFL:
        raise SyncError("worker_requires_default_sigchld")


def _kill_and_reap(pid, deadline):
    """Signal only our unreaped direct child/group, never an arbitrary PID/PGID.

    The child calls setsid before it can run a task or spawn DNS. Keeping it
    unreaped until after killpg prevents PID reuse from targeting an unrelated group.
    """
    try:
        # Observe ownership without reaping an exited group leader: descendants
        # may still need killing. Never signal if ownership was already lost.
        os.waitid(os.P_PID, pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
    except ChildProcessError:
        raise SyncError("worker_ownership_lost") from None
    try:
        os.killpg(pid, signal.SIGKILL)
    except ProcessLookupError:
        # Child may not have reached setsid; it cannot have spawned a worker yet.
        try: os.kill(pid, signal.SIGKILL)
        except ProcessLookupError: pass
    while True:
        try:
            reaped, _ = os.waitpid(pid, os.WNOHANG)
        except ChildProcessError:
            return True
        if reaped: return True
        if time.monotonic() >= deadline: return False
        time.sleep(min(0.01, max(0, deadline - time.monotonic())))


def _supervised_call(task, deadline, reap_deadline):
    """Linux/single-thread process isolation; no network or database work in parent.

    Only bounded JSON metadata crosses the pipe. Child stdout/stderr go to /dev/null,
    so fetched data and untrusted exception text cannot become supervisor output.
    Deadlines are absolute monotonic times, including IPC; no join/read can wait forever.
    """
    _supervisor_preconditions()
    if time.monotonic() >= deadline: raise SyncError("sync_deadline")
    protected = {signal.SIGINT, signal.SIGTERM}
    previous_mask = signal.pthread_sigmask(signal.SIG_BLOCK, protected)
    previous_handlers = {sig: signal.getsignal(sig) for sig in protected}
    cancelled = False
    def interrupted(_signal, _frame):
        # Do not raise asynchronously at cleanup entry or while reaping. The
        # bounded poll observes this flag, and finally always completes first.
        nonlocal cancelled
        cancelled = True
    read_fd = write_fd = pid = None
    try:
        for sig in protected: signal.signal(sig, interrupted)
        # Establish cleanup before fork or any parent-side close/allocation.
        # Signals remain blocked until the child PID is safely owned by finally.
        read_fd, write_fd = os.pipe()
        pid = os.fork()
        if pid == 0:
            try:
                os.close(read_fd)
                os.setsid()
                null_fd = os.open(os.devnull, os.O_RDWR)
                for fd in (0, 1, 2): os.dup2(null_fd, fd)
                if null_fd > 2: os.close(null_fd)
                for sig, handler in previous_handlers.items(): signal.signal(sig, handler)
                signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
                try: result = {"ok": True, "result": task()}
                except BaseException as exc: result = {"ok": False, "error": safe_error(exc)}
                raw = encoded(result)
                if len(raw) > MAX_WORKER_OUTPUT:
                    raw = encoded({"ok": False, "error": "worker_output_limit"})
                while raw:
                    sent = os.write(write_fd, raw)
                    raw = raw[sent:]
            except BaseException:
                pass
            finally:
                os._exit(0)  # No inherited Python shutdown handlers/flushes.
        fd, write_fd = write_fd, None
        os.close(fd)
        raw = bytearray()
        signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
        while True:
            if cancelled: raise SyncError("sync_interrupted")
            remaining = deadline - time.monotonic()
            if remaining <= 0: raise SyncError("sync_deadline")
            ready, _, _ = select.select([read_fd], [], [], min(remaining, 0.1))
            if not ready: continue
            chunk = os.read(read_fd, min(4096, MAX_WORKER_OUTPUT + 1 - len(raw)))
            if not chunk: break
            raw.extend(chunk)
            if len(raw) > MAX_WORKER_OUTPUT: raise SyncError("worker_output_limit")
        try: result = strict_json(raw)
        except Exception: raise SyncError("worker_failed") from None
        if not isinstance(result, dict) or type(result.get("ok")) is not bool:
            raise SyncError("worker_failed")
        if not result["ok"]:
            code = result.get("error")
            raise SyncError(code if isinstance(code, str) and code in SAFE_ERRORS else "worker_failed")
        return result.get("result")
    finally:
        # Repeated termination signals cannot interrupt group kill/reaping or
        # skip descriptor cleanup. Pending signals run only after ownership ends.
        signal.pthread_sigmask(signal.SIG_BLOCK, protected)
        cleanup_complete = False
        try:
            try:
                if pid is not None and pid > 0 and not _kill_and_reap(pid, reap_deadline):
                    raise SyncError("sync_interrupted_retention_pending")
                cleanup_complete = True
            finally:
                for fd in (read_fd, write_fd):
                    if fd is not None:
                        try: os.close(fd)
                        except OSError: pass
        finally:
            try:
                # Deliver pending termination to our non-raising handler only
                # after the child is gone; then restore the caller's handlers.
                signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
            finally:
                for sig, handler in previous_handlers.items(): signal.signal(sig, handler)
            if cancelled and cleanup_complete: raise SyncError("sync_interrupted")


def _retention_only(registry, path, fixed_now):
    """Recover an interrupted SQLite journal, then commit policy purges; no fetches."""
    if not Path(path).exists(): return None
    with catalog(path, registry["max_catalog_bytes"]) as db:
        enforce_retention(db, registry, int(time.time()) if fixed_now is None else fixed_now)
    return None


def encoded(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()


def digest(value):
    return hashlib.sha256(encoded(value)).hexdigest()


def strict_json(raw):
    def object_pairs(pairs):
        result = {}
        for key, value in pairs:
            if key in result: raise SyncError("duplicate_json_field")
            result[key] = value
        return result
    def bad_constant(_): raise SyncError("invalid_json_number")
    return json.loads(raw.decode("utf-8"), object_pairs_hook=object_pairs, parse_constant=bad_constant)


def timestamp(value):
    if not isinstance(value, str) or len(value) > 40: raise SyncError("invalid_timestamp")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        if parsed.tzinfo is None: raise ValueError()
        return int(parsed.timestamp())
    except ValueError: raise SyncError("invalid_timestamp") from None


def bounded_text(value, maximum, code):
    if not isinstance(value, str) or len(value.encode("utf-8")) > maximum or "\x00" in value:
        raise SyncError(code)
    return value


def https_url(value):
    bounded_text(value, 4096, "invalid_https_url")
    try:
        url = urlsplit(value)
        if url.scheme != "https" or not url.hostname or url.username or url.password or url.fragment or url.port not in (None, 443):
            raise ValueError()
        if any(ord(c) <= 32 or ord(c) == 127 for c in value) or "\\" in value:
            raise ValueError()
        hostname = url.hostname.encode("idna").decode("ascii").lower()
        if hostname == "localhost" or hostname.endswith((".localhost", ".local", ".internal")) or "%" in hostname:
            raise ValueError()
        try:
            address = ipaddress.ip_address(hostname)
        except ValueError:
            address = None
        if address is not None and (not address.is_global or address.is_multicast): raise ValueError()
        return url, hostname
    except ValueError: raise SyncError("invalid_https_url") from None


def load_registry(path):
    raw = Path(path).read_bytes()
    return registry_from_bytes(raw)


def registry_from_bytes(raw):
    """Validate exactly one caller-owned buffer; normalization never changes its hash."""
    if len(raw) > 1048576: raise SyncError("registry_too_large")
    registry = strict_json(raw)
    if not isinstance(registry, dict) or set(registry) - {"version", "sources", "max_catalog_bytes"} or registry.get("version") != 1:
        raise SyncError("unsupported_registry")
    sources = registry.get("sources")
    if not isinstance(sources, list) or len(sources) > 50: raise SyncError("source_count_limit")
    seen = set()
    for source in sources:
        if not isinstance(source, dict) or set(source) - SOURCE_FIELDS: raise SyncError("invalid_source_fields")
        source_id = source.get("id", "")
        if not isinstance(source_id, str) or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,63}", source_id) or source_id in seen:
            raise SyncError("invalid_source_identity")
        seen.add(source_id)
        bounded_text(source.get("name", ""), 256, "invalid_source_name")
        if source.get("adapter") not in ("jsonfeed-1.1", CUTTLE_ADAPTER) or type(source.get("enabled")) is not bool:
            raise SyncError("unsupported_adapter_or_state")
        if source["adapter"] == CUTTLE_ADAPTER and source.get("feed_url") != CUTTLE_FEED:
            raise SyncError("cuttle_feed_binding")
        if source.get("feed_url") is not None: https_url(source["feed_url"])
        if source["enabled"] and not source.get("feed_url"): raise SyncError("feed_url_required")
        if source.get("terms_url"): https_url(source["terms_url"])
        permissions = source.get("permissions", {})
        if not isinstance(permissions, dict) or set(permissions) - RIGHTS: raise SyncError("unknown_permission")
        for grant in permissions.values():
            if not isinstance(grant, dict) or set(grant) - {"approved", "evidence_url", "reviewed_at", "expires_at", "scope"} or type(grant.get("approved")) is not bool:
                raise SyncError("invalid_permission")
            if grant["approved"]:
                https_url(grant.get("evidence_url", ""))
                reviewed, expires = timestamp(grant.get("reviewed_at")), timestamp(grant.get("expires_at"))
                if reviewed >= expires: raise SyncError("invalid_permission_period")
                bounded_text(grant.get("scope", ""), 1024, "invalid_permission_scope")
                if not grant.get("scope"): raise SyncError("permission_scope_required")
        if source["adapter"] == CUTTLE_ADAPTER and permissions.get("hugging_face", {}).get("approved"):
            raise SyncError("cuttle_hf_permission_unresolved")
        limits = {**DEFAULT_LIMITS, **source.get("limits", {})}
        if set(limits) != set(DEFAULT_LIMITS) or any(type(v) is not int or not 1 <= v <= HARD_LIMITS[k] for k, v in limits.items()):
            raise SyncError("invalid_source_limits")
        source["limits"] = limits
    maximum = registry.setdefault("max_catalog_bytes", 268435456)
    if type(maximum) is not int or not 1048576 <= maximum <= 1073741824: raise SyncError("invalid_catalog_limit")
    return registry


def permitted(source, right, now):
    # Current source robots signals expressly disallow AI training. A CC0
    # statement does not silently resolve that conflict for downstream exports.
    if source.get("adapter") == CUTTLE_ADAPTER and right == "hugging_face": return False
    grant = source.get("permissions", {}).get(right, {})
    return bool(grant.get("approved") is True and timestamp(grant["reviewed_at"]) <= now < timestamp(grant["expires_at"]))


def collectable(source, now):
    return source["enabled"] and bool(source.get("feed_url")) and permitted(source, "automated_collection", now)


def public_addresses(hostname):
    # Bound libc DNS independently of its resolver retry configuration. Arguments
    # are passed directly, without a shell; this subprocess only resolves one host.
    resolver = "import json,socket,sys; print(json.dumps(sorted({x[4][0] for x in socket.getaddrinfo(sys.argv[1],443,type=socket.SOCK_STREAM)})))"
    try:
        result = subprocess.run([sys.executable, "-c", resolver, hostname], capture_output=True, timeout=5, check=True)
        addresses = json.loads(result.stdout)
    except (subprocess.SubprocessError, ValueError): raise SyncError("dns_failed") from None
    return check_addresses(addresses)


def check_addresses(addresses):
    if not isinstance(addresses, list) or not 1 <= len(addresses) <= 16: raise SyncError("dns_address_limit")
    for value in addresses:
        try: address = ipaddress.ip_address(value)
        except ValueError: raise SyncError("invalid_dns_address") from None
        if not address.is_global or address.is_multicast: raise SyncError("nonpublic_dns_address")
    return addresses


class PinnedHTTPS(http.client.HTTPSConnection):
    def __init__(self, hostname, address, timeout):
        super().__init__(hostname, port=443, timeout=timeout, context=ssl.create_default_context())
        self.address = address

    def connect(self):
        transport = socket.create_connection((self.address, 443), timeout=self.timeout)
        try: self.sock = self._context.wrap_socket(transport, server_hostname=self.host)
        except BaseException:
            transport.close()
            raise


def safe_validator(value):
    if value is None: return None
    if not isinstance(value, str) or len(value) > 1024 or any(ord(c) < 32 or ord(c) == 127 for c in value):
        raise SyncError("invalid_cache_validator")
    return value


def fetch_feed(source, validators, resolver=public_addresses, connection_factory=PinnedHTTPS):
    if source.get("adapter") == CUTTLE_ADAPTER and source.get("feed_url") != CUTTLE_FEED:
        raise SyncError("cuttle_feed_binding")
    url, hostname = https_url(source["feed_url"])
    addresses = check_addresses(resolver(hostname))
    connection = connection_factory(hostname, addresses[0], 5)
    headers = {"Accept": "application/feed+json, application/json", "Accept-Encoding": "identity",
               "User-Agent": "SwarmMemoSourceSync/0.1 (approved source collection)"}
    if validators.get("etag"): headers["If-None-Match"] = safe_validator(validators["etag"])
    if validators.get("last_modified"): headers["If-Modified-Since"] = safe_validator(validators["last_modified"])
    target = urlunsplit(("", "", url.path or "/", url.query, ""))
    try:
        connection.request("GET", target, headers=headers)
        response = connection.getresponse()
        result = {"status": response.status, "etag": safe_validator(response.getheader("ETag")),
                  "last_modified": safe_validator(response.getheader("Last-Modified")), "body": b""}
        if response.status == 304: return result
        if response.status != 200: raise SyncError("feed_http_status")  # Includes all redirects.
        if response.getheader("Content-Type", "").split(";", 1)[0].strip().lower() not in ("application/feed+json", "application/json"):
            raise SyncError("feed_content_type")
        if response.getheader("Content-Encoding", "identity").lower() != "identity": raise SyncError("compressed_feed_rejected")
        maximum = source["limits"]["feed_bytes"]
        length = response.getheader("Content-Length")
        if length is not None and (not length.isdecimal() or int(length) > maximum): raise SyncError("feed_byte_limit")
        chunks, size, deadline = [], 0, time.monotonic() + 20
        while True:
            if time.monotonic() >= deadline: raise SyncError("feed_deadline")
            chunk = response.read1(min(65536, maximum + 1 - size))
            if not chunk: break
            size += len(chunk)
            if size > maximum: raise SyncError("feed_byte_limit")
            chunks.append(chunk)
        # HTTPResponse.read1() can return EOF before a declared Content-Length
        # without raising IncompleteRead. Never accept or cache a partial feed.
        if length is not None and size != int(length): raise SyncError("truncated_feed")
        result["body"] = b"".join(chunks)
        return result
    finally:
        connection.close()


class PlainHTML(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.parts, self.suppressed = [], 0
    def handle_starttag(self, tag, attrs):
        if tag in ("script", "style"): self.suppressed += 1
        elif tag in ("p", "br", "div", "li"): self.parts.append("\n")
    def handle_endtag(self, tag):
        if tag in ("script", "style") and self.suppressed: self.suppressed -= 1
    def handle_data(self, text):
        if not self.suppressed: self.parts.append(text)


def author_refs(values):
    if not isinstance(values, list) or len(values) > 20: raise SyncError("author_limit")
    result = []
    for author in values:
        if not isinstance(author, dict): raise SyncError("invalid_author")
        name = bounded_text(author.get("name", ""), 512, "invalid_author_name")
        url = author.get("url", "")
        if url: https_url(url)
        result.append({"name": name, "url": url})  # Avatar/image URLs are never collected.
    return result


def cuttle_publication_time(value):
    """Validate source date without inventing timezone for its legacy rows."""
    bounded_text(value, 40, "invalid_timestamp")
    if re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}", value):
        try: datetime.strptime(value, "%Y-%m-%d %H:%M:%S")
        except ValueError: raise SyncError("invalid_timestamp") from None
        return False
    timestamp(value)
    return True


def cuttle_index_as_feed(index, source):
    """Pure bounded adaptation; never fetch post URLs or infer deletion/updates.

    The fixed version describes our observed source shape, not a version claimed
    by Cuttle. Schema drift requires review. Only index excerpts are retained.
    """
    if source.get("feed_url") != CUTTLE_FEED: raise SyncError("cuttle_feed_binding")
    if not isinstance(index, list): raise SyncError("unsupported_cuttle_index")
    # Bound the WHOLE input array before selecting worklog series. Do not turn
    # out-of-scope rows into a way to bypass parsing/capacity limits.
    if len(index) > source["limits"]["items_per_fetch"]: raise SyncError("feed_item_limit")
    selected, seen = [], set()
    for row in index:
        if not isinstance(row, dict) or set(row) != CUTTLE_FIELDS:
            raise SyncError("invalid_cuttle_item")
        if len(encoded(row)) > source["limits"]["item_bytes"]: raise SyncError("item_byte_limit")
        slug = bounded_text(row["slug"], 256, "invalid_cuttle_item")
        # The inspected index includes an underscore-containing slug. Allow a
        # bounded safe ASCII path component, never separators/escapes/dot paths.
        if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", slug): raise SyncError("invalid_cuttle_item")
        if slug in seen: raise SyncError("duplicate_external_id")
        seen.add(slug)
        if row["url"] != CUTTLE_ORIGIN + "/" + slug + "/": raise SyncError("invalid_cuttle_item")
        title = bounded_text(row["title"], 1024, "invalid_item_title")
        if not title.strip(): raise SyncError("invalid_item_title")
        excerpt = bounded_text(row["excerpt"], 8192, "invalid_item_body")
        cuttle_publication_time(row["published_at"])
        series = row["series"]
        if series is not None and (not isinstance(series, str) or len(series) > 128
                                  or not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", series)):
            raise SyncError("invalid_cuttle_item")
        # These unused fields were null throughout the inspected source. A new
        # shape/extension is not guessed into authority or extra fetch targets.
        if row["6529_learning_path"] is not None or row["agents_learning_path"] is not None:
            raise SyncError("invalid_cuttle_item")
        if series not in CUTTLE_SERIES: continue
        selected.append({"id": slug, "url": row["url"], "title": title,
                         "date_published": row["published_at"],
                         "content_text": "External co-created worklog index excerpt; not a native post or open job.\n"
                                         + "Source series: " + series + "\n\n" + excerpt})
    return {"version": FEED_VERSION, "title": "Cuttle Blog — external worklog references",
            "authors": [{"name": "@0xCuttlefish", "url": CUTTLE_ORIGIN + "/about/"},
                        {"name": "Trurl (Hermes Agent; site-declared co-creator)", "url": CUTTLE_ORIGIN + "/llms.txt"}],
            "items": selected}


def parse_feed(raw, source, now):
    if len(raw) > source["limits"]["feed_bytes"]: raise SyncError("feed_byte_limit")
    feed = strict_json(raw)
    if source.get("adapter") == CUTTLE_ADAPTER: feed = cuttle_index_as_feed(feed, source)
    if not isinstance(feed, dict) or feed.get("version") != FEED_VERSION: raise SyncError("unsupported_feed_version")
    bounded_text(feed.get("title"), 1024, "invalid_feed_title")
    items = feed.get("items")
    if not isinstance(items, list) or len(items) > source["limits"]["items_per_fetch"]: raise SyncError("feed_item_limit")
    fallback_authors = author_refs(feed.get("authors", []))
    result, seen = [], set()
    store_body = permitted(source, "full_text_storage", now)
    for item in items:
        if not isinstance(item, dict) or len(encoded(item)) > source["limits"]["item_bytes"]:
            raise SyncError("item_byte_limit")
        external_id = bounded_text(item.get("id"), 512, "invalid_external_id")
        if not external_id or external_id in seen: raise SyncError("duplicate_external_id")
        seen.add(external_id)
        extension = item.get("_swarmmemo", {})
        if not isinstance(extension, dict) or set(extension) - {"deleted", "revision", "about"}:
            raise SyncError("unsupported_deletion_extension")
        if "deleted" in extension and type(extension["deleted"]) is not bool: raise SyncError("invalid_deletion_flag")
        deleted = extension.get("deleted", False)
        revision = bounded_text(extension.get("revision", ""), 128, "invalid_source_revision")
        if deleted:
            result.append({"external_id": external_id, "deleted": True, "source_revision": revision,
                           "content_hash": digest({"id": external_id, "deleted": True}), "body": None, "body_text": ""})
            continue
        title = bounded_text(item.get("title", ""), 1024, "invalid_item_title")
        external_url = item.get("url", "")
        if external_url: https_url(external_url)
        published, modified = item.get("date_published", ""), item.get("date_modified", "")
        if published:
            if source.get("adapter") == CUTTLE_ADAPTER: cuttle_publication_time(published)
            else: timestamp(published)
        if modified: timestamp(modified)
        authors = author_refs(item["authors"]) if "authors" in item else fallback_authors
        if "content_text" in item:
            body_format, body = "text", bounded_text(item["content_text"], source["limits"]["item_bytes"], "invalid_item_body")
            body_text = body
        elif "content_html" in item:
            body_format, body = "html", bounded_text(item["content_html"], source["limits"]["item_bytes"], "invalid_item_body")
            parser = PlainHTML(); parser.feed(body); body_text = "".join(parser.parts)
        else: raise SyncError("item_content_required")
        normalized = {"external_id": external_id, "external_url": external_url, "title": title,
                      "authors_json": encoded(authors).decode(), "published_at": published, "modified_at": modified,
                      "source_revision": revision, "body_format": body_format, "body": body, "deleted": False}
        normalized["content_hash"] = digest(normalized)
        normalized["body"] = body if store_body else None
        normalized["body_text"] = body_text if store_body else ""
        result.append(normalized)
    return result


SCHEMA = """
CREATE TABLE IF NOT EXISTS sources(id TEXT PRIMARY KEY, feed_url TEXT, adapter TEXT NOT NULL,
 body_mode INTEGER NOT NULL DEFAULT 0, etag TEXT, last_modified TEXT, last_checked INTEGER,
 last_success INTEGER, status TEXT NOT NULL DEFAULT 'new');
CREATE TABLE IF NOT EXISTS items(rowid INTEGER PRIMARY KEY, source_id TEXT NOT NULL REFERENCES sources(id),
 external_id TEXT NOT NULL, external_url TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '',
 authors_json TEXT NOT NULL DEFAULT '[]', published_at TEXT NOT NULL DEFAULT '', modified_at TEXT NOT NULL DEFAULT '',
 first_observed INTEGER NOT NULL, last_observed INTEGER NOT NULL, source_revision TEXT NOT NULL DEFAULT '',
 content_hash TEXT NOT NULL, body_format TEXT NOT NULL DEFAULT '', body TEXT, body_text TEXT NOT NULL DEFAULT '',
 deleted INTEGER NOT NULL DEFAULT 0, UNIQUE(source_id,external_id));
CREATE TABLE IF NOT EXISTS revisions(id INTEGER PRIMARY KEY, item_id INTEGER NOT NULL REFERENCES items(rowid),
 content_hash TEXT NOT NULL, observed_at INTEGER NOT NULL, source_revision TEXT NOT NULL,
 title TEXT NOT NULL, body TEXT, body_text TEXT NOT NULL, metadata_json TEXT NOT NULL DEFAULT '{}',
 deleted INTEGER NOT NULL, purged INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS revision_item ON revisions(item_id,id);
CREATE TABLE IF NOT EXISTS sync_runs(id INTEGER PRIMARY KEY, source_id TEXT NOT NULL, observed_at INTEGER NOT NULL,
 status TEXT NOT NULL, item_count INTEGER NOT NULL DEFAULT 0, response_bytes INTEGER NOT NULL DEFAULT 0);
CREATE VIRTUAL TABLE IF NOT EXISTS item_search USING fts5(title,body_text,content='items',content_rowid='rowid');
CREATE TRIGGER IF NOT EXISTS search_insert AFTER INSERT ON items BEGIN
 INSERT INTO item_search(rowid,title,body_text) VALUES(new.rowid,new.title,new.body_text); END;
CREATE TRIGGER IF NOT EXISTS search_delete AFTER DELETE ON items BEGIN
 INSERT INTO item_search(item_search,rowid,title,body_text) VALUES('delete',old.rowid,old.title,old.body_text); END;
CREATE TRIGGER IF NOT EXISTS search_update AFTER UPDATE ON items BEGIN
 INSERT INTO item_search(item_search,rowid,title,body_text) VALUES('delete',old.rowid,old.title,old.body_text);
 INSERT INTO item_search(rowid,title,body_text) VALUES(new.rowid,new.title,new.body_text); END;
PRAGMA user_version=1;
"""


@contextmanager
def catalog(path, maximum):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    lock_fd = os.open(str(path) + ".lock", os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(lock_fd, "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fd = os.open(path, os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
        info = os.fstat(fd); os.close(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077: raise SyncError("catalog_permissions")
        db = sqlite3.connect(path, timeout=5)
        db.row_factory = sqlite3.Row
        try:
            db.execute("PRAGMA foreign_keys=ON"); db.execute("PRAGMA secure_delete=ON")
            db.execute("PRAGMA journal_mode=WAL"); db.execute("PRAGMA synchronous=FULL")
            version = db.execute("PRAGMA user_version").fetchone()[0]
            if version not in (0, 1): raise SyncError("unsupported_catalog_version")
            if version == 0: db.executescript(SCHEMA)
            page_size = db.execute("PRAGMA page_size").fetchone()[0]
            if db.execute("PRAGMA page_count").fetchone()[0] * page_size > maximum:
                raise SyncError("catalog_byte_limit")
            db.execute("PRAGMA max_page_count=" + str(maximum // page_size))
            yield db
            db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        finally: db.close()


def purge_bodies(db, source_id):
    db.execute("UPDATE revisions SET body=NULL,body_text='',purged=1 WHERE item_id IN (SELECT rowid FROM items WHERE source_id=?)", (source_id,))
    db.execute("UPDATE items SET body=NULL,body_text='' WHERE source_id=? AND body IS NOT NULL", (source_id,))


def enforce_retention(db, registry, now):
    """Commit removals independently of source registration or fetch outcomes."""
    current = {source["id"]: source for source in registry["sources"]}
    for old in db.execute("SELECT * FROM sources").fetchall():
        source = current.get(old["id"])
        if source is None or not collectable(source, now) or not permitted(source, "full_text_storage", now):
            with db:
                purge_bodies(db, old["id"])
                # Storage permission and collection success are distinct. A
                # successful metadata-only collection remains a usable source.
                blocked = source is None or not collectable(source, now)
                db.execute("UPDATE sources SET body_mode=0,etag=NULL,last_modified=NULL,status=CASE WHEN ? THEN 'permission_blocked' ELSE status END WHERE id=?", (blocked, old["id"]))


@contextmanager
def retention_guard(db, registry, fixed_now):
    try:
        yield
    finally:
        # Roll back only an unfinished batch. Already committed content changes
        # and retention removals stay durable. Recheck all sources, even skipped
        # or unselected ones, after failures and after the final source finishes.
        db.rollback()
        enforce_retention(db, registry, int(time.time()) if fixed_now is None else fixed_now)


def reconcile_policy(db, registry, now):
    enforce_retention(db, registry, now)
    current = {source["id"]: source for source in registry["sources"]}
    with db:
        for source in current.values():
            body_mode = int(collectable(source, now) and permitted(source, "full_text_storage", now))
            old = db.execute("SELECT * FROM sources WHERE id=?", (source["id"],)).fetchone()
            if old and (old["feed_url"] != source.get("feed_url") or old["adapter"] != source["adapter"]):
                raise SyncError("source_identity_changed_use_new_id")
            db.execute("INSERT OR IGNORE INTO sources(id,feed_url,adapter,body_mode) VALUES(?,?,?,?)",
                       (source["id"], source.get("feed_url"), source["adapter"], body_mode))
            if old and old["body_mode"] != body_mode:
                db.execute("UPDATE sources SET etag=NULL,last_modified=NULL WHERE id=?", (source["id"],))
            db.execute("UPDATE sources SET body_mode=? WHERE id=?", (body_mode, source["id"]))


def apply_items(db, source, items, now, response):
    changed, tombstones, stale = 0, 0, 0
    with db:
        for item in items:
            old = db.execute("SELECT * FROM items WHERE source_id=? AND external_id=?", (source["id"], item["external_id"])).fetchone()
            if old and old["deleted"]:
                # Permanent local deletion latch. Neither a stale nor newer feed body
                # may resurrect this identity without a future explicit recovery policy.
                stale += int(not item["deleted"])
                continue
            if old and old["content_hash"] == item["content_hash"] and (old["body"] is not None or item["body"] is None):
                db.execute("UPDATE items SET last_observed=? WHERE rowid=?", (now, old["rowid"]))
                continue
            if not old:
                count = db.execute("SELECT count(*) FROM items WHERE source_id=?", (source["id"],)).fetchone()[0]
                if count >= source["limits"]["total_items"]: raise SyncError("source_item_capacity")
                cursor = db.execute("INSERT INTO items(source_id,external_id,first_observed,last_observed,content_hash) VALUES(?,?,?,?,?)",
                                    (source["id"], item["external_id"], now, now, item["content_hash"]))
                item_id = cursor.lastrowid
            else: item_id = old["rowid"]
            if item["deleted"]:
                db.execute("UPDATE items SET title='',body=NULL,body_text='',deleted=1,content_hash=?,last_observed=?,source_revision=? WHERE rowid=?",
                           (item["content_hash"], now, item["source_revision"], item_id))
                db.execute("UPDATE revisions SET title='',body=NULL,body_text='',purged=1 WHERE item_id=?", (item_id,))
                title, body, body_text = "", None, ""
                tombstones += 1
            else:
                title, body, body_text = item["title"], item["body"], item["body_text"]
                db.execute("UPDATE items SET external_url=?,title=?,authors_json=?,published_at=?,modified_at=?,source_revision=?,content_hash=?,body_format=?,body=?,body_text=?,last_observed=? WHERE rowid=?",
                    (item["external_url"], title, item["authors_json"], item["published_at"], item["modified_at"], item["source_revision"],
                     item["content_hash"], item["body_format"], body, body_text, now, item_id))
            metadata = {key: item.get(key, old[key] if old is not None else "")
                        for key in ("external_url", "authors_json", "published_at", "modified_at", "body_format")}
            db.execute("INSERT INTO revisions(item_id,content_hash,observed_at,source_revision,title,body,body_text,metadata_json,deleted,purged) VALUES(?,?,?,?,?,?,?,?,?,?)",
                (item_id, item["content_hash"], now, item["source_revision"], title, body, body_text, encoded(metadata).decode(), int(item["deleted"]), int(item["deleted"])))
            db.execute("DELETE FROM revisions WHERE item_id=? AND id NOT IN (SELECT id FROM revisions WHERE item_id=? ORDER BY id DESC LIMIT ?)",
                       (item_id, item_id, source["limits"]["revisions_per_item"]))
            changed += 1
        db.execute("UPDATE sources SET etag=?,last_modified=?,last_checked=?,last_success=?,status='ok' WHERE id=?",
                   (response.get("etag"), response.get("last_modified"), now, now, source["id"]))
        db.execute("INSERT INTO sync_runs(source_id,observed_at,status,item_count,response_bytes) VALUES(?,?,'ok',?,?)",
                   (source["id"], now, len(items), len(response.get("body", b""))))
        db.execute("DELETE FROM sync_runs WHERE id NOT IN (SELECT id FROM sync_runs ORDER BY id DESC LIMIT 1000)")
    return {"source": source["id"], "status": "ok", "changed": changed, "tombstones": tombstones, "resurrections_blocked": stale}


def _synchronize(registry, path, selected=None, now=None, fetcher=fetch_feed):
    """Unsupervised internal primitive for isolated child execution/fixture injection only."""
    fixed_now = now
    now = int(time.time()) if fixed_now is None else fixed_now
    results = []
    with catalog(path, registry["max_catalog_bytes"]) as db, retention_guard(db, registry, fixed_now):
        reconcile_policy(db, registry, now)
        for source in registry["sources"]:
            now = int(time.time()) if fixed_now is None else fixed_now
            if selected is not None and source["id"] != selected: continue
            if not collectable(source, now):
                results.append({"source": source["id"], "status": "permission_blocked"}); continue
            try:
                validators = dict(db.execute("SELECT etag,last_modified FROM sources WHERE id=?", (source["id"],)).fetchone())
                response = fetcher(source, validators)
                now = int(time.time()) if fixed_now is None else fixed_now
                if not collectable(source, now):
                    with db:
                        purge_bodies(db, source["id"])
                        db.execute("UPDATE sources SET etag=NULL,last_modified=NULL,body_mode=0,status='permission_blocked' WHERE id=?", (source["id"],))
                    results.append({"source": source["id"], "status": "permission_blocked"})
                    continue
                if not permitted(source, "full_text_storage", now):
                    # A storage grant can expire during an otherwise authorized
                    # request, including a 304 that supplies no replacement body.
                    with db:
                        purge_bodies(db, source["id"])
                        db.execute("UPDATE sources SET etag=NULL,last_modified=NULL,body_mode=0 WHERE id=?", (source["id"],))
                if response["status"] == 304:
                    with db:
                        db.execute("UPDATE sources SET last_checked=?,last_success=?,status='not_modified' WHERE id=?", (now, now, source["id"]))
                    results.append({"source": source["id"], "status": "not_modified"}); continue
                if response["status"] != 200: raise SyncError("feed_http_status")
                items = parse_feed(response["body"], source, now)
                results.append(apply_items(db, source, items, now, response))
            except Exception as exc:
                db.rollback()
                code = safe_error(exc)
                with db: db.execute("UPDATE sources SET last_checked=?,status='failed' WHERE id=?", (now, source["id"]))
                results.append({"source": source["id"], "status": "failed", "error": code})
    return results


@contextmanager
def publication_lock(path):
    """Permanent outer lock shared by ordinary collection and publication.

    Never unlink this inode: another process may still hold its open description.
    Catalog children continue taking their separate inner catalog.lock unchanged.
    """
    path = Path(os.path.abspath(path))
    trusted_publication_ancestors(path, allow_missing=True)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    trusted_publication_ancestors(path)
    lock_path = str(path) + ".publication.lock"
    fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1:
            raise SyncError("publication_path_unsafe")
        try: fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError: raise SyncError("publication_busy") from None
        yield path
    finally:
        os.close(fd)


def trusted_publication_ancestors(path, *, allow_missing=False):
    """Same-UID operator is trusted; other users cannot rename ancestor entries.

    Root-owned sticky ancestors such as /tmp are allowed, but never as the
    immediate parent of a publication or lock file.
    """
    path = Path(os.path.abspath(path))
    for parent in (path.parent, *path.parent.parents):
        try: info = parent.lstat()
        except FileNotFoundError:
            if allow_missing: continue
            raise SyncError("publication_path_unsafe") from None
        sticky_root = info.st_uid == 0 and info.st_mode & stat.S_ISVTX and parent != path.parent
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid not in (os.geteuid(), 0)
                or (info.st_mode & 0o022 and not sticky_root)):
            raise SyncError("publication_path_unsafe")


def synchronize(registry, path, selected=None, now=None, fetcher=fetch_feed,
                total_timeout=DEFAULT_TOTAL_TIMEOUT):
    """Ordinary private collection refuses publication-bound catalogs.

    The marker check and complete supervised invocation share the outer lock, so
    publication activation cannot race an already-authorized ordinary collector.
    """
    # Keep invalid-budget/thread/signal calls nonmutating, as before.
    if type(total_timeout) is not int or not MIN_TOTAL_TIMEOUT <= total_timeout <= MAX_TOTAL_TIMEOUT:
        raise SyncError("invalid_total_timeout")
    _supervisor_preconditions()
    with publication_lock(path) as bound_path:
        try: os.lstat(str(bound_path) + ".publication-bound.json")
        except FileNotFoundError: pass
        except OSError: raise SyncError("publication_guard_required") from None
        else: raise SyncError("publication_guard_required")
        return _run_synchronize(registry, bound_path, selected, now, fetcher, total_timeout)


def _run_synchronize(registry, path, selected=None, now=None, fetcher=fetch_feed,
                total_timeout=DEFAULT_TOTAL_TIMEOUT):
    """Bounded public collection API. Preserve atomic batches, never enable a source.

    Reserve a no-network cleanup phase within the same deadline. Already committed
    source batches survive interruption; an in-flight SQLite transaction does not.
    Cleanup failure is explicit, not a claim that expired stored bodies were purged.
    """
    if type(total_timeout) is not int or not MIN_TOTAL_TIMEOUT <= total_timeout <= MAX_TOTAL_TIMEOUT:
        raise SyncError("invalid_total_timeout")
    _supervisor_preconditions()
    def interrupted(_signal, _frame): raise SyncError("sync_interrupted")
    previous = signal.signal(signal.SIGTERM, interrupted)
    try:
        return _synchronize_supervised(registry, path, selected, now, fetcher, total_timeout)
    finally:
        signal.signal(signal.SIGTERM, previous)


def _synchronize_supervised(registry, path, selected, now, fetcher, total_timeout):
    # The private timeout plumbing accepts small fixture budgets; the public API
    # and CLI permit only validated whole seconds in the documented safe range.
    deadline = time.monotonic() + total_timeout
    collection_end = deadline - RETENTION_SECONDS - 2 * REAP_SECONDS
    try:
        return _supervised_call(lambda: _synchronize(registry, path, selected, now, fetcher),
                                collection_end, collection_end + REAP_SECONDS)
    except BaseException as original:
        try:
            _supervised_call(lambda: _retention_only(registry, path, now),
                             deadline - REAP_SECONDS, deadline)
        except BaseException:
            raise SyncError("sync_interrupted_retention_pending") from None
        if isinstance(original, KeyboardInterrupt): raise SyncError("sync_interrupted") from None
        raise original


def search_catalog(registry, path, query, limit=20, now=None):
    now = int(time.time()) if now is None else now
    bounded_text(query, 256, "query_limit")
    if not query.strip() or not 1 <= limit <= 100: raise SyncError("invalid_search")
    # Search is an explicit public-archive view, not an operator bypass. Every call
    # re-evaluates the current registry, including removal, disabled state and expiry.
    eligible = [source for source in registry["sources"] if collectable(source, now) and permitted(source, "public_archive", now)]
    if not eligible: return []
    path = Path(path).resolve()
    db = sqlite3.connect(path.as_uri() + "?mode=ro", uri=True, timeout=5)
    db.row_factory = sqlite3.Row
    try:
        if db.execute("PRAGMA user_version").fetchone()[0] != 1: raise SyncError("unsupported_catalog_version")
        results = []
        phrase = '"' + query.replace('"', '""') + '"'
        for source in eligible:
            metadata = db.execute("SELECT feed_url,adapter FROM sources WHERE id=?", (source["id"],)).fetchone()
            if metadata is None: continue
            if metadata["feed_url"] != source.get("feed_url") or metadata["adapter"] != source["adapter"]:
                raise SyncError("source_identity_changed_use_new_id")
            body_allowed = permitted(source, "full_text_storage", now)
            expression = ("{title body_text}:" if body_allowed else "title:") + phrase
            rows = db.execute("SELECT i.* FROM item_search JOIN items i ON i.rowid=item_search.rowid WHERE item_search MATCH ? AND i.source_id=? AND i.deleted=0 ORDER BY i.last_observed DESC,i.external_id LIMIT ?",
                              (expression, source["id"], limit)).fetchall()
            for row in rows:
                result = {"source": source["id"], "external_id": row["external_id"], "url": row["external_url"],
                    "title": row["title"], "authors": json.loads(row["authors_json"]), "source_published_at": row["published_at"],
                    "source_modified_at": row["modified_at"], "first_observed_at": row["first_observed"], "last_observed_at": row["last_observed"],
                    "content_hash": row["content_hash"], "body_text": row["body_text"] if body_allowed else None,
                    "hugging_face_eligible": bool(body_allowed and permitted(source, "hugging_face", now)), "untrusted_content": True}
                if source["adapter"] == CUTTLE_ADAPTER:
                    result.update({"content_kind": "external_worklog_index_excerpt",
                                   "authorship_basis": "site_declared_human_agent_co_creation",
                                   "source_publication_timezone_known": cuttle_publication_time(row["published_at"]),
                                   "native_identity": False, "claimable_job": False})
                results.append(result)
        return sorted(results, key=lambda row: (-row["last_observed_at"], row["source"], row["external_id"]))[:limit]
    finally: db.close()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--registry", required=True)
    parser.add_argument("--catalog", required=True)
    parser.add_argument("--source")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--sync", action="store_true")
    mode.add_argument("--search", metavar="TEXT")
    parser.add_argument("--limit", type=int, default=20)
    parser.add_argument("--total-timeout", type=int, default=DEFAULT_TOTAL_TIMEOUT,
                        help="Total --sync walltime budget in seconds, 10–300 (default 60), including retention cleanup")
    args = parser.parse_args(argv)
    try:
        if not MIN_TOTAL_TIMEOUT <= args.total_timeout <= MAX_TOTAL_TIMEOUT:
            raise SyncError("invalid_total_timeout")
        registry = load_registry(args.registry)
        now = int(time.time())
        if args.source and args.source not in {source["id"] for source in registry["sources"]}:
            raise SyncError("unknown_source")
        if args.search is not None:
            if args.source: registry = {**registry, "sources": [s for s in registry["sources"] if s["id"] == args.source]}
            result = {"mode": "search", "results": search_catalog(registry, args.catalog, args.search, args.limit, now)}
        elif args.sync:
            result = {"mode": "local_sync", "sources": synchronize(registry, args.catalog, args.source,
                       total_timeout=args.total_timeout), "board_published": False}
        else:
            result = {"mode": "dry-run", "network_requests": 0, "catalog_changed": False,
                      "sources": [{"source": s["id"], "collection_allowed": collectable(s, now),
                                   "body_storage_allowed": permitted(s, "full_text_storage", now),
                                   "public_archive_allowed": permitted(s, "public_archive", now),
                                   "hugging_face_allowed": permitted(s, "hugging_face", now)}
                                  for s in registry["sources"] if not args.source or s["id"] == args.source]}
        print(json.dumps(result, ensure_ascii=False))
        return 1 if any(s.get("status") == "failed" for s in result.get("sources", [])) else 0
    except Exception as exc:
        print(json.dumps({"error": safe_error(exc)}), file=sys.stderr)
        return 1


if __name__ == "__main__": raise SystemExit(main())
