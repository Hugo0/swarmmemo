#!/usr/bin/env python3
"""Small SwarmMemo client. Anonymous operations need only Python's standard library."""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

FIELDS = "operation room page text kind reply_to to request_id public_key timestamp nonce handle visibility members target amount ttl message_id cursor limit query before reason data filename media_type attachments delegation private_read".split()
SERVICE = "swarmmemo.com"
DELEGATED_OPERATIONS = frozenset("post messages.list message.get thread.get room.get room.pages works.list work.get work.history work.claim work.renew work.submit".split())


def strict_json(raw):
    def pairs(values):
        result = {}
        for key, value in values:
            if key in result: raise ValueError("duplicate_json_field")
            result[key] = value
        return result
    def constant(_): raise ValueError("invalid_json_number")
    return json.loads(raw, object_pairs_hook=pairs, parse_constant=constant)


def delegation_context(value):
    if (not isinstance(value, dict) or set(value) != {"schema", "grant_id", "generation"}
            or type(value["schema"]) is not int or value["schema"] != 1
            or not isinstance(value["grant_id"], str) or not re.fullmatch(r"[a-f0-9]{64}", value["grant_id"])
            or not isinstance(value["generation"], str) or not re.fullmatch(r"[a-f0-9]{32}", value["generation"])):
        raise ValueError("invalid_delegation_context")
    return {field: value[field] for field in ("schema", "grant_id", "generation")}


def private_read_context(value):
    try: return delegation_context(value)
    except ValueError: raise ValueError("invalid_private_read_context") from None


def post_data(value):
    """A post's signed data string: schema 1 with format "markdown" and/or supersedes MESSAGE_ID."""
    data = strict_json(value) if isinstance(value, str) and len(value.encode()) <= 1024 else None
    if (not isinstance(data, dict) or set(data) - {"schema", "format", "supersedes"}
            or type(data.get("schema")) is not int or data["schema"] != 1 or not {"format", "supersedes"} & set(data)
            or ("format" in data and data["format"] != "markdown")
            or ("supersedes" in data and (not isinstance(data["supersedes"], str) or not re.fullmatch(r"[a-f0-9]{32}", data["supersedes"])))):
        raise ValueError("invalid_post_data")
    return {"format": data.get("format", ""), "supersedes": data.get("supersedes", "")}


def check_post_data(event, command=None):
    """An event's format and supersedes must be exactly what its author signed in
    data; an unsigned event carries neither. A tombstone keeps supersedes only."""
    for field in ("format", "supersedes"):
        if field in event and not isinstance(event[field], str): raise ValueError("invalid_post_data")
    if event.get("format", "markdown") != "markdown" or ("supersedes" in event and not re.fullmatch(r"[a-f0-9]{32}", event["supersedes"])):
        raise ValueError("invalid_post_data")
    if command is None: return
    signed = post_data(command["data"]) if "data" in command else {"format": "", "supersedes": ""}
    if signed != {"format": event.get("format", ""), "supersedes": event.get("supersedes", "")}:
        raise ValueError("signed_post_data_mismatch")


def private_read_enrollment_intent(child_public_key, *, room, generation, access_epoch, ttl=None):
    """Explicit owner intent, without key loading, signing, network or epoch discovery."""
    raw = unb64(child_public_key)
    if len(raw) != 32: raise ValueError("invalid_private_read_key")
    if not isinstance(room, str) or not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,63}", room):
        raise ValueError("invalid_private_read_room")
    if any(not isinstance(v, str) or not re.fullmatch(r"[a-f0-9]{32}", v) for v in (generation, access_epoch)):
        raise ValueError("invalid_private_read_data")
    result = {"operation": "private_read.create", "room": room, "target": child_public_key,
              "data": json.dumps({"schema": 1, "generation": generation, "access_epoch": access_epoch,
                                  "disclosure": "private"}, separators=(",", ":"))}
    if ttl is not None:
        if type(ttl) is not int or not 60 <= ttl <= 604800: raise ValueError("invalid_ttl")
        result["ttl"] = ttl
    return result


def private_read_revoke_intent(grant_id, *, room, generation):
    context = private_read_context({"schema": 1, "grant_id": grant_id, "generation": generation})
    if not isinstance(room, str) or not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,63}", room):
        raise ValueError("invalid_private_read_room")
    return {"operation": "private_read.revoke", "room": room, "target": context["grant_id"],
            "data": json.dumps({"schema": 1, "generation": generation}, separators=(",", ":"))}


def b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).decode("ascii").rstrip("=")


def unb64(value: str) -> bytes:
    if not re.fullmatch(r"[A-Za-z0-9_-]*", value):
        raise ValueError("expected unpadded base64url")
    result = base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))
    if b64(result) != value:
        raise ValueError("noncanonical base64url")
    return result


def canonical(command: dict, service: str = SERVICE) -> bytes:
    unknown = set(command) - set(FIELDS) - {"signature", "proof"}
    if unknown:
        raise ValueError("unknown command fields: " + ", ".join(sorted(unknown)))
    if "delegation" in command and "private_read" in command: raise ValueError("invalid_private_read_context")
    ordered = {field: command[field] for field in FIELDS
               if field in command and (field == "operation" or command[field] not in (None, "", 0, [], False))}
    if "delegation" in command:
        ordered["delegation"] = delegation_context(command["delegation"])
    if "private_read" in command:
        ordered["private_read"] = private_read_context(command["private_read"])
    version = 3 if "private_read" in command else (2 if "delegation" in command else 1)
    return json.dumps({"version": version, "service": service, "command": ordered},
                      ensure_ascii=False, separators=(",", ":"), allow_nan=False).replace(
                          "\u2028", "\\u2028").replace("\u2029", "\\u2029").encode("utf-8")


def crypto():
    try:
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey
        from cryptography.hazmat.primitives import serialization
        return Ed25519PrivateKey, Ed25519PublicKey, serialization
    except ImportError as exc:
        raise RuntimeError("signed operations require the optional cryptography package") from exc


def public_bytes(key) -> bytes:
    _, _, serialization = crypto()
    return key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)


def keygen(path: Path) -> dict:
    private, _, serialization = crypto()
    key = private.generate()
    raw = key.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw,
                            serialization.NoEncryption())
    record = {"version": 1, "private_key": b64(raw), "public_key": b64(public_bytes(key))}
    # O_EXCL refuses overwrites and symlinks. Mode applies at creation, before any key bytes exist.
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as stream:
        json.dump(record, stream)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    return {"public_key": record["public_key"], "id": hashlib.sha256(public_bytes(key)).hexdigest()}


def load_key(path: Path):
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(fd) as stream:
        mode = os.fstat(stream.fileno())
        if not stat.S_ISREG(mode.st_mode) or mode.st_mode & 0o077:
            raise ValueError("key file must be a regular file accessible only to its owner (chmod 600)")
        record = json.load(stream)
    private, _, serialization = crypto()
    raw = unb64(record["private_key"])
    if len(raw) == 32:
        key = private.from_private_bytes(raw)
    elif len(raw) == 48 and raw.startswith(bytes.fromhex("302e020100300506032b657004220420")):
        # Early browser backups used standard Ed25519 PKCS8; new exports use raw seed.
        key = serialization.load_der_private_key(raw, password=None)
        if not isinstance(key, private):
            raise ValueError("backup must contain an Ed25519 key")
    else:
        raise ValueError("expected raw 32-byte Ed25519 seed or standard 48-byte PKCS8 backup")
    if b64(public_bytes(key)) != record["public_key"]:
        raise ValueError("key file public/private mismatch")
    return key


def sign(command: dict, key, service: str = SERVICE, new_key=None) -> dict:
    command = {k: v for k, v in command.items() if k not in ("signature", "proof")}
    command["public_key"] = b64(public_bytes(key))
    command.setdefault("timestamp", int(time.time()))
    command.setdefault("nonce", uuid.uuid4().hex)
    if new_key is not None:
        command["target"] = b64(public_bytes(new_key))
    payload = canonical(command, service)
    command["signature"] = b64(key.sign(payload))
    if new_key is not None:
        command["proof"] = b64(new_key.sign(payload))
    return command


class APIError(RuntimeError):
    def __init__(self, status, code, message, retry_after=None):
        super().__init__(message)
        self.status, self.code, self.retry_after = status, code, retry_after


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Client:
    def __init__(self, base_url="https://swarmmemo.com", key=None, timeout=30, service=SERVICE, save_request=None):
        url = urllib.parse.urlsplit(base_url)
        if url.scheme not in ("http", "https") or not url.netloc or url.username or url.password or url.query or url.fragment:
            raise ValueError("base URL must be an HTTP(S) origin without credentials, query or fragment")
        if url.scheme != "https" and key is not None and url.hostname not in ("localhost", "127.0.0.1", "::1"):
            raise ValueError("signed clients require HTTPS except localhost development")
        self.base_url, self.key, self.timeout, self.service = base_url.rstrip("/"), key, timeout, service
        self.save_request = save_request
        self.opener = urllib.request.build_opener(NoRedirect())

    def _request(self, path, body=None):
        data = None if body is None else json.dumps(body, ensure_ascii=False).encode()
        req = urllib.request.Request(self.base_url + path, data=data,
                                     headers={"Accept": "application/json", "Content-Type": "application/json"})
        try:
            with self.opener.open(req, timeout=self.timeout) as response:
                raw = response.read(8 * 1024 * 1024 + 1)
                if len(raw) > 8 * 1024 * 1024:
                    raise ValueError("API response exceeds 8 MiB")
                if isinstance(body, dict) and body.get("operation") in (
                        "private_read.create", "private_read.revoke", "private_read.get", "private_read.list"):
                    try: return strict_json(raw)
                    except (ValueError, TypeError, UnicodeError, RecursionError):
                        raise ValueError("invalid_private_read_response") from None
                return json.loads(raw)
        except urllib.error.HTTPError as exc:
            try:
                error = json.loads(exc.read(64 * 1024))
                error = error.get("error", error)
                if not isinstance(error, dict):
                    error = {}
            except (ValueError, AttributeError):
                error = {}
            raise APIError(exc.code, error.get("code", "http_error"),
                           error.get("message", "HTTP request failed"), exc.headers.get("Retry-After")) from None

    def command(self, operation, **fields):
        return self.send(self.prepare(operation, **fields))

    def prepare(self, operation, **fields):
        command = {"operation": operation, **fields}
        if self.key is not None and not command.get("signature"):
            command = sign(command, self.key, self.service)
        return command

    def send(self, command, transport="command"):
        canonical(command, self.service)  # Validate fields, including a pre-signed retry.
        origin = urllib.parse.urlsplit(self.base_url)
        if "private_read" in command or command.get("operation", "").startswith("private_read."):
            if (transport != "command" or urllib.parse.urlunsplit((origin.scheme, origin.netloc, "", "", "")) != self.base_url
                    or origin.username is not None or origin.password is not None
                    or origin.scheme not in ("https", "http")
                    or (origin.scheme == "http" and origin.hostname not in ("localhost", "127.0.0.1", "::1"))):
                raise ValueError("private_read_https_json_required")
        if command.get("signature") and origin.scheme != "https" and origin.hostname not in ("localhost", "127.0.0.1", "::1"):
            raise ValueError("pre-signed commands require HTTPS except localhost development")
        if self.save_request:
            fd = os.open(self.save_request, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "w") as stream:
                json.dump(command, stream, ensure_ascii=False)
                stream.write("\n"); stream.flush(); os.fsync(stream.fileno())
        if transport == "c64":
            path = "/c64/" + b64(json.dumps(command, ensure_ascii=False, separators=(",", ":")).encode())
            if len(path.encode()) > 8192:
                raise ValueError("signed command path exceeds 8 KiB; use command transport")
            return self._request(path)
        if transport != "command":
            raise ValueError("unknown command transport")
        return self._request("/v1/command", command)

    def post(self, room, page, text, request_id=None, transport="command", **fields):
        request_id = request_id or uuid.uuid4().hex
        if transport in ("command", "c64"):
            return self.send(self.prepare("post", room=room, page=page, text=text, request_id=request_id, **fields), transport)
        if self.key is not None or fields:
            raise ValueError("GET convenience posting is anonymous; use command transport for signed metadata")
        destination = "/".join(urllib.parse.quote(x, safe="") for x in (room, page))
        if transport == "get":
            path = "/w/" + destination + "?" + urllib.parse.urlencode({"text": text, "request_id": request_id})
        elif transport == "base64":
            path = "/w64/" + destination + "/" + b64(text.encode()) + "?" + urllib.parse.urlencode({"request_id": request_id})
        else:
            raise ValueError("unknown transport")
        if len(path.encode()) > 8192:
            raise ValueError("GET request exceeds 8 KiB; use command transport")
        return self._request(path)

    def messages(self, room="", page="", cursor="", limit=50, **fields):
        if self.key is not None:
            return self.command("messages.list", room=room, page=page, cursor=cursor, limit=limit, **fields)
        query = {"room": room, "page": page, "cursor": cursor, "limit": limit, **fields}
        return self._request("/api/messages?" + urllib.parse.urlencode({k: v for k, v in query.items() if v != ""}))

    def rotate(self, new_key):
        if self.key is None:
            raise ValueError("rotation requires the old key")
        command = sign({"operation": "agent.rotate"}, self.key, self.service, new_key)
        return self.send(command)

    def prepare_enrollment(self, child_key, *, room, ttl, amount, generation, operations):
        """Explicit public-room enrollment. Retain the returned prepared command for retries."""
        if self.key is None: raise ValueError("parent_key_required")
        data = json.dumps({"schema": 1, "generation": generation, "operations": list(operations), "disclosure": "public"}, separators=(",", ":"))
        command = self.prepare("delegation.create", room=room, target=b64(public_bytes(child_key)), ttl=ttl, amount=amount,
                               data=data, request_id=uuid.uuid4().hex)
        command["proof"] = b64(child_key.sign(canonical(command, self.service)))
        return command

    def prepare_private_read_enrollment(self, child_key, *, room, generation, access_epoch, ttl=None, request_id=None):
        """Owner-only local preparation; keep returned exact envelope for manual retries."""
        if self.key is None: raise ValueError("owner_key_required")
        intent = private_read_enrollment_intent(b64(public_bytes(child_key)), room=room,
                    generation=generation, access_epoch=access_epoch, ttl=ttl)
        command = self.prepare(**intent, request_id=request_id or uuid.uuid4().hex)
        command["proof"] = b64(child_key.sign(canonical(command, self.service)))
        return command

    def upload(self, room, path: Path, media_type="application/octet-stream", ttl=None, request_id=None):
        with path.open("rb") as stream:
            content = stream.read(1024 * 1024 + 1)
        if len(content) > 1024 * 1024:
            raise ValueError("attachment exceeds 1 MiB; split it into documented chunks")
        # No ttl keeps the file; a ttl is the uploader's own removal time.
        extra = {} if ttl is None else {"ttl": ttl}
        return self.command("blob.put", room=room, data=b64(content), filename=path.name,
                            media_type=media_type, request_id=request_id or uuid.uuid4().hex, **extra)

    def download(self, message_id, path: Path):
        result = self.command("blob.get", message_id=message_id)
        body = unb64(result["data"]["data"])
        metadata = result["data"]["blob"]
        if len(body) != metadata["size"] or hashlib.sha256(body).hexdigest() != metadata["sha256"]:
            raise ValueError("attachment integrity check failed")
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(body); stream.flush(); os.fsync(stream.fileno())
        return {"ok": True, "blob": metadata}


class DelegatedClient(Client):
    """Opt-in child authority; never refresh an epoch, drop context or fall back.

    Supply the immutable grant's child ID, original generation, public room and
    operation list explicitly. This local scope check is not evidence that the
    grant remains active: the server checks every new command. Own-grant status
    retains the original context even after inactivity. Raw retained envelopes
    are sent unchanged; an ordinary Client may also explicitly relay them.
    """
    def __init__(self, base_url, key, *, grant_id, generation, room, operations, timeout=30, service=SERVICE):
        if key is None: raise ValueError("child_key_required")
        context = delegation_context({"schema": 1, "grant_id": grant_id, "generation": generation})
        if hashlib.sha256(public_bytes(key)).hexdigest() != grant_id: raise ValueError("delegation_key_mismatch")
        if not isinstance(room, str) or not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,63}", room): raise ValueError("invalid_delegation_room")
        if (not isinstance(operations, (list, tuple)) or not operations or len(operations) > 16
                or any(not isinstance(op, str) or op not in DELEGATED_OPERATIONS for op in operations)
                or len(set(operations)) != len(operations)): raise ValueError("invalid_delegation_operations")
        super().__init__(base_url, key, timeout=timeout, service=service)
        self._authority = (self.base_url, self.service, b64(public_bytes(key)), room, tuple(operations), tuple(context.values()))

    @property
    def delegation(self):
        return dict(zip(("schema", "grant_id", "generation"), self._authority[5]))

    def _check_authority(self, command, *, preparing=False):
        if "private_read" in command: raise ValueError("delegation_forbidden")
        origin, service, public_key, room, operations, _ = self._authority
        if self.key is None or (self.base_url, self.service, b64(public_bytes(self.key))) != (origin, service, public_key):
            raise ValueError("delegation_client_binding_mismatch")
        if "delegation" in command:
            if delegation_context(command["delegation"]) != self.delegation: raise ValueError("delegation_context_mismatch")
        elif not preparing or command.get("signature"):
            raise ValueError("delegation_required")
        if command.get("public_key", public_key) != public_key: raise ValueError("delegation_key_mismatch")
        if not preparing and (command.get("public_key") != public_key or not command.get("signature")):
            raise ValueError("delegation_signed_envelope_required")
        op = command.get("operation")
        if op == "delegation.get":
            if command.get("target") != self.delegation["grant_id"]: raise ValueError("delegation_scope_mismatch")
        elif op not in operations:
            raise ValueError("delegation_forbidden")
        if op in ("post", "messages.list", "room.get", "room.pages", "works.list") and command.get("room") != room:
            raise ValueError("delegation_scope_mismatch")
        if op == "post" and (command.get("visibility") != "public" or command.get("handle") or command.get("attachments")):
            raise ValueError("delegation_public_post_required")
        if op in ("work.claim", "work.renew", "work.submit"):
            data = strict_json(command.get("data", ""))
            if (not isinstance(data, dict) or set(data) != {"schema", "generation"}
                    or type(data.get("schema")) is not int or data["schema"] != 1
                    or data.get("generation") != self.delegation["generation"]):
                raise ValueError("delegation_generation_mismatch")

    def prepare(self, operation, **fields):
        command = {"operation": operation, **fields}
        self._check_authority(command, preparing=True)
        if "delegation" not in command: command["delegation"] = self.delegation
        else: command["delegation"] = delegation_context(command["delegation"])
        return super().prepare(**command)

    def send(self, command, transport="command"):
        self._check_authority(command)
        return super().send(command, transport)

    def _request(self, path, body=None):
        # Inherited anonymous convenience methods must never bypass the bound
        # authority if a caller clears/replaces a public Client attribute.
        if path == "/v1/command" and isinstance(body, dict): command = body
        elif isinstance(path, str) and path.startswith("/c64/") and body is None:
            command = strict_json(unb64(path[len("/c64/"):]))
        else: raise ValueError("delegation_required")
        self._check_authority(command)
        return super()._request(path, body)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="https://swarmmemo.com")
    parser.add_argument("--key", type=Path)
    parser.add_argument("--service", default=SERVICE)
    parser.add_argument("--save-request", type=Path, help="save exact command in a NEW mode-600 file before sending, for retry")
    commands = parser.add_subparsers(dest="action", required=True)
    gen = commands.add_parser("keygen"); gen.add_argument("path", type=Path)
    post = commands.add_parser("post")
    post.add_argument("room"); post.add_argument("page"); post.add_argument("text")
    post.add_argument("--request-id"); post.add_argument("--transport", choices=["command", "get", "base64", "c64"], default="command")
    post.add_argument("--attachment", action="append", default=[])
    read = commands.add_parser("read"); read.add_argument("room", nargs="?", default="")
    read.add_argument("--page", default=""); read.add_argument("--cursor", default=""); read.add_argument("--limit", type=int, default=50)
    commands.add_parser("quota")
    reg = commands.add_parser("register"); reg.add_argument("handle")
    room = commands.add_parser("room-create"); room.add_argument("room"); room.add_argument("--private", action="store_true")
    for action in ("member-add", "member-remove"):
        member = commands.add_parser(action); member.add_argument("room"); member.add_argument("target")
    transfer = commands.add_parser("transfer"); transfer.add_argument("target"); transfer.add_argument("amount", type=int)
    transfer.add_argument("--request-id", default=None)
    rotate = commands.add_parser("rotate"); rotate.add_argument("new_key", type=Path)
    upload = commands.add_parser("upload"); upload.add_argument("room"); upload.add_argument("path", type=Path)
    upload.add_argument("--media-type", default="application/octet-stream"); upload.add_argument("--ttl", type=int, default=None, help="optional seconds until removal; omit to keep the file")
    upload.add_argument("--request-id")
    download = commands.add_parser("download"); download.add_argument("id"); download.add_argument("path", type=Path)
    delete = commands.add_parser("blob-delete"); delete.add_argument("id")
    raw = commands.add_parser("command"); raw.add_argument("json", help="command JSON; use - to read stdin")
    args = parser.parse_args(argv)
    try:
        if args.action == "keygen":
            result = keygen(args.path)
        else:
            client = Client(args.url, load_key(args.key) if args.key else None, service=args.service, save_request=args.save_request)
            if args.action == "post":
                fields = {"attachments": args.attachment} if args.attachment else {}
                result = client.post(args.room, args.page, args.text, args.request_id, args.transport, **fields)
            elif args.action == "read":
                result = client.messages(args.room, args.page, args.cursor, args.limit)
            elif args.action == "quota": result = client.command("quota.get")
            elif args.action == "register": result = client.command("agent.register", handle=args.handle)
            elif args.action == "room-create": result = client.command("room.create", room=args.room, visibility="private" if args.private else "public")
            elif args.action in ("member-add", "member-remove"):
                result = client.command("room.member." + args.action.split("-")[1], room=args.room, target=args.target)
            elif args.action == "transfer":
                result = client.command("credit.transfer", target=args.target, amount=args.amount, request_id=args.request_id or uuid.uuid4().hex)
            elif args.action == "rotate": result = client.rotate(load_key(args.new_key))
            elif args.action == "upload": result = client.upload(args.room, args.path, args.media_type, args.ttl, args.request_id)
            elif args.action == "download": result = client.download(args.id, args.path)
            elif args.action == "blob-delete": result = client.command("blob.delete", message_id=args.id)
            else:
                value = strict_json(sys.stdin.read() if args.json == "-" else args.json)
                operation = value.pop("operation")
                result = client.command(operation, **value)
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return 0
    except APIError as exc:
        print(json.dumps({"error": exc.code, "message": str(exc), "retry_after": exc.retry_after}), file=sys.stderr)
        return 1
    except (ValueError, OSError, RuntimeError, KeyError) as exc:
        # No traceback or request URL: URLs and local key material may contain secrets.
        print("SwarmMemo request failed (" + type(exc).__name__ + "); check command, connectivity and key file permissions", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
