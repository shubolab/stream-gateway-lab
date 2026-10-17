# 管道 stdin 的 SIGINT 退出修复

- 时间：2026-10-17 22:34（Asia/Shanghai）。
- 环境：本机 macOS / arm64，Go 1.24.3。
- 结论：**管道写端保持打开时，Brain 收到 SIGINT 可以及时以 0 退出，网关释放其身份和旧通道；同一身份可立即重新接入。**

## 最小修改

此前 `runBrainInput` 在主循环直接调用 `Scanner.Scan()`，取消时尝试关闭 `os.Stdin`。对于继承的阻塞管道，这个关闭操作不能保证唤醒正在进行的读取，主流程因此无法返回。

生产代码只改 `main.go` 的 `runBrainInput`：一个固定 goroutine 读取并复制输入行，通过无缓冲 channel 交给主循环；主循环同时等待输入和 `ctx.Done()`。取消后直接返回并执行现有连接取消逻辑，不再等待 stdin 读取结束。EOF、逐行解析、输入大小限制和串行工具调用保持原有行为。

该读取 goroutine 属于一次 CLI 进程；若系统读取仍阻塞，由进程退出释放 stdin。没有引入跨平台输入框架、额外依赖、新协议或连接重连策略。

## 先复现，再回归

新增的 `TestBrainStdinStops/local/idle` 在修复前实际失败：管道写端不关闭，发送真实 SIGINT 后等待 2 秒，Brain 仍未退出。对应错误为：

```text
Brain did not exit within 2 seconds; pipe writer is still open
```

修复后，`TestBrainStdinStops` 的六个场景均通过：Local／Remote 各覆盖等待新行、半行输入时的 SIGINT，以及正常 EOF。子进程运行真实 `main` 和信号处理器；信号场景保持父进程管道写端打开，防止 EOF 掩盖问题。

远程场景另外检查：Executor 旧通道取消；同一个 Brain 身份能直接重新连接并成功调用 echo；Executor 原有健康 socket 保持不变。race 子进程退出约 1 秒，其中包括 Go race runtime 的退出等待，测试限时 2 秒。

## 编译产物的独立进程验收

使用 `go build -o stream-gateway-lab .` 的产物，再通过 Python 父进程建立真实 stdin 管道并发送 SIGINT。每个 Brain 先完成一轮 echo／wait，再在管道写端仍打开时等待新行或接收半行；退出前不发送 EOF。

Gateway PID 为 66054，Executor A PID 为 66055，整个验收期间持续运行。

| Brain 场景 | PID | 发送 SIGINT 后退出耗时 | 退出码 |
| --- | --- | --- | --- |
| Local，等待新行 | 66056 | 4.173 ms | 0 |
| Local，半行输入 | 66057 | 1.293 ms | 0 |
| Remote，等待新行 | 66058 | 1.293 ms | 0 |
| Remote，半行输入 | 66059 | 1.451 ms | 0 |

远程两次退出后分别在 1.384 ms、1.560 ms 内观察到网关移除旧 Brain 并作废 epoch。随后同身份的一次性 Brain 接入，`after-sigint` 调用成功；Executor 全程只有一次连接，没有重拨。没有额外输出半行解析错误，也未用强制结束处理这些 Brain。结束后网关与 Executor 也正常退出，端口释放。

上述时间是这次本机实测值，不作为硬实时承诺。测试辅助入口和进程控制只在 `_test.go`，没有增加产品命令。

## 复现命令

```sh
go test -race -v -run '^TestBrainStdinStops$' .
go test -race -v ./...
go vet ./...
go build -o stream-gateway-lab .
```

以上检查均通过；完整 race 回归约 49 秒，包含既有的两组隔离、断线、静默失联与有限重连场景。
