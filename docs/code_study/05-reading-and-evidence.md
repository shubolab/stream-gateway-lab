# 按问题读源码与核验证据

## 第一遍：只看决策如何切换执行方式

如果连 `message="A-only", wait_ms=800` 的含义还不明确，先读[具体例子的第 1～3 节](07-concrete-call-walkthrough.md)：文字和时长是谁给的、两个工具做什么、为何要先 echo 再 wait。

按顺序打开：

1. [`main.go`](../../main.go)：`runBrain`。先确认大脑只做两次 Call，中间根据 echo 输出决定 wait 参数。
2. [`executor.go`](../../executor.go)：`Executor` 接口、`LocalExecutor.Call`。建立“Call 就是运行一次工具”的直觉。
3. [`tools.go`](../../tools.go)：`executeTool`。看清 echo、wait 的输入输出和取消方式。
4. 回到 `main.go` 的 `run`：找到 `executor = remote`。这就是本地／远程切换的装配点。

这一遍暂时不需要理解网关、心跳和重连。

## 第二遍：沿一次远程调用走完整条链

| 顺序 | 文件与函数 | 只盯住这件事 |
| --- | --- | --- |
| 1 | `executor.go`：`RemoteExecutor.Call` | 生成 id，保存原 channel，先登记 pending 再发送。 |
| 2 | `client.go`：`send` | 当前通道校验，给 body 套上 epoch 外层。 |
| 3 | `gateway.go`：`pairRoute.ServeHTTP` 的读取循环 | 只检查外层，然后把原始 data 发给同组另一端。 |
| 4 | `client.go`：`readConnection` 的 `case "data"` | 验证 conn 和 epoch，再调用注入的 handle。 |
| 5 | `executor_endpoint.go`：`receive` | 占用 slot，派生 toolCtx，异步执行，使用原 channel 回结果。 |
| 6 | `executor.go`：`RemoteExecutor.receive` | 把符合 channel 和 id 的结果交给当前等待者。 |
| 7 | `executor.go`：`waitResult` | 统一检查终态，再返回结果或错误。 |

`handle` 不是反射或框架魔法，只是普通函数参数：Brain 传 `RemoteExecutor.receive`，Executor 传 `executorEndpoint.receive`。相同读循环因此能服务两个角色。

## 第三遍：追踪一个旧通道怎样失效

从 [`client.go`](../../client.go) 的 `invalidateLocked` 开始：

```text
旧 current.stop(cause)
→ current = nil
→ notifyLocked 唤醒状态等待者
```

然后找它的调用处：收到不同 state、读连接退出、主动 disconnect。再分别向两端追：Brain 的 `waitResult` 监听旧通道 ctx；Executor 的 toolCtx 以旧通道 ctx 为父级。

最后读 `disconnect` 和 [`gateway.go`](../../gateway.go) 的 `remove`。两者都先问“我要清理的对象还是当前对象吗”，这是旧回调不会误伤新连接的关键。

## 第四遍：区分恢复连接与恢复调用

读 `client.run` 的 `for attempt := 0; attempt <= maxRedials; attempt++`：

- 初次拨号之外，整个 client 生命周期最多再拨 3 次，每次前等 5 秒。
- 拨号成功不会把 attempt 重置；成功一次不等于恢复了全部重拨额度。
- 只有自身读取连接退出才进入后续重拨；自身 WS 健康、仅对端离线时，留在读循环里等待状态变化，最多等 30 秒。
- 401／403、等待对端超时等会停止端点；不是所有错误都自动恢复。
- 循环只重建连接，没有保存或重发 toolMessage 的业务队列。

这些数值来自当前常量，属于实验默认边界，不是一般 WebSocket 系统必须遵守的数字。

## 哪些 Go 写法容易遮住实际流程

| 写法 | 在这里怎样理解 |
| --- | --- |
| `go func() { ... }()` | 把工具执行放到另一个 goroutine，让连接读循环继续工作；不代表工具自动可取消。 |
| `defer ...` | 函数返回时做清理；同一函数中后登记的先执行。例如网关会先关 conn，再 remove peer。 |
| `TryLock()` | 只尝试一次，拿不到立即失败，不等待执行槽。 |
| `Lock()` / `Unlock()` | 保护共享状态。读到锁时先确认它属于某个 pair、client，还是 RemoteExecutor。 |
| `select` | 等多个信号之一；多个都就绪时，不表达业务优先级，所以 waitResult 后面还要检查。 |
| `select { case ch <- v: default: }` | 尝试非阻塞投递；收件箱已有结果时不等待、不积压。 |
| `close(c.changed)` | 广播“状态发生变化”；不携带工具结果，也不关闭 WebSocket。 |
| `json.RawMessage` | 保存一段 JSON 字节，让网关无需把 body 解成工具结构；并不意味着外层 JSON 无需校验。 |

## 源码与测试如何对应

| 理解要点 | 测试 | 证据范围 |
| --- | --- | --- |
| 同一 runBrain 可用本地／远程执行器 | [`TestBrainMatchesAcrossExecutors`](../../lab_test.go) | 本地与真实本机 WS 返回一致；不是接真实模型。 |
| A/B 请求路由隔离 | [`TestPairsRouteConcurrently`](../../lab_test.go) | 两组并发结果隔离；不代表做过吞吐压力测试。 |
| 同连接换 epoch、旧清理与迟到结果 | [`TestEpochAndLateEvents`](../../epoch_test.go) | 同 socket 换代、Brain 重连、Executor 重连三个场景；换代／迟到帧由测试注入。 |
| 超时与结果同时就绪的裁定 | [`TestTimeoutResultArbitration`](../../timeout_test.go) | 预置事件的定点单元测试，不是网络时延测量。 |
| 超时、取消、迟到成功、恢复后新调用 | [`TestTimeoutDiscardsLateResultAndRecovers`](../../timeout_test.go) | 真实本机 WS，观察 pending 清理、旧结果隔离及 B 不受影响。 |
| 工具不响应取消仍占用 slot | [`TestUncancellableToolKeepsSlotAcrossReconnect`](../../uncancellable_test.go) | 测试注入不合作工具；当前 CLI 的 echo/wait 仍是合作取消工具。 |

其他已有测试可按需要继续读：[`reconnect_test.go`](../../reconnect_test.go) 的明确断线、静默失联、重拨额度测试，以及 [`stdin_test.go`](../../stdin_test.go) 的 SIGINT／EOF 退出测试。本轮没有重新运行这些专项，原有结果见上一级对应验收文档。

## 首轮共读核验记录

2026-10-18，在本机 macOS / arm64、Go 1.24.3 上执行：

```sh
go test -race -count=1 -v \
  -run '^Test(BrainMatchesAcrossExecutors|PairsRouteConcurrently|EpochAndLateEvents|Timeout.*|UncancellableToolKeepsSlotAcrossReconnect)$' .
```

结果：上述六个顶层测试全部通过，`ok stream-gateway-lab 29.558s`，无 race 报告。`-count=1` 表示本次实际执行，不复用测试结果缓存。

这是对本文核心行为的针对性核对。本轮只修改文档与图，没有修改 Go 实现，也没有重新声称完成所有历史验收或跨主机验证。五张 Mermaid 图已成功渲染为 SVG 并检查预览，文档内的本地链接已检查。

### 讨论 004 的补充观察

2026-10-18，为解释 A-only／800 这个具体例子，实际执行：

```sh
go run . brain -mode local -message A-only -wait-ms 800
```

退出码 0。两条输出分别是 echo 的 `{"message":"A-only","wait_ms":800}` 和 wait 的 `{"waited_ms":800}`，完整输出见[具体例子](07-concrete-call-walkthrough.md)。这次是本地运行；远程路径依据源码核对及上面已记录的测试，不声称重新运行了远程 A-only／800 场景。

## 图的维护

静态图使用 Mermaid CLI 12.0.0 从 `diagrams/*.mmd` 渲染。图源是后续修改入口，`SVG` 供不支持 Mermaid 的阅读器直接查看。样式由 [`diagrams/mermaid-config.json`](diagrams/mermaid-config.json) 固定。

本轮渲染工具安装在临时目录，不向 Go 工程添加 Node 依赖。已有 `mmdc` 环境时，可从仓库根目录执行：

```sh
for diagram in docs/code_study/diagrams/*.mmd; do
  mmdc -i "$diagram" -o "${diagram%.mmd}.svg" \
    -c docs/code_study/diagrams/mermaid-config.json --no-font-embed
done
```

CLI 需要可用的 Chromium／Chrome；本轮通过临时 Puppeteer 配置指定本机 Chrome。渲染环境配置不属于产品运行依赖。
