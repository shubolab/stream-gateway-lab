# epoch 切换与迟到事件验收

- 时间：2026-10-17 23:02（Asia/Shanghai）。
- 环境：本机 macOS / arm64，Go 1.24.3。
- 结论：**连接不关闭、直接切换 epoch，仍会使旧调用失败并取消旧执行上下文；旧清理与迟到结果不能改变新通道或新调用，B 不受影响。**

## 最小范围

只新增 `epoch_test.go` 和文档，生产代码、协议、依赖和 CLI 参数均未修改。现有通道对象、epoch、调用 ID 与网关 peer 身份校验通过了本轮检查。

本轮是一个 Go 测试进程内的两组 Brain／Executor 和一个网关，连接使用真实本机 TCP／WS。只在测试里直接修改网关 epoch、发送状态与结果帧、触发旧回调；没有新增运行时换代接口或故障平台。

## 三种状态切换

| 场景 | 操作 | 验收结果 |
| --- | --- | --- |
| `same-sockets` | A 的 Brain 与 Executor 两条 WS 都保持不变；按 Executor→Brain 顺序直接发送新 epoch，不先发送空 epoch | 旧调用失败、旧 wait 观察到 `context canceled`；两条 socket 对象不变，双方通道对象与 epoch 更新。 |
| `reconnect-brain` | 关闭 A Brain WS，按原有 5 秒间隔重拨；新调用执行中再次触发旧 peer 清理回调 | 旧 peer 已被替换；迟到清理没有删除新路由、清空新 epoch 或中断新调用。 |
| `reconnect-executor` | 关闭 A Executor WS，恢复后在新调用执行中触发旧 peer 清理回调 | 同样保留新路由、新 epoch 与新调用。 |

每种场景都先确认 A 的 10 秒 wait 和 B 的 700 ms wait 已真正开始。A 旧调用须在切换／断线后 1 秒内失败，旧工具须观察到取消并释放执行槽；随后才启动 A 新调用。

首次专项 race 测试中，旧调用分别在 **0.190 ms、0.183 ms、0.305 ms** 内失败。三个子场景分别约 0.71 秒、5.01 秒、5.01 秒。这些是本机观察值，不作为硬实时保证。

## 在新调用执行期间注入迟到事件

新调用启动后，通过一个受 context 限时的测试同步点暂停工具返回，确保以下事件发生时确实存在新调用，避免仅在空闲时检查：

| 注入事件 | 检查 |
| --- | --- |
| 旧通道的 `disconnect` 回调 | Brain 和 Executor 的旧通道都不能关闭新通道；同一 socket 换 epoch 的场景也适用。 |
| 旧 peer 的网关 `remove` 回调 | 仅在真实重连并替换 peer 后触发；当前 peer、epoch 均不变。 |
| 旧执行通道尝试发送结果 | 发送立即被拒绝，不能借用新通道。 |
| 旧通道结果回调，故意携带新调用 ID | `RemoteExecutor.receive` 仍按原通道身份拒绝，不能仅因 ID 匹配而交付。 |
| 旧 epoch 结果，故意携带新调用 ID | 一次经 Executor→Gateway 路径发送，一次直接注入网关到 Brain 的 WS；两种路径均不交付给新调用。后者单独覆盖端点读循环对旧 epoch 的过滤。 |
| 当前 epoch、旧调用 ID 的结果 | 确认结果到达 `RemoteExecutor.receive`，但不能结束当前调用。 |

原始 WS 写入故意绕过端点 `send` 的前置校验，使用新调用 ID 验证 epoch 隔离，防止单靠不同 ID 让测试误通过。测试沿同一条 WS 发送后续合法但不匹配当前调用的结果，作为处理进度标记；收到标记后才检查结果，避免仅靠短暂 sleep 推断旧消息已处理。没有新增业务 ACK 或消息类型。

通过上述检查后，A 新调用仍未结束、其执行上下文未被取消。允许工具返回后，调用只收到 `"A-new-result"`，没有被注入的 `"must-not-win"` 覆盖。A 全程只启动旧、新两个工具，没有重放或额外执行。

## B 的隔离与日志

B 的原 700 ms wait 正常返回；A 新调用仍被测试暂停期间，B 再发起 echo，得到 `"B-unaffected"`。每种场景结束时，B 两端均保持原通道对象、原 socket 和原 epoch。

同连接换代场景的实际日志节选：

```text
23:02:05 executor pair=A ready epoch=Z6UNH6C4VLVPUXC2GPAIX4BKZW
23:02:05 executor pair=A channel lost epoch=Z6UNH6C4VLVPUXC2GPAIX4BKZW
23:02:05 executor pair=A ready epoch=GDJAD32JVLL5OFCANBXQZ3CRUN
23:02:05 executor pair=A id=IM2QFGUIDL7VP75B74QR3ALSZB cancel requested: channel lost: peer disconnected or epoch changed
old call failed after 189.959µs
23:02:05 executor pair=A id=IM2QFGUIDL7VP75B74QR3ALSZB tool=wait finished err=context canceled; old result discarded
old call failed and execution canceled; stale cleanup/results ignored while new call was active; new result correct; B unchanged
```

这里 `channel lost` 指旧 epoch 对应的逻辑通道失效，测试已额外核对两条物理 WS 没有关闭或更换。

## 复现

```sh
go test -race -v -run '^TestEpochAndLateEvents$' .
go test -race -v ./...
go vet ./...
go build -o stream-gateway-lab .
```

以上检查均通过：专项 race 测试约 10.7 秒，完整 race 回归约 70.5 秒。此前的本地／远程一致性、两组隔离、断线与心跳、有限重连、SIGINT 退出、工具忽略取消检查均保留且通过。

本轮覆盖 M4 的上述三种状态切换及迟到事件。独立 cancel 消息、调用超时与结果竞争等剩余 M6 验收、跨主机验证不在本轮范围。
