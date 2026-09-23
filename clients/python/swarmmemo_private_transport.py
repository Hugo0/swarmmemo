#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Private-room read transport. Local pilot; never an anonymous/public fallback."""
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


# Arm before client/crypto/policy imports or accepting a key path. No preexec_fn.
if __name__ == "__main__":
    try:
        if len(sys.argv) != 3 or sys.argv[1] != "_fetch": raise ValueError()
        _bind_parent(int(sys.argv[2]))
    except BaseException:
        sys.exit(1)

import base64
from contextlib import contextmanager
import hashlib
import json
import math
from pathlib import Path
import re
import select
import signal
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
MAX_EVENT = 256 * 1024
MAX_IPC = 2 * MAX_RESPONSE
SLUG = r"[a-z0-9][a-z0-9_-]{0,63}"
IDENTIFIER = r"[A-Za-z0-9_-]{1,128}"
HASH = r"[0-9a-f]{64}"
GENERATION = r"[0-9a-f]{32}"
BINDING_FIELDS = set("schema type origin service_id room reader_public_key start_mode storage offline_bodies".split())
EVENT_FIELDS = set("id sequence room page text kind author handle public_key signature signed_payload created_at sha256 reply_to to hidden reason type visibility archive_eligible attachments hidden_by".split())
POST_FIELDS = set("operation room page text kind reply_to to request_id public_key timestamp nonce handle visibility attachments".split())
ATTACHMENT_FIELDS = set("id room filename media_type sha256 size created_at expires_at deleted expired".split())


class PrivateInboxError(ValueError):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def encode(value):
    try:
        return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")
    except (ValueError, TypeError, UnicodeError, RecursionError):
        raise PrivateInboxError("invalid_json") from None


def strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result: raise PrivateInboxError("duplicate_json_field")
            result[key] = value
        return result
    def bad(_): raise PrivateInboxError("invalid_json")
    try:
        if isinstance(raw, bytes): raw = raw.decode("utf-8")
        return json.loads(raw, object_pairs_hook=pairs, parse_constant=bad)
    except (ValueError, TypeError, UnicodeError, RecursionError) as exc:
        if isinstance(exc, PrivateInboxError): raise
        raise PrivateInboxError("invalid_json") from None


def matches(value, pattern):
    return isinstance(value, str) and re.fullmatch(pattern, value) is not None


def sha(raw): return hashlib.sha256(raw).hexdigest()


def _private_cursor_fits(value):
    try: return isinstance(value, str) and len(value.encode("utf-8")) <= 4096
    except UnicodeError: return False


def binding_validated(binding):
    if not isinstance(binding, dict): raise PrivateInboxError("invalid_binding")
    granted = type(binding.get("schema")) is int and binding["schema"] == 2
    if (set(binding) != BINDING_FIELDS | ({"private_read"} if granted else set())
            or type(binding["schema"]) is not int or binding["schema"] not in (1, 2)
            or binding["type"] != ("private-room-grant-inbox" if granted else "private-room-inbox") or binding["storage"] != "metadata-only"
            or binding["offline_bodies"] != "deny" or binding["start_mode"] not in ("history", "recent")
            or not matches(binding["room"], SLUG)
            or not matches(binding["service_id"], r"[A-Za-z0-9.-]{1,128}")):
        raise PrivateInboxError("invalid_binding")
    try:
        reader = memo.unb64(binding["reader_public_key"])
        if len(reader) != 32: raise ValueError()
        if granted and memo.private_read_context(binding["private_read"])["grant_id"] != sha(reader): raise ValueError()
        origin = binding["origin"]
        if not isinstance(origin, str) or len(origin) > 2048: raise ValueError()
        url = urllib.parse.urlsplit(origin)
        if (url.scheme not in ("http", "https") or not url.hostname or url.username is not None
                or url.password is not None or url.path or url.query or url.fragment or url.port == 0
                or urllib.parse.urlunsplit((url.scheme, url.netloc, "", "", "")) != origin
                or any(ord(c) <= 32 or ord(c) == 127 or c == "\\" for c in origin)):
            raise ValueError()
        if url.scheme == "http" and url.hostname not in ("localhost", "127.0.0.1", "::1"): raise ValueError()
    except (ValueError, TypeError, KeyError):
        raise PrivateInboxError("invalid_binding") from None
    return strict_json(encode(binding))


def protected_path(path):
    try:
        path = Path(path)
        if not path.is_absolute() or ".." in path.parts: raise PrivateInboxError("private_path_required")
        for part in [path, *path.parents]:
            if part.is_symlink(): raise PrivateInboxError("symlink_not_allowed")
        info = path.parent.stat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise PrivateInboxError("private_directory_required")
        return path
    except (OSError, TypeError, ValueError) as error:
        if isinstance(error, PrivateInboxError): raise
        raise PrivateInboxError("private_path_unavailable") from None


def private_fd(path, flags):
    fd = os.open(path, flags | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        os.close(fd)
        raise PrivateInboxError("private_regular_file_required")
    return fd


def _load_key(path, binding):
    path = protected_path(path)
    with os.fdopen(private_fd(path, os.O_RDONLY), "rb") as stream:
        raw = stream.read(8193)
    if len(raw) > 8192: raise PrivateInboxError("key_file_limit")
    record = strict_json(raw)
    if (not isinstance(record, dict) or set(record) != {"version", "private_key", "public_key"}
            or type(record["version"]) is not int or record["version"] != 1
            or record["public_key"] != binding["reader_public_key"]):
        raise PrivateInboxError("reader_key_mismatch")
    try:
        seed = memo.unb64(record["private_key"])
        private, _, serialization = memo.crypto()
        if len(seed) == 32: key = private.from_private_bytes(seed)
        elif len(seed) == 48 and seed.startswith(bytes.fromhex("302e020100300506032b657004220420")):
            key = serialization.load_der_private_key(seed, password=None)
        else: raise ValueError()
        if not isinstance(key, private) or memo.b64(memo.public_bytes(key)) != binding["reader_public_key"]: raise ValueError()
    except Exception:
        raise PrivateInboxError("reader_key_mismatch") from None
    return key


def validate_private_event(event, binding):
    """Separate restrictive entry point: no public, anonymous or child provenance."""
    if not isinstance(event, dict) or set(event) - EVENT_FIELDS or len(encode(event)) > MAX_EVENT:
        raise PrivateInboxError("invalid_event_fields_or_size")
    required = set("id sequence room page text kind author created_at sha256 hidden type visibility archive_eligible".split())
    if not required <= set(event): raise PrivateInboxError("missing_event_field")
    if (event["room"] != binding["room"] or event["visibility"] != "private"
            or event["archive_eligible"] is not False): raise PrivateInboxError("event_scope_mismatch")
    if (not matches(event["id"], IDENTIFIER) or not all(matches(event[f], SLUG) for f in ("room", "page", "kind"))
            or not matches(event["author"], HASH) or not matches(event["sha256"], HASH)
            or type(event["sequence"]) is not int or not 1 <= event["sequence"] <= 9223372036854775807
            or type(event["created_at"]) is not int or not 0 <= event["created_at"] <= 9223372036854775807
            or type(event["hidden"]) is not bool): raise PrivateInboxError("invalid_event_metadata")
    for field in ("text", "handle", "public_key", "signature", "signed_payload", "reply_to", "to", "reason"):
        if field in event and (not isinstance(event[field], str) or "\x00" in event[field]):
            raise PrivateInboxError("invalid_event_text")
    if "hidden_by" in event and (event["type"] != "tombstone" or event["hidden_by"] not in ("operator", "room")):
        raise PrivateInboxError("invalid_event_metadata")
    if event.get("to") and not matches(event["to"], HASH): raise PrivateInboxError("invalid_recipient")
    if event.get("reply_to") and not matches(event["reply_to"], IDENTIFIER): raise PrivateInboxError("invalid_reply")
    try:
        key = memo.unb64(event.get("public_key", ""))
        if len(key) != 32 or sha(key) != event["author"]: raise ValueError()
    except Exception: raise PrivateInboxError("invalid_author_key") from None
    attachments = event.get("attachments", [])
    if not isinstance(attachments, list) or len(attachments) > 8: raise PrivateInboxError("attachment_limit")
    seen = set()
    for item in attachments:
        if not isinstance(item, dict) or set(item) != ATTACHMENT_FIELDS: raise PrivateInboxError("invalid_attachment_fields")
        if (not matches(item["id"], IDENTIFIER) or item["id"] in seen or item["room"] != binding["room"]
                or not matches(item["sha256"], HASH) or type(item["size"]) is not int or not 0 <= item["size"] <= MAX_RESPONSE
                or type(item["created_at"]) is not int or type(item["expires_at"]) is not int
                or not 0 <= item["created_at"] <= 9223372036854775807
                or not (item["expires_at"] == 0 or item["created_at"] <= item["expires_at"] <= 9223372036854775807)
                or type(item["deleted"]) is not bool or type(item["expired"]) is not bool
                or not isinstance(item["filename"], str) or not isinstance(item["media_type"], str)
                or "\x00" in item["filename"] or "\x00" in item["media_type"]):
            raise PrivateInboxError("invalid_attachment_metadata")
        seen.add(item["id"])
    if event["type"] == "tombstone":
        if event["hidden"] is not True or any(event.get(f) for f in ("text", "signature", "signed_payload", "attachments")):
            raise PrivateInboxError("tombstone_contains_payload")
        return event
    if event["type"] != "message" or event["hidden"] or sha(event["text"].encode()) != event["sha256"]:
        raise PrivateInboxError("event_hash_or_state_mismatch")
    try:
        payload = event["signed_payload"].encode()
        envelope = strict_json(payload)
        if (not isinstance(envelope, dict) or set(envelope) != {"version", "service", "command"}
                or type(envelope["version"]) is not int or envelope["version"] != 1
                or envelope["service"] != binding["service_id"]): raise PrivateInboxError("signature_service_mismatch")
        command = envelope["command"]
        if not isinstance(command, dict) or set(command) - POST_FIELDS: raise PrivateInboxError("invalid_signed_post_fields")
        for field, value in command.items():
            if field == "timestamp": valid = type(value) is int and 0 < value <= 9223372036854775807
            elif field == "attachments": valid = isinstance(value, list) and all(isinstance(v, str) for v in value)
            else: valid = isinstance(value, str) and "\x00" not in value
            if not valid: raise PrivateInboxError("invalid_signed_post_fields")
        if payload != memo.canonical(command, binding["service_id"]) or command.get("operation") != "post":
            raise PrivateInboxError("noncanonical_signed_event")
        memo.crypto()[1].from_public_bytes(key).verify(memo.unb64(event["signature"]), payload)
        for field in ("room", "page", "kind", "text", "public_key", "reply_to", "to"):
            default = {"room": "lobby", "page": "main", "kind": "note"}.get(field, "")
            if (command.get(field) or default) != event.get(field, ""): raise PrivateInboxError("signed_event_field_mismatch")
        # A signed handle is a request; the event's handle is the key's registered one (protocol.md#handles).
        if command.get("visibility", "private") != "private": raise PrivateInboxError("signed_event_field_mismatch")
        if command.get("attachments", []) != [item["id"] for item in attachments]: raise PrivateInboxError("signed_attachment_mismatch")
    except PrivateInboxError: raise
    except Exception: raise PrivateInboxError("invalid_event_signature") from None
    return event


def immutable(event):
    result = {k: event.get(k, "") for k in ("id", "room", "page", "kind", "author", "public_key", "created_at", "sha256", "reply_to", "to")}
    # Preserve prior attachment identity across payload-free tombstones.
    if event["type"] != "tombstone":
        fields = ("id", "room", "sha256", "size", "created_at", "expires_at")
        result["attachments"] = [{k: item[k] for k in fields} for item in event.get("attachments", [])]
    return result


def metadata(event):
    result = immutable(event)
    result.update({k: event[k] for k in ("type", "visibility", "archive_eligible", "hidden")})
    fields = ("id", "room", "sha256", "size", "created_at", "expires_at", "deleted", "expired")
    if event["type"] != "tombstone":
        result["attachments"] = [{k: item[k] for k in fields} for item in event.get("attachments", [])]
    return result


def digest(event): return sha(encode(metadata(event)))


@contextmanager
def _cancellation_guard():
    """Defer caller signal semantics until all owned worker cleanup is complete."""
    if threading.current_thread() is not threading.main_thread():
        raise PrivateInboxError("worker_main_thread_required")
    signals = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)
    originals = {number: signal.getsignal(number) for number in signals}
    pending = set()

    def remember(number, _frame):
        pending.add(number)

    def check():
        if pending: raise PrivateInboxError("interrupted")

    initial_mask = signal.pthread_sigmask(signal.SIG_BLOCK, signals)
    try:
        for number in signals:
            # An ignored signal must neither cancel nor become observable.
            if originals[number] != signal.SIG_IGN:
                signal.signal(number, remember)
        signal.pthread_sigmask(signal.SIG_SETMASK, initial_mask)
        yield check
    finally:
        # No raising handler is installed until the caller's owned finally has
        # killed/reaped its child and closed IPC. Queue recorded signals under
        # the mask so default, ignored and custom semantics remain the caller's.
        signal.pthread_sigmask(signal.SIG_BLOCK, signals)
        try:
            # Another unmasked thread can deliver a process signal whose
            # Python handler runs here despite this thread's mask. An original
            # raising handler must not skip restoration of the remaining ones.
            try:
                signal.signal(signal.SIGINT, originals[signal.SIGINT])
            finally:
                try:
                    signal.signal(signal.SIGTERM, originals[signal.SIGTERM])
                finally:
                    signal.signal(signal.SIGHUP, originals[signal.SIGHUP])
        finally:
            try:
                for number in signals:
                    if number in pending:
                        signal.pthread_kill(threading.get_ident(), number)
            finally:
                signal.pthread_sigmask(signal.SIG_SETMASK, initial_mask)


def _http(request):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), memo.NoRedirect())
    try: response = opener.open(request, timeout=5)
    except urllib.error.HTTPError as error: response = error
    with response:
        if sum(len(k) + len(v) for k, v in response.headers.items()) > 65536: raise PrivateInboxError("header_limit")
        for name in ("Content-Length", "Transfer-Encoding", "Content-Encoding", "Content-Type"):
            if len(response.headers.get_all(name, [])) > 1: raise PrivateInboxError("ambiguous_response_framing")
        transfer = response.headers.get("Transfer-Encoding")
        if transfer is not None and (transfer.lower() != "chunked" or response.headers.get("Content-Length") is not None):
            raise PrivateInboxError("ambiguous_response_framing")
        if response.headers.get("Content-Encoding", "identity").lower() != "identity": raise PrivateInboxError("compressed_response")
        if response.headers.get_content_type() != "application/json": raise PrivateInboxError("response_content_type")
        length = response.headers.get("Content-Length")
        if length is not None and (not re.fullmatch(r"[0-9]{1,10}", length) or int(length) > MAX_RESPONSE):
            raise PrivateInboxError("response_byte_limit")
        chunks, size = [], 0
        while True:
            chunk = response.read1(min(65536, MAX_RESPONSE + 1 - size))
            if not chunk: break
            size += len(chunk)
            if size > MAX_RESPONSE: raise PrivateInboxError("response_byte_limit")
            chunks.append(chunk)
        if length is not None and int(length) != size: raise PrivateInboxError("truncated_response")
        return {"status": response.status, "body": base64.b64encode(b"".join(chunks)).decode("ascii")}


def _worker():
    try:
        raw = sys.stdin.buffer.read(16385)
        if len(raw) > 16384: raise PrivateInboxError("invalid_fetch_request")
        request = strict_json(raw)
        if not isinstance(request, dict) or set(request) != {"binding", "key_path", "action", "arguments"}:
            raise PrivateInboxError("invalid_fetch_request")
        binding = binding_validated(request["binding"])
        action, arguments = request["action"], request["arguments"]
        allowed = {"capabilities": set(), "room.get": set(), "messages.list": {"cursor", "limit"}, "message.get": {"message_id"}}
        if not isinstance(action, str) or action not in allowed or not isinstance(arguments, dict) or set(arguments) != allowed[action]:
            raise PrivateInboxError("invalid_fetch_request")
        if action == "messages.list" and (not isinstance(arguments["cursor"], str) or len(arguments["cursor"]) > 4096
                or type(arguments["limit"]) is not int or not 1 <= arguments["limit"] <= 100): raise PrivateInboxError("invalid_fetch_request")
        if binding["schema"] == 2 and action == "messages.list" and (arguments["limit"] != 10
                or not _private_cursor_fits(arguments["cursor"])):
            raise PrivateInboxError("invalid_fetch_request")
        if action == "message.get" and not matches(arguments["message_id"], IDENTIFIER): raise PrivateInboxError("invalid_fetch_request")
        headers = {"Accept": "application/json", "Accept-Encoding": "identity"}
        if action == "capabilities":
            req = urllib.request.Request(binding["origin"] + "/capabilities", headers=headers, method="GET")
        else:
            key = _load_key(request["key_path"], binding)
            command = {"operation": action, "room": binding["room"], **arguments}
            if binding["schema"] == 2: command["private_read"] = binding["private_read"]
            command = memo.sign(command, key, binding["service_id"])
            headers["Content-Type"] = "application/json"
            req = urllib.request.Request(binding["origin"] + "/v1/command", data=encode(command), headers=headers, method="POST")
        result = _http(req)
    except PrivateInboxError as error:
        result = {"error": error.code}
    except BaseException:
        result = {"error": "network_failed"}
    raw = encode(result)
    if len(raw) > MAX_IPC: raw = encode({"error": "response_byte_limit"})
    sys.stdout.buffer.write(raw)
    sys.stdout.buffer.flush()


class ReadSession:
    def __init__(self, binding, key_path, max_requests=10, deadline_seconds=30):
        if (sys.platform != "linux" or signal.getsignal(signal.SIGCHLD) != signal.SIG_DFL):
            raise PrivateInboxError("worker_platform_required")
        if threading.current_thread() is not threading.main_thread():
            raise PrivateInboxError("worker_main_thread_required")
        if (type(max_requests) is not int or not 1 <= max_requests <= 20
                or type(deadline_seconds) not in (int, float) or not math.isfinite(deadline_seconds)
                or not 0 < deadline_seconds <= 30): raise PrivateInboxError("poll_budget_limit")
        self.binding = binding_validated(binding)
        self.key_path = str(protected_path(key_path))
        self.max_requests = max_requests
        self.deadline = time.monotonic() + deadline_seconds
        self.requests = 0
        self._capabilities_checked = False

    def _fetch(self, action, arguments):
        with _cancellation_guard() as check_cancelled:
            result = self._fetch_owned(action, arguments, check_cancelled)
            check_cancelled()
            return result

    def _fetch_owned(self, action, arguments, check_cancelled):
        check_cancelled()
        remaining = self.deadline - time.monotonic() - 0.25
        if remaining <= 0: raise PrivateInboxError("deadline_exceeded")
        if self.requests >= self.max_requests: raise PrivateInboxError("request_budget")
        self.requests += 1
        request = encode({"binding": self.binding, "key_path": self.key_path, "action": action, "arguments": arguments})
        if len(request) > 16384: raise PrivateInboxError("invalid_fetch_request")
        env = {k: os.environ[k] for k in ("PATH", "LANG", "LC_ALL") if k in os.environ}
        worker, complete = None, False
        signals = {signal.SIGINT, signal.SIGTERM, signal.SIGHUP}
        try:
            # Pending termination is recorded only after the child object is
            # assigned under an established finally. The child inherits this
            # mask, but parent-death and cleanup use unblockable SIGKILL.
            oldmask = signal.pthread_sigmask(signal.SIG_BLOCK, signals)
            try:
                worker = subprocess.Popen([sys.executable, "-I", "-B", str(Path(__file__).resolve()), "_fetch", str(os.getpid())],
                                          stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                          close_fds=True, start_new_session=True, env=env)
            finally:
                signal.pthread_sigmask(signal.SIG_SETMASK, oldmask)
            os.set_blocking(worker.stdin.fileno(), False)
            os.set_blocking(worker.stdout.fileno(), False)
            output, sent = bytearray(), 0
            while True:
                check_cancelled()
                remaining = self.deadline - time.monotonic() - 0.25
                if remaining <= 0: raise PrivateInboxError("deadline_exceeded")
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
                    if len(output) > MAX_IPC: raise PrivateInboxError("response_byte_limit")
            raw, complete = bytes(output), True
        finally:
            if worker is not None:
                # Repeated terminal signals cannot interrupt owned cleanup.
                oldmask = signal.pthread_sigmask(signal.SIG_BLOCK, signals)
                try:
                    if complete:
                        try: worker.wait(timeout=max(0, min(0.05, self.deadline - time.monotonic())))
                        except subprocess.TimeoutExpired: pass
                    if worker.poll() is None: worker.kill()
                    try: worker.wait(timeout=max(0, self.deadline - time.monotonic()))
                    except subprocess.TimeoutExpired: raise PrivateInboxError("worker_cleanup_failed") from None
                finally:
                    try:
                        for stream in (worker.stdin, worker.stdout):
                            if stream is not None: stream.close()
                    finally: signal.pthread_sigmask(signal.SIG_SETMASK, oldmask)
        if time.monotonic() >= self.deadline: raise PrivateInboxError("deadline_exceeded")
        if len(raw) > MAX_IPC or worker.returncode != 0: raise PrivateInboxError("fetch_worker_failed")
        result = strict_json(raw)
        if not isinstance(result, dict): raise PrivateInboxError("fetch_worker_failed")
        if "error" in result:
            allowed = {"network_failed", "header_limit", "compressed_response", "response_content_type", "response_byte_limit",
                       "truncated_response", "invalid_fetch_request", "reader_key_mismatch", "private_directory_required",
                       "private_regular_file_required", "private_path_required", "symlink_not_allowed", "key_file_limit",
                       "ambiguous_response_framing", "private_path_unavailable"}
            raise PrivateInboxError(result["error"] if isinstance(result["error"], str) and result["error"] in allowed else "fetch_worker_failed")
        if set(result) != {"status", "body"} or type(result["status"]) is not int: raise PrivateInboxError("fetch_worker_failed")
        try: body = base64.b64decode(result["body"], validate=True)
        except Exception: raise PrivateInboxError("fetch_worker_failed") from None
        if len(body) > MAX_RESPONSE: raise PrivateInboxError("response_byte_limit")
        parsed = strict_json(body)
        if not isinstance(parsed, dict): raise PrivateInboxError("invalid_response")
        status = result["status"]
        if status != 200:
            error = parsed.get("error")
            if not isinstance(error, dict) or not isinstance(error.get("code"), str): raise PrivateInboxError("invalid_error_response")
            if status == 404 and error["code"] == "not_found": return None
            if status in (401, 403): raise PrivateInboxError("reauth_required")
            if status == 409 and error["code"] == "cursor_reset": raise PrivateInboxError("cursor_reset")
            raise PrivateInboxError("source_http_error")
        return parsed

    def capabilities(self):
        result = self._fetch("capabilities", {})
        if (not isinstance(result, dict) or result.get("service_id") != self.binding["service_id"]
                or not isinstance(result.get("private_reads"), dict)
                or result["private_reads"].get("message_get_room_filter") is not True
                or not isinstance(result.get("public_corrections"), dict)
                or result["public_corrections"].get("message_read_generation") is not True):
            raise PrivateInboxError("private_read_capability_required")
        if self.binding["schema"] == 2:
            descriptor = result.get("private_read_grants")
            expected = {"schema": 1, "canonical_version": 3, "context_field": "private_read",
                        "room_visibility": "private", "issuer": "room_owner",
                        "operations": ["room.get", "messages.list", "message.get"],
                        "transport": "https_json_post", "endpoint": "/v1/command",
                        "room_response": "data.private_room", "message_generation": True,
                        "private_mcp": False, "attachment_downloads": False}
            if (not isinstance(descriptor, dict)
                    or any(type(descriptor.get(k)) is not type(v) or descriptor[k] != v for k, v in expected.items())
                    or not isinstance(result.get("canonical_versions"), list)
                    or not any(type(v) is int and v == 3 for v in result["canonical_versions"])
                    or not isinstance(result.get("command_fields"), list)
                    or result["command_fields"][-1:] != ["private_read"]):
                raise PrivateInboxError("private_read_capability_required")
        self._capabilities_checked = True
        return {"service_id": self.binding["service_id"], "message_get_room_filter": True}

    def _check_capability(self):
        if not self._capabilities_checked: raise PrivateInboxError("capability_check_required")

    def room(self):
        self._check_capability()
        result = self._fetch("room.get", {})
        if result is None: raise PrivateInboxError("scope_unavailable")
        if self.binding["schema"] == 2:
            if (set(result) != {"ok", "data"} or result["ok"] is not True
                    or not isinstance(result["data"], dict) or set(result["data"]) != {"private_room"}
                    or not isinstance(result["data"]["private_room"], dict)
                    or set(result["data"]["private_room"]) != {"name", "visibility"}):
                raise PrivateInboxError("invalid_room_response")
            room = result["data"]["private_room"]
            if room != {"name": self.binding["room"], "visibility": "private"}:
                raise PrivateInboxError("scope_unavailable")
            return dict(room)
        if set(result) != {"ok", "room"} or result["ok"] is not True or not isinstance(result["room"], dict):
            raise PrivateInboxError("invalid_room_response")
        room = result["room"]
        if room.get("name") != self.binding["room"] or room.get("visibility") != "private":
            raise PrivateInboxError("scope_unavailable")
        # Never expose/persist membership lists or room activity metadata.
        return {"name": self.binding["room"], "visibility": "private"}

    def _events(self, result, *, message_id=None):
        if result is None:
            if message_id is not None: return None
            raise PrivateInboxError("scope_unavailable")
        # data.has_more is the only data the message reads carry (PROTOCOL.md).
        if (set(result) - {"ok", "messages", "next_cursor", "generation", "data"} or result.get("ok") is not True
                or not matches(result.get("generation"), GENERATION)
                or ("data" in result and (not isinstance(result["data"], dict) or set(result["data"]) != {"has_more"}
                                          or type(result["data"]["has_more"]) is not bool))):
            raise PrivateInboxError("invalid_event_response")
        events, cursor = result.get("messages", []), result.get("next_cursor", "")
        if (not isinstance(events, list) or len(events) > (1 if message_id is not None else 100)
                or (message_id is not None and len(events) != 1) or not isinstance(cursor, str)
                or len(cursor) > 4096 or not cursor): raise PrivateInboxError("invalid_event_response")
        if self.binding["schema"] == 2 and not _private_cursor_fits(cursor):
            raise PrivateInboxError("invalid_event_response")
        ids, last_sequence = set(), 0
        for event in events:
            validate_private_event(event, self.binding)
            if event["id"] in ids or event["sequence"] <= last_sequence: raise PrivateInboxError("event_order_mismatch")
            if message_id is not None and event["id"] != message_id: raise PrivateInboxError("message_identifier_mismatch")
            ids.add(event["id"]); last_sequence = event["sequence"]
        if time.monotonic() >= self.deadline: raise PrivateInboxError("deadline_exceeded")
        return {"messages": events, "next_cursor": cursor, "generation": result["generation"]}

    def messages(self, cursor, limit=None):
        self._check_capability()
        if limit is None: limit = 10 if self.binding["schema"] == 2 else 100
        if self.binding["schema"] == 2 and (type(limit) is not int or limit != 10 or not _private_cursor_fits(cursor)):
            raise PrivateInboxError("invalid_event_cursor_or_limit")
        if not isinstance(cursor, str) or len(cursor) > 4096 or type(limit) is not int or not 1 <= limit <= 100:
            raise PrivateInboxError("invalid_event_cursor_or_limit")
        result = self._events(self._fetch("messages.list", {"cursor": cursor, "limit": limit}))
        if len(result["messages"]) > limit: raise PrivateInboxError("event_page_limit")
        return result

    def event(self, message_id):
        self._check_capability()
        if not matches(message_id, IDENTIFIER): raise PrivateInboxError("invalid_message_identifier")
        return self._events(self._fetch("message.get", {"message_id": message_id}), message_id=message_id)


if __name__ == "__main__":
    try: _worker()
    except BaseException: sys.exit(1)
