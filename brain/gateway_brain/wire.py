"""One task-bound WebSocket channel. No business replay or reconnect within a turn."""

import asyncio
import json
import uuid
from collections.abc import Callable
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

from websockets.asyncio.client import connect


class ExecutorUnavailable(Exception):
    """The current turn must continue in chat-only mode."""


class ToolFailure(Exception):
    """A confirmed tool failure which the model may inspect and correct."""


class RemoteExecutor:
    def __init__(self, url: str, executor_id: str, token: str, emit: Callable):
        parts = urlsplit(url)
        query = dict(parse_qsl(parts.query))
        query.update(pair=executor_id, role="brain")
        self.url = urlunsplit(parts._replace(query=urlencode(query)))
        self.token = token
        self.executor_id = executor_id
        self.emit = emit
        self.socket = None
        self.reader = None
        self.epoch = ""
        self.available = False
        self.lost = False
        self.closing = False
        self.ready = asyncio.Event()
        self.slot = asyncio.Lock()
        self.pending = None

    async def start(self):
        try:
            self.socket = await connect(
                self.url,
                additional_headers={"Authorization": f"Bearer {self.token}"},
                max_size=65536,
                ping_interval=5,
                ping_timeout=10,
                open_timeout=5,
                close_timeout=1,
                proxy=None,
            )
            self.reader = asyncio.create_task(self._read())
            await asyncio.wait_for(self.ready.wait(), timeout=10)
        except Exception:
            self.disable("Executor is unavailable; this turn is chat-only.")

    def disable(self, reason: str):
        if not self.lost:
            self.lost = True
            self.available = False
            self.emit("executor_offline", message=reason)
        if self.pending is not None:
            _, future = self.pending
            if not future.done():
                future.set_exception(ExecutorUnavailable(reason))
        self.ready.set()

    async def _read(self):
        try:
            async for raw in self.socket:
                if not isinstance(raw, str):
                    raise ValueError("expected text frame")
                msg = json.loads(raw)
                if msg.get("type") == "state":
                    epoch = msg.get("epoch", "")
                    if self.epoch and epoch != self.epoch:
                        self.disable(
                            "Executor disconnected. In-flight operation outcome is unknown; do not replay it."
                        )
                    if epoch and not self.lost:
                        self.epoch, self.available = epoch, True
                        self.ready.set()
                elif msg.get("type") == "data":
                    if self.lost or msg.get("epoch") != self.epoch or self.pending is None:
                        continue
                    body = msg.get("body", {})
                    call_id, future = self.pending
                    if body.get("kind") != "result" or body.get("id") != call_id:
                        continue
                    if not future.done():
                        if ("output" in body) == bool(body.get("error")):
                            raise ValueError("invalid tool result")
                        future.set_result(body)
                else:
                    raise ValueError("unknown gateway frame")
        except asyncio.CancelledError:
            raise
        except Exception:
            pass
        finally:
            if not self.closing:
                self.disable(
                    "Executor channel closed. In-flight operation outcome is unknown; do not replay it."
                )

    async def call(self, tool: str, arguments: dict):
        async with self.slot:
            if not self.available:
                raise ExecutorUnavailable(
                    "Chat-only: the bound Executor is offline. Machine tools are disabled."
                )
            call_id = uuid.uuid4().hex
            frame = json.dumps(
                {
                    "type": "data",
                    "epoch": self.epoch,
                    "body": {
                        "kind": "call",
                        "id": call_id,
                        "tool": tool,
                        "input": arguments,
                        "timeout_ms": 15000,
                    },
                },
                ensure_ascii=False,
            )
            if len(frame.encode()) > 65536:
                raise ToolFailure("Tool arguments exceed the 64 KiB transport limit; reduce the request.")
            future = asyncio.get_running_loop().create_future()
            self.pending = (call_id, future)
            try:
                await asyncio.wait_for(self.socket.send(frame), timeout=2)
                result = await asyncio.wait_for(future, timeout=15)
                if not self.available:
                    raise ExecutorUnavailable("Executor disconnected; operation outcome is unknown.")
                if result.get("error"):
                    raise ToolFailure(result["error"])
                return result["output"]
            except (ToolFailure, ExecutorUnavailable):
                raise
            except asyncio.CancelledError:
                self.disable("Turn canceled; operation outcome may be unknown.")
                await self.close()
                raise
            except Exception as exc:
                self.disable("Executor transport failed; operation outcome is unknown. Do not replay it.")
                await self.close()
                raise ExecutorUnavailable("Executor transport failed; operation outcome is unknown.") from exc
            finally:
                if not future.done():
                    future.cancel()
                elif not future.cancelled():
                    future.exception()  # Consume terminal errors after a failed send.
                self.pending = None

    async def close(self):
        self.closing = True
        self.available = False
        if self.socket is not None:
            await self.socket.close()
        if self.reader is not None and self.reader is not asyncio.current_task():
            self.reader.cancel()
            await asyncio.gather(self.reader, return_exceptions=True)
