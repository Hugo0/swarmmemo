"""Local attachment files are uploaded to the post's room before their IDs are attached."""
import contextlib
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import swarmmemo as memo


class AttachmentFiles(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory()
        self.addCleanup(self.folder.cleanup)
        self.file = Path(self.folder.name) / "report.txt"
        self.file.write_bytes(b"checked evidence\n")
        self.sent = []
        self.key = memo.crypto()[0].from_private_bytes(bytes(range(32)))

    def run_post(self, extra):
        def fake(client, path, body=None):
            self.sent.append(body)
            if body["operation"] == "blob.put":
                n = sum(row["operation"] == "blob.put" for row in self.sent)
                return {"ok": True, "data": {"blob": {"id": f"{n:032x}"}}}
            return {"ok": True, "receipt": {"id": "f" * 32}}
        with patch.object(memo, "load_key", return_value=self.key), \
                patch.object(memo.Client, "_request", fake), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            return memo.main(["--key", "test-key.json", "post", "research", "main", "Evidence", *extra])

    def test_upload_and_attach_to_the_same_room(self):
        existing = "a" * 32
        self.assertEqual(self.run_post(["--attachment", existing, "--attachment-file", str(self.file)]), 0)
        self.assertEqual([row["operation"] for row in self.sent], ["blob.put", "post"])
        uploaded, posted = self.sent
        self.assertEqual(uploaded["room"], "research")
        self.assertEqual(posted["room"], "research")
        self.assertEqual(memo.unb64(uploaded["data"]), self.file.read_bytes())
        self.assertEqual(uploaded["filename"], "report.txt")
        self.assertEqual(uploaded["media_type"], "text/plain")
        self.assertEqual(posted["attachments"], [existing, "0" * 31 + "1"])

    def test_explicit_request_id_keeps_upload_and_post_retry_ids(self):
        args = ["--request-id", "same-request", "--attachment-file", str(self.file)]
        self.assertEqual(self.run_post(args), 0)
        first = [row["request_id"] for row in self.sent]
        self.sent.clear()
        self.assertEqual(self.run_post(args), 0)
        self.assertEqual([row["request_id"] for row in self.sent], first)
        self.assertNotEqual(first[0], first[1])

    def test_missing_second_file_writes_nothing(self):
        self.assertEqual(self.run_post(["--attachment-file", str(self.file), "--attachment-file", str(self.file.with_name("missing.txt"))]), 1)
        self.assertEqual(self.sent, [])

    def test_oversized_file_writes_nothing(self):
        self.file.write_bytes(b"x" * (memo.ATTACHMENT_BYTES + 1))
        self.assertEqual(self.run_post(["--attachment-file", str(self.file)]), 1)
        self.assertEqual(self.sent, [])

    def test_too_many_files_write_nothing(self):
        args = [arg for _ in range(memo.ATTACHMENTS_PER_MESSAGE + 1) for arg in ("--attachment-file", str(self.file))]
        self.assertEqual(self.run_post(args), 1)
        self.assertEqual(self.sent, [])

    def test_save_request_is_refused_before_upload(self):
        with patch.object(memo.Client, "_request", side_effect=AssertionError("nothing may be sent")), \
                contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as stopped:
            memo.main(["--save-request", str(Path(self.folder.name) / "request.json"), "post", "research", "main", "Evidence", "--attachment-file", str(self.file)])
        self.assertEqual(stopped.exception.code, 2)
        self.assertFalse((Path(self.folder.name) / "request.json").exists())

    def test_get_transport_is_refused_before_upload(self):
        self.assertEqual(self.run_post(["--transport", "get", "--attachment-file", str(self.file)]), 1)
        self.assertEqual(self.sent, [])


if __name__ == "__main__":
    unittest.main()
