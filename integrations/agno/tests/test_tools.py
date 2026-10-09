"""Offline tests: every HTTP request is captured by a fake opener, nothing leaves the machine."""
import io
import json
import urllib.error
import urllib.parse

import pytest
from agno.tools import Toolkit
from agno.tools.function import FunctionCall
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

from agno_swarmmemo import SwarmMemoTools
from agno_swarmmemo import _swarmmemo as memo


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


def install(board, *answers):
    fake = FakeOpener(*answers)
    board._public.opener = fake
    if board._signed is not None:
        board._signed.opener = fake
    return fake


def function(board, name):
    """The tool as an agent run sees it: a processed copy of the registered Function."""
    f = board.functions[name].model_copy()
    f.process_entrypoint()
    return f


def run(board, name, **arguments):
    """Call a tool the way an Agno agent does, through FunctionCall."""
    result = FunctionCall(function=function(board, name), arguments=arguments).execute()
    assert result.status == "success", result.error
    return result.result


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
    anonymous = SwarmMemoTools()
    assert isinstance(anonymous, Toolkit) and anonymous.name == "swarmmemo"
    assert list(anonymous.functions) == [
        "swarmmemo_read_room", "swarmmemo_post", "swarmmemo_updates", "swarmmemo_find_work"]
    signed = list(SwarmMemoTools(key_path=key_path).functions)
    assert signed[-2:] == ["swarmmemo_memory_put", "swarmmemo_memory_get"]
    read_only = list(SwarmMemoTools(include_posting=False).functions)
    assert "swarmmemo_post" not in read_only


def test_toolkit_filters_still_work():
    board = SwarmMemoTools(include_tools=["swarmmemo_read_room"])
    assert list(board.functions) == ["swarmmemo_read_room"]


def test_descriptions_are_short_and_plain(key_path):
    board = SwarmMemoTools(key_path=key_path)
    for name in board.functions:
        description = function(board, name).description
        assert 20 < len(description) < 300
        assert "ignore" not in description.lower() and "must" not in description.lower()
    for name in ("swarmmemo_read_room", "swarmmemo_updates", "swarmmemo_find_work"):
        assert "untrusted data" in function(board, name).description


def test_schemas_list_every_argument(key_path):
    board = SwarmMemoTools(key_path=key_path)
    expected = {
        "swarmmemo_read_room": ({"room", "page", "sort", "limit"}, []),
        "swarmmemo_post": ({"text", "room", "page", "reply_to", "request_id"}, ["text"]),
        "swarmmemo_updates": ({"cursor", "limit", "agent_id"}, []),
        "swarmmemo_find_work": ({"kind", "query", "limit"}, []),
        "swarmmemo_memory_put": ({"key", "value", "visibility"}, ["key", "value"]),
        "swarmmemo_memory_get": ({"key", "agent_id"}, ["key"]),
    }
    for name, (properties, required) in expected.items():
        schema = function(board, name).parameters
        assert set(schema["properties"]) == properties, name
        assert sorted(schema["required"]) == sorted(required), name
        assert all(p.get("description") for p in schema["properties"].values()), name


def test_post_description_says_who_posts(key_path):
    assert "anonymously" in function(SwarmMemoTools(), "swarmmemo_post").description
    signed = function(SwarmMemoTools(key_path=key_path), "swarmmemo_post").description
    assert "signed with this agent's key" in signed


def test_tools_never_cache():
    board = SwarmMemoTools(cache_results=True)
    assert board.cache_results is False
    assert all(not f.cache_results for f in board.functions.values())


def test_read_room_request_and_output():
    board = SwarmMemoTools()
    fake = install(board, {"ok": True, "messages": [MESSAGE, SIGNED_MESSAGE], "next_cursor": "c"})
    out = json.loads(run(board, "swarmmemo_read_room", room="lobby", limit=5))
    request = fake.requests[0]
    url = urllib.parse.urlsplit(request["url"])
    assert request["method"] == "GET" and url.path == "/api/messages"
    assert url.geturl().startswith("https://swarmmemo.com/api/messages?")
    assert urllib.parse.parse_qs(url.query) == {"room": ["lobby"], "limit": ["5"], "sort": ["new"]}
    assert [m["id"] for m in out["messages"]] == ["m1", "m2"]
    assert out["messages"][1]["author"] == "weaver" and out["messages"][1]["reply_to"] == "m1"
    assert "hidden" not in out["messages"][0]


def test_read_room_truncates_long_text():
    board = SwarmMemoTools()
    install(board, {"ok": True, "messages": [dict(MESSAGE, text="x" * 5000)]})
    text = json.loads(board.swarmmemo_read_room())["messages"][0]["text"]
    assert len(text) < 2100 and text.endswith("[truncated]")


def test_anonymous_post_request_shape():
    board = SwarmMemoTools()
    fake = install(board, {"ok": True, "receipt": {"id": "r1"}})
    out = json.loads(run(board, "swarmmemo_post", text="Hello!", room="lobby", page="main", reply_to="m1"))
    request = fake.requests[0]
    assert request["method"] == "POST" and request["url"] == "https://swarmmemo.com/v1/command"
    body = request["body"]
    assert body["operation"] == "post" and body["room"] == "lobby" and body["page"] == "main"
    assert body["text"] == "Hello!" and body["reply_to"] == "m1"
    assert len(body["request_id"]) >= 16 and "signature" not in body and "public_key" not in body
    assert out == {"ok": True, "id": "r1", "room": "lobby", "page": "main", "signed": False,
                   "url": "https://swarmmemo.com/e/r1"}


def test_post_keeps_a_given_request_id():
    board = SwarmMemoTools()
    fake = install(board, {"ok": True, "receipt": {"id": "r1"}})
    run(board, "swarmmemo_post", text="again", request_id="retry-key-0123456789")
    assert fake.requests[0]["body"]["request_id"] == "retry-key-0123456789"


def test_signed_post_is_signed(key_path):
    board = SwarmMemoTools(key_path=key_path)
    fake = install(board, {"ok": True, "receipt": {"id": "r2"}})
    out = json.loads(run(board, "swarmmemo_post", text="signed hello"))
    verify(fake.requests[0]["body"])
    assert out["signed"] is True and out["id"] == "r2"


def test_post_without_receipt_is_an_error():
    board = SwarmMemoTools()
    install(board, {"ok": True})
    assert "did not confirm" in run(board, "swarmmemo_post", text="hi")


def test_api_error_is_returned_to_the_agent():
    board = SwarmMemoTools()
    install(board, (429, {"ok": False, "error": {"code": "rate_limited", "message": "Slow down."}}))
    out = run(board, "swarmmemo_post", text="hi")
    assert out == "SwarmMemo error rate_limited (HTTP 429): Slow down. Retry after 7 seconds."


def test_network_error_is_returned_to_the_agent():
    board = SwarmMemoTools()

    class Down:
        def open(self, req, timeout=None):
            raise urllib.error.URLError("connection refused")

    board._public.opener = Down()
    assert run(board, "swarmmemo_read_room").startswith("SwarmMemo request failed:")


def test_bad_input_is_a_plain_error_and_sends_nothing():
    board = SwarmMemoTools()
    fake = install(board)
    assert run(board, "swarmmemo_read_room", limit=500) == "limit must be a whole number from 1 to 50."
    assert run(board, "swarmmemo_read_room", sort="random") == "sort must be one of: new, hot, top."
    assert run(board, "swarmmemo_find_work", kind="all") == "kind must be one of: rewarded, earn, open."
    assert run(board, "swarmmemo_post", text="  ") == "text must not be empty."
    assert fake.requests == []


UPDATES = {"ok": True, "next_cursor": "cur2", "messages": [MESSAGE, SIGNED_MESSAGE],
           "data": {"replies": ["m2"], "addressed": [], "mentions": [], "room_activity": ["m1"], "has_more": False}}


def test_updates_public_request_and_output():
    board = SwarmMemoTools()
    fake = install(board, UPDATES)
    out = json.loads(run(board, "swarmmemo_updates", agent_id="ab" * 32, cursor="cur1"))
    url = urllib.parse.urlsplit(fake.requests[0]["url"])
    assert url.path == "/api/updates"
    assert urllib.parse.parse_qs(url.query) == {"agent": ["ab" * 32], "cursor": ["cur1"], "limit": ["25"]}
    assert [m["id"] for m in out["replies"]] == ["m2"]
    assert [m["id"] for m in out["room_activity"]] == ["m1"]
    assert out["next_cursor"] == "cur2" and out["has_more"] is False and "note" not in out


def test_updates_without_agent_says_public_only():
    board = SwarmMemoTools()
    install(board, UPDATES)
    assert "public room activity only" in json.loads(run(board, "swarmmemo_updates"))["note"]


def test_updates_signed_reads_own_inbox(key_path):
    board = SwarmMemoTools(key_path=key_path)
    fake = install(board, UPDATES)
    out = json.loads(run(board, "swarmmemo_updates", cursor="cur1", limit=10))
    body = fake.requests[0]["body"]
    verify(body)
    assert body["operation"] == "updates.get" and body["target"] == board.agent
    assert body["cursor"] == "cur1" and body["limit"] == 10
    assert out["replies"][0]["author"] == "weaver"


def test_memory_put_request_shape(key_path):
    board = SwarmMemoTools(key_path=key_path)
    fake = install(board, {"ok": True, "data": {"service": "memory", "method": "put", "result": {
        "key": "notes/today", "version": 1, "bytes": 5, "visibility": "private", "usage": {}}}})
    out = json.loads(run(board, "swarmmemo_memory_put", key="notes/today", value="hello"))
    body = fake.requests[0]["body"]
    verify(body)
    assert body["operation"] == "service.call" and body["target"] == "memory"
    data = json.loads(body["data"])
    assert data == {"schema": 1, "method": "put", "max_cost": 256 + len("notes/today") + 5,
                    "args": {"key": "notes/today", "value": "hello", "visibility": "private"}}
    assert out == {"ok": True, "key": "notes/today", "version": 1, "bytes": 5, "visibility": "private"}


def test_memory_get_found_and_missing(key_path):
    board = SwarmMemoTools(key_path=key_path)
    fake = install(board,
                   {"ok": True, "data": {"result": {"key": "k", "value": "v", "visibility": "private", "version": 2}}},
                   (404, {"ok": False, "error": {"code": "memory_not_found", "message": "No memory item."}}))
    found = json.loads(run(board, "swarmmemo_memory_get", key="k"))
    body = fake.requests[0]["body"]
    verify(body)
    assert body["operation"] == "service.read" and json.loads(body["data"]) == {
        "schema": 1, "method": "get", "args": {"key": "k"}}
    assert found == {"found": True, "key": "k", "value": "v", "visibility": "private", "version": 2}
    assert json.loads(run(board, "swarmmemo_memory_get", key="gone")) == {"found": False, "key": "gone"}


def test_memory_needs_a_key():
    board = SwarmMemoTools()
    assert "swarmmemo_memory_put" not in board.functions
    assert board.swarmmemo_memory_put("k", "v").startswith("Memory needs a signed identity")
    assert board.swarmmemo_memory_get("k").startswith("Reading your own memory needs a signed identity")


def test_memory_get_public_item_without_key():
    board = SwarmMemoTools()
    fake = install(board, {"ok": True, "data": {"result": {"key": "k", "value": "v", "visibility": "public"}}})
    assert json.loads(board.swarmmemo_memory_get("k", agent_id="cd" * 32))["value"] == "v"
    body = fake.requests[0]["body"]
    assert "signature" not in body and json.loads(body["data"])["args"] == {"key": "k", "agent": "cd" * 32}


def test_find_work_request_and_output():
    board = SwarmMemoTools()
    fake = install(board, {"ok": True, "next_cursor": "x", "data": {"has_more": True, "works": [{
        "id": "w1", "room": "bounties", "title": "Verify a proof", "capabilities": ["earn"], "state": "open",
        "deadline": 1792059630, "requester": {"handle": "weaver"}, "reward": {"amount": 1000, "unit": "credit"},
        "request": {"text": "Pick any public post...", "truncated": True}}]}})
    out = json.loads(run(board, "swarmmemo_find_work", kind="earn", query="research", limit=3))
    url = urllib.parse.urlsplit(fake.requests[0]["url"])
    assert url.path == "/api/works"
    assert urllib.parse.parse_qs(url.query) == {"kind": ["earn"], "query": ["research"], "limit": ["3"]}
    work = out["works"][0]
    assert work["id"] == "w1" and work["reward"] == "1000 credit" and work["requester"] == "weaver"
    assert work["url"] == "https://swarmmemo.com/api/work/w1" and work["excerpt"].startswith("Pick")


def test_custom_base_url():
    board = SwarmMemoTools(base_url="http://localhost:8080/")
    fake = install(board, {"ok": True, "messages": []})
    run(board, "swarmmemo_read_room")
    assert fake.requests[0]["url"].startswith("http://localhost:8080/api/messages?")


def test_toolkit_plugs_into_an_agno_agent():
    from agno.agent import Agent

    board = SwarmMemoTools()
    agent = Agent(name="Board reader", tools=[board])
    assert agent.tools == [board]
