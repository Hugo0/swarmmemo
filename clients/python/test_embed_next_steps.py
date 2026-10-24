"""Embed setup's copyable commands must keep the instance and signing identity.

The transport is entirely in memory: no request reaches a real server.
"""
import contextlib
import io
from pathlib import Path
import shlex
import tempfile
import unittest
from unittest.mock import patch

import swarmmemo as memo
from test_helpers import FakeBoard


class EmbedNextSteps(unittest.TestCase):
    def setup_output(self, origin, service, key):
        board = FakeBoard()
        with patch.object(memo.Client, "_request", lambda client, path, body=None: board(client, path, body)), \
                contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()):
            status = memo.main(["--url", origin, "--service", service, "--key", str(key),
                                "embed", "setup", "example-site", "--page", "intro"])
        self.assertEqual(status, 0, out.getvalue())
        commands = [shlex.split(line.split("python3 swarmmemo.py ", 1)[1])
                    for line in out.getvalue().splitlines() if "python3 swarmmemo.py " in line]
        self.assertEqual(len(commands), 5)
        return commands

    def test_each_next_step_keeps_the_configured_instance(self):
        with tempfile.TemporaryDirectory() as directory:
            key = Path(directory) / "site.json"
            for origin, service in (("http://127.0.0.1:9", "local.example"),
                                    ("https://memo.example/", "memo.example"),
                                    ("https://swarmmemo.com", memo.SERVICE)):
                for command in self.setup_output(origin, service, key):
                    with self.subTest(origin=origin, command=command):
                        args = memo.build_parser().parse_args(command)
                        self.assertEqual(args.url, origin.rstrip("/"))
                        self.assertEqual(args.service, service)
                        self.assertEqual(args.key, key)

    def test_copyable_key_argument_survives_spaces_and_shell_characters(self):
        with tempfile.TemporaryDirectory() as directory:
            key = Path(directory) / "site's $test key.json"
            for command in self.setup_output("https://memo.example", "memo.example", key):
                with self.subTest(command=command):
                    self.assertEqual(memo.build_parser().parse_args(command).key, key)

    def test_printed_updates_command_uses_the_same_instance(self):
        with tempfile.TemporaryDirectory() as directory:
            key = Path(directory) / "site.json"
            commands = self.setup_output("http://127.0.0.1:9", "local.example", key)
            command = next(argv for argv in commands if "updates" in argv)
            observed = []

            def read(client, path, body=None):
                observed.append((client.base_url, client.service, body["operation"]))
                return {"ok": True, "messages": []}

            with patch.object(memo.Client, "_request", read), \
                    contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(memo.main(command), 0)
            self.assertEqual(observed, [("http://127.0.0.1:9", "local.example", "updates.get")])


if __name__ == "__main__":
    unittest.main()
