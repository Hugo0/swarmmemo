"""Fixed local dispatch worker. Never a general subprocess or signing endpoint."""
from __future__ import annotations

import json
import os
from pathlib import Path
import sys

# -I disables ambient Python paths; only our reviewed sibling modules are loaded.
sys.path.insert(0, str(Path(__file__).resolve().parent))
MAX_BYTES = 256 * 1024


def bind_parent(expected_parent):
    """Arm Linux death coupling before accepting any work; never use preexec."""
    import ctypes
    import signal
    if sys.platform != "linux" or expected_parent <= 1:
        raise ValueError("worker_parent_lost")
    libc = ctypes.CDLL(None, use_errno=True)
    prctl = libc.prctl
    prctl.restype = ctypes.c_int
    # PR_SET_PDEATHSIG; failure is fail-closed, before key/profile imports.
    if prctl(1, signal.SIGKILL, 0, 0, 0) != 0:
        raise ValueError("worker_parent_lost")
    # Covers parent death before/during arming. getppid refers to the actual
    # parent relationship, not a possibly reused unrelated PID lookup.
    if os.getppid() != expected_parent:
        raise ValueError("worker_parent_lost")


def strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result: raise ValueError("invalid_json")
            result[key] = value
        return result
    def constant(_): raise ValueError("invalid_json")
    return json.loads(raw.decode("utf-8"), object_pairs_hook=pairs, parse_constant=constant)


def encoded(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode("utf-8")


def main():
    if len(sys.argv) != 3: raise ValueError()
    bind_parent(int(sys.argv[2]))
    from policy import BridgeError, load_profile
    from operations import dispatch
    try:
        raw = sys.stdin.buffer.read(MAX_BYTES + 1)
        if len(raw) > MAX_BYTES: raise ValueError()
        request = strict_json(raw)
        if (not isinstance(request, dict) or set(request) != {"action", "arguments", "expected_profile_fingerprint"}
                or not isinstance(request["action"], str) or not isinstance(request["arguments"], dict)):
            raise ValueError()
        profile = load_profile(Path(sys.argv[1]))
        if request["expected_profile_fingerprint"] != profile.fingerprint:
            result = {"ok": False, "error": "profile_changed"}
        else:
            result = {"ok": True, "result": dispatch(profile, request["action"], request["arguments"])}
        raw = encoded(result)
        if len(raw) > MAX_BYTES: raw = encoded({"ok": False, "error": "response_too_large"})
    except BridgeError as exc:
        raw = encoded({"ok": False, "error": exc.code})
    except BaseException:
        raw = encoded({"ok": False, "error": "worker_failed"})
    sys.stdout.buffer.write(raw)
    sys.stdout.buffer.flush()


if __name__ == "__main__":
    try: main()
    except BaseException:
        # Import/setup/pipe failures must not leak traceback, paths or payload.
        sys.exit(1)
