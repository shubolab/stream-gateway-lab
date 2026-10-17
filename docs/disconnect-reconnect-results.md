# 最小断线处理与有限重连验收

- 验证时间：2026-10-17 22:07（Asia/Shanghai）。
- 环境：本机 macOS / arm64，Go 1.24.3；未增加依赖。
- 结论：**A 执行中断线后旧调用失败，Executor 尝试取消并丢弃旧结果；B 正常完成；A 在原进程内重连后只执行显式新调用。**
- 本次覆盖 M3 的明确关闭 WS 路径和 M6 的重拨额度分项；没有把静默失联、全部竞态或取消失败场景算作已验收。

后续补充：[心跳与静默失联验收](./heartbeat-silent-loss-results.md)。下文保留本轮明确断线实验的原始范围与结果。

## 实现边界

端点生命周期与通道生命周期分开。端点可以重拨；每个通道固定绑定一条 socket 和一个 epoch，失效后永不恢复。旧调用等待和工具执行使用旧通道的上下文，不能随新连接继续。

网关仍只维护静态绑定、连接和 epoch。任一端离开就作废 epoch，向健康对端发送空 epoch；健康对端保留自己的 WS 并有限等待。工具、调用结果和重放逻辑都没有进入网关。

| 边界 | 当前实现 |
| --- | --- |
| 重拨额度 | 初次拨号之外最多 3 次，固定间隔 5 秒；成功不重置 |
| 拨号上限 | 每次 5 秒 |
| 等待对端 | 连接正常但对端不在线，最多等 30 秒后停止 |
| 已知离线时调用 | 立即失败，不等待恢复、不排队 |
| 旧结果 | 发送与交付都检查原通道；失效即丢弃 |
| 停止 | 主动停止不重连；额度耗尽需重新启动端点 |

为观察同一 Brain 进程的恢复，增加可选 `-stdin`：一行 JSON 是一次显式的新请求，输出一次结果或错误；不自动重试失败输入。统一 `Executor.Call` 与 `runBrain` 保持原职责。

## 五个独立进程的实际断线

| 角色 | PID | 本次入口 |
| --- | --- | --- |
| Gateway | 57778 | `gateway` |
| Executor A | 57780 | `executor -pair A -url ws://127.0.0.1:53361/ws` |
| Executor B | 57781 | `executor -pair B` |
| Brain A | 57782 | `brain -pair A -stdin` |
| Brain B | 57783 | `brain -pair B -stdin` |

仅为本次手工验收，在 A Executor 与网关之间临时放置了本机 TCP 字节转发连接。在 A、B 的 wait 都已开始后，于 **22:07:48.714** 关闭 A 的这一条连接；五个角色进程都保持运行。该临时转发器不解释 WS 消息，没有写入工程，也未增加产品故障接口或修改系统防火墙。自动测试则直接关闭网关侧的真实 WS，不需要此转发器。

Brain A 实际依次输出：

```json
{"error":"channel lost: peer disconnected or epoch changed"}
{"error":"endpoint unavailable"}
{"results":[{"tool":"echo","output":{"message":"A-new","wait_ms":100}},{"tool":"wait","output":{"waited_ms":100}}]}
```

对应三个显式输入：执行中的 `A-old / 10000 ms`、离线时的 `must-not-be-queued / 1 ms`、恢复后的 `A-new / 100 ms`。第二个输入没有到达 Executor。

Executor A 的实际日志节选：

```text
22:07:48.686076 executor pair=A id=6FMNKU3EERPEB6NJFF4RU6SHIZ tool=wait started epoch=O6UUFYRSCSGW4TG7IIXJNCVTJH
22:07:48.715726 executor pair=A channel lost epoch=O6UUFYRSCSGW4TG7IIXJNCVTJH
22:07:48.715953 executor pair=A redial=1/3 in=5s
22:07:48.715969 executor pair=A id=6FMNKU3EERPEB6NJFF4RU6SHIZ cancel requested: channel lost: failed to get reader: failed to read frame header: EOF
22:07:48.716013 executor pair=A id=6FMNKU3EERPEB6NJFF4RU6SHIZ tool=wait finished err=context canceled; old result discarded
22:07:53.725722 executor pair=A ready epoch=PM3W4VHGM7ZVZAPUUSTPGLER77
22:07:53.937563 executor pair=A id=IJXZCDO646ITNSSGREVWFZMHV5 tool=wait started epoch=PM3W4VHGM7ZVZAPUUSTPGLER77
22:07:54.038915 executor pair=A id=IJXZCDO646ITNSSGREVWFZMHV5 tool=wait returned
```

A Executor 只重拨一次，原进程完成了第二次连接；A Brain 保留原 WS，没有重拨。恢复后先观察无额外工具执行，再显式发送 `A-new`。最终 A 共启动四次工具：旧流程的 echo、被取消的 wait，以及新流程的 echo、wait。旧 wait 只启动一次，离线输入没有补发。

网关对 A 实际转发四条请求、三条结果：旧 wait 没有回传成功结果。Brain A 的旧错误没有在恢复后变成成功。

## B 不受影响

B 的 `B-unaffected / 1500 ms` 在 **22:07:50.187655** 回传 wait 结果，此时 A 尚未恢复；A 到 **22:07:53.725989** 才重新就绪。随后显式提交 `B-still-working / 100 ms` 也成功。

B 的两个端点始终只有一次就绪事件，epoch 一直为 `QVNCIMLDKVUQ2RQV22CFBYEHI3`，没有重拨或通道替换。验收结束后正常停止全部五个角色进程，退出码均为 0，并清理临时转发器。

## 自动检查

`go test -race -v ./...`、`go vet ./...` 和编译均通过。本轮完整 race 测试运行约 28 秒，保留 M1、M2 回归检查，并新增：

- `TestDisconnectFailsOldCallAndRecovers`：分别关闭 A Brain、A Executor 的真实 WS。等待工具确实开始后断开，验证旧调用及时失败、工具观察到取消、离线调用立即失败、B 在断线期间完成、健康侧 socket 不变、新 epoch 只处理新调用，且旧通道不能补发结果。
- `TestReconnectBudgetIsFinite`：测试服务端让每次连接成功后立即断开。最终恰好连接四次（首次＋3 次重拨），即使中途成功也不重置额度，随后停止。实际终态为 `reconnect exhausted after 3 redials`。

可通过 [README](../README.md) 中的测试命令复现。当前取消只对合作退出的模拟工具验证；取消不代表真实操作被回滚。主动心跳、静默网络中断、未响应取消的工具及完整 M4–M6 竞态检查仍未开展。
