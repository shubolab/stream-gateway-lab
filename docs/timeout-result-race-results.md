# 调用超时与结果竞争验收

- 时间：2026-10-17 23:14（Asia/Shanghai）。
- 环境：本机 macOS / arm64，Go 1.24.3。
- 结论：**超时调用返回一次失败，执行端观察到取消；迟到成功和重复旧结果不能改变终态或影响新调用，B 保持正常。**

## 最小改动

`executor.go` 将结果、调用超时和通道失效三个等待分支汇入 `waitResult` 的同一个终态检查：先检查调用 context，再检查原通道，最后接受工具结果。`Call` 仍同步返回一次，并在返回时清除 pending。没有引入完成结果缓存或新的生命周期状态。

取消继续使用已有退路：Brain 自身调用超时后关闭该调用的 WS，网关通知 Executor 原通道失效；Executor 的工具 context 还受收到的剩余时间预算限制。恢复继续使用原有 5 秒重拨间隔和有限次数。协议、网关、模拟工具、依赖和 CLI 参数均不变。

新增 `timeout_test.go`。同步点和故意返回迟到成功的工具只在测试中存在，没有新增故障注入服务或测试时钟。

## 终态选择

`TestTimeoutResultArbitration` 直接验证生产 `waitResult` 的三个时序：

| 时序 | 预置条件 | 结果 |
| --- | --- | --- |
| 结果先到 | 调用 context 有效，结果已经就绪 | 返回成功。 |
| 超时先到 | 调用 context 已超时，没有结果 | 返回 `context deadline exceeded`，输出为 nil。 |
| 两者同时就绪 | 调用 context 已超时，同时预先放入成功结果 | 无论 `select` 选择哪一分支，都返回超时，输出为 nil。 |

“同时就绪”通过预置两个已就绪事件验证，不依赖反复 sleep 恰好撞上调度窗口。判断点是终态检查时的 context 状态；已在期限内完成的成功不会因之后的超时变成失败。

## 真实 WS 验收

`TestTimeoutDiscardsLateResultAndRecovers` 在一个 Go 测试进程内启动一个网关和 A／B 两组端点，通过真实本机 TCP／WS 通信：

1. 确认 B 的 1000 ms wait 已启动，再以 500 ms 调用 context 启动 A。A 工具观察到取消后暂缓退出，确保 Brain 自身的 deadline 分支真正触发。
2. A 返回一次 `context deadline exceeded`，无成功输出。检查原 Brain 通道、Executor 通道和工具 context 均被取消，pending 已清空。再次触发旧通道结果回调，旧等待槽没有接受该结果。
3. 允许 A 旧工具返回 `"A-late-success"`；执行端丢弃旧结果并释放执行槽。B 的原 wait 正常返回，随后 echo 也返回 `"B-unaffected"`。
4. A 按既有 5 秒间隔重连，双方获得一致的新 epoch，Executor 原健康 socket 保留。显式发起新调用，并暂缓新工具返回。
5. 使用当前 WS 和当前 epoch 连续发送两次旧调用 ID 的成功结果。Brain 的观测点位于 `receive` 之后，确认旧消息已经处理，随后检查新调用仍未结束。
6. 允许新工具返回，A 只得到 `"A-new-result"`。A 总共执行旧、新两个工具，无重放；B 两端的通道对象、socket 和 epoch 均未变化。

当前 epoch 上的旧 ID 注入故意绕过“旧 epoch 会被过滤”这一保护，用来独立验证请求关联；它不是执行端在恢复后补发业务结果。

首次专项 race 测试的实际日志：

```text
cancel requested: context deadline exceeded
brain pair=A redial=1/3 in=5s
500 ms call returned timeout after 504.961375ms
tool=wait finished err=context deadline exceeded; old result discarded
one timeout return; pending cleared; cancellation observed; old success and duplicate results discarded; fresh call succeeded; B unchanged
--- PASS: TestTimeoutDiscardsLateResultAndRecovers (5.51s)
```

504.96 ms 是本机观察值，不是硬实时保证。500 ms 仅通过测试调用的 context 传入，生产调用上限仍为 15 秒。若合作退出的 Executor 工具先按自身预算返回错误，Brain 也可能先收到 `tool failed: context deadline exceeded`；本测试延迟工具退出，专门覆盖 Brain 自身超时及其后到达的成功结果。

## 复现

```sh
go test -race -v -run '^TestTimeout' .
go test -race -v ./...
go vet ./...
go build -o stream-gateway-lab .
```

以上检查均通过：专项 race 测试约 7.1 秒，完整 race 回归约 76.7 秒，无竞态报告。此前的本地／远程一致性、两组隔离、断线与心跳、有限重连、SIGINT 退出、工具忽略取消、epoch 与迟到事件检查均保留且通过。

本轮覆盖 M6 的调用超时与结果竞争，沿用已有断线恢复与隔离机制；跨主机实验、独立 cancel 消息和其他工程化能力仍未扩展。
