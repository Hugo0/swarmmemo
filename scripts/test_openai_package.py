# SPDX-License-Identifier: Apache-2.0
"""scripts/openai_package.py builds the OpenAI upload folder and zip."""

import json
import pathlib
import sys
import tempfile
import unittest
import zipfile

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import openai_package  # noqa: E402

SOURCE = openai_package.SOURCE
# TestPluginSkillsOnCoreProfile pins the same set from the Go side.
EXCLUDED = {"screen-before-acting"}


class PackageTests(unittest.TestCase):
    def build(self, tmp, name):
        out = pathlib.Path(tmp) / name
        zip_path = pathlib.Path(tmp) / f"{name}.zip"
        openai_package.main([str(out), "--zip", str(zip_path)])
        return out / "swarmmemo", zip_path

    def test_package(self):
        with tempfile.TemporaryDirectory() as tmp:
            dest, zip_one = self.build(tmp, "one")
            _, zip_two = self.build(tmp, "two")
            mcp = json.loads((dest / "mcp.json").read_text())
            self.assertEqual(mcp["mcpServers"]["swarmmemo"]["url"], "https://swarmmemo.com/mcp/core")
            # OpenAI parses the root file with the agent-plugins schema: a Codex-format
            # server ({"type": "http"}) counts as none (upload refused 11-07).
            source = json.loads((SOURCE / "mcp.json").read_text())
            self.assertEqual(mcp["$schema"], source["$schema"])
            self.assertEqual(mcp["mcpServers"]["swarmmemo"]["type"], "streamable-http")
            self.assertEqual(len(mcp["mcpServers"]), 1)
            source["mcpServers"]["swarmmemo"]["url"] = "https://swarmmemo.com/mcp/core"
            self.assertEqual(mcp, source)
            for name in (".claude-plugin", ".cursor-plugin", ".git", "__pycache__"):
                self.assertFalse((dest / name).exists(), name)
            skills = {p.name for p in (SOURCE / "skills").iterdir() if p.is_dir()}
            self.assertTrue(EXCLUDED <= skills)
            self.assertEqual({p.name for p in (dest / "skills").iterdir()}, skills - EXCLUDED)
            for name in ("plugin.json", ".codex-plugin/plugin.json", ".codex-plugin/mcp.json"):
                self.assertEqual((dest / name).read_bytes(), (SOURCE / name).read_bytes(), name)
            self.assertEqual(zip_one.read_bytes(), zip_two.read_bytes())
            with zipfile.ZipFile(zip_one) as z:
                names = z.namelist()
            self.assertEqual(names, sorted(names))
            self.assertTrue(all(n.startswith("swarmmemo/") for n in names))
            self.assertIn("swarmmemo/mcp.json", names)

    def test_refuses_non_empty_out_dir(self):
        with tempfile.TemporaryDirectory() as tmp:
            (pathlib.Path(tmp) / "x").write_text("x")
            with self.assertRaises(SystemExit):
                openai_package.build(tmp)


if __name__ == "__main__":
    unittest.main()
