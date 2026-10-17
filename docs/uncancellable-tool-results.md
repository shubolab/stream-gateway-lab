# 工具不响应取消：执行槽与旧结果验证

- 时间：2026-10-17 22:47（Asia/Shanghai）。
- 环境：本机 macOS / arm64，Go 1.24.3。
- 结论：**A 断线后旧调用失败，旧工具继续占用执行槽；恢复后的新调用立即返回忙碌。旧工具真正退出后，旧结果丢弃，容量恢复，B 不受影响。**

## 最小范围

本轮没有修改生产代码、工具参数或协议，只增加 `uncancellable_test.go` 和说明。现有 `executorEndpoint` 的执行槽跨 socket／epoch 保留，直到实际工具函数退出才释放；现有旧通道检查阻止旧结果回到新通道。这次用实际场景验证这些规则。

模拟工具通过已有的 `execute` 函数字段注入，只用于测试：A 的第一次 wait 收到取消后继续等待，测试在验证重连和忙碌后允许它结束；即使未收到测试结束信号，最多 12 秒也会退出。它最终故意返回成功值 `"A-old-result"`，检验晚到成功是否被丢弃。

本次是单个 Go 测试进程内的两个 Brain、两个 Executor 和一个网关，通过真实本机 TCP／WS 通信。没有新增五进程编排、CLI 故障开关或通用测试框架；没有把本次结果说成跨主机验收。

## 验收流程

1. A 启动忽略取消的 wait，B 同时启动正常的 700 ms wait；确认两端工具都进入执行后再断线。
2. 分别关闭网关侧的 A Brain、A Executor WS，保留端点生命周期，按原规则在 5 秒后重拨。
3. 要求 A 旧调用在 1 秒内失败；Executor 工具上下文已取消，但旧工具尚未退出。B 原调用在 A 离线期间正常结束。
4. A 获得新 epoch 后，在旧工具仍运行时显式调用 echo，1 秒内收到 `tool failed: executor busy`。测试在 Brain 的真实 WS 收包处核对忙碌结果，排除只是调用方本地等待槽返回忙碌的情况。
5. A 忙碌期间，B 新 echo 成功；A 忙碌请求没有进入工具函数。
6. 让旧工具结束并返回成功值；等待实际执行槽释放，再显式提交 `A-after-old-exit`。新调用成功，旧成功值不交付。
7. 最终 A 工具启动数恰好是一个旧 wait、一个新 echo；忙碌请求没有排队，旧 wait 没有重放。Brain A 只观察到忙碌结果和新调用结果，随后 300 ms 无额外结果；B 双方通道和 A 的健康侧 socket 均保持不变。

## 实际结果

首次专项 race 验证结果如下，两个子场景均通过：

| A 断开的链路 | 旧调用失败耗时 | 重连并收到忙碌结果 | 新调用收到忙碌耗时 |
| --- | --- | --- | --- |
| Brain → Gateway | 0.695 ms | 断线后约 5.003 s | 0.738 ms |
| Executor → Gateway | 0.203 ms | 断线后约 5.002 s | 0.699 ms |

这些是本机观察值，不是硬实时保证。模拟工具在恢复后仍未退出，忙碌检查成功后才被测试允许结束。

Executor 链路断开场景的日志节选：

```text
22:47:07 executor pair=A id=PP2SMXZDW2QW4AWTELPV4LLGA5 cancel requested: channel lost: failed to get reader: failed to read frame header: EOF
22:47:07 executor pair=A redial=1/3 in=5s
22:47:12 executor pair=A ready epoch=G2UWX4X2FHFQ33C6XOW6ZG2MEI
recovered after 5.002165958s; old tool still running; new call returned busy in 698.75µs
22:47:12 executor pair=A id=PP2SMXZDW2QW4AWTELPV4LLGA5 tool=wait finished err=context canceled; old result discarded
old success discarded; busy request never executed; fresh request succeeded; B unchanged
```

这里的 `finished err=context canceled` 是执行端发现原上下文已取消后覆盖了工具返回状态；模拟工具本身仍然返回成功值。调用失败、请求取消与实际执行结束是三个不同时间点。

## 复现

```sh
go test -race -v -run '^TestUncancellableToolKeepsSlotAcrossReconnect$' .
go test -race -v ./...
go vet ./...
go build -o stream-gateway-lab .
```

以上检查均通过。完整 race 回归约 60 秒，本轮两个子场景约 10.6 秒；此前的两组隔离、明确断线、静默失联、有限重连及管道 SIGINT 退出检查均保留且通过。

本轮验证 M5 的远程断线恢复路径。生产 `echo`／`wait` 仍响应取消，没有增加“强行终止工具”能力；LocalExecutor 接入不合作工具、完整 M4／M6 竞态及跨主机场景不在本次验证范围。
