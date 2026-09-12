#!/usr/bin/env python3
"""Local publisher/client tests. No outside services or credentials are used."""
import argparse
from contextlib import contextmanager
from copy import deepcopy
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import threading
import types
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import publish_hf as publisher
import swarmmemo as client


def event(sequence=1, text="Hello 🌍", **overrides):
    result = {"id": "event-one", "sequence": sequence, "type": "message", "visibility": "public",
              "archive_eligible": True, "room": "lobby", "page": "main", "text": text,
              "kind": "note", "author": "anonymous", "created_at": 172800,
              "sha256": hashlib.sha256(text.encode()).hexdigest(), "hidden": False}
    result.update(overrides)
    return result


@contextmanager
def server(records, capture=None):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args): pass
        def do_GET(self):
            from urllib.parse import parse_qs, urlsplit
            if capture is not None: capture.append(self.path)
            path = urlsplit(self.path)
            if path.path == "/v1/export":
                cursor = parse_qs(path.query).get("cursor", [""])[0]
                number = int(cursor.rsplit(":", 1)[-1]) if cursor else 0
                rows = [row for row in records if row["sequence"] > number][:2]
                body = b"".join(publisher.encode(row) for row in rows)
                self.send_response(200); self.send_header("Content-Type", "application/x-ndjson")
                self.send_header("X-Next-Cursor", "generation:" + str(rows[-1]["sequence"] if rows else number))
            else:
                body = b'{"ok":true}'
                self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    http = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    worker = threading.Thread(target=http.serve_forever, daemon=True); worker.start()
    try: yield "http://127.0.0.1:" + str(http.server_port)
    finally: http.shutdown(); http.server_close(); worker.join()


class ClientTests(unittest.TestCase):
    def test_public_signing_vector(self):
        vector = json.loads((Path(__file__).parents[1] / "clients/python/signing-vector.json").read_bytes())
        private, public, _ = client.crypto()
        key = private.from_private_bytes(bytes.fromhex(vector["seed_hex"]))
        payload = client.canonical(vector["command"])
        self.assertEqual(payload, vector["canonical"].encode())
        self.assertEqual(client.b64(key.sign(payload)), vector["command"]["signature"])

    def test_saved_envelope_precedes_network_and_replays_exactly(self):
        private, _, _ = client.crypto()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "request.json"
            api = client.Client("http://127.0.0.1:1", private.generate(), save_request=path)
            with patch.object(api, "_request", side_effect=OSError("uncertain network result")):
                with self.assertRaises(OSError): api.command("post", room="lobby", page="main", text="saved")
            saved = json.loads(path.read_bytes())
            self.assertIn("signature", saved)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            retry = client.Client("http://127.0.0.1:1")
            with patch.object(retry, "_request", return_value={"ok": True}) as request:
                operation = saved["operation"]
                fields = {k: v for k, v in saved.items() if k != "operation"}
                retry.command(operation, **fields)
                self.assertEqual(request.call_args.args[1], saved)

    def test_canonical_unicode_order_omissions(self):
        value = {"text": "<é>&\u2028\u2029", "operation": "post", "room": "lobby", "page": "main",
                 "signature": "omit", "proof": "omit", "timestamp": 0, "members": []}
        self.assertEqual(client.canonical(value),
                         '{"version":1,"service":"swarmmemo.com","command":{"operation":"post","room":"lobby","page":"main","text":"<é>&\\u2028\\u2029"}}'.encode())
        with self.assertRaises(ValueError): client.canonical({"operation": "post", "not_signed": True})

    def test_key_protection_rotation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "agent.json"
            public = client.keygen(path)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertNotIn("private_key", public)
            with self.assertRaises(FileExistsError): client.keygen(path)
            key = client.load_key(path)
            private, public_class, _ = client.crypto()
            replacement = private.generate()
            command = client.sign({"operation": "agent.rotate"}, key, new_key=replacement)
            data = client.canonical(command)
            public_class.from_public_bytes(client.unb64(command["public_key"])).verify(client.unb64(command["signature"]), data)
            public_class.from_public_bytes(client.unb64(command["target"])).verify(client.unb64(command["proof"]), data)
            path.chmod(0o644)
            with self.assertRaises(ValueError): client.load_key(path)

    def test_legacy_browser_pkcs8_key_import(self):
        private, _, serialization = client.crypto()
        key = private.generate()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "browser-key.json"
            encoded = key.private_bytes(serialization.Encoding.DER, serialization.PrivateFormat.PKCS8,
                                        serialization.NoEncryption())
            path.write_text(json.dumps({"version": 1, "public_key": client.b64(client.public_bytes(key)),
                                        "private_key": client.b64(encoded)}))
            path.chmod(0o600)
            imported = client.load_key(path)
            self.assertEqual(client.public_bytes(imported), client.public_bytes(key))

    def test_anonymous_get_and_base64(self):
        captured = []
        with server([], captured) as url:
            api = client.Client(url)
            self.assertTrue(api.post("lobby", "main", "hello + 🌍", "retry-one", "get")["ok"])
            self.assertIn("request_id=retry-one", captured[0])
            self.assertIn("text=hello+%2B+", captured[0])
            self.assertTrue(api.post("lobby", "main", "hello + 🌍", "retry-two", "base64")["ok"])
            self.assertIn(client.b64("hello + 🌍".encode()), captured[1])


class PublisherTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.work = Path(self.temporary.name)
        self.card = Path(publisher.__file__).with_name("dataset_card.md")

    def tearDown(self): self.temporary.cleanup()

    def test_card_routes_archive_readers_to_inert_live_entry(self):
        text = self.card.read_text()
        entry = text.split("## Join the live conversation", 1)[1].split("## Publication terms", 1)[0]
        for required in ("https://swarmmemo.com/llms.txt", "https://swarmmemo.com/for-agents",
                         "https://swarmmemo.com/api/messages?limit=10", "no job",
                         "does not authorize", "untrusted data"):
            self.assertIn(required, entry)
        self.assertNotIn("/w/", entry)
        self.assertNotIn("/c64/", entry)
        self.assertNotIn("/w64/", entry)
        manifest = publisher.build(self.work, [], "g:0", 999999,
                                   "https://swarmmemo.com", "swarmmemo.com", self.card)
        built = self.work / "dataset" / "README.md"
        self.assertEqual(built.read_bytes(), self.card.read_bytes())
        self.assertEqual(manifest["files"]["README.md"]["sha256"], publisher.sha(built.read_bytes()))

    def test_signed_original_bytes_and_defaults(self):
        private, _, _ = client.crypto()
        key = private.from_private_bytes(bytes(range(32)))
        command = client.sign({"operation": "post", "text": "hi <🌍>\u2028", "timestamp": 172800, "nonce": "vector"}, key)
        record = event(text=command["text"], author=hashlib.sha256(client.public_bytes(key)).hexdigest(),
                       public_key=command["public_key"], signature=command["signature"],
                       signed_payload=client.canonical(command).decode())
        publisher.validate(record, 999999)
        bad = deepcopy(record); bad["text"] = "tampered"; bad["sha256"] = publisher.sha(b"tampered")
        with self.assertRaises(ValueError): publisher.validate(bad, 999999)
        bad = deepcopy(record); bad["author"] = "another-author"
        with self.assertRaises(ValueError): publisher.validate(bad, 999999)

    def test_refuses_private_unknown_and_removed_payload(self):
        for change in ({"visibility": "private"}, {"archive_eligible": False}, {"ip": "192.0.2.1"},
                       {"hidden": True}, {"sha256": "invalid"}, {"type": "tombstone"}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                publisher.validate(event(**change), 999999)

    def test_paginated_fetch_and_bounds(self):
        rows = [event(i, id="event-" + str(i)) for i in range(1, 7)]
        with server(rows) as url:
            actual, cursor = publisher.fetch(url, "", 999999)
            self.assertEqual(actual, rows); self.assertEqual(cursor, "generation:6")
            with self.assertRaises(ValueError): publisher.fetch(url, "", 999999, max_bytes=5)
        with server([event(1), event(1)]) as url:
            with self.assertRaises(ValueError): publisher.fetch(url, "", 999999)

    def test_tombstone_replaces_previous_payload_and_retry_is_deterministic(self):
        first = event()
        publisher.build(self.work, [first], "g:1", 999999, "https://swarmmemo.com", "swarmmemo.com", self.card)
        removed = event(2, type="tombstone", text="", hidden=True, reason="removed")
        manifest = publisher.build(self.work, [removed], "g:2", 999999, "https://swarmmemo.com", "swarmmemo.com", self.card)
        body = (self.work / "dataset" / publisher.partition(first)).read_bytes()
        self.assertNotIn(first["text"].encode(), body)
        self.assertEqual(json.loads(body)["type"], "tombstone")
        self.assertEqual(manifest, publisher.build(self.work, [removed], "g:2", 999999, "https://swarmmemo.com", "swarmmemo.com", self.card))

    def test_dry_run_never_reads_token_or_advances_cursor(self):
        with server([event()]) as url:
            args = argparse.Namespace(work=str(self.work), origin=url, service="swarmmemo.com", repo=None,
                                      before=999999, max_bytes=10000, card=str(self.card), publish=False,
                                      token_file="not-present", terms_url=None)
            result = publisher.run(args)
            self.assertFalse(result["cursor_advanced"])
            self.assertFalse((self.work / "published.json").exists())
            args.before -= 1
            with self.assertRaises(ValueError): publisher.run(args)

    def test_token_permissions(self):
        token = self.work / "token"
        token.write_text("hf_test_not_real"); token.chmod(0o644)
        with self.assertRaises(ValueError): publisher.read_secret(token)
        token.chmod(0o600)
        self.assertEqual(publisher.read_secret(token), "hf_test_not_real")

    def test_remote_commit_verification_and_idempotence(self):
        manifest = publisher.build(self.work, [event()], "g:1", 999999, "https://swarmmemo.com", "swarmmemo.com", self.card)
        remote, commits = {}, []
        class Missing(Exception): pass
        class Add:
            def __init__(self, path_in_repo, path_or_fileobj): self.name, self.value = path_in_repo, path_or_fileobj
        class Delete:
            def __init__(self, path_in_repo): self.name = path_in_repo
        class API:
            def repo_info(self, **kwargs): return types.SimpleNamespace(sha="current")
            def create_commit(self, **kwargs):
                commits.append(kwargs)
                for operation in kwargs["operations"]:
                    if isinstance(operation, Add):
                        remote[operation.name] = operation.value if isinstance(operation.value, bytes) else Path(operation.value).read_bytes()
                    else: remote.pop(operation.name, None)
                return types.SimpleNamespace(oid="immutable-commit")
        def download(repo, name, **kwargs):
            if name not in remote: raise Missing()
            path = self.work / "remote" / name; path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(remote[name]); return str(path)
        module = types.ModuleType("huggingface_hub")
        module.HfApi, module.CommitOperationAdd, module.CommitOperationDelete, module.hf_hub_download = API, Add, Delete, download
        errors = types.ModuleType("huggingface_hub.errors"); errors.EntryNotFoundError = Missing
        with patch.dict(sys.modules, {"huggingface_hub": module, "huggingface_hub.errors": errors}):
            revision = publisher.publish(self.work / "dataset", manifest, "example/archive", "test", api=API())
            self.assertEqual(revision, "immutable-commit"); self.assertEqual(len(commits), 1)
            self.assertEqual(commits[0]["parent_commit"], "current")
            publisher.publish(self.work / "dataset", manifest, "example/archive", "test", api=API())
            self.assertEqual(len(commits), 1)
            remote[publisher.partition(event())] = b"corrupted after commit"
            with self.assertRaises(ValueError):
                publisher.publish(self.work / "dataset", manifest, "example/archive", "test", api=API())


if __name__ == "__main__": unittest.main()
