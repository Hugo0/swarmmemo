"""Run the client tests with a disposable home and detect writes to the real one.

From the repository root, run python clients/python/run_isolated_tests.py.
Pass unittest arguments to select another suite, for example: discover -p "test_*.py".
"""

import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile


def snapshot(root: Path) -> dict[Path, tuple[int, ...]]:
    """Record names and metadata without reading contents or following links."""
    entries = {}
    pending = [root]
    while pending:
        path = pending.pop()
        try:
            info = path.lstat()
        except FileNotFoundError:
            continue
        entries[path.relative_to(root)] = (
            info.st_mode, info.st_size, info.st_mtime_ns, info.st_ctime_ns, info.st_ino,
        )
        if stat.S_ISDIR(info.st_mode) and not (
            getattr(info, "st_file_attributes", 0) & stat.FILE_ATTRIBUTE_REPARSE_POINT
        ):
            pending.extend(path.iterdir())
    return entries


def main() -> int:
    real_state = Path.home() / ".swarmmemo"
    before = snapshot(real_state)
    with tempfile.TemporaryDirectory(prefix="swarmmemo-test-home-") as home:
        env = {**os.environ, "HOME": home, "USERPROFILE": home}
        env.pop("SWARMMEMO_HOME", None)
        env.pop("SWARMMEMO_KEY", None)
        args = sys.argv[1:] or ["test_chat"]
        result = subprocess.run(
            [sys.executable, "-B", "-X", "utf8", "-m", "unittest", *args],
            cwd=Path(__file__).resolve().parent,
            env=env,
            check=False,
        )
    if snapshot(real_state) != before:
        print("Tests changed .swarmmemo outside the disposable home.", file=sys.stderr)
        return 1
    return result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
