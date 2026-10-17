# 第一步运行记录：一对闭环与统一大脑

本记录保留第一步当时的运行证据；当前版本已增加[两组并发隔离](./step-2-pair-isolation-results.md)，配置格式及复现命令以最新 [README](../README.md) 为准。

- 验证时间：2026-10-17 18:13（Asia/Shanghai），执行侧统一命名为 Executor 后重新运行。
- 环境：当前 macOS / arm64，Go 1.24.3；WS 依赖固定为 `github.com/coder/websocket v1.8.14`。
- 结论：**M1 通过。本地与远程标准输出逐字一致；远程调用经过真实 WS、网关与独立执行端进程。**
- 本次只实现第一步；没有启动第二对，也没有开展 M2–M6 的整组验收。

## 相同大脑、相同结果

本地和远程均调用 `main.go` 中的 `runBrain`。它先 echo，再从 echo 结果读取 `wait_ms=200`，据此调用 wait；只有启动装配时选择不同 Executor。

本地进程先运行，此时网关尚未启动，也没有设置凭证。随后启动远程的三个进程。两次结果分别捕获后逐字比较，结果相同：

```json
[
  {
    "tool": "echo",
    "output": {
      "message": "hello from brain",
      "wait_ms": 200
    }
  },
  {
    "tool": "wait",
    "output": {
      "waited_ms": 200
    }
  }
]
```

## 独立进程与真实链路

| 角色 | 本次 PID | 启动参数 | 退出码 |
| --- | --- | --- | --- |
| 本地大脑 | 46357 | `brain -mode local` | 0 |
| 网关 | 46358 | `gateway` | 0（验证后发送 SIGTERM） |
| 执行端 Executor | 46359 | `executor` | 0（会话结束） |
| 远程大脑 | 46360 | `brain -mode remote` | 0 |

执行 wait 期间用 `ps` 确认后三个进程同时存在，`lsof` 记录如下，三个进程均来自本工作区编译的同一个程序：

```text
PID 46358  TCP 127.0.0.1:8787 (LISTEN)
PID 46358  TCP 127.0.0.1:8787->127.0.0.1:60388 (ESTABLISHED)
PID 46358  TCP 127.0.0.1:8787->127.0.0.1:60389 (ESTABLISHED)
PID 46359  TCP 127.0.0.1:60388->127.0.0.1:8787 (ESTABLISHED)
PID 46360  TCP 127.0.0.1:60389->127.0.0.1:8787 (ESTABLISHED)
```

只有网关监听端口，执行端和大脑各自主动连接它。以下为实际网关日志节选，只有路由信息，没有工具内容：

```text
18:13:57.891753 gateway pair=A role=executor connected
18:13:57.908175 gateway pair=A role=brain connected
18:13:57.908226 gateway pair=A ready
18:13:57.908489 gateway pair=A forward brain->executor bytes=195
18:13:57.908721 gateway pair=A forward executor->brain bytes=165
18:13:57.908874 gateway pair=A forward brain->executor bytes=161
18:13:58.111569 gateway pair=A forward executor->brain bytes=138
```

执行端实际运行了两个工具：

```text
18:13:57.908609 executor pair=A tool=echo started
18:13:57.908657 executor pair=A tool=echo returned
18:13:57.908931 executor pair=A tool=wait started
18:13:58.111304 executor pair=A tool=wait returned
```

运行中用断言核对了两次请求、两次结果，所有子进程均已退出，本次没有遗留后台服务。复现命令见 [README](../README.md)。

## 代码检查与尚未证明的部分

`go test -race -v ./...`、`go vet ./...`、`go build` 均通过。两个测试覆盖真实 WS 下的同一大脑流程与工具错误回传，以及错误角色凭证被拒绝；race 检查仅针对本次执行到的路径。

这一步证明调用接口与远程通路成立。主动心跳、有限重连、第二对隔离、代次切换后的恢复、取消失败和迟到结果等完整故障场景尚未验收。当前通过结束整次会话处理断线／超时，不恢复或补发旧调用；再次演示需要重新启动两端。
