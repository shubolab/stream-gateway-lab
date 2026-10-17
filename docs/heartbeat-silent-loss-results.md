# 最小心跳与静默失联验收

- 验证时间：2026-10-17 22:25（Asia/Shanghai）。
- 环境：本机 macOS / arm64，Go 1.24.3；没有新增依赖。
- 结论：**A 链路只丢弃数据、不正常关闭时，约 3 秒发现失联，旧调用失败、执行端取消旧 wait；B 保持正常；恢复后只执行显式新调用。**

## 本轮实现

网关与端点共用 `heartbeat.go`：每条 WS 每 1 秒发送 Ping，最多等 2 秒对应 Pong；失败取消本条连接的读循环并强制关闭 socket，复用原有的失联、epoch 作废与有限重连处理。读循环持续处理控制帧；工具执行、等待配对及没有业务消息时也保活。

没有新业务消息、缓存、队列、重放或持久化。统一 `Executor.Call` 和大脑决策流程不变；重拨仍为初次之外最多 3 次、间隔 5 秒，成功不补充额度。心跳绑定具体 socket，不拿 Pair 锁等待 Pong，不会因 A 失联而重建 B 的连接。

实验采用 1 秒／2 秒，让失联检测明显早于原有的 15 秒调用期限，避免把调用超时误当成心跳有效。约 3 秒是正常本机调度下的检测时间，不是硬实时保证。

## 五个独立进程

先按 [README](../README.md) 配置四个实验凭证并编译。实际使用以下进程，Brain 均以 `-stdin` 保持同一进程接受显式新输入：

| 角色 | PID | 本次入口 |
| --- | --- | --- |
| Gateway | 63222 | `gateway` |
| Executor A | 63223 | `executor -pair A -url ws://127.0.0.1:58581/ws` |
| Executor B | 63224 | `executor -pair B` |
| Brain A | 63225 | `brain -pair A -stdin` |
| Brain B | 63226 | `brain -pair B -stdin` |

A Executor 到网关之间临时放置一个 TCP 字节转发器，其他端点直连网关。等待 A 的 10,000 ms wait 与 B 的 4,500 ms wait 都真正开始后，只切换为双向丢弃字节；不注入 Close 帧、TCP FIN 或 EOF，不保存待补发字节。转发器也不把一侧后来主动关闭产生的 EOF 传给另一侧，因此两侧必须各自发现失联。故障期间实际丢弃 Executor→Gateway 7 字节、反向 3 字节。

临时转发器仅用于本次验收，没有新增产品故障接口、通用代理程序或系统防火墙规则。

| 时间 | 实际结果 |
| --- | --- |
| 22:25:34.127 | 开始双向丢弃 A Executor 链路数据，五个角色进程继续运行。 |
| 22:25:37.088 | A Executor 报 `heartbeat failed`，旧 wait 以 `context canceled` 结束，旧结果丢弃。 |
| 22:25:37.089 | 网关独立报该链路 `heartbeat failed` 并作废 epoch；Brain A 输出旧调用失败，距故障 **2.963 秒**。 |
| 22:25:37.090 | 离线新输入立即返回不可用；恢复网络转发，不补发丢弃的字节。 |
| 22:25:38.639 | B 原 4,500 ms 调用正常完成，A 尚未重新就绪。 |
| 22:25:38.803 | B 的另一次显式新调用也完成，A 仍未重新就绪。 |
| 22:25:42.093 | A Executor 第一次重拨成功，双方收到新的 epoch。 |
| 22:25:42.519 | A 显式提交的 `A-new / 100 ms` 完成。 |

Brain A 依次输出：

```json
{"error":"channel lost: peer disconnected or epoch changed"}
{"error":"endpoint unavailable"}
{"results":[{"tool":"echo","output":{"message":"A-new","wait_ms":100}},{"tool":"wait","output":{"waited_ms":100}}]}
```

Executor A 日志节选：

```text
22:25:34.107317 executor pair=A id=EROK5ZXQOVZJTGXLU74FMXMJCO tool=wait started epoch=AAFHDLDWT3AHGFH4B67LVS4GWQ
22:25:37.088451 executor pair=A disconnected: heartbeat failed: failed to ping: failed to wait for pong: context deadline exceeded
22:25:37.088463 executor pair=A redial=1/3 in=5s
22:25:37.088473 executor pair=A id=EROK5ZXQOVZJTGXLU74FMXMJCO cancel requested: channel lost: heartbeat failed: failed to ping: failed to wait for pong: context deadline exceeded
22:25:37.088703 executor pair=A id=EROK5ZXQOVZJTGXLU74FMXMJCO tool=wait finished err=context canceled; old result discarded
22:25:42.093371 executor pair=A ready epoch=ANXLJEGQCPBFAN7A3IMGV2KZAO
```

恢复后先观察 300 ms，没有额外工具启动或迟到成功，再提交 `A-new`。A 全程只启动四次工具：旧流程的 echo、被取消的 wait，新流程的 echo、wait；网关对应四条请求、三条结果。离线输入没有排队，旧 wait 没有重放。A Brain 保留原 WS，A Executor 只重拨一次。

B 两端直到主动结束实验都只有一次就绪事件，epoch 始终为 `FSUTWDRIVEVDHH6W23WDRRKMFW`，无重拨。验收期间五个进程均保持存活；结束时给 Brain 输入 EOF，再停止 Executor 和网关，五个进程退出码均为 0，8787 端口已释放。

本次另观察到既有实验入口的边界：管道形式的 `brain -stdin` 在等待输入时，仅发 SIGINT 可能不能及时结束阻塞读取；首次验收清理时两个 Brain 需强制结束。改用文档约定的 EOF 后重跑，正常收尾。本轮没有修改该输入入口或扩展退出机制。

后续已单独修复上述输入阻塞问题，见 [SIGINT 修复与验收](./stdin-sigint-results.md)；此处保留发现问题时的原始记录。

## 自动复现与回归

在仓库根目录执行：

```sh
go test -race -v -run TestSilentLossFailsOldCallAndRecovers .
```

测试分别对 A Brain、A Executor 链路实施双向丢弃：要求旧调用在 4 秒内失败，失联端的原因确实为心跳失败，执行端观察到取消；已知离线调用立即失败，B 在 A 离线期间完成原调用和新调用。随后检查 A 新 epoch、健康侧原 socket、B 原通道均符合预期，旧通道不能发结果，A 只执行一个旧 wait 和一个显式新 echo。测试中的丢包转发器只存在于 `_test.go`。

完整 `go test -race -v ./...` 通过，约 43 秒；静默失联子场景分别在 2.993 秒（Brain）和 3.006 秒（Executor）使旧调用失败。`go vet ./...` 与编译均通过，保留本地／远程一致性、两对隔离、明确断线和固定重拨额度的回归检查。

当前只验证合作取消的模拟工具、本机 WS 与双向静默失联；未开展跨主机验收、取消失败和完整故障竞态检查。取消仍不意味着真实操作已经回滚。
