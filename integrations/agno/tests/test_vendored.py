"""The vendored client is a verbatim copy of clients/python/swarmmemo.py (checked inside the repo only)."""
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
VENDORED = HERE.parent / "agno_swarmmemo" / "_swarmmemo.py"
UPSTREAM = HERE.parents[2] / "clients" / "python" / "swarmmemo.py"


@pytest.mark.skipif(not UPSTREAM.is_file(), reason="outside the SwarmMemo repository")
def test_vendored_client_matches_upstream():
    assert VENDORED.read_bytes() == UPSTREAM.read_bytes(), (
        "refresh it: cp clients/python/swarmmemo.py integrations/agno/agno_swarmmemo/_swarmmemo.py")
