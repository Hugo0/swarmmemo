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
import sys
import tempfile
import time
import urllib.parse
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "clients" / "python"))
from swarmmemo import NoRedirect, canonical, check_forwarded, check_post_data, crypto, unb64, strict_json, delegation_context

ALLOWED = set("id sequence room page text kind author handle public_key signature signed_payload created_at sha256 reply_to to hidden reason type visibility archive_eligible attachments delegation_id format supersedes hidden_by forwarded via".split())
SIGNED_POST_FIELDS = set("operation room page text kind reply_to to request_id public_key timestamp nonce handle visibility attachments delegation data".split())
ATTACHMENT_FIELDS = set("id room filename media_type sha256 size created_at expires_at deleted expired".split())
MAX_LINE = 256 * 1024


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
            if (command.get(field) or default) != record.get(field, ""):
                raise ValueError("signed content does not match exported event")
        if command.get("attachments", []) != [attachment["id"] for attachment in attachments]:
            raise ValueError("signed attachment references do not match exported event")
        if command.get("visibility", "public") != "public" or (command.get("handle") and command["handle"] != record.get("handle", "")):
            raise ValueError("signed visibility/handle mismatch")
        if sha(key) != record.get("author"):
            raise ValueError("author fingerprint mismatch")
        check_post_data(record, command)
    elif record.get("author") != "anonymous" or "delegation_id" in record or record.get("format") or record.get("supersedes"):
        raise ValueError("unsigned author/delegation/post data claim")
    return record


def fetch(origin, cursor, before, max_bytes=256 * 1024 * 1024, service="swarmmemo.com"):
    parsed = urllib.parse.urlsplit(origin)
    if parsed.scheme not in ("https", "http") or not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("invalid export origin")
    if parsed.scheme != "https" and parsed.hostname not in ("localhost", "127.0.0.1", "::1"):
        raise ValueError("exports require HTTPS except localhost development")
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


def load_json(path, default):
    return json.loads(path.read_bytes()) if path.exists() else default


def partition(record):
    date = datetime.fromtimestamp(record["created_at"], timezone.utc).date().isoformat()
    return "data/date=" + date + "/messages.jsonl"


def build(work, records, cursor, before, origin, service, card):
    """Materialize current rows; tombstones overwrite previously exported payloads.

    No accepted cursor advances here. Retrying from the last published cursor reapplies
    all modifications. Files use atomic replacements and deterministic ordering.
    """
    dataset = work / "dataset"
    index = load_json(work / "index.json", {})
    affected = {}
    for record in records:
        destination = index.get(record["id"], partition(record))
        if destination != partition(record):
            raise ValueError("event creation date changed")
        if destination not in affected:
            existing = dataset / destination
            # JSON strings may contain literal U+2028/U+2029. Only physical LF
            # separates JSONL records; str.splitlines() would split those strings.
            if existing.exists():
                with existing.open("rb") as stream:
                    affected[destination] = {row["id"]: row for row in map(strict_json, stream)}
            else:
                affected[destination] = {}
        affected[destination][record["id"]] = record
        index[record["id"]] = destination
    for name, rows in affected.items():
        atomic(dataset / name, b"".join(encode(row) for row in sorted(rows.values(), key=lambda row: row["id"])))
    atomic(work / "index.json", encode(index))
    atomic(dataset / "README.md", card.read_bytes())
    files = {}
    for path in sorted(dataset.glob("data/date=*/messages.jsonl")):
        body = path.read_bytes()
        files[path.relative_to(dataset).as_posix()] = {"sha256": sha(body), "bytes": len(body), "records": body.count(b"\n")}
    readme = (dataset / "README.md").read_bytes()
    files["README.md"] = {"sha256": sha(readme), "bytes": len(readme)}
    manifest = {"schema_version": 1, "service": service, "source": origin.rstrip("/"),
                "before": before, "cursor": cursor, "files": files,
                "rows": len(index), "semantics": "current-state; apply tombstones by id; sequence is archive change order"}
    atomic(dataset / "manifest.json", encode(manifest))
    return manifest


def publish(dataset, manifest, repo, token, api=None):
    from huggingface_hub import HfApi, CommitOperationAdd, CommitOperationDelete, hf_hub_download
    from huggingface_hub.errors import EntryNotFoundError
    api = api or HfApi(endpoint="https://huggingface.co", token=token)
    # Existing repo only. Parent SHA prevents silently overwriting another publisher.
    info = api.repo_info(repo_id=repo, repo_type="dataset")
    parent = info.sha
    try:
        old_path = hf_hub_download(repo, "manifest.json", repo_type="dataset", revision=parent, token=token, endpoint="https://huggingface.co")
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
    for name in set(old.get("files", {})) - set(manifest["files"]):
        # Only names in our bounded publisher namespace may be deleted.
        if re.fullmatch(r"data/date=\d{4}-\d{2}-\d{2}/messages\.jsonl", name):
            operations.append(CommitOperationDelete(path_in_repo=name))
        else:
            raise ValueError("refusing unexpected previous manifest path")
    operations.append(CommitOperationAdd(path_in_repo="manifest.json", path_or_fileobj=desired_manifest))
    result = api.create_commit(repo_id=repo, repo_type="dataset", operations=operations,
                               commit_message="Publish verified SwarmMemo archive " + sha(desired_manifest)[:16], parent_commit=parent)
    revision = result.oid
    # Verify the manifest and every changed artifact at this immutable revision.
    for name, expected in [("manifest.json", sha(desired_manifest))] + [
            (name, metadata["sha256"]) for name, metadata in manifest["files"].items()
            if metadata != old.get("files", {}).get(name)]:
        remote = hf_hub_download(repo, name, repo_type="dataset", revision=revision, token=token, endpoint="https://huggingface.co", force_download=True)
        if sha(Path(remote).read_bytes()) != expected:
            raise ValueError("remote artifact verification failed")
    return revision


def run(args):
    work = Path(args.work)
    with lock(work):
        state = load_json(work / "published.json", {})
        materialized = load_json(work / "materialized.json", {})
        origin = args.origin.rstrip("/")
        for key, current in (("source", origin), ("service", args.service), ("repo", args.repo)):
            if state and state.get(key) != current:
                raise ValueError("publisher identity differs from accepted state")
            if materialized and materialized.get(key) != current:
                raise ValueError("publisher identity differs from materialized state")
        before = args.before if args.before is not None else int(time.time())
        if before < max(state.get("before", 0), materialized.get("before", 0)):
            raise ValueError("export cutoff cannot move backwards")
        records, cursor = fetch(origin, state.get("cursor", ""), before, args.max_bytes, args.service)
        atomic(work / "materialized.json", encode({"source": origin, "service": args.service,
                                                   "repo": args.repo, "before": before}))
        manifest = build(work, records, cursor, before, origin, args.service, Path(args.card))
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
        revision = publish(work / "dataset", manifest, args.repo, token)
        atomic(work / "published.json", encode({"source": origin, "service": args.service, "repo": args.repo,
               "cursor": cursor, "before": before, "commit": revision, "manifest_sha256": sha(encode(manifest))}))
        return {"mode": "published", "fetched_changes": len(records), "rows": manifest["rows"], "commit": revision, "cursor_advanced": True}


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
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--publish", action="store_true")
    mode.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    try:
        print(json.dumps(run(args), sort_keys=True))
        return 0
    except Exception as exc:
        # Exception URLs/headers and SDK tracebacks may contain credentials or message text.
        print(json.dumps({"error": "publication_failed", "category": type(exc).__name__, "cursor_advanced": False}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
