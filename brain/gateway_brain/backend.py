"""DeepAgents backend: filesystem paths refer to the selected Executor's OS."""

import json

from deepagents.backends.protocol import (
    EditResult,
    ExecuteResponse,
    GlobResult,
    GrepResult,
    LsResult,
    ReadResult,
    SandboxBackendProtocol,
    WriteResult,
)

from .wire import ExecutorUnavailable, ToolFailure


class GatewayExecutorBackend(SandboxBackendProtocol):
    def __init__(self, remote, workdir: str, executor_id: str):
        self.remote = remote
        self.workdir = workdir
        self.executor_id = executor_id

    @property
    def id(self):
        return self.executor_id

    async def invoke(self, name: str, **args):
        if self.remote is None:
            raise ExecutorUnavailable(
                "Chat-only: machine tools are disabled because the Executor is offline."
            )
        return await self.remote.call(name, {"workdir": self.workdir, **args})

    async def als(self, path):
        try:
            r = await self.invoke("fs_ls", path=path)
            return LsResult(
                entries=r["entries"], error="Listing limited to 100 entries" if r["truncated"] else None
            )
        except (ToolFailure, ExecutorUnavailable) as e:
            return LsResult(error=str(e))

    async def aread(self, file_path, offset=0, limit=2000):
        if limit <= 0:
            return ReadResult(no_lines_requested=True)
        try:
            r = await self.invoke("fs_read", path=file_path, offset=offset, limit=min(limit, 200))
            pagination = {}
            if r["next_offset"] > r["offset"]:
                pagination = {
                    "start_line": r["offset"] + 1,
                    "end_line": r["next_offset"],
                    "total_lines": r["total_lines"],
                    "next_offset": r["next_offset"],
                }
            return ReadResult(file_data={"content": r["content"], "encoding": "utf-8"}, **pagination)
        except (ToolFailure, ExecutorUnavailable) as e:
            return ReadResult(error=str(e))

    async def awrite(self, file_path, content):
        try:
            r = await self.invoke("fs_write", path=file_path, content=content)
            return WriteResult(path=r["path"])
        except (ToolFailure, ExecutorUnavailable) as e:
            return WriteResult(error=str(e))

    async def aedit(self, file_path, old_string, new_string, replace_all=False):
        try:
            r = await self.invoke(
                "fs_edit",
                path=file_path,
                old_string=old_string,
                new_string=new_string,
                replace_all=replace_all,
            )
            return EditResult(path=r["path"], occurrences=r["occurrences"])
        except (ToolFailure, ExecutorUnavailable) as e:
            return EditResult(error=str(e))

    async def aglob(self, pattern, path=None):
        try:
            r = await self.invoke("fs_glob", path=path or ".", pattern=pattern)
            return GlobResult(
                matches=r["items"],
                truncated=r["truncated"],
                truncation_reason="budget" if r["truncated"] else None,
            )
        except (ToolFailure, ExecutorUnavailable) as e:
            return GlobResult(error=str(e))

    async def agrep(self, pattern, path=None, glob=None, *, max_count=None):
        try:
            r = await self.invoke("fs_grep", path=path or ".", pattern=pattern, glob=glob or "")
            matches = r["items"] if max_count is None else r["items"][:max_count]
            return GrepResult(matches=matches, truncated=r["truncated"] or len(matches) < len(r["items"]))
        except (ToolFailure, ExecutorUnavailable) as e:
            return GrepResult(error=str(e))

    async def aexecute(self, command, *, timeout=None):
        try:
            if timeout is not None and not 1 <= timeout <= 12:
                return ExecuteResponse(
                    output="This Executor supports command timeouts of 1..12 seconds.", exit_code=2
                )
            r = await self.invoke("shell_exec", command=command, timeout_ms=(timeout or 10) * 1000)
            # Nonzero exits are observations, not transport failures. The agent can read errors and fix code.
            return ExecuteResponse(
                output=json.dumps(r, ensure_ascii=False),
                exit_code=124 if r["timed_out"] else r["exit_code"],
                truncated=r["truncated"],
            )
        except (ToolFailure, ExecutorUnavailable) as e:
            return ExecuteResponse(output=str(e), exit_code=1)
