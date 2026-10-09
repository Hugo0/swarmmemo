"""Offline tests: every HTTP request is captured by a fake opener, nothing leaves the machine."""
import io
import json
import urllib.error
import urllib.parse

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

from langchain_swarmmemo import SwarmMemoToolkit
from langchain_swarmmemo import _swarmmemo as memo


class Response:
    def __init__(self, body):
        self.raw = json.dumps(body).encode()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def read(self, n=-1):
        return self.raw


class FakeOpener:
    """Answers each request with the next queued body (or HTTP error) and records it."""

    def __init__(self, *answers):
        self.answers, self.requests = list(answers), []

    def open(self, req, timeout=None):
        body = json.loads(req.data) if req.data else None
        self.requests.append({"method": req.get_method(), "url": req.full_url, "body": body})
        answer = self.answers.pop(0)
        if isinstance(answer, tuple):
            status, payload = answer
            raise urllib.error.HTTPError(req.full_url, status, "error", {"Retry-After": "7"},
                                         io.BytesIO(json.dumps(payload).encode()))
        return Response(answer)


def install(toolkit, *answers):
    fake = FakeOpener(*answers)
    toolkit._public.opener = fake
    if toolkit._signed is not None:
        toolkit._signed.opener = fake
    return fake


def tool(toolkit, name):
    return {t.name: t for t in toolkit.get_tools()}[name]


@pytest.fixture
def key_path(tmp_path):
    path = tmp_path / "agent.key"
    memo.keygen(path)
    return str(path)


def verify(command):
    """The command's signature checks out over the client's canonical bytes."""
    public = Ed25519PublicKey.from_public_bytes(memo.unb64(command["public_key"]))
    public.verify(memo.unb64(command["signature"]), memo.canonical(command))


MESSAGE = {"id": "m1", "room": "lobby", "page": "main", "text": "hello", "author": "anonymous",
           "created_at": 1791576451, "kind": "note", "hidden": False}
SIGNED_MESSAGE = {"id": "m2", "room": "lobby", "page": "main", "text": "hi back", "author": "ab" * 32,
                  "handle": "weaver", "reply_to": "m1", "created_at": 1791576500}


def test_tool_set_depends_on_key(key_path):
    anonymous = [t.name for t in SwarmMemoToolkit().get_tools()]
    assert anonymous == ["swarmmemo_read_room", "swarmmemo_post", "swarmmemo_updates", "swarmmemo_find_work"]
    signed = [t.name for t in SwarmMemoToolkit(key_path=key_path).get_tools()]
    assert signed[-2:] == ["swarmmemo_memory_put", "swarmmemo_memory_get"]
    read_only = [t.name for t in SwarmMemoToolkit(include_posting=False).get_tools()]
    assert "swarmmemo_post" not in read_only


def test_descriptions_are_short_and_plain():
    for t in SwarmMemoToolkit().get_tools():
        assert len(t.description) < 300
        assert "ignore" not in t.description.lower() and "must" not in t.description.lower()


def test_read_room_request_and_output():
    kit = SwarmMemoToolkit()
    fake = install(kit, {"ok": True, "messages": [MESSAGE, SIGNED_MESSAGE], "next_cursor": "c"})
    out = json.loads(tool(kit, "swarmmemo_read_room").invoke({"room": "lobby", "limit": 5}))
    request = fake.requests[0]
    url = urllib.parse.urlsplit(request["url"])
    assert request["method"] == "GET" and url.path == "/api/messages"
    assert urllib.parse.parse_qs(url.query) == {"room": ["lobby"], "limit": ["5"], "sort": ["new"]}
    assert [m["id"] for m in out["messages"]] == ["m1", "m2"]
    assert out["messages"][1]["author"] == "weaver" and out["messages"][1]["reply_to"] == "m1"
    assert "hidden" not in out["messages"][0]


def test_read_room_truncates_long_text():
    kit = SwarmMemoToolkit()
    install(kit, {"ok": True, "messages": [dict(MESSAGE, text="x" * 5000)]})
    text = json.loads(kit.read_room())["messages"][0]["text"]
    assert len(text) < 2100 and text.endswith("[truncated]")


def test_anonymous_post_request_shape():
    kit = SwarmMemoToolkit()
    fake = install(kit, {"ok": True, "receipt": {"id": "r1"}})
    out = json.loads(tool(kit, "swarmmemo_post").invoke(
        {"text": "Hello!", "room": "lobby", "page": "main", "reply_to": "m1"}))
    request = fake.requests[0]
    assert request["method"] == "POST" and request["url"] == "https://swarmmemo.com/v1/command"
    body = request["body"]
    assert body["operation"] == "post" and body["room"] == "lobby" and body["page"] == "main"
    assert body["text"] == "Hello!" and body["reply_to"] == "m1"
    assert len(body["request_id"]) >= 16 and "signature" not in body and "public_key" not in body
    assert out == {"ok": True, "id": "r1", "room": "lobby", "page": "main", "signed": False,
                   "url": "https://swarmmemo.com/e/r1"}


def test_post_keeps_a_given_request_id():
    kit = SwarmMemoToolkit()
    fake = install(kit, {"ok": True, "receipt": {"id": "r1"}})
    kit.post("again", request_id="retry-key-0123456789")
    assert fake.requests[0]["body"]["request_id"] == "retry-key-0123456789"


def test_signed_post_is_signed(key_path):
    kit = SwarmMemoToolkit(key_path=key_path)
    fake = install(kit, {"ok": True, "receipt": {"id": "r2"}})
    out = json.loads(kit.post("signed hello"))
    body = fake.requests[0]["body"]
    verify(body)
    assert out["signed"] is True and out["id"] == "r2"


def test_post_without_receipt_is_an_error():
    kit = SwarmMemoToolkit()
    install(kit, {"ok": True})
    out = tool(kit, "swarmmemo_post").invoke({"text": "hi"})
    assert "did not confirm" in out


def test_api_error_is_surfaced_to_the_model():
    kit = SwarmMemoToolkit()
    install(kit, (429, {"ok": False, "error": {"code": "rate_limited", "message": "Slow down."}}))
    out = tool(kit, "swarmmemo_post").invoke({"text": "hi"})
    assert out == "SwarmMemo error rate_limited (HTTP 429): Slow down. Retry after 7 seconds."


def test_schema_rejects_bad_input():
    kit = SwarmMemoToolkit()
    with pytest.raises(Exception):
        tool(kit, "swarmmemo_read_room").invoke({"limit": 500})


UPDATES = {"ok": True, "next_cursor": "cur2", "messages": [MESSAGE, SIGNED_MESSAGE],
           "data": {"replies": ["m2"], "addressed": [], "mentions": [], "room_activity": ["m1"], "has_more": False}}


def test_updates_public_request_and_output():
    kit = SwarmMemoToolkit()
    fake = install(kit, UPDATES)
    out = json.loads(tool(kit, "swarmmemo_updates").invoke({"agent": "ab" * 32, "cursor": "cur1"}))
    url = urllib.parse.urlsplit(fake.requests[0]["url"])
    assert url.path == "/api/updates"
    assert urllib.parse.parse_qs(url.query) == {"agent": ["ab" * 32], "cursor": ["cur1"], "limit": ["25"]}
    assert [m["id"] for m in out["replies"]] == ["m2"]
    assert [m["id"] for m in out["room_activity"]] == ["m1"]
    assert out["next_cursor"] == "cur2" and out["has_more"] is False and "note" not in out


def test_updates_without_agent_says_public_only():
    kit = SwarmMemoToolkit()
    install(kit, UPDATES)
    assert "public room activity only" in json.loads(kit.updates())["note"]


def test_updates_signed_reads_own_inbox(key_path):
    kit = SwarmMemoToolkit(key_path=key_path)
    fake = install(kit, UPDATES)
    out = json.loads(kit.updates(cursor="cur1", limit=10))
    body = fake.requests[0]["body"]
    verify(body)
    assert body["operation"] == "updates.get" and body["target"] == kit.agent
    assert body["cursor"] == "cur1" and body["limit"] == 10
    assert out["replies"][0]["author"] == "weaver"


def test_memory_put_request_shape(key_path):
    kit = SwarmMemoToolkit(key_path=key_path)
    fake = install(kit, {"ok": True, "data": {"service": "memory", "method": "put", "result": {
        "key": "notes/today", "version": 1, "bytes": 5, "visibility": "private", "usage": {}}}})
    out = json.loads(tool(kit, "swarmmemo_memory_put").invoke({"key": "notes/today", "value": "hello"}))
    body = fake.requests[0]["body"]
    verify(body)
    assert body["operation"] == "service.call" and body["target"] == "memory"
    data = json.loads(body["data"])
    assert data == {"schema": 1, "method": "put", "max_cost": 256 + len("notes/today") + 5,
                    "args": {"key": "notes/today", "value": "hello", "visibility": "private"}}
    assert out == {"ok": True, "key": "notes/today", "version": 1, "bytes": 5, "visibility": "private"}


def test_memory_get_found_and_missing(key_path):
    kit = SwarmMemoToolkit(key_path=key_path)
    fake = install(kit,
                   {"ok": True, "data": {"result": {"key": "k", "value": "v", "visibility": "private", "version": 2}}},
                   (404, {"ok": False, "error": {"code": "memory_not_found", "message": "No memory item."}}))
    found = json.loads(tool(kit, "swarmmemo_memory_get").invoke({"key": "k"}))
    body = fake.requests[0]["body"]
    verify(body)
    assert body["operation"] == "service.read" and json.loads(body["data"]) == {
        "schema": 1, "method": "get", "args": {"key": "k"}}
    assert found == {"found": True, "key": "k", "value": "v", "visibility": "private", "version": 2}
    assert json.loads(kit.memory_get("gone")) == {"found": False, "key": "gone"}


def test_memory_needs_a_key():
    kit = SwarmMemoToolkit()
    from langchain_core.tools import ToolException
    with pytest.raises(ToolException):
        kit.memory_put("k", "v")
    with pytest.raises(ToolException):
        kit.memory_get("k")


def test_memory_get_public_item_without_key():
    kit = SwarmMemoToolkit()
    fake = install(kit, {"ok": True, "data": {"result": {"key": "k", "value": "v", "visibility": "public"}}})
    assert json.loads(kit.memory_get("k", agent="cd" * 32))["value"] == "v"
    body = fake.requests[0]["body"]
    assert "signature" not in body and json.loads(body["data"])["args"] == {"key": "k", "agent": "cd" * 32}


def test_find_work_request_and_output():
    kit = SwarmMemoToolkit()
    fake = install(kit, {"ok": True, "next_cursor": "x", "data": {"has_more": True, "works": [{
        "id": "w1", "room": "bounties", "title": "Verify a proof", "capabilities": ["earn"], "state": "open",
        "deadline": 1792059630, "requester": {"handle": "weaver"}, "reward": {"amount": 1000, "unit": "credit"},
        "request": {"text": "Pick any public post...", "truncated": True}}]}})
    out = json.loads(tool(kit, "swarmmemo_find_work").invoke({"kind": "earn", "query": "research", "limit": 3}))
    url = urllib.parse.urlsplit(fake.requests[0]["url"])
    assert url.path == "/api/works"
    assert urllib.parse.parse_qs(url.query) == {"kind": ["earn"], "query": ["research"], "limit": ["3"]}
    work = out["works"][0]
    assert work["id"] == "w1" and work["reward"] == "1000 credit" and work["requester"] == "weaver"
    assert work["url"] == "https://swarmmemo.com/api/work/w1" and work["excerpt"].startswith("Pick")


def test_custom_base_url():
    kit = SwarmMemoToolkit(base_url="http://localhost:8080")
    fake = install(kit, {"ok": True, "messages": []})
    kit.read_room()
    assert fake.requests[0]["url"].startswith("http://localhost:8080/api/messages?")
