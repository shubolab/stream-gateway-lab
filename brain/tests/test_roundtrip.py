"""Real Go Gateway/Executor + Python DeepAgents, with a deterministic Responses API fixture.

This verifies integration, not real model reasoning quality. No provider credentials are used.
"""

import json
import os
import platform
import shlex
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest
from langchain_core.messages import AIMessage, HumanMessage, messages_to_dict

ROOT = Path(__file__).resolve().parents[2]
BRAIN = ROOT / "brain"


def eventually(fn, seconds=30):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = fn()
        if value:
            return value
        time.sleep(0.05)
    raise AssertionError("condition timed out")


class ResponsesFixture:
    def __init__(self, script):
        self.script = script
        self.requests = []
        self.errors = []
        self.hold = threading.Event()
        self.release = threading.Event()
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                try:
                    assert self.path == "/v1/responses", "must use Responses, never Chat Completions"
                    body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                    fixture.requests.append(body)
                    assert body.get("store") is False
                    assert body.get("parallel_tool_calls") is False
                    inputs = body["input"]
                    users = [x for x in inputs if x.get("role") == "user"]
                    text = json.dumps(users[-1], ensure_ascii=False)
                    outputs = [x for x in inputs if x.get("type") == "function_call_output"]
                    step = len(outputs)
                    tools = [x.get("name") for x in body.get("tools", [])]
                    assert "task" not in tools, "subagents must be disabled"
                    if "hold-model" in text or ("late-tool" in text and step == 0):
                        fixture.hold.set()
                        fixture.release.wait(10)
                    if "Context Extraction Assistant" in text:
                        assert not tools
                        output = fixture.text("OLDER_CONTEXT_SUMMARIZED")
                    elif "late-tool" in text:
                        if step == 0:
                            output = fixture.call("execute", {"command": "echo unsafe > must-not-exist"})
                        else:
                            assert not tools
                            assert "NOT sent" in json.dumps(outputs)
                            output = fixture.text("掉线后返回的工具调用已阻止。")
                    elif "offline" in text:
                        assert not tools, "offline chat must send no tools to the model"
                        output = fixture.text("仅聊天：执行端离线，没有进行任何机器操作。")
                    elif "remember" in text:
                        assert len(users) >= 2
                        assert any("VERIFIED:4" in json.dumps(x) for x in outputs)
                        output = fixture.text("之前已修复并验证 VERIFIED:4。")
                    elif "reconnected" in text:
                        assert "execute" in tools
                        output = fixture.text("Executor 恢复在线，下一轮工具已启用。")
                    elif "cancel-command" in text:
                        output = (
                            fixture.call(
                                "execute",
                                {
                                    "command": "echo $$ > parent.pid; sleep 30 & echo $! > child.pid; wait",
                                    "timeout": 12,
                                },
                            )
                            if step == 0
                            else fixture.text("stopped")
                        )
                    elif "disconnect-command" in text:
                        if step == 0:
                            output = fixture.call(
                                "execute", {"command": "echo started > started; sleep 8", "timeout": 12}
                            )
                        else:
                            assert not tools
                            assert "unknown" in json.dumps(outputs).lower()
                            output = fixture.text("执行端掉线，结果未知。现在仅聊天，不重放命令。")
                    elif "hold-model" in text:
                        output = fixture.text("stopped")
                    elif step == 0:
                        assert "read_file" in tools and "execute" in tools
                        output = fixture.call("read_file", {"file_path": str(fixture.script)})
                    elif step == 1:
                        assert "len(values) - 3" in json.dumps(outputs)
                        output = fixture.call(
                            "execute",
                            {"command": f"{shlex.quote(sys.executable)} {shlex.quote(str(fixture.script))}"},
                        )
                    elif step == 2:
                        assert "ZeroDivisionError" in json.dumps(outputs)
                        output = fixture.call(
                            "edit_file",
                            {
                                "file_path": str(fixture.script),
                                "old_string": "len(values) - 3",
                                "new_string": "len(values)",
                            },
                        )
                    elif step == 3:
                        output = fixture.call(
                            "execute",
                            {"command": f"{shlex.quote(sys.executable)} {shlex.quote(str(fixture.script))}"},
                        )
                    else:
                        assert "VERIFIED:4" in json.dumps(outputs)
                        output = fixture.text("已读取脚本，复现 ZeroDivisionError，修复后验证 VERIFIED:4。")
                    payload = json.dumps(
                        {
                            "id": "resp_" + uuid.uuid4().hex,
                            "object": "response",
                            "created_at": int(time.time()),
                            "status": "completed",
                            "model": "fixture-agent",
                            "output": [output],
                            "error": None,
                            "incomplete_details": None,
                            "parallel_tool_calls": False,
                            "usage": {"input_tokens": 20, "output_tokens": 10, "total_tokens": 30},
                        }
                    ).encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                except (BrokenPipeError, ConnectionResetError):
                    pass
                except Exception as exc:
                    fixture.errors.append(repr(exc))
                    self.send_error(500)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @staticmethod
    def call(name, args):
        return {
            "type": "function_call",
            "id": "fc_" + uuid.uuid4().hex,
            "call_id": "call_" + uuid.uuid4().hex,
            "name": name,
            "arguments": json.dumps(args),
            "status": "completed",
        }

    @staticmethod
    def text(text):
        return {
            "type": "message",
            "id": "msg_" + uuid.uuid4().hex,
            "status": "completed",
            "role": "assistant",
            "content": [{"type": "output_text", "text": text, "annotations": []}],
        }


@pytest.fixture(scope="session")
def binary(tmp_path_factory):
    if configured := os.environ.get("LAB_TEST_BINARY"):
        return Path(configured).resolve(strict=True)
    dest = tmp_path_factory.mktemp("build") / "lab"
    subprocess.run(["go", "build", "-o", str(dest), "."], cwd=ROOT, check=True)
    return dest


@pytest.fixture
def lab(tmp_path, binary):
    script = tmp_path / "broken.py"
    script.write_text(
        "values = [2, 4, 6]\nresult = sum(values) / (len(values) - 3)\n"
        'assert result == 4\nprint("VERIFIED:4")\n'
    )
    model = ResponsesFixture(script)
    config = tmp_path / "model.json"
    config.write_text(
        json.dumps(
            {
                "base_url": f"http://127.0.0.1:{model.server.server_port}/v1",
                "api_key": "fixture-key",
                "model": "fixture-agent",
            }
        )
    )
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    address = f"http://127.0.0.1:{port}"
    processes = []
    logs = open(tmp_path / "processes.log", "w")

    def start(*args):
        p = subprocess.Popen(
            [str(binary), *args],
            cwd=ROOT,
            env={**os.environ, "LAB_ADMIN_TOKEN": "fixture-admin", "GIN_MODE": "release"},
            stdout=logs,
            stderr=logs,
        )
        processes.append(p)
        return p

    gateway_args = (
        "gateway",
        "-listen",
        f"127.0.0.1:{port}",
        "-data-dir",
        str(tmp_path / "data"),
        "-brain-python",
        sys.executable,
        "-brain-dir",
        str(BRAIN),
        "-model-config",
        str(config),
    )
    gateway = start(*gateway_args)

    def api(path, body=None, token="fixture-admin", expected=200):
        req = urllib.request.Request(
            address + path,
            data=None if body is None else json.dumps(body).encode(),
            headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=5) as r:
                assert r.status == expected
                return json.load(r)
        except urllib.error.HTTPError as e:
            assert e.code == expected, e.read().decode()
            return json.load(e)

    def ready():
        try:
            return api("/api/state")
        except (OSError, urllib.error.URLError):
            return None

    eventually(ready)

    def executor(name):
        join = api("/api/enrollments", {}, expected=201)
        identity = api(
            "/api/register",
            {"name": name, "hostname": name, "os": platform.system().lower(), "arch": platform.machine()},
            token=join["token"],
            expected=201,
        )
        identity.update(gateway=address)
        registration = tmp_path / f"{name}.json"
        registration.write_text(json.dumps(identity))
        p = start("executor", "-registration", str(registration))
        eventually(
            lambda: any(e["id"] == identity["id"] and e["online"] for e in api("/api/state")["executors"])
        )
        return p, identity, registration

    a, aid, registration = executor("A")
    b, bid, _ = executor("B")
    session = api("/api/sessions", {"executor_id": aid["id"], "workdir": str(tmp_path)}, expected=201)
    result = dict(
        api=api,
        session=session,
        a=a,
        b=b,
        aid=aid,
        bid=bid,
        model=model,
        directory=tmp_path,
        gateway=gateway,
        gateway_args=gateway_args,
        start=start,
        registration=registration,
    )
    try:
        yield result
    finally:
        model.release.set()
        for p in reversed(processes):
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait()
        model.server.shutdown()
        logs.close()


def turn(lab, message, session=None):
    session = session or lab["session"]
    task = lab["api"](f"/api/sessions/{session['id']}/messages", {"message": message}, expected=201)

    def finished():
        detail = lab["api"](f"/api/sessions/{session['id']}")
        return next((t for t in detail["turns"] if t["id"] == task["id"] and t["status"] != "running"), None)

    result = eventually(finished)
    assert not lab["model"].errors, lab["model"].errors
    assert result["status"] == "succeeded", result
    return result


def test_repair_and_conversation(lab):
    result = turn(lab, "Read, run, fix the actual error, then verify the script.")
    assert "VERIFIED:4" in result["results"]["answer"]
    assert [e["name"] for e in result["events"] if e["type"] == "tool_start"] == [
        "read_file",
        "execute",
        "edit_file",
        "execute",
    ]
    assert result["executor_id"] == lab["aid"]["id"]
    assert result["chat_only"] is False
    assert "len(values) - 3" not in (lab["directory"] / "broken.py").read_text()
    second = turn(lab, "remember the previous verification")
    assert "VERIFIED:4" in second["results"]["answer"]
    logs = (lab["directory"] / "processes.log").read_text()
    assert f"executor pair={lab['bid']['id']} id=" not in logs


def test_offline_chat_reconnect_and_restart(lab):
    api, sid = lab["api"], lab["session"]["id"]
    api("/api/sessions", {"executor_id": "missing"}, expected=409)
    lab["a"].terminate()
    lab["a"].wait(timeout=5)
    eventually(lambda: api(f"/api/sessions/{sid}")["session"]["mode"] == "chat_only")
    api("/api/sessions", {"executor_id": lab["aid"]["id"]}, expected=409)
    result = turn(lab, "offline: continue chatting")
    assert result["chat_only"] is True
    assert not [e for e in result["events"] if e["type"] == "tool_start"]
    lab["start"]("executor", "-registration", str(lab["registration"]))
    eventually(lambda: api(f"/api/sessions/{sid}")["session"]["mode"] == "ready")
    result = turn(lab, "reconnected: check capabilities")
    assert result["chat_only"] is False
    lab["gateway"].terminate()
    lab["gateway"].wait(timeout=5)
    lab["start"](*lab["gateway_args"])

    def restored():
        try:
            return api(f"/api/sessions/{sid}")
        except (OSError, urllib.error.URLError):
            return None

    detail = eventually(restored)
    assert len(detail["turns"]) == 2
    assert detail["session"]["executor_id"] == lab["aid"]["id"]


def test_disconnect_during_command_latches_chat_only(lab):
    api, sid = lab["api"], lab["session"]["id"]
    task = api(f"/api/sessions/{sid}/messages", {"message": "disconnect-command"}, expected=201)
    eventually(lambda: (lab["directory"] / "started").exists())
    lab["a"].terminate()
    lab["a"].wait(timeout=5)

    def done():
        return next(
            (
                t
                for t in api(f"/api/sessions/{sid}")["turns"]
                if t["id"] == task["id"] and t["status"] != "running"
            ),
            None,
        )

    result = eventually(done)
    assert not lab["model"].errors, lab["model"].errors
    assert result["status"] == "succeeded", result
    assert result["chat_only"] is True
    assert len([e for e in result["events"] if e["type"] == "tool_start"]) == 1
    assert "结果未知" in result["results"]["answer"]


def test_cancel_kills_command_and_releases_reservation(lab):
    api, sid = lab["api"], lab["session"]["id"]
    task = api(f"/api/sessions/{sid}/messages", {"message": "cancel-command"}, expected=201)
    eventually(lambda: (lab["directory"] / "child.pid").exists())
    api(f"/api/sessions/{sid}/messages", {"message": "duplicate"}, expected=409)
    api("/api/sessions", {"executor_id": lab["aid"]["id"]}, expected=409)
    api(f"/api/tasks/{task['id']}/cancel", {}, expected=202)

    def done():
        return next(
            (
                t
                for t in api(f"/api/sessions/{sid}")["turns"]
                if t["id"] == task["id"] and t["status"] != "running"
            ),
            None,
        )

    assert eventually(done)["status"] == "canceled"
    for name in ["parent.pid", "child.pid"]:
        pid = int((lab["directory"] / name).read_text())

        def exited():
            ps = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True)
            return not ps.stdout.strip() or ps.stdout.strip().startswith("Z")

        eventually(exited, 5)
    assert api(f"/api/sessions/{sid}")["session"]["mode"] == "ready"
    assert not lab["model"].errors, lab["model"].errors


def test_disconnect_while_model_is_thinking_blocks_late_tool(lab):
    api, sid = lab["api"], lab["session"]["id"]
    api(f"/api/sessions/{sid}/messages", {"message": "late-tool"}, expected=201)
    assert lab["model"].hold.wait(10)
    lab["a"].terminate()
    lab["a"].wait(timeout=5)
    eventually(lambda: api(f"/api/sessions/{sid}")["session"]["mode"] == "chat_only")
    lab["model"].release.set()
    result = eventually(
        lambda: next((t for t in api(f"/api/sessions/{sid}")["turns"] if t["status"] != "running"), None)
    )
    assert result["status"] == "succeeded", result
    assert result["chat_only"] is True
    assert not [e for e in result["events"] if e["type"] == "tool_start"]
    assert any(e.get("blocked") for e in result["events"])
    assert not (lab["directory"] / "must-not-exist").exists()
    assert not lab["model"].errors, lab["model"].errors


def test_offline_context_compaction_is_persisted_without_machine_tools(lab):
    sid = lab["session"]["id"]
    history = lab["directory"] / "data" / "histories" / f"{sid}.json"
    history.parent.mkdir(exist_ok=True)
    messages = [HumanMessage(content="older detail " * 18000), AIMessage(content="old response")]
    for i in range(8):
        messages.extend([HumanMessage(content=f"recent {i}"), AIMessage(content="noted")])
    history.write_text(json.dumps(messages_to_dict(messages)))
    lab["a"].terminate()
    lab["a"].wait(timeout=5)
    result = turn(lab, "offline: continue after compaction")
    assert result["chat_only"] is True
    saved = history.read_text()
    assert "OLDER_CONTEXT_SUMMARIZED" in saved and len(saved) < 20000
    assert not [e for e in result["events"] if e["type"] == "tool_start"]
    summary_calls = sum("Context Extraction Assistant" in json.dumps(r) for r in lab["model"].requests)
    assert summary_calls == 1
    turn(lab, "offline: keep the summary across turns")
    assert sum("Context Extraction Assistant" in json.dumps(r) for r in lab["model"].requests) == 1
