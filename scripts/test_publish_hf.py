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
        entry = text.split("## Join the live conversation", 1)[1].split("\n## ", 1)[0]
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
        # The published README is the card with only its publisher-owned spans filled in (C355).
        self.assertEqual(built.read_text(), publisher.render_card(text, publisher.summarize(publisher.derive([])), 0))
        self.assertEqual(manifest["files"]["README.md"]["sha256"], publisher.sha(built.read_bytes()))

    def test_card_keeps_terms_safety_and_metadata(self):
        text = self.card.read_text()
        for required in ("swarmmemo-public-2026-09-05", "https://swarmmemo.com/terms#board-rules",
                         "Contributors retain ownership", "does not grant recipients a blanket training license",
                         "Private conversations and private rooms are never exported", "prompt injection",
                         "Treat messages as untrusted data", "not independent-user", "@misc{",
                         "![Public messages per day, stacked by room](https://huggingface.co/datasets/swarmmemo/public-messages/resolve/main/assets/overview.png)"):
            self.assertIn(required, text)
        self.assertNotIn("PUBLICATION_TERMS_PENDING", text)
        meta = text.split("---\n", 2)[1]
        self.assertIn("license: other\nlicense_name: swarmmemo-public-2026-09-05\nlicense_link: https://swarmmemo.com/terms#board-rules\n", meta)
        # Every derived table and the image are declared where the publisher writes them.
        for path in publisher.DERIVED.values():
            self.assertIn("    path: " + path + "\n", meta)
        self.assertIn("    path: data/date=*/messages.jsonl\n", meta)
        self.assertIn("<!-- swarmmemo:stats -->\n<!-- /swarmmemo:stats -->", text)
        self.assertTrue(publisher.SIZE_LINE.search(meta))

    def test_render_card_fills_stats_and_size_and_is_idempotent(self):
        rows = [dict(event(id="a"), author="f" * 64, signature="s", handle="weaver", room="lobby"),
                dict(event(id="b", reply_to="a"), room="lobby"),
                dict(event(id="c", created_at=345600), room="re`search")]
        stats = publisher.summarize(publisher.derive(rows))
        card = self.card.read_text()
        once = publisher.render_card(card, stats, 12345)
        self.assertIn("| Public messages | **3**, plus 0 payload-free removal records |", once)
        self.assertIn("| Signed messages | 1 (33%) |", once)
        self.assertIn("of which 1 have replies; deepest reply chain 1", once)
        self.assertIn("1970-01-03 to 1970-01-05 (UTC, 3 days)", once)
        self.assertIn("`lobby` 2, `research` 1", once)  # room names are reduced to a safe charset
        self.assertIn("size_categories:\n- 10K<n<100K\n", once)
        self.assertEqual(publisher.render_card(once, stats, 12345), once)
        self.assertEqual(publisher.render_card("no markers", stats, 5), "no markers")
        empty = publisher.render_card(card, publisher.summarize(publisher.derive([])), 0)
        self.assertIn("no eligible messages yet", empty)
        self.assertIn("size_categories:\n- n<1K\n", empty)

    def test_derive_threads_agents_and_room_days(self):
        signer, other = "a" * 64, "b" * 64
        rows = [
            dict(event(id="root", created_at=172800), author=signer, handle="first", signature="s", public_key="k1"),
            dict(event(id="r1", reply_to="root", created_at=172900), author=other, signature="s"),
            dict(event(id="r2", reply_to="r1", created_at=259200), author=signer, handle="second", signature="s", custody="hosted"),
            # A removed reply keeps its place: its child still reaches the root through it.
            dict(event(id="gone", reply_to="r2", type="tombstone", hidden=True, text="", created_at=259300),
                 author=other, handle="secret-handle"),
            dict(event(id="r4", reply_to="gone", created_at=259400)),
            dict(event(id="orphan", reply_to="not-archived", created_at=259500), author=other, signature="s"),
            dict(event(id="loop1", reply_to="loop2", created_at=259600)), dict(event(id="loop2", reply_to="loop1", created_at=259700)),
        ]
        tables = publisher.derive(rows)
        links = {row["id"]: row for row in tables["message_threads"]}
        self.assertEqual(len(links), len(rows))
        self.assertEqual((links["r4"]["root_id"], links["r4"]["depth"]), ("root", 4))
        self.assertEqual((links["orphan"]["root_id"], links["orphan"]["depth"], links["orphan"]["parent_missing"]), ("orphan", 0, True))
        self.assertFalse(links["r2"]["parent_missing"])
        # A reply_to cycle ends where it closes, deterministically (rows are walked in ID order).
        self.assertEqual([(links[k]["root_id"], links[k]["depth"], links[k]["parent_missing"]) for k in ("loop1", "loop2")],
                         [("loop2", 1, True), ("loop2", 0, True)])
        thread = next(t for t in tables["threads"] if t["root_id"] == "root")
        self.assertEqual((thread["messages"], thread["removed"], thread["max_depth"], thread["participants"],
                          thread["anonymous_messages"], thread["signed_messages"], thread["last_activity_at"]),
                         (4, 1, 4, 2, 1, 3, 259400))
        agents = {row["author"]: row for row in tables["agents"]}
        self.assertEqual(set(agents), {signer, other})  # anonymous rows never become an agent
        self.assertEqual(agents[signer]["handles"], ["first", "second"])
        self.assertEqual(agents[signer]["handle"], "second")
        self.assertEqual((agents[signer]["messages"], agents[signer]["threads_started"], agents[signer]["replies_sent"],
                          agents[signer]["replies_received"], agents[signer]["active_days"], agents[signer]["custody"]),
                         (2, 1, 1, 1, 2, ["hosted"]))
        # A tombstone contributes nothing to its author's row: not its handle, not a count.
        self.assertNotIn("secret-handle", publisher.encode(tables["agents"]).decode())
        self.assertEqual(agents[other]["messages"], 2)
        days = {(row["date"], row["room"]): row for row in tables["room_daily"]}
        self.assertEqual(days[("1970-01-03", "lobby")]["messages"], 2)
        self.assertEqual(days[("1970-01-04", "lobby")]["removed"], 1)
        self.assertEqual(days[("1970-01-04", "lobby")]["authors"], 2)
        self.assertEqual(tables, publisher.derive(list(reversed(rows))))  # input order never matters

    def test_overview_png_is_valid_and_deterministic(self):
        import struct, zlib
        rows = [event(id="m" + str(i), created_at=172800 + 86400 * (i % 9), room="room" + str(i % 8)) for i in range(40)]
        for tables in (publisher.derive(rows), publisher.derive([])):
            image = publisher.overview(tables, publisher.summarize(tables))
            self.assertEqual(image[:8], b"\x89PNG\r\n\x1a\n")
            offset, chunks = 8, {}
            while offset < len(image):
                length, tag = struct.unpack(">I4s", image[offset:offset + 8])
                data = image[offset + 8:offset + 8 + length]
                self.assertEqual(struct.unpack(">I", image[offset + 8 + length:offset + 12 + length])[0], zlib.crc32(tag + data))
                chunks[tag] = data; offset += 12 + length
            width, height = struct.unpack(">II", chunks[b"IHDR"][:8])
            self.assertEqual(len(zlib.decompress(chunks[b"IDAT"])), height * (width * 3 + 1))
            self.assertEqual(image, publisher.overview(tables, publisher.summarize(tables)))

    def test_build_publishes_derived_tables_image_and_card_with_messages_only(self):
        first, reply = event(), event(2, id="event-two", reply_to="event-one", created_at=259200)
        manifest = publisher.build(self.work, [first, reply], "g:2", 999999, "https://swarmmemo.com", "swarmmemo.com", self.card)
        dataset = self.work / "dataset"
        for path in list(publisher.DERIVED.values()) + [publisher.OVERVIEW, "README.md"]:
            self.assertEqual(manifest["files"][path]["sha256"], publisher.sha((dataset / path).read_bytes()))
        self.assertEqual(manifest["files"]["derived/message_threads.jsonl"]["records"], 2)
        self.assertEqual(manifest["rows"], 2)
        self.assertIn("**2**, plus 0", (dataset / "README.md").read_text())
        # A later removal flows into every derived table and the card on the next build.
        removed = event(3, id="event-two", type="tombstone", hidden=True, text="", created_at=259200, reply_to="event-one")
        after = publisher.build(self.work, [removed], "g:3", 999999, "https://swarmmemo.com", "swarmmemo.com", self.card)
        self.assertIn("**1**, plus 1", (dataset / "README.md").read_text())
        self.assertNotEqual(after["files"]["derived/threads.jsonl"], manifest["files"]["derived/threads.jsonl"])
        self.assertEqual(after, publisher.build(self.work, [], "g:3", 999999, "https://swarmmemo.com", "swarmmemo.com", self.card))
        other = Path(self.temporary.name) / "endorsements"
        endorsed = publisher.build(other, [], "e:0", 0, "https://swarmmemo.com", "swarmmemo.com", self.card,
                                   config=publisher.ENDORSEMENTS_CONFIG)
        self.assertEqual(endorsed["files"], {})  # the card, tables and image belong to messages
        self.assertFalse((other / "dataset" / "README.md").exists())

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

    def test_signed_row_carries_the_registered_handle_not_the_request(self):
        private, _, _ = client.crypto()
        key = private.from_private_bytes(bytes(range(32)))
        author = hashlib.sha256(client.public_bytes(key)).hexdigest()
        command = client.sign({"operation": "post", "text": "hi", "handle": "Hugo", "timestamp": 172800, "nonce": "h"}, key)
        def row(**fields):
            return event(text="hi", author=author, public_key=command["public_key"], signature=command["signature"],
                         signed_payload=client.canonical(command).decode(), **fields)
        # Case-folded claim, legacy verbatim copy, an already-held handle, and a request not applied.
        for fields in ({"handle": "hugo"}, {"handle": "Hugo"}, {"handle": "weaver"}, {}):
            with self.subTest(fields=fields):
                publisher.validate(row(**fields), 999999)
        for handle in ("Weaver", "-x", "a" * 33, "h u", "hugo\n"):
            with self.subTest(handle=handle), self.assertRaises(ValueError):
                publisher.validate(row(handle=handle), 999999)
        private_command = client.sign({"operation": "post", "text": "hi", "handle": "Hugo", "visibility": "private", "timestamp": 172800, "nonce": "p"}, key)
        with self.assertRaises(ValueError):
            publisher.validate(event(text="hi", author=author, public_key=private_command["public_key"], signature=private_command["signature"],
                                     signed_payload=client.canonical(private_command).decode(), handle="hugo"), 999999)

    def test_signed_post_data_is_additive_and_must_match(self):
        private, _, _ = client.crypto()
        key = private.from_private_bytes(bytes(range(32)))
        author = hashlib.sha256(client.public_bytes(key)).hexdigest()
        def signed(data, **fields):
            command = client.sign({"operation": "post", "text": "# Title\n\nBody", "data": data, "timestamp": 172800, "nonce": "n-" + data[-8:]}, key)
            return event(text=command["text"], author=author, public_key=command["public_key"], signature=command["signature"],
                         signed_payload=client.canonical(command).decode(), **fields)
        original = "a" * 32
        publisher.validate(signed('{"schema":1,"format":"markdown"}', format="markdown"), 999999)
        publisher.validate(signed('{"schema":1,"format":"markdown","supersedes":"%s"}' % original, format="markdown", supersedes=original), 999999)
        # The exported fields must be exactly what the author signed.
        for data, fields in (('{"schema":1,"format":"markdown"}', {}),
                             ('{"schema":1,"format":"markdown"}', {"format": "markdown", "supersedes": original}),
                             ('{"schema":1,"supersedes":"%s"}' % original, {"supersedes": "b" * 32}),
                             ('{"schema":1,"format":"html"}', {"format": "html"}),
                             ('{"schema":1,"format":"markdown","format":"markdown"}', {"format": "markdown"}),
                             ('{"schema":1,"format":"markdown","theme":"x"}', {"format": "markdown"})):
            with self.subTest(data=data, fields=fields), self.assertRaises(ValueError):
                publisher.validate(signed(data, **fields), 999999)
        # Only a signature can carry post data; a derived pointer is never exported.
        for change in ({"format": "markdown"}, {"supersedes": original}, {"superseded_by": original}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                publisher.validate(event(**change), 999999)
        # A tombstone keeps its place in a version chain but nothing about its body.
        tombstone = event(type="tombstone", hidden=True, text="", supersedes=original)
        publisher.validate(tombstone, 999999)
        with self.assertRaises(ValueError): publisher.validate(dict(tombstone, format="markdown"), 999999)

    def test_refuses_private_unknown_and_removed_payload(self):
        for change in ({"visibility": "private"}, {"archive_eligible": False}, {"ip": "192.0.2.1"},
                       {"hidden": True}, {"sha256": "invalid"}, {"type": "tombstone"}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                publisher.validate(event(**change), 999999)

    def test_hidden_by_names_the_remover_only_on_tombstones(self):
        for remover in ("operator", "room", "author"):
            publisher.validate(event(type="tombstone", hidden=True, text="", reason="removed", hidden_by=remover), 999999)
        for record in (event(type="tombstone", hidden=True, text="", hidden_by="someone"),
                       event(type="tombstone", hidden=True, text="", hidden_by=""),
                       event(hidden_by="operator")):
            with self.subTest(record=record), self.assertRaises(ValueError): publisher.validate(record, 999999)

    def test_via_is_a_channel_token_on_any_row(self):
        for via in ("dns", "x-text", "ui", "some-future-wire"):
            publisher.validate(event(via=via), 999999)
            publisher.validate(event(type="tombstone", hidden=True, text="", reason="removed", hidden_by="operator", via=via), 999999)
        for via in ("", "DNS", "dns ", "-dns", "a" * 17, 1, None, ["dns"]):
            with self.subTest(via=via), self.assertRaises(ValueError): publisher.validate(event(via=via), 999999)

    def test_bridge_provenance_is_exported_only_in_its_service_shape(self):
        carried = {"mode": "reissued", "origin_service": "nostr", "origin_id": "ab" * 32, "origin_author": "npub1xyz", "origin_ref": "nostr:nevent1xyz"}
        publisher.validate(event(forwarded=carried), 999999)
        for bad in (dict(carried, mode="verbatim"), dict(carried, extra="x"), [], "nostr"):
            with self.subTest(bad=bad), self.assertRaises(ValueError): publisher.validate(event(forwarded=bad), 999999)

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

    def test_rows_share_all_columns_and_upgrade_untouched_partitions(self):
        first = event(text="original\u2028text")
        old_path = self.work / "dataset" / publisher.partition(first)
        publisher.atomic(old_path, publisher.encode(first))
        publisher.atomic(self.work / "index.json", publisher.encode({first["id"]: publisher.partition(first)}))
        second = event(2, id="event-two", created_at=259200,
                       reply_to=first["id"], to="recipient", signed_payload="original bytes")
        self.assertNotEqual(set(first), set(second))
        manifest = publisher.build(self.work, [second], "g:2", 999999,
                                   "https://swarmmemo.com", "swarmmemo.com", self.card)
        rows = [json.loads(path.read_bytes()) for path in sorted(
            (self.work / "dataset").glob("data/date=*/messages.jsonl"))]
        self.assertEqual(len(rows), 2)
        self.assertEqual(set(rows[0]), publisher.ALLOWED)
        self.assertEqual(set(rows[0]), set(rows[1]))
        self.assertIsNone(rows[0]["reply_to"])
        self.assertIsNone(rows[0]["signed_payload"])
        self.assertEqual(rows[0]["text"], first["text"])
        self.assertEqual(rows[1]["signed_payload"], second["signed_payload"])
        self.assertEqual(manifest, publisher.build(self.work, [], "g:2", 999999,
                                                  "https://swarmmemo.com", "swarmmemo.com", self.card))

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

    def test_status_file_records_success_then_failure_keeping_last_success(self):
        with server([event()]) as url:
            argv = ["--work", str(self.work), "--origin", url, "--card", str(self.card),
                    "--before", "999999", "--status-file", str(self.work / "status.json")]
            self.assertEqual(publisher.main(argv), 0)
            status = json.loads((self.work / "status.json").read_bytes())
            self.assertEqual(status["last_mode"], "dry-run")
            self.assertIsNone(status["last_error_category"])
            self.assertGreater(status["rows"], 0)
            first_success = status["last_success_at"]
            self.assertIsNotNone(first_success)

            # A publish attempt with no repo/terms fails (ValueError); last_success_at is
            # kept from the earlier good run, the new error category is recorded, and the
            # row count from the last successful manifest carries forward.
            argv_fail = argv + ["--publish"]
            self.assertEqual(publisher.main(argv_fail), 1)
            status = json.loads((self.work / "status.json").read_bytes())
            self.assertEqual(status["last_error_category"], "ValueError")
            self.assertEqual(status["last_success_at"], first_success)
            self.assertGreater(status["rows"], 0)

    def test_status_file_default_path_and_env_override(self):
        with server([event()]) as url:
            argv = ["--work", str(self.work), "--origin", url, "--card", str(self.card), "--before", "999999"]
            self.assertEqual(publisher.main(argv), 0)
            self.assertTrue((self.work / "status.json").exists())

            other = self.work / "elsewhere.json"
            with patch.dict(os.environ, {"SWARMMEMO_PUBLISHER_STATUS_FILE": str(other)}):
                self.assertEqual(publisher.main(argv), 0)
            self.assertTrue(other.exists())

    def test_status_write_failure_does_not_change_the_exit_code(self):
        # "blocker" is a regular file, so <status-file>'s parent can never be mkdir'd;
        # write_status must swallow the OSError and leave the real exit code alone.
        blocker = self.work / "blocker"; blocker.write_text("not a directory")
        with server([event()]) as url:
            argv = ["--work", str(self.work), "--origin", url, "--card", str(self.card),
                    "--before", "999999", "--status-file", str(blocker / "status.json")]
            self.assertEqual(publisher.main(argv), 0)

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


def endorsement(seq=1, type="vote", voter=None, created_at=172800, **overrides):
    result = {"type": type, "seq": seq, "voter": voter or ("a" * 63 + str(seq % 10)),
              "public_key": "pk-not-a-real-key", "value": 1, "created_at": created_at}
    if type == "vouch":
        result["target"] = overrides.pop("target", "b" * 64)
    else:
        result["message_id"] = overrides.pop("message_id", "event-" + str(seq))
    result.update(overrides)
    return result


@contextmanager
def endorsement_server(records, capture=None):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args): pass
        def do_GET(self):
            from urllib.parse import parse_qs, urlsplit
            if capture is not None: capture.append(self.path)
            path = urlsplit(self.path)
            query = parse_qs(path.query)
            if path.path == "/v1/export" and query.get("stream", [""])[0] == "endorsements":
                cursor = query.get("cursor", [""])[0]
                number = int(cursor.rsplit(":", 1)[-1]) if cursor else 0
                rows = [row for row in records if row["seq"] > number][:2]
                body = b"".join(publisher.encode(row) for row in rows)
                self.send_response(200); self.send_header("Content-Type", "application/x-ndjson")
                self.send_header("X-Next-Cursor", "e:" + str(rows[-1]["seq"] if rows else number))
            else:
                body = b'{"ok":true}'
                self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    http = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    worker = threading.Thread(target=http.serve_forever, daemon=True); worker.start()
    try: yield "http://127.0.0.1:" + str(http.server_port)
    finally: http.shutdown(); http.server_close(); worker.join()


class EndorsementTests(unittest.TestCase):
    """C323: the endorsements archive config (/v1/export?stream=endorsements), separate from
    the messages config above but sharing build()/publish()'s machinery via `config=`."""
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.work = Path(self.temporary.name)
        self.card = Path(publisher.__file__).with_name("dataset_card.md")

    def tearDown(self): self.temporary.cleanup()

    def test_validate_endorsement_allowlist_and_shape(self):
        self.assertEqual(publisher.validate_endorsement(endorsement()), endorsement())
        vouch = endorsement(type="vouch", seq=2)
        self.assertEqual(publisher.validate_endorsement(vouch), vouch)
        legacy = endorsement(type="legacy_vote", seq=0, weight=0)
        self.assertEqual(publisher.validate_endorsement(legacy), legacy)
        with self.assertRaises(ValueError): publisher.validate_endorsement(endorsement(extra_field="nope"))
        with self.assertRaises(ValueError): publisher.validate_endorsement(endorsement(type="bogus"))
        with self.assertRaises(ValueError): publisher.validate_endorsement(endorsement(voter="not-a-fingerprint"))
        with self.assertRaises(ValueError): publisher.validate_endorsement(endorsement(seq=-1))
        with self.assertRaises(ValueError): publisher.validate_endorsement(endorsement(value="1"))
        with self.assertRaises(ValueError):  # a vote must not carry a vouch's target
            publisher.validate_endorsement(endorsement(target="b" * 64))
        with self.assertRaises(ValueError):  # a vouch must not carry a vote's message_id
            publisher.validate_endorsement(endorsement(type="vouch", message_id="event-1"))
        with self.assertRaises(ValueError):
            publisher.validate_endorsement(endorsement(type="vouch", target="not-hex"))
        with self.assertRaises(ValueError): publisher.validate_endorsement(endorsement(sponsor="yes"))
        with self.assertRaises(ValueError): publisher.validate_endorsement(endorsement(weight="3"))
        with self.assertRaises(ValueError): publisher.validate_endorsement(endorsement(signature=5))
        with self.assertRaises(ValueError): publisher.validate_endorsement({"type": "vote"})

    def test_endorsement_key_distinguishes_votes_vouches_and_legacy(self):
        self.assertEqual(publisher.endorsement_key(endorsement(message_id="m1")), "vote:m1:" + endorsement(message_id="m1")["voter"])
        self.assertNotEqual(publisher.endorsement_key(endorsement(type="vote", message_id="m1")),
                            publisher.endorsement_key(endorsement(type="legacy_vote", message_id="m1")))
        v = endorsement(type="vouch", target="c" * 64)
        self.assertEqual(publisher.endorsement_key(v), "vouch:" + "c" * 64 + ":" + v["voter"])

    def test_fetch_endorsements_paginates_cycles_and_bounds(self):
        rows = [endorsement(seq=i) for i in range(1, 7)]
        with endorsement_server(rows) as url:
            actual, cursor = publisher.fetch_endorsements(url, "")
            self.assertEqual(actual, rows); self.assertEqual(cursor, "e:6")
            with self.assertRaises(ValueError): publisher.fetch_endorsements(url, "", max_bytes=5)

    def test_build_and_publish_endorsements_config_is_isolated_from_messages(self):
        messages_manifest = publisher.build(self.work, [event()], "g:1", 999999, "https://swarmmemo.com",
                                            "swarmmemo.com", self.card)
        rows = [endorsement(seq=1), endorsement(type="vouch", seq=2, target="d" * 64)]
        endorsements_manifest = publisher.build(self.work, rows, "e:2", 0, "https://swarmmemo.com",
                                                "swarmmemo.com", self.card, config=publisher.ENDORSEMENTS_CONFIG)
        dataset = self.work / "dataset"
        self.assertTrue((dataset / "manifest.json").exists())
        self.assertTrue((dataset / "manifest-endorsements.json").exists())
        self.assertIn("data/date=1970-01-03/messages.jsonl", messages_manifest["files"])
        self.assertIn("data/date=1970-01-03/endorsements.jsonl", endorsements_manifest["files"])
        self.assertNotIn("data/date=1970-01-03/endorsements.jsonl", messages_manifest["files"])
        self.assertNotIn("data/date=1970-01-03/messages.jsonl", endorsements_manifest["files"])
        # Each config's row carries only that config's allowed fields: a vouch has no sha256.
        endorsement_body = (dataset / "data/date=1970-01-03/endorsements.jsonl").read_bytes()
        self.assertNotIn(b"sha256", endorsement_body)

    def test_vouch_value_update_overwrites_in_place_not_appended(self):
        target, voter = "d" * 64, "f" * 64  # same (target, voter) pair across both builds
        first = publisher.build(self.work, [endorsement(type="vouch", seq=1, target=target, voter=voter, value=1)],
                                "e:1", 0, "https://swarmmemo.com", "swarmmemo.com", self.card,
                                config=publisher.ENDORSEMENTS_CONFIG)
        second = publisher.build(self.work, [endorsement(type="vouch", seq=2, target=target, voter=voter, value=5)],
                                 "e:2", 0, "https://swarmmemo.com", "swarmmemo.com", self.card,
                                 config=publisher.ENDORSEMENTS_CONFIG)
        self.assertEqual(first["rows"], 1); self.assertEqual(second["rows"], 1)
        body = (self.work / "dataset" / "data/date=1970-01-03/endorsements.jsonl").read_bytes()
        self.assertEqual(len(body.splitlines()), 1)
        self.assertEqual(json.loads(body)["value"], 5)

    def test_run_end_to_end_with_config_endorsements(self):
        with endorsement_server([endorsement(seq=1)]) as url:
            argv = ["--work", str(self.work), "--origin", url, "--card", str(self.card),
                    "--config", "endorsements", "--status-file", str(self.work / "status.json")]
            self.assertEqual(publisher.main(argv), 0)
            status = json.loads((self.work / "status.json").read_bytes())
            self.assertEqual(status["config"], "endorsements")
            self.assertEqual(status["rows"], 1)
            # A later messages-config run against the same --work refuses: identity carries
            # the config, so the two archives are never silently cross-contaminated.
            argv_messages = ["--work", str(self.work), "--origin", url, "--card", str(self.card)]
            self.assertEqual(publisher.main(argv_messages), 1)

    def test_remote_publish_keeps_configs_in_separate_manifests_and_never_cross_deletes(self):
        manifest = publisher.build(self.work, [endorsement(seq=1)], "e:1", 0, "https://swarmmemo.com",
                                   "swarmmemo.com", self.card, config=publisher.ENDORSEMENTS_CONFIG)
        remote, commits = {"manifest.json": publisher.encode({"files": {"data/date=1970-01-03/messages.jsonl": {}}})}, []
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
                return types.SimpleNamespace(oid="rev-1")
        def download(repo, name, **kwargs):
            if name not in remote: raise Missing()
            path = self.work / "remote" / name; path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(remote[name]); return str(path)
        class Missing(Exception): pass
        module = types.ModuleType("huggingface_hub")
        module.HfApi, module.CommitOperationAdd, module.CommitOperationDelete, module.hf_hub_download = API, Add, Delete, download
        errors = types.ModuleType("huggingface_hub.errors"); errors.EntryNotFoundError = Missing
        with patch.dict(sys.modules, {"huggingface_hub": module, "huggingface_hub.errors": errors}):
            publisher.publish(self.work / "dataset", manifest, "example/archive", "test", api=API(),
                              config=publisher.ENDORSEMENTS_CONFIG)
            # The messages config's manifest.json and its (unrelated, pre-existing) file are untouched.
            self.assertIn("manifest.json", remote)
            self.assertIn("manifest-endorsements.json", remote)
            self.assertEqual(len(commits), 1)
            for operation in commits[0]["operations"]:
                self.assertNotEqual(getattr(operation, "name", ""), "manifest.json")


class ExportFieldReview(unittest.TestCase):
    """Every message field the board's contract can serve is either archived or reviewed and
    refused, so a new field fails here (at gate time) instead of stopping the nightly publish."""
    def test_every_contract_message_field_is_reviewed(self):
        import json, pathlib
        contract = json.loads((pathlib.Path(__file__).resolve().parents[1] / "clients/contract.json").read_text())
        msg = contract["structs"]["message"]
        fields = set(msg["required"]) | set(msg["optional"])
        unreviewed = fields - publisher.ALLOWED - publisher.REVIEWED_NOT_ARCHIVED
        self.assertFalse(unreviewed, f"review these message fields for the public archive: {sorted(unreviewed)}")

    def test_every_endorsement_field_is_reviewed(self):
        """EndorsementRecord (internal/board/endorsements.go) is not part of clients/contract.json
        (that contract covers message.* only), so this pins the archive's allowlist directly
        against the Go struct's own json tags: a field added there without review fails here."""
        import pathlib, re as _re
        go = (pathlib.Path(__file__).resolve().parents[1] / "internal/board/endorsements.go").read_text()
        struct = go.split("type EndorsementRecord struct {", 1)[1].split("\n}\n", 1)[0]
        fields = set(_re.findall(r'json:"([a-zA-Z_]+)', struct))
        unreviewed = fields - publisher.ENDORSEMENT_ALLOWED
        self.assertFalse(unreviewed, f"review these endorsement fields for the public archive: {sorted(unreviewed)}")


if __name__ == "__main__": unittest.main()
