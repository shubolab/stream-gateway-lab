"""One chat turn per process, with a persistent conversation and JSONL events."""

import argparse
import asyncio
import json
import os
import signal
import sys
import tempfile
import uuid
from pathlib import Path
from urllib.parse import urlsplit

from deepagents import (
    GeneralPurposeSubagentProfile,
    HarnessProfile,
    create_deep_agent,
    register_harness_profile,
)
from deepagents.middleware.filesystem import FilesystemMiddleware
from langchain.agents.middleware import ModelCallLimitMiddleware, SummarizationMiddleware
from langchain.agents.middleware.types import AgentMiddleware
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage, messages_from_dict, messages_to_dict
from langchain_openai import ChatOpenAI

from .backend import GatewayExecutorBackend
from .wire import RemoteExecutor


def emit(kind, **fields):
    print(json.dumps({"type": kind, **fields}, ensure_ascii=False), flush=True)


def load_config(filename):
    cfg = json.loads(Path(filename).read_text())
    for key in ("base_url", "api_key", "model"):
        if not isinstance(cfg.get(key), str) or not cfg[key].strip():
            raise ValueError(f"model configuration requires {key}")
    cfg["base_url"] = cfg["base_url"].rstrip("/")
    url = urlsplit(cfg["base_url"])
    if url.scheme not in ("https", "http") or not url.netloc or url.username or url.query or url.fragment:
        raise ValueError("base_url must be an HTTP(S) API base, e.g. https://host/v1")
    if url.path.endswith("/responses"):
        raise ValueError("base_url must end at the API base (/v1), not /responses")
    for key, default, low, high in (
        ("max_model_calls", 20, 1, 100),
        ("request_timeout", 90, 1, 300),
        ("max_output_tokens", 4096, 128, 32768),
    ):
        value = cfg.get(key, default)
        if not isinstance(value, int) or isinstance(value, bool) or not low <= value <= high:
            raise ValueError(f"invalid {key}")
        cfg[key] = value
    return cfg


def save_history(filename, messages):
    path = Path(filename)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temp = tempfile.mkstemp(dir=path.parent, prefix=".history-")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(messages_to_dict(messages), f, ensure_ascii=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(temp, path)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def load_history(filename):
    path = Path(filename)
    messages = messages_from_dict(json.loads(path.read_text())) if path.exists() else []
    # A killed Brain may have saved an assistant call before its result. Close
    # those call IDs explicitly; never reissue them while rebuilding context.
    pending = {}
    for message in messages:
        if isinstance(message, AIMessage):
            pending.update({c["id"]: c["name"] for c in message.tool_calls})
        elif isinstance(message, ToolMessage):
            pending.pop(message.tool_call_id, None)
    for call_id, name in pending.items():
        messages.append(
            ToolMessage(
                content=(
                    "Previous turn was interrupted. Operation outcome is unknown; "
                    "verify before any new mutation."
                ),
                tool_call_id=call_id,
                name=name,
                status="error",
            )
        )
    return messages


class BoundExecutorMiddleware(AgentMiddleware):
    """Serial tool execution and a hard, one-way chat-only latch for this turn."""

    def __init__(self, remote):
        self.remote = remote
        self.slot = asyncio.Lock()
        self.calls = 0

    def online(self):
        return self.remote is not None and self.remote.available

    async def awrap_model_call(self, request, handler):
        emit("model_start", chat_only=not self.online())
        if not self.online():
            request = request.override(tools=[], tool_choice="none")
        return await handler(request)

    async def awrap_tool_call(self, request, handler):
        async with self.slot:
            call = request.tool_call
            if not self.online():
                message = (
                    "Chat-only: bound Executor is offline. Tool was NOT sent. "
                    "Do not retry machine operations."
                )
                emit("tool_result", call_id=call["id"], name=call["name"], output=message, blocked=True)
                return ToolMessage(content=message, tool_call_id=call["id"], status="error")
            self.calls += 1
            emit("tool_start", call_id=call["id"], name=call["name"], arguments=call["args"])
            result = await handler(request)
            content = result.content if isinstance(result, ToolMessage) else str(result)
            emit("tool_result", call_id=call["id"], name=call["name"], output=content)
            return result


async def run(args):
    cfg = load_config(args.model_config)
    request = json.load(sys.stdin)
    message = request.get("message", "")
    if not isinstance(message, str) or not message.strip():
        raise ValueError("message is required")
    remote = None
    try:
        if not args.chat_only:
            token = os.environ.get("LAB_TASK_TOKEN", "")
            if not token:
                raise ValueError("missing task credential")
            remote = RemoteExecutor(args.url, args.executor_id, token, emit)
            await remote.start()
        backend = GatewayExecutorBackend(remote, args.workdir, args.executor_id)
        gate = BoundExecutorMiddleware(remote)
        options = dict(
            model=cfg["model"],
            api_key=cfg["api_key"],
            base_url=cfg["base_url"],
            use_responses_api=True,
            use_previous_response_id=False,
            store=False,
            max_retries=0,
            timeout=cfg["request_timeout"],
            max_tokens=cfg["max_output_tokens"],
            disable_streaming=True,
            output_version="responses/v1",
            model_kwargs={"parallel_tool_calls": False},
        )
        if "reasoning" in cfg:
            options["reasoning"] = cfg["reasoning"]
        model = ChatOpenAI(**options)
        register_harness_profile(
            f"openai:{cfg['model']}",
            HarnessProfile(
                general_purpose_subagent=GeneralPurposeSubagentProfile(enabled=False),
                excluded_tools=frozenset({"task", "delete"}),
            ),
        )
        agent = create_deep_agent(
            model=model,
            backend=backend,
            system_prompt=(
                "You are a machine-operation and coding assistant. Reply in the user's language. "
                f"This conversation is permanently bound to Executor {args.executor_id}. "
                f"Its working directory is {args.workdir}. All machine tools run on that Executor. "
                "Filesystem paths are real paths on the remote machine; "
                "relative paths use the working directory. "
                "Full access is enabled: perform requested operations directly, "
                "with no permission approval steps. "
                "Read relevant files, execute, inspect actual errors, fix the cause, "
                "then execute again to verify. "
                "Report evidence and do not claim success without successful tool results. "
                "Shell commands last at most 12 seconds and background processes "
                "are cleaned up after each call. "
                "Do not start services or long-running jobs in this version. "
                "If the Executor goes offline, continue conversation using text only. "
                "Never claim to have operated on the machine while tools are unavailable. "
                "An interrupted remote operation may already have happened; never automatically replay it."
            ),
            middleware=[
                # Replace DeepAgents' filesystem-backed compaction. Conversation
                # memory belongs to Brain and must also work without an Executor.
                SummarizationMiddleware(model=model, trigger=("tokens", 24000), keep=("messages", 12)),
                FilesystemMiddleware(
                    backend=backend,
                    tools=["ls", "read_file", "write_file", "edit_file", "glob", "grep", "execute"],
                    tool_token_limit_before_evict=None,
                    human_message_token_limit_before_evict=None,
                    max_execute_timeout=12,
                ),
                gate,
                ModelCallLimitMiddleware(run_limit=cfg["max_model_calls"], exit_behavior="error"),
            ],
        )
        history = load_history(args.history)
        history.append(HumanMessage(content=message, id=uuid.uuid4().hex))
        save_history(args.history, history)
        seen = {m.id for m in history if m.id}
        answer = ""
        async for state in agent.astream(
            {"messages": history},
            config={"recursion_limit": cfg["max_model_calls"] * 4 + 10},
            stream_mode="values",
        ):
            messages = state.get("messages", [])
            if not messages:
                continue
            save_history(args.history, messages)
            for item in messages:
                if item.id in seen:
                    continue
                seen.add(item.id)
                if isinstance(item, AIMessage) and item.text:
                    answer = item.text
                    emit("assistant", text=answer)
        if not answer:
            raise RuntimeError("Agent ended without a final answer")
        emit("final", answer=answer, tool_calls=gate.calls, chat_only=not gate.online())
    finally:
        if remote is not None:
            await remote.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--executor-id", required=True)
    parser.add_argument("--workdir", required=True)
    parser.add_argument("--model-config", required=True)
    parser.add_argument("--history", required=True)
    parser.add_argument("--chat-only", action="store_true")
    args = parser.parse_args()

    async def with_signals():
        task = asyncio.current_task()
        for sig in (signal.SIGINT, signal.SIGTERM):
            asyncio.get_running_loop().add_signal_handler(sig, task.cancel)
        await run(args)

    try:
        asyncio.run(with_signals())
    except (KeyboardInterrupt, asyncio.CancelledError):
        emit("error", message="Turn canceled.")
        return 130
    except Exception as exc:
        # Provider exceptions may contain response bodies. Keep credentials and
        # raw provider diagnostics out of chat history and Gateway logs.
        detail = str(exc) if isinstance(exc, ValueError) else type(exc).__name__
        emit("error", message=f"Brain failed: {detail}. Check model configuration or service availability.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
