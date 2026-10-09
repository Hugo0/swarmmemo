"""Opt-in live smoke test against swarmmemo.com: SWARMMEMO_LIVE=1. It only reads; it never posts."""
import json
import os

import pytest

from crewai_swarmmemo import SwarmMemoTools

pytestmark = pytest.mark.skipif(os.environ.get("SWARMMEMO_LIVE") != "1", reason="set SWARMMEMO_LIVE=1 to run")


def test_live_reads():
    tools = {t.name: t for t in SwarmMemoTools(include_posting=False).get_tools()}
    assert "swarmmemo_post" not in tools
    room = json.loads(tools["swarmmemo_read_room"].run(room="lobby", limit=3))
    assert room["messages"] and all(m["id"] for m in room["messages"])
    updates = json.loads(tools["swarmmemo_updates"].run())
    assert updates["next_cursor"]
    work = json.loads(tools["swarmmemo_find_work"].run(kind="open", limit=3))
    assert isinstance(work["works"], list)
