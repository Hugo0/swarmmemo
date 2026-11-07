#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Build the OpenAI plugin upload folder from plugins/swarmmemo.

    python3 plugins/openai_package.py OUT_DIR [--zip OUT.zip]

OUT_DIR/swarmmemo/ is plugins/swarmmemo/ less the Claude and Cursor manifests,
its root mcp.json kept in the agent-plugins format ($schema, streamable-http)
with only the URL swapped for .codex-plugin/mcp.json's (the directory server,
/mcp/core): OpenAI's validator reads the root one, refuses a URL other than
the registered app's, and counts no server in the Codex format. Skills that name a tool /mcp/core leaves out are left
out. --zip also writes a deterministic zip of that folder.
"""

import argparse
import json
import pathlib
import re
import shutil
import sys
import zipfile

SOURCE = pathlib.Path(__file__).resolve().parent / "swarmmemo"
DROP = {".claude-plugin", ".cursor-plugin", ".git", "__pycache__"}
# Tools /mcp/core does not list (internal/httpapi/mcp_core.go coreExcluded).
CORE_LACKS = re.compile(r"\b(screen_[a-z_]+|public_data_[a-z_]+|fetch_page|tools_search|tools_call|x402_[a-z_]+|inference_complete|corroborate_resolve)\b")
ZIP_TIME = (1980, 1, 1, 0, 0, 0)


def build(out_dir):
    out_dir = pathlib.Path(out_dir)
    if out_dir.exists() and any(out_dir.iterdir()):
        raise SystemExit(f"{out_dir} is not empty")
    dest = out_dir / "swarmmemo"
    shutil.copytree(SOURCE, dest, ignore=lambda d, names: [n for n in names if n in DROP])
    root = json.loads((SOURCE / "mcp.json").read_text(encoding="utf-8"))
    codex = json.loads((SOURCE / ".codex-plugin" / "mcp.json").read_text(encoding="utf-8"))
    if set(root["mcpServers"]) != set(codex["mcpServers"]):
        raise SystemExit("mcp.json and .codex-plugin/mcp.json name different servers")
    for name, server in root["mcpServers"].items():
        server["url"] = codex["mcpServers"][name]["url"]
    (dest / "mcp.json").write_text(json.dumps(root, indent=2) + "\n", encoding="utf-8")
    for skill in sorted((dest / "skills").glob("*/SKILL.md")):
        if CORE_LACKS.search(skill.read_text(encoding="utf-8")):
            shutil.rmtree(skill.parent)
    return dest


def write_zip(folder, zip_path):
    folder = pathlib.Path(folder)
    files = sorted(p for p in folder.rglob("*") if p.is_file())
    with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED) as z:
        for path in files:
            info = zipfile.ZipInfo(f"{folder.name}/{path.relative_to(folder).as_posix()}", ZIP_TIME)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = 0o644 << 16
            z.writestr(info, path.read_bytes())


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("out_dir")
    parser.add_argument("--zip", dest="zip_path")
    args = parser.parse_args(argv)
    dest = build(args.out_dir)
    if args.zip_path:
        write_zip(dest, args.zip_path)
    print(dest)


if __name__ == "__main__":
    sys.exit(main())
