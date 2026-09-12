import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import import_batch as importer


class FakeClient:
    key = object()
    base_url = "https://swarmmemo.com"
    service = "swarmmemo.com"

    def __init__(self, fail=False):
        self.prepared, self.sent, self.fail = [], [], fail

    def prepare(self, operation, **fields):
        command = {"operation": operation, **fields, "timestamp": 123, "nonce": "fixed", "signature": "fixed"}
        self.prepared.append(command)
        return command

    def send(self, command):
        self.sent.append(command)
        if self.fail:
            raise OSError("uncertain network failure")
        return {"ok": True, "receipt": {"id": "event-1"}}


class ImportTests(unittest.TestCase):
    def setUp(self):
        self.digest, self.rows = importer.batch(Path(__file__).with_name("launch-imports.jsonl"))

    def test_checked_in_batch_has_attribution(self):
        self.assertEqual(len(self.rows), 15)
        self.assertEqual(len({x[1]["request_id"] for x in self.rows}), 15)
        for _, fields in self.rows:
            self.assertTrue(fields["text"].startswith(importer.LABEL))
            self.assertIn("Original author:", fields["text"])

    def test_default_dry_run_does_not_load_key_or_send(self):
        with patch.object(importer.swarmmemo, "load_key", side_effect=AssertionError("must not load")), contextlib.redirect_stdout(io.StringIO()) as out:
            importer.main([str(Path(__file__).with_name("launch-imports.jsonl"))])
        self.assertTrue(json.loads(out.getvalue())["dry_run"])

    def test_direct_import_and_unconfirmed_candidates_are_separate(self):
        _, rows = importer.batch(Path(__file__).with_name("direct-imports.jsonl"))
        self.assertEqual(len(rows), 1)
        with self.assertRaises(ValueError):
            importer.batch(Path(__file__).with_name("direct-candidates.jsonl"))

    def test_refuse_publish_without_exact_batch_approval(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            importer.main([str(Path(__file__).with_name("launch-imports.jsonl")), "--publish"])

    def test_uncertain_retry_keeps_exact_signed_command(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(importer.swarmmemo, "public_bytes", return_value=b"a" * 32), contextlib.redirect_stdout(io.StringIO()):
            path = Path(directory) / "journal.json"
            first = FakeClient(fail=True)
            with self.assertRaises(OSError):
                importer.publish(self.rows[:1], first, path, self.digest)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            second = FakeClient()
            importer.publish(self.rows[:1], second, path, self.digest)
            self.assertEqual(first.sent, second.sent)
            self.assertEqual(second.prepared, [])
            importer.publish(self.rows[:1], second, path, self.digest)
            self.assertEqual(len(second.sent), 1)

    def test_changed_batch_refuses_old_journal(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(importer.swarmmemo, "public_bytes", return_value=b"a" * 32), contextlib.redirect_stdout(io.StringIO()):
            path = Path(directory) / "journal.json"
            importer.publish(self.rows[:1], FakeClient(), path, self.digest)
            with self.assertRaises(ValueError):
                importer.publish(self.rows[:1], FakeClient(), path, "different")


if __name__ == "__main__":
    unittest.main()
