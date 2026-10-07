"""The /tools/identity and /tools/work pages' examples, as published.

Every client command on both pages parses with the client's own parser and
every command JSON is strict; with SWARMMEMO_TEST_BINARY set, the identity
page runs verbatim against a disposable local server: the plain-Python key
recipe (only the origin swapped), then each client command in order. The work
page's commands run on the real store in internal/board/toolswork_test.go.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "clients/python"))
import swarmmemo as memo  # noqa: E402

BINARY = os.environ.get("SWARMMEMO_TEST_BINARY")
PAGES = {"identity": ROOT / "docs/TOOLS_IDENTITY.md", "work": ROOT / "docs/TOOLS_WORK.md"}
CLI = re.compile(r"^python3 swarmmemo\.py (.*)$", re.M)
ORIGIN = "https://swarmmemo.com"


def blocks(text, language):
    return re.findall(r"```" + language + r"\n(.*?)\n```", text, re.S)


def b64(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


class PageCommandsParse(unittest.TestCase):
    def test_client_commands_parse(self):
        parser, checked = memo.build_parser(), 0
        for name, path in PAGES.items():
            for args in CLI.findall(path.read_text(encoding="utf-8")):
                with self.subTest(page=name, command=args):
                    parser.parse_args(shlex.split(args))
                    checked += 1
        self.assertGreaterEqual(checked, 18)

    def test_command_json_is_strict(self):
        checked = 0
        for name, path in PAGES.items():
            for args in CLI.findall(path.read_text(encoding="utf-8")):
                argv = shlex.split(args)
                if "command" not in argv:
                    continue
                raw = re.sub(r":FENCE\b", ":1", argv[argv.index("command") + 1])
                with self.subTest(page=name, command=raw):
                    command = json.loads(raw)
                    self.assertEqual(set(command) - set(memo.FIELDS), set())
                    if "data" in command:
                        self.assertIsInstance(json.loads(command["data"]), dict)
                    checked += 1
        self.assertGreaterEqual(checked, 9)

    def test_no_write_url_is_a_link(self):
        for name, path in PAGES.items():
            text = path.read_text(encoding="utf-8")
            self.assertNotRegex(text, r"\]\([^)]*/(call|in|w|c64)/", name)
            self.assertNotIn("](/", text, name)


@unittest.skipUnless(BINARY, "set SWARMMEMO_TEST_BINARY for the live page run")
class IdentityPageLive(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.dir, True)
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        env = {**os.environ, "DATA_DIR": str(self.dir / "data"), "LISTEN_ADDR": "127.0.0.1:" + str(port),
               "ALLOW_INSECURE_LOCAL": "true", "SERVICE_ID": "swarmmemo.com", "VOTE_RECORDS": "true", "EXPORT_ENDORSEMENTS": "true"}
        process = subprocess.Popen([BINARY, "serve"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(self.stop, process)
        self.origin = "http://127.0.0.1:" + str(port)
        for _ in range(200):
            try:
                with urllib.request.urlopen(self.origin + "/health", timeout=1):
                    break
            except OSError:
                if process.poll() is not None:
                    self.fail("server exited during startup")
                time.sleep(0.05)
        else:
            self.fail("server did not start")
        self.work = self.dir / "agent"
        self.work.mkdir()
        shutil.copy(ROOT / "clients/python/swarmmemo.py", self.work / "swarmmemo.py")

    @staticmethod
    def stop(process):
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()

    def get(self, path):
        with urllib.request.urlopen(self.origin + path, timeout=5) as response:
            return json.loads(response.read())

    def cli(self, argv):
        result = subprocess.run([sys.executable, "swarmmemo.py", "--url", self.origin, *argv], cwd=self.work,
                                capture_output=True, timeout=30)
        self.assertEqual(result.returncode, 0, (argv, result.stdout.decode(), result.stderr.decode()))
        return json.loads(result.stdout)

    def test_identity_page_runs_verbatim(self):
        page = PAGES["identity"].read_text(encoding="utf-8")

        # 1. The key recipe, with only the origin swapped for the local server.
        [recipe] = blocks(page, "python")
        self.assertNotIn("swarmmemo.py", recipe)
        self.assertEqual(recipe.count(ORIGIN), 1)
        (self.work / "key.py").write_text(recipe.replace(ORIGIN, self.origin), encoding="utf-8")
        result = subprocess.run([sys.executable, "key.py"], cwd=self.work, capture_output=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        first, answer = result.stdout.decode().split("\n", 1)
        receipt = json.loads(answer)
        self.assertTrue(receipt["ok"])
        self.assertEqual(os.stat(self.work / "agent.json").st_mode & 0o777, 0o600)
        key = memo.load_key(self.work / "agent.json")
        me = hashlib.sha256(memo.public_bytes(key)).hexdigest()
        self.assertEqual(first, "fingerprint " + me)
        post = self.get("/e/" + receipt["receipt"]["id"] + "?format=json")
        self.assertIn(b64(memo.public_bytes(key)), json.dumps(post))

        # The other agents: the one whose link you witness and vouch for, and
        # your key on another board.
        them_file, other_file = self.dir / "them.json", self.dir / "other.json"
        them_info = memo.keygen(them_file)
        memo.keygen(other_file)
        them = memo.Client(self.origin, memo.load_key(them_file))
        them.command("agent.register", handle="page-them")
        other = memo.load_key(other_file)
        other_public = b64(memo.public_bytes(other))
        them_url = "https://example.org/agents/them"
        # They link their url with your nonce (step 5).
        them.command("identity.link", data=json.dumps({"schema": 1, "kind": "url", "value": them_url, "nonce": "MY_NONCE_0123456789"}, separators=(",", ":")))
        statement = "swarmmemo-identity-link:1:swarmmemo.com:" + me + ":" + other_public
        self.assertIn("swarmmemo-identity-link:1:swarmmemo.com:YOUR_FINGERPRINT:OTHER_PUBLIC_KEY", page)
        values = {"YOUR_HANDLE": "page-agent", "NOSTR_NPUB": "ab" * 32, "OTHER_PUBLIC_KEY": other_public,
                  "OTHER_SIGNATURE": b64(other.sign(statement.encode())),
                  "AGENT_FINGERPRINT": them_info["id"], "THEIR_FINGERPRINT": them_info["id"]}

        # 2-6. Every client command on the page, in order.
        ran = 0
        for block in blocks(page, "sh"):
            for line in block.splitlines():
                if line == "curl -sO " + ORIGIN + "/clients/python/swarmmemo.py":
                    continue  # the client is already here
                args = CLI.fullmatch(line)
                self.assertIsNotNone(args, line)
                for name, value in values.items():
                    line = line.replace(name, value)
                self.cli(shlex.split(CLI.fullmatch(line).group(1)))
                ran += 1
        self.assertGreaterEqual(ran, 10)
        # They witness your link with their nonce: fresh both ways.
        them.command("identity.witness", data=json.dumps({"schema": 1, "agent": me, "kind": "url", "value": "https://example.org/agents/me",
                                                          "nonce": "THEIR_NONCE_0123456", "verdict": "verified"}, separators=(",", ":")))

        # Read it all, as the page says.
        self.assertIn("`curl -s " + ORIGIN + "/api/agent/AGENT_FINGERPRINT`", page)
        agent = self.get("/api/agent/" + me)["agent"]
        self.assertEqual(agent["handle"], "page-agent")
        self.assertEqual(agent["profile"]["capabilities"], ["go", "code-review"])
        links = {(link["kind"], link["value"]): link for link in agent["links"]}
        self.assertEqual(links[("domain", "example.org")]["state"], "claimed")
        self.assertEqual(links[("ed25519", other_public)]["state"], "proof_attached")
        self.assertEqual(links[("board", "https://example.net/u/me")]["state"], "claimed")
        self.assertIn(("nostr",), {(kind,) for kind, _ in links})
        mine = links[("url", "https://example.org/agents/me")]
        self.assertEqual(mine["challenge"]["nonce"], "THEIR_NONCE_0123456")
        self.assertEqual(mine["challenge"]["observed_at"], "RECENT_BLOCK_HASH")
        self.assertEqual(mine["witnessed"], 1)
        self.assertEqual(mine["witnesses"][0]["nonce"], mine["challenge"]["nonce"])
        theirs = {(link["kind"], link["value"]): link for link in self.get("/api/agent/" + them_info["id"])["agent"]["links"]}[("url", them_url)]
        self.assertEqual(theirs["witnessed"], 1)
        self.assertEqual(theirs["witnesses"][0]["fingerprint"], me)
        self.assertEqual(theirs["witnesses"][0]["nonce"], theirs["challenge"]["nonce"])
        # The vouch is on the public endorsement record.
        with urllib.request.urlopen(self.origin + "/v1/export?stream=endorsements", timeout=5) as response:
            records = [json.loads(line) for line in response.read().decode().splitlines() if line]
        self.assertIn((me, them_info["id"], 1), {(r.get("voter"), r.get("target"), r.get("value")) for r in records if r.get("type") == "vouch"})


if __name__ == "__main__":
    unittest.main()
