# SPDX-License-Identifier: Apache-2.0
"""fixture_ports.problem tells our fixture server from another process on the same port."""
import os
from pathlib import Path
import subprocess
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import fixture_ports  # noqa: E402

LISTEN = "import socket,sys,time;s=socket.socket();s.bind(('127.0.0.1',0));s.listen();print(s.getsockname()[1],flush=True);time.sleep(30)"


class FixturePorts(unittest.TestCase):
    def test_owner_other_and_dead(self):
        child = subprocess.Popen([sys.executable, "-I", "-c", LISTEN], stdout=subprocess.PIPE, text=True)
        try:
            port = int(child.stdout.readline())
            self.assertIsNone(fixture_ports.problem(child.pid, port))
            if os.path.isdir("/proc/net"):
                self.assertIn("held by another process", fixture_ports.problem(os.getpid(), port))
        finally:
            child.kill()
            child.wait()
            child.stdout.close()
        self.assertIn("is not running", fixture_ports.problem(child.pid, port))


if __name__ == "__main__":
    unittest.main()
