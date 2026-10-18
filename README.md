# stream-gateway-lab

## 当前：Python + DeepAgents 聊天工作台

Gateway / Executor 保持 Go，Brain 使用 Python + DeepAgents，通过 OpenAI Responses API 思考并调用固定 Executor 的文件与 shell 工具。页面采用精简聊天布局，默认完全访问、无需工具审批；创建会话前必须选择在线 Executor，掉线后已有会话降为仅聊天，下一轮在重新上线后恢复工具能力。

```sh
sh scripts/setup-brain.sh
sh scripts/build-distribution.sh
./dist/stream-gateway-lab gateway -listen 0.0.0.0:8787
```

打开 Gateway 地址，用 `data/admin-token` 登录，在侧栏「模型设置」填写 API 地址、模型名称和密钥，保存后下一轮生效，无需重启。接入或更新 Executor，然后选机器和工作目录开始会话。会话与轮记录落盘；支持多轮上下文、工具轨迹和停止。目前已用 Responses 测试服务验证真实 DeepAgents / Go 进程的完整修复链路，真实模型效果仍待验收。

详细状态机、目录职责、API 配置和验收步骤见 [第三阶段说明](docs/agent-chat.md)。公网入口：[gateway-lab.shubolab.com](https://gateway-lab.shubolab.com)，服务器模型配置位于 `/var/lib/stream-gateway-lab/model.json`。

## 第二阶段记录：共享 Executor 池与 Web 调度

这一阶段新增 Gin 控制台：Linux/amd64 与 macOS/arm64 安装助手注册机器入池，选择 Executor 创建任务，Gateway 预占该成员并启动独立 Brain 子进程，运行同一份 echo → wait 流程。所有机器属于同一个共享池。旧任务 API 保留；默认页面已升级为上述聊天工作台。

```sh
sh scripts/build-distribution.sh
./dist/stream-gateway-lab gateway -listen 0.0.0.0:8787
```

打开 Gateway 的 HTTP 根地址，用 `data/admin-token` 中的共享管理密钥登录（也可设置 `LAB_ADMIN_TOKEN`）。点击「接入 Executor」生成安装命令；将 Gateway 地址设置为外部 Linux 主机可达的地址。机器上线后，选择它、填写 message 和 wait_ms，启动任务并查看结果或停止进程。

注册身份保存在数据目录；任务结果只保留在当前 Gateway 会话。忙碌或离线的机器拒绝新任务，失败任务不自动重跑。详见[第二阶段设计、安装与 API](docs/pool-control-plane.md)和[本轮验收记录](docs/pool-acceptance-results.md)。

公网实验入口：<https://gateway-lab.shubolab.com>。登录、macOS 安装与服务维护见 [homelab 部署说明](docs/homelab-deployment.md)。

**下文保留第一阶段的静态绑定实验。现在默认 `gateway` 启动共享池，旧实验需要显式指定 `gateway -config bindings.example.json`。**

## 第一阶段：静态绑定实验

当前实验：两对大脑（Brain）和执行端（Executor）通过同一个网关（Gateway）并发调用 `echo`、`wait`；支持心跳检测静默失联及有限重连，旧调用失败且不重放。一个 Go 程序、三个角色入口、两组静态绑定，没有其他服务依赖。

代码阅读与持续讨论入口：[docs/code_study](docs/code_study/README.md)，包含架构图、`ctx`／`pair`／`epoch` 术语对照、调用与恢复时序，以及讨论记录。

[`internal/app/brain.go`](internal/app/brain.go) 的 `RunBrain` 是唯一的大脑流程：先调用 echo，从结果读取 `wait_ms`，再决定 wait 的参数。启动时选择 `LocalExecutor` 或 `RemoteExecutor`，大脑流程完全相同。

```text
本地：Brain → LocalExecutor → 模拟工具
远程：Brain A → RemoteExecutor A ─┐             ┌→ Executor A → 模拟工具
                               ├─ WS Gateway ┤
      Brain B → RemoteExecutor B ─┘             └→ Executor B → 模拟工具
```

## 目录与职责

```text
main.go                  程序入口，调用 app.Main
internal/
  app/                   三种角色的参数、准备和启动；模拟 Brain 流程
  gateway/               静态绑定、身份校验、端点配对与在线转发
  endpoint/              两端连接、Local/Remote 调用、执行端与模拟工具
  transport/             共用消息格式、WebSocket 写入和心跳
test/                   原有 7 个测试文件
```

依赖方向是 `main → app → gateway / endpoint → transport`。`app/app.go` 保留三个角色各自的启动函数，`app/brain.go` 放 echo → wait 流程；网关和端点不互相依赖。

测试集中在 `test/`，仍由原生 `go test ./...` 执行。单独运行某项测试时，目标包写 `./test`；根目录现在只有程序入口。为保留旧连接、epoch、迟到结果等白盒检查，测试所需的已有名称改为导出名称，仍位于 `internal/`，没有增加测试代理层。

## 运行

需要 Go 1.24 或以上。在本目录执行：

```sh
go build -o stream-gateway-lab .
./stream-gateway-lab brain -mode local
```

远程模式按下面顺序启动。以下是本机实验凭证，配置文件只存环境变量名；网关固定监听 `127.0.0.1:8787`。

终端 1，启动网关：

```sh
LAB_BRAIN_TOKEN=brain-A-demo LAB_EXECUTOR_TOKEN=executor-A-demo \
LAB_BRAIN_B_TOKEN=brain-B-demo LAB_EXECUTOR_B_TOKEN=executor-B-demo \
./stream-gateway-lab gateway -config bindings.example.json
```

终端 2，启动执行端 A：

```sh
LAB_EXECUTOR_TOKEN=executor-A-demo ./stream-gateway-lab executor -pair A
```

终端 3，启动执行端 B：

```sh
LAB_EXECUTOR_B_TOKEN=executor-B-demo ./stream-gateway-lab executor -pair B
```

终端 4，同时启动两份相同的 Brain 程序，使用不同实验标记和等待时间，分别保存结果：

```sh
lab_results_dir=$(mktemp -d)
LAB_BRAIN_TOKEN=brain-A-demo ./stream-gateway-lab brain -pair A \
  -message A-only -wait-ms 800 > "$lab_results_dir/A.json" &
lab_brain_a=$!
LAB_BRAIN_B_TOKEN=brain-B-demo ./stream-gateway-lab brain -pair B \
  -message B-only -wait-ms 1200 > "$lab_results_dir/B.json" &
lab_brain_b=$!
wait "$lab_brain_a"
wait "$lab_brain_b"
cat "$lab_results_dir/A.json" "$lab_results_dir/B.json"
```

预期 A 只收到 `A-only` 和 `waited_ms: 800`，B 只收到 `B-only` 和 `waited_ms: 1200`。本地对照使用同一组参数，只改变执行模式：

```sh
./stream-gateway-lab brain -mode local -message A-only -wait-ms 800
./stream-gateway-lab brain -mode local -message B-only -wait-ms 1200
```

默认加载 `bindings.example.json`，支持 `-config <文件>`。配置现为绑定数组，示例包含 A、B；`-pair` 默认 A。网关需要四份不同凭证，每个端点只需要本组、本角色的一份。重复绑定名和重复凭证会在网关启动时被拒绝。Local 模式仍不需要配置、凭证或网关。

连接与转发日志输出到标准错误。已连接的端点最多等对端 30 秒，请在此期间启动大脑。默认 Brain 完成一次流程后退出；Executor 保留连接，最多再等 30 秒，期间可以接入新的 Brain。结束实验时用 Ctrl+C 停止 Executor 和网关。

## 断线后提交新调用

用 `-stdin` 保持同一个 Brain 进程，每输入一行 JSON 显式发起一次新流程；失败只输出一次错误，不自动重试。仍然调用同一个 `RunBrain`，不改变工具调用接口：

```sh
LAB_BRAIN_TOKEN=brain-A-demo ./stream-gateway-lab brain -pair A -stdin
```

输入示例，每行一次请求：

```json
{"message":"A-old","wait_ms":10000}
```

在执行中断开 A 的一条 WS，旧调用返回 `{"error":"channel lost: ..."}`。离线期间继续输入会立即返回 `{"error":"endpoint unavailable"}`，不会留到恢复后执行。日志再次出现 `ready epoch=...` 后，手工输入新的一行：

```json
{"message":"A-new","wait_ms":100}
```

这次会返回包含 `A-new` 与 `waited_ms: 100` 的 `results`。EOF 结束 Brain；等待输入时收到 SIGINT（Ctrl+C）也会退出并释放连接，即使管道写端仍打开或只输入了半行。鉴权失败、等待对端超时或重拨额度耗尽时进程结束。默认一次性 Brain 仍会在调用失败后退出，需要验证同进程恢复时使用 `-stdin`。

管道退出的回归命令为 `go test -race -v -run '^TestBrainStdinStops$' ./test`，覆盖本地／远程模式的空闲、半行输入和正常 EOF；详见 [SIGINT 修复记录](docs/stdin-sigint-results.md)。

不需要手工修改网络即可复现故障的自动检查：

```sh
go test -race -v -run 'TestDisconnectFailsOldCallAndRecovers|TestSilentLossFailsOldCallAndRecovers|TestReconnectBudgetIsFinite' ./test
```

测试在工具确实开始后关闭 A 的真实 WS，分别覆盖 Brain 链路和 Executor 链路；B 保持连接。连接预算固定为首次拨号之外再尝试 3 次、间隔 5 秒，单次拨号最多 5 秒；成功不补充额度，主动停止不重连。对端离线时保留自身健康 WS，最多等待 30 秒。只有连接会重试，调用不会重试。

工具不响应取消的场景可单独验证：

```sh
go test -race -v -run '^TestUncancellableToolKeepsSlotAcrossReconnect$' ./test
```

该测试通过真实 WS 在 A 执行中断线，注入的工具故意忽略取消，最长运行 12 秒。A 重连后先验证新调用收到 `tool failed: executor busy`，再让旧工具退出；旧结果丢弃，后续显式新调用成功，B 的调用和连接不受影响。模拟工具只存在于测试中，产品 `echo`、`wait` 和运行参数不变。

epoch 切换与迟到事件可单独验证：

```sh
go test -race -v -run '^TestEpochAndLateEvents$' ./test
```

覆盖两条 WS 都保持不变时直接下发新 epoch（不先发离线通知），以及 A Brain／Executor 重连后的旧清理回调。旧调用失败并请求取消旧执行；在新调用仍执行时注入旧通道回调、旧 epoch 结果和旧调用 ID，确认新通道与新结果不受影响，B 原调用和新调用正常。状态切换与事件注入只在测试中进行，没有增加网关控制接口。

调用超时与结果竞争可单独验证：

```sh
go test -race -v -run '^TestTimeout' ./test
```

覆盖结果先到、超时先到，以及超时和结果同时就绪时的终态选择；另用真实 WS 验证 A 的 500 ms 调用超时后只返回一次失败，执行端观察到取消，迟到成功被丢弃。恢复后，在新调用执行中注入两次旧结果，确认新调用只收到自己的结果，B 原调用、新调用和连接保持正常。500 ms 只来自测试传入的 context，运行时上限仍为 15 秒。

静默失联测试只丢弃 A 链路的双向字节，不注入 TCP／WS 关闭，也不保存字节等恢复。网关和端点各自每 1 秒发 WS Ping，2 秒内未收到对应 Pong 就关闭这条连接，走原有的失联、取消与有限重连路径。正常本机调度下探测约需 2–3 秒；这不是硬实时保证。健康 B 即使没有业务消息也会正常响应心跳。

## 验证与边界

```sh
go test -race -v ./...
go vet ./...
```

测试使用真实本机 WS 检查：统一大脑流程、本地与远程结果一致、工具错误回传、两组并发隔离、身份校验，以及明确断线／静默失联后的失败、取消、有限重连、恢复后只执行新调用。并发测试核对请求实际到达的 Executor 身份以及结果返回的 Brain，不只比较 echo 内容。

本次分包前后均运行了完整 race 测试，结果一致：Go 1.26.4 下原有 4 个 SIGINT 子场景失败，其他用例通过，无 race 报告；详见[分包验证记录](docs/code_study/discussions/2026-10-18-006-package-and-test-layout.md)。

[超时与结果竞争记录](docs/timeout-result-race-results.md)给出此前终态选择、取消和恢复证据；[epoch 与迟到事件记录](docs/epoch-late-events-results.md)、[工具忽略取消记录](docs/uncancellable-tool-results.md)、[心跳与静默失联记录](docs/heartbeat-silent-loss-results.md)、[断线与重连记录](docs/disconnect-reconnect-results.md)、[两组隔离记录](docs/step-2-pair-isolation-results.md)和[第一步记录](docs/step-1-results.md)保留前几轮验收。

每组仍一次一个调用、单次 JSON 结果。网关按 pair 分别维护锁、连接与 epoch，只读路由外层并在线转发；两端只保留当前调用的工作内存，不保存完成结果，不排队、不重放、不落库。

每次调用绑定原来的 socket 与 epoch；断线或通道变化就取消旧上下文，新连接不接管旧等待。执行槽一直保留到工具真正退出，旧结果不会改用新通道发送。已验证工具忽略取消时，恢复后的调用先返回忙碌，旧工具退出后容量恢复。取消不代表真实操作可以回滚。

等待结果的分支已汇入同一个终态检查，并有超时与结果竞争的测试和记录。独立 cancel 消息仍未实现；调用超时／主动取消沿用关闭本次 WS 请求执行端取消，之后仍按有限额度恢复连接。没有新增协议、依赖或 CLI 参数。

完整实验的后续约定见[技术设计](docs/technical-design-and-implementation-plan.md)，不代表这些能力已经实现。
