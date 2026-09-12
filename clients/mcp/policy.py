"""Offline immutable policy and durable binding for the optional local bridge."""
from __future__ import annotations

from dataclasses import dataclass
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import stat
from urllib.parse import urlsplit

SAFE_CODES = frozenset("bridge_error invalid_profile unsafe_path invalid_origin invalid_key binding_mismatch binding_missing invalid_arguments tool_unavailable operation_forbidden generation_mismatch intent_too_large response_too_large invalid_response scope_mismatch public_not_found transport_error unknown_intent intent_id_conflict intent_digest_mismatch outbox_not_head outbox_busy queue_item_limit queue_byte_limit invalid_acknowledgement delegation_inactive delegation_context_mismatch delegation_forbidden delegation_scope_mismatch delegation_generation_mismatch delegation_quota_exhausted stale_signature quota_exceeded cursor_reset invalid_cursor work_conflict work_not_found blocked".split())


class BridgeError(ValueError):
    def __init__(self, code):
        self.code = code if code in SAFE_CODES else "bridge_error"
        super().__init__(self.code)


def encoded(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")


def strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result: raise BridgeError("invalid_profile")
            result[key] = value
        return result
    try:
        return json.loads(raw, object_pairs_hook=pairs, parse_constant=lambda _: (_ for _ in ()).throw(BridgeError("invalid_profile")))
    except (ValueError, UnicodeError, TypeError):
        raise BridgeError("invalid_profile") from None


def protected_path(raw, *, directory=False):
    if not isinstance(raw, str) or not raw.startswith("/") or "\x00" in raw:
        raise BridgeError("unsafe_path")
    path = Path(raw)
    if str(path) != raw or ".." in path.parts:
        raise BridgeError("unsafe_path")
    # A protected final component must not resolve through an ancestor symlink.
    try:
        for part in (path, *path.parents):
            if stat.S_ISLNK(part.lstat().st_mode): raise BridgeError("unsafe_path")
        info = path.stat()
        valid_type = stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)
        if not valid_type or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise BridgeError("unsafe_path")
    except OSError: raise BridgeError("unsafe_path") from None
    return path


def read_private(path, limit=32768):
    protected_path(str(path))
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                raise BridgeError("unsafe_path")
            raw = stream.read(limit + 1)
        if len(raw) > limit: raise BridgeError("invalid_profile")
        return raw
    except OSError: raise BridgeError("unsafe_path") from None


@dataclass(frozen=True)
class Profile:
    fingerprint: str
    mode: str
    origin: str
    service_id: str
    room: str
    public_key: str
    grant_id: str
    generation: str
    operations: tuple[str, ...]
    key_path: Path | None
    state_dir: Path

    @property
    def delegation(self):
        return {"schema": 1, "grant_id": self.grant_id, "generation": self.generation}


def load_profile(path) -> Profile:
    value = strict_json(read_private(path))
    required = {"schema", "origin", "service_id", "public_key", "room", "delegation", "operations", "state_dir"}
    if not isinstance(value, dict) or not required <= value.keys() or value.keys() - required - {"mode", "key_path"} or any(v is None for v in value.values()):
        raise BridgeError("invalid_profile")
    if type(value["schema"]) is not int or value["schema"] != 1: raise BridgeError("invalid_profile")
    mode = value.get("mode", "draft")
    if mode not in ("draft", "scoped-send") or ("key_path" in value) != (mode == "scoped-send"):
        raise BridgeError("invalid_profile")
    origin = value["origin"]
    try:
        if not isinstance(origin, str) or any(ord(c) <= 32 or ord(c) >= 127 for c in origin): raise ValueError()
        u = urlsplit(origin)
        host = u.hostname
        if u.scheme not in ("https", "http") or not host or u.username is not None or u.password is not None or u.path or u.query or u.fragment: raise ValueError()
        port = u.port
        if port is not None and not 1 <= port <= 65535: raise ValueError()
        if host != "::1" and not re.fullmatch(r"[a-z0-9][a-z0-9.-]*", host): raise ValueError()
        authority = ("[::1]" if host == "::1" else host) + (":" + str(port) if port is not None else "")
        if origin != u.scheme + "://" + authority: raise ValueError()
        if u.scheme == "http" and host not in ("127.0.0.1", "localhost", "::1"): raise ValueError()
    except (ValueError, TypeError): raise BridgeError("invalid_origin") from None
    for key, pattern in (("room", r"[a-z0-9][a-z0-9_-]{0,63}"), ("service_id", r"[A-Za-z0-9.-]{1,128}")):
        if not isinstance(value[key], str) or not re.fullmatch(pattern, value[key]): raise BridgeError("invalid_profile")
    try:
        public = value["public_key"]
        if not isinstance(public, str) or not re.fullmatch(r"[A-Za-z0-9_-]{43}", public): raise ValueError()
        key = base64.urlsafe_b64decode(public + "=")
        if len(key) != 32 or base64.urlsafe_b64encode(key).decode().rstrip("=") != public: raise ValueError()
    except (ValueError, TypeError): raise BridgeError("invalid_key") from None
    d = value["delegation"]
    if (not isinstance(d, dict) or set(d) != {"schema", "grant_id", "generation"}
            or type(d["schema"]) is not int or d["schema"] != 1
            or d["grant_id"] != hashlib.sha256(key).hexdigest()
            or not isinstance(d["generation"], str) or not re.fullmatch(r"[a-f0-9]{32}", d["generation"])):
        raise BridgeError("invalid_profile")
    operations = value["operations"]
    if (not isinstance(operations, list) or not 1 <= len(operations) <= 4
            or any(not isinstance(op, str) or op not in ("post", "work.claim", "work.renew", "work.submit") for op in operations)
            or len(set(operations)) != len(operations)):
        raise BridgeError("invalid_profile")
    state = protected_path(value["state_dir"], directory=True)
    key_path = protected_path(value["key_path"]) if mode == "scoped-send" else None
    repository = Path(__file__).resolve().parents[2]
    if state.is_relative_to(repository) or (key_path and key_path.is_relative_to(repository)):
        raise BridgeError("unsafe_path")
    normalized = {**value, "mode": mode, "operations": sorted(operations)}
    fingerprint = hashlib.sha256(encoded(normalized)).hexdigest()
    profile = Profile(fingerprint, mode, origin, value["service_id"], value["room"], public,
                      d["grant_id"], d["generation"], tuple(sorted(operations)), key_path, state)
    check_binding(profile)
    return profile


def check_binding(profile, *, create=False):
    protected_path(str(profile.state_dir), directory=True)
    path = profile.state_dir / "mcp-policy.json"
    expected = encoded({"schema": 1, "fingerprint": profile.fingerprint})
    if path.exists() or path.is_symlink():
        if read_private(path) != expected: raise BridgeError("binding_mismatch")
        if create: _sync_binding(path, profile.state_dir)
        return
    # Never adopt a surviving queue without its durable policy authority.
    if any(profile.state_dir.glob("mcp-outbox.sqlite*")): raise BridgeError("binding_missing")
    if not create: return
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    except FileExistsError:
        return check_binding(profile, create=create)
    except OSError: raise BridgeError("unsafe_path") from None
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(expected); stream.flush(); os.fsync(stream.fileno())
        _sync_binding(path, profile.state_dir)
    except OSError: raise BridgeError("unsafe_path") from None


def _sync_binding(path, state_dir):
    # A surviving complete file may follow an earlier failed fsync. Revalidate
    # and synchronize both inode and directory before any explicit enqueue.
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        try: os.fsync(fd)
        finally: os.close(fd)
        directory = os.open(state_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try: os.fsync(directory)
        finally: os.close(directory)
    except OSError: raise BridgeError("unsafe_path") from None
