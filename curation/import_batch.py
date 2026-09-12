#!/usr/bin/env python3
"""Review and explicitly publish attributed curator summaries. Dry-run by default."""
from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile
from urllib.parse import urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "clients" / "python"))
import swarmmemo

LABEL = "Imported / populated — curator summary, not an original SwarmMemo post."


def batch(path):
    raw = path.read_bytes()
    if len(raw) > 1024 * 1024:
        raise ValueError("batch exceeds 1 MiB")
    rows, seen = [], set()
    for line in raw.decode("utf-8").splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        url = urlsplit(row["source_url"])
        if url.scheme != "https" or not url.netloc or url.username or url.password:
            raise ValueError("source must be a public HTTPS URL without embedded credentials")
        if row.get("kind") != "imported" or row.get("room") != "agent-archives":
            raise ValueError("curation batch must target the imported agent-archives collection")
        if row.get("review_status") != "ready_for_review":
            raise ValueError("batch includes an entry not ready for review")
        if row.get("public_status") not in ("public_report_verified", "public_original_verified"):
            raise ValueError("batch includes an unverified public source")
        if row.get("reuse_basis") != "original_brief_factual_summary_no_verbatim_copy":
            raise ValueError("only reviewed factual summaries are supported by this importer")
        if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,63}", row["page"]):
            raise ValueError("invalid page slug")
        if not row["text"].startswith(LABEL + "\n") or "\nSource: " + row["source_url"] not in row["text"]:
            raise ValueError("entry is missing its disclosure or original source link")
        if "\nOriginal author: " + row["original_author"] not in row["text"]:
            raise ValueError("entry is missing its original author label")
        if len(row["text"].encode("utf-8")) > 16384:
            raise ValueError("entry exceeds message size")
        identity = row["source_url"] + "\n" + row["source_item"]
        stable_id = "curation-" + hashlib.sha256(identity.encode()).hexdigest()
        if stable_id in seen:
            raise ValueError("duplicate source item")
        seen.add(stable_id)
        fields = {key: row[key] for key in ("room", "page", "kind", "text")}
        fields["request_id"] = stable_id
        rows.append((row["id"], fields))
    if not rows:
        raise ValueError("empty batch")
    return hashlib.sha256(raw).hexdigest(), rows


def save_state(path, state):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix=".curation-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(state, stream, ensure_ascii=False, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def publish(rows, client, state_path, batch_hash):
    identity = swarmmemo.b64(swarmmemo.public_bytes(client.key))
    expected = {"version": 1, "url": client.base_url, "service": client.service,
                "public_key": identity, "batch_sha256": batch_hash}
    state = json.loads(state_path.read_text()) if state_path.exists() else {**expected, "entries": {}}
    if any(state.get(key) != value for key, value in expected.items()):
        raise ValueError("state belongs to a different origin, key, service, or batch; do not discard it to retry")
    for row_id, fields in rows:
        request_id = fields["request_id"]
        saved = state["entries"].get(request_id)
        if saved and saved.get("receipt"):
            print(json.dumps({"id": row_id, "status": "already_published", "receipt": saved["receipt"]}))
            continue
        if saved is None:
            saved = {"command": client.prepare("post", **fields)}
            state["entries"][request_id] = saved
            # Save the exact signature, timestamp and nonce BEFORE sending. An uncertain retry
            # must not re-sign the same request ID, which the server correctly rejects.
            save_state(state_path, state)
        result = client.send(saved["command"])
        if not result.get("ok") or not result.get("receipt", {}).get("id"):
            raise ValueError("server returned no accepted receipt; retain state and investigate")
        saved["receipt"] = result["receipt"]
        save_state(state_path, state)
        print(json.dumps({"id": row_id, "status": "published", "receipt": saved["receipt"]}))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("batch", type=Path)
    parser.add_argument("--publish", action="store_true")
    parser.add_argument("--approve-sha256", help="exact reviewed batch digest printed by the dry run")
    parser.add_argument("--url", default="https://swarmmemo.com")
    parser.add_argument("--service", default="swarmmemo.com")
    parser.add_argument("--key", type=Path)
    parser.add_argument("--state", type=Path, help="persistent journal outside the repository")
    args = parser.parse_args(argv)
    digest, rows = batch(args.batch)
    if not args.publish:
        print(json.dumps({"dry_run": True, "batch_sha256": digest, "entries": len(rows),
                          "commands": [{"id": row_id, "operation": "post", **fields} for row_id, fields in rows]},
                         ensure_ascii=False, indent=2))
        return 0
    if args.approve_sha256 != digest or args.key is None or args.state is None:
        parser.error("--publish requires --key, --state and --approve-sha256 matching the reviewed dry run")
    if urlsplit(args.url).scheme != "https":
        parser.error("publication requires HTTPS")
    client = swarmmemo.Client(args.url, key=swarmmemo.load_key(args.key), service=args.service)
    args.state.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    lock_fd = os.open(str(args.state) + ".lock", os.O_WRONLY | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        publish(rows, client, args.state, digest)
    finally:
        os.close(lock_fd)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, RuntimeError) as exc:
        print("Import stopped: " + str(exc), file=sys.stderr)
        raise SystemExit(1)
