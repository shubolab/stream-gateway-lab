# 第二对绑定验收：同一网关下 A、B 并发隔离

本记录保留该轮验收时的行为。当前已补充[断线与有限重连](./disconnect-reconnect-results.md)，端点生命周期与复现命令以最新 [README](../README.md) 为准。

- 验证时间：2026-10-17 21:47（Asia/Shanghai）。
- 环境：本机 macOS / arm64，Go 1.24.3；依赖未增加。
- 结论：**M2 通过。A、B 同时调用同一网关，标记与结果分别正确，两组 wait 的执行时间重叠约 800 ms。**
- 本轮只实现两组静态绑定与隔离验证；此前第二步计划中的重连、故障恢复等部分尚未开展。

## 最小改动

网关启动时建立 `pair → pairRoute` 静态表，每条 route 独立维护凭证、两端连接、epoch 和锁。发送与清理都限制在本组；网关仍不解析工具内容，不增加调用表、缓存或重放。

配置从单对象改为绑定数组，包含 A、B 四份独立凭证。Brain 和 Executor 用 `-pair` 选择身份，每个进程只承担一个逻辑实例。Brain 的统一流程保持不变，仅增加实验输入 `-message`、`-wait-ms`，以区分两组结果。

## 实际结果

网关启动前，先运行两组本地对照；随后启动同一网关下的两对远程端点。逐字比较每组本地／远程标准输出，均一致：

| 绑定 | echo 结果 | wait 结果 | 本地／远程比较 |
| --- | --- | --- | --- |
| A | `{"message":"A-only","wait_ms":800}` | `{"waited_ms":800}` | 完全一致 |
| B | `{"message":"B-only","wait_ms":1200}` | `{"waited_ms":1200}` | 完全一致 |

两组远程 wait 实际日志：

```text
21:47:45.470562 executor pair=A tool=wait started
21:47:45.471753 executor pair=B tool=wait started
21:47:46.271857 executor pair=A tool=wait returned
21:47:46.673796 executor pair=B tool=wait returned
```

重叠区间为 `21:47:45.471753` 至 `21:47:46.271857`，共 **800.104 ms**。A 的 Executor 于 `21:47:46.272459` 随会话结束退出；B 随后仍正常完成。这是实际并发调用，未依次跑两组来代替。

## 进程与连接证据

执行期间用 `ps` 确认以下五个独立进程同时存在：

| 角色 | PID | 启动参数 |
| --- | --- | --- |
| Gateway | 51584 | `gateway` |
| Executor A | 51585 | `executor -pair A` |
| Executor B | 51586 | `executor -pair B` |
| Brain A | 51587 | `brain -mode remote -pair A -message A-only -wait-ms 800` |
| Brain B | 51588 | `brain -mode remote -pair B -message B-only -wait-ms 1200` |

`lsof` 同时记录了唯一监听端口和四条出站连接，省略网关侧重复的连接行：

```text
PID 51584  TCP 127.0.0.1:8787 (LISTEN)
PID 51585  TCP 127.0.0.1:63908->127.0.0.1:8787 (ESTABLISHED)
PID 51586  TCP 127.0.0.1:63909->127.0.0.1:8787 (ESTABLISHED)
PID 51587  TCP 127.0.0.1:63910->127.0.0.1:8787 (ESTABLISHED)
PID 51588  TCP 127.0.0.1:63911->127.0.0.1:8787 (ESTABLISHED)
```

网关关键日志如下。脚本断言每组各有两次请求、两次结果，共八条 data 转发：

```text
21:47:45.426442 gateway listening on 127.0.0.1:8787 bindings=2
21:47:45.469999 gateway pair=A forward brain->executor bytes=185
21:47:45.470324 gateway pair=A forward executor->brain bytes=155
21:47:45.470490 gateway pair=A forward brain->executor bytes=161
21:47:45.471307 gateway pair=B forward brain->executor bytes=186
21:47:45.471527 gateway pair=B forward executor->brain bytes=156
21:47:45.471688 gateway pair=B forward brain->executor bytes=162
21:47:46.272000 gateway pair=A forward executor->brain bytes=138
21:47:46.674187 gateway pair=B forward executor->brain bytes=139
```

两个本地对照进程 PID 为 51582、51583。所有七个进程退出码均为 0；网关在验收后收到 SIGTERM，未遗留后台服务。

## 自动检查与边界

`go test -race -v ./...`、`go vet ./...`、`go build` 均通过：

- 原有统一大脑的本地／远程对照及工具错误回传继续通过。
- 两组各进行三轮调用；每轮等两个请求都抵达执行端后才允许返回，确保同时在途。测试端点报告自己的身份，分别断言请求到达本组 Executor、结果返回本组 Brain，能识别请求和结果同时错配的情况。
- A 的 Brain 凭证访问 B 或冒用 Executor，以及访问不存在的绑定，均返回 HTTP 401。
- 第二条 A Brain 连接返回 HTTP 409，原有 A、B 仍可完成调用；重复绑定名和重复凭证在启动时被拒绝。

没有开展慢连接、主动心跳、断线重连、代次恢复、取消失败或完整 M3–M6 验收；race 检查也只覆盖本次运行路径。复现命令见 [README](../README.md)。
