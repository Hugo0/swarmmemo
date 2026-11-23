# SPDX-License-Identifier: Apache-2.0
"""Is the disposable fixture server on this port ours?

The test fixtures pick a free port, close it and start a disposable server on it. When suites run
side by side, another process can take that port in between, and a readiness probe would then pass
against someone else's server. After readiness a fixture asks: is our process alive, and does it
hold the listening socket on the port?

    python3 scripts/fixture_ports.py PID PORT   # exit 0: ours; exit 1: not ours, with the reason
    problem(pid, port)                          # None when ours, else the reason

Ownership is read from Linux /proc; where /proc cannot tell, only the process check applies.
"""
import os
import sys


def _listening_inodes(port):
    """Socket inodes listening on TCP port, or None when /proc/net cannot tell."""
    found, readable = set(), False
    for table in ("/proc/net/tcp", "/proc/net/tcp6"):
        try:
            with open(table, encoding="ascii") as f:
                rows = f.read().splitlines()[1:]
        except OSError:
            continue
        readable = True
        for row in rows:
            fields = row.split()
            if len(fields) > 9 and fields[3] == "0A" and int(fields[1].rsplit(":", 1)[1], 16) == port:
                found.add(fields[9])
    return found if readable else None


def _socket_inodes(pid):
    inodes = set()
    for fd in os.listdir(f"/proc/{pid}/fd"):
        try:
            target = os.readlink(f"/proc/{pid}/fd/{fd}")
        except OSError:
            continue  # closed between listdir and readlink
        if target.startswith("socket:["):
            inodes.add(target[8:-1])
    return inodes


def problem(pid, port):
    """None when process pid is alive and holds the listening socket on port; else why not."""
    try:
        os.kill(pid, 0)
    except OSError:
        return f"fixture server pid {pid} is not running"
    listening = _listening_inodes(port)
    if listening is None:
        return None
    if not listening:
        return f"nothing listens on port {port}"
    try:
        mine = _socket_inodes(pid)
    except OSError as exc:
        return f"fixture server pid {pid}: cannot read its sockets ({exc})"
    if not listening & mine:
        return f"port {port} is held by another process, not fixture server pid {pid}"
    return None


if __name__ == "__main__":
    reason = problem(int(sys.argv[1]), int(sys.argv[2]))
    if reason:
        print(reason, file=sys.stderr)
        sys.exit(1)
