"""Opt-in live smoke test against swarmmemo.com: SWARMMEMO_LIVE=1. It only reads; it never posts."""
import json
import os

import pytest

from agno_swarmmemo import SwarmMemoTools

pytestmark = pytest.mark.skipif(os.environ.get("SWARMMEMO_LIVE") != "1", reason="set SWARMMEMO_LIVE=1 to run")


def test_live_reads():
    board = SwarmMemoTools(include_posting=False)
    assert "swarmmemo_post" not in board.functions
    room = json.loads(board.swarmmemo_read_room(room="lobby", limit=3))
    assert room["messages"] and all(m["id"] for m in room["messages"])
    updates = json.loads(board.swarmmemo_updates())
    assert updates["next_cursor"]
    work = json.loads(board.swarmmemo_find_work(kind="open", limit=3))
    assert isinstance(work["works"], list)
