# 讨论 006：按现有职责分包，并把测试集中到 test

- 日期：2026-10-18（Asia/Shanghai）。
- 用户要求：代码分包要清晰合理，不增加复杂度、设计或功能；测试文件集中到 `test/`。
- 用户已确认：保留原生 `go test ./...`，接受为此调整包结构和测试所需的访问名称。

## 现有职责怎样归位

| 位置 | 放什么 | 依赖 |
| --- | --- | --- |
| [`main.go`](../../../main.go) | 调用 `app.Main` | app |
| [`internal/app`](../../../internal/app) | 三个角色的参数、准备、启动；模拟 Brain 流程与标准输入 | gateway、endpoint |
| [`internal/gateway`](../../../internal/gateway) | 静态绑定、鉴权、配对、在线转发 | transport |
| [`internal/endpoint`](../../../internal/endpoint) | 两端共用连接、Local/Remote 调用、执行端和模拟工具 | transport |
| [`internal/transport`](../../../internal/transport) | 消息结构、共用常量、心跳和有限超时写入 | 无项目内包依赖 |
| [`test`](../../../test) | 原有 7 个测试文件 | 上述内部包 |

`app/app.go` 仍然分为 `runGatewayCommand`、`runBrainCommand`、`runExecutorCommand` 三个入口；`app/brain.go` 单独放 echo → wait 的模拟决策流程。

原 `main.go` 的绑定定义移到 `gateway/binding.go`；原 `gateway.go` 中两端也会使用的 `writeFrame` 移到 `transport/frame.go`，改名为 `WriteFrame`。其余主要按原文件职责归位。没有增加构造器封装、注册机制或新的业务接口，也没有新增运行依赖。

## 为什么有些名称变成大写

Go 的不同目录属于不同包。测试移到 `test/` 后，不能再直接访问原包的小写名称。

原有测试会检查配对状态、注入旧 epoch、模拟迟到结果，并触发旧连接清理。为了保留这些检查，跨包使用的已有类型、字段和方法改成导出名称，例如 `PairRoute.Mu`、`PairRoute.Peers`、`RemoteExecutor.Pending`、`Client.Snapshot`。访问受锁保护的状态时，仍使用原来的锁。

这些包位于 `internal/`，供本工程使用。此次没有把白盒检查改成弱化的结果检查，也没有复制业务源码到测试目录、引入生成步骤或新增一套测试代理方法。

## 迁移前后核对

环境：本机 macOS / arm64，Go 1.26.4。迁移前保留了包含此前未提交改动的源码快照，以下比较以该快照为基线。

- 源码结构核对：忽略注释、标识符改名和新增包前缀后，原有 76 个声明全部对应，语句结构、条件、运算符、字面量和测试断言保持一致。根目录仅新增调用 `app.Main` 的薄入口。
- `go build .` 与 `go vet ./...` 通过。
- 36 组 CLI 对照覆盖帮助、无效参数、配置错误、本地调用与逐行输入；排除时间戳和 PID 后，退出码、标准输出和标准错误一致。
- 实际进程对照分别运行迁移前后版本：网关、A/B 执行端、并发 A/B Brain；A 一次性请求、B 两行标准输入均成功，工具输出一致。
- 迁移前后均执行 `go test -race -count=1 -json ./...`。测试及子测试事件均为 26 个通过、5 个失败事件；失败事件包括下列 4 个子场景及它们的父测试。所有用例的结果一致，没有 race 报告。

原有失败是 `TestBrainStdinStops` 的 `local/idle`、`local/partial`、`remote/idle`、`remote/partial`：收到 SIGINT 后报告 `interrupt signal received` 并以状态 1 退出。该现象在分包前已复现，此次没有修复或掩盖；因此本轮不声称完整测试通过。

## 现在怎样运行

构建和启动方式不变：`go build .`、`go run . gateway`、`go run . brain ...`、`go run . executor ...`。

```sh
go test -race -count=1 ./...
go test -race -count=1 -run '^TestEpochAndLateEvents$' ./test
go vet ./...
```

历史验收文档保留当时的命令和输出。其中单项测试命令末尾的 `.` 如今需改成 `./test`，否则根目录没有测试可运行。当前 README 已更新这些命令。

[配对状态锁的压测评估待办](2026-10-18-005-pair-lock-performance-todo.md)继续保留。这次仅随分包调整名称，没有修改持锁范围、重连规则、调用限制或消息格式。
