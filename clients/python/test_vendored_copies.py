"""Every copy of the client the repository ships elsewhere is byte-identical to
the one here: the framework integrations vendor it. The plugin ships none (the
Claude directory holds a plugin whose scripts it cannot inspect)."""
from pathlib import Path
import re
import tomllib
import unittest

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
COPIES = {
    "swarmmemo.py": [
        "integrations/langchain/langchain_swarmmemo/_swarmmemo.py",
        "integrations/crewai/crewai_swarmmemo/_swarmmemo.py",
        "integrations/agno/agno_swarmmemo/_swarmmemo.py",
    ],
}


class VendoredCopyTests(unittest.TestCase):
    def test_copies_match_the_client(self):
        checked = 0
        for name, copies in COPIES.items():
            upstream = (HERE / name).read_bytes()
            for copy in copies:
                path = ROOT / copy
                if not path.is_file():
                    continue  # a public snapshot that leaves this copy out
                with self.subTest(copy=copy):
                    self.assertEqual(path.read_bytes(), upstream, f"refresh it: cp clients/python/{name} {copy}")
                checked += 1
        if (ROOT / "integrations").is_dir():
            self.assertGreaterEqual(checked, 3)
        self.assertFalse((ROOT / "plugins/swarmmemo/skills/talk-privately/scripts").exists(), "the plugin ships no code")

    def test_integration_versions_have_one_source(self):
        """Each integration's version is its package's __version__ alone: pyproject.toml
        reads it from there (hatch dynamic version), so the two cannot drift (C1148)."""
        checked = 0
        for project in sorted((ROOT / "integrations").glob("*/pyproject.toml")):
            with self.subTest(project=str(project.relative_to(ROOT))):
                with project.open("rb") as f:
                    meta = tomllib.load(f)
                self.assertNotIn("version", meta["project"], "set the version in the package's __version__ only")
                self.assertIn("version", meta["project"].get("dynamic", []))
                source = project.parent / meta["tool"]["hatch"]["version"]["path"]
                found = re.findall(r'^__version__ = "([0-9]+\.[0-9]+\.[0-9]+)"$', source.read_text(), re.M)
                self.assertEqual(len(found), 1, f"{source} sets __version__ once, as a literal")
                checked += 1
        if (ROOT / "integrations").is_dir():
            self.assertGreaterEqual(checked, 3)


if __name__ == "__main__":
    unittest.main()
