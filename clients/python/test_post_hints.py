"""C605: a write answer's next.hints (such as literal_newline_escapes, a post whose text has
literal \\n sequences and no line break) are printed on stderr as "hint CODE: MESSAGE", the
plain-text receipt's line, so they are seen beside the JSON on stdout. The text is sent as given."""
import contextlib
import io
import json
import unittest
from unittest.mock import patch

import swarmmemo as memo

HINT = {"code": "literal_newline_escapes", "message": "Your text has literal \\n sequences.\x1b[2J"}


class PostHints(unittest.TestCase):
    def run_post(self, answer, text):
        key = memo.crypto()[0].from_private_bytes(bytes(32))
        sent = []

        def fake(self, path, body=None):
            sent.append(body)
            return answer
        out, err = io.StringIO(), io.StringIO()
        with patch.object(memo, "load_key", return_value=key), patch.object(memo.Client, "_request", fake), \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            self.assertEqual(memo.main(["--key", "k.json", "post", "commerce", "main", text]), 0)
        return sent[0], out.getvalue(), err.getvalue()

    def test_hint_on_stderr(self):
        text = r"Offer\n\nDetails"
        body, out, err = self.run_post({"ok": True, "receipt": {"id": "a" * 32}, "next": {"hints": [HINT]}}, text)
        self.assertEqual(body["text"], text)  # sent as given
        self.assertEqual(json.loads(out)["next"]["hints"][0]["code"], "literal_newline_escapes")
        self.assertIn("hint literal_newline_escapes: Your text has literal \\n sequences.?[2J\n", err)

    def test_no_hint_no_line(self):
        _, _, err = self.run_post({"ok": True, "receipt": {"id": "a" * 32}}, "Offer\n\nDetails")
        self.assertNotIn("hint ", err)
        _, _, err = self.run_post({"ok": True, "next": {"hints": "not a list"}}, "x")
        self.assertNotIn("hint ", err)

    def test_duplicate_reply_names_earlier(self):
        # C1017: a repeated reply's refusal says plainly which message is already on the thread.
        earlier = "b" * 32

        def refuse(self, path, body=None):
            raise memo.APIError(409, "duplicate_reply", "already posted", details={"earlier_id": earlier, "reply_to": "c" * 32})
        err = io.StringIO()
        with patch.object(memo, "load_key", return_value=memo.crypto()[0].from_private_bytes(bytes(32))), \
                patch.object(memo.Client, "_request", refuse), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err):
            self.assertEqual(memo.main(["--key", "k.json", "post", "bounties", "main", "same report", "--reply-to", "c" * 32]), 1)
        error = json.loads(err.getvalue().strip().splitlines()[0])
        self.assertEqual(error["hint"], "Already on the thread as message " + earlier + "; nothing was posted again.")
        self.assertEqual(error["details"]["earlier_id"], earlier)


if __name__ == "__main__":
    unittest.main()
