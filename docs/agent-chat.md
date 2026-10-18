# 第三阶段：Python Brain 与固定 Executor 聊天

页面只保留会话列表、聊天、工具调用记录、停止当前轮、新建会话、模型设置和接入 Executor。默认完全访问，工具直接执行，没有权限审批。工作目录只决定相对路径与 shell 的 cwd；绝对路径可访问 Executor 用户有权限访问的任意位置。

## 启动和模型配置

```sh
sh scripts/setup-brain.sh
sh scripts/build-distribution.sh
./dist/stream-gateway-lab gateway -listen 0.0.0.0:8787
```

`setup-brain.sh` 使用 uv 安装锁定的 Python 3.12 环境和依赖，并且只在不存在时生成私密配置文件。Python 包支持 3.12 / 3.13，核心依赖是 DeepAgents 0.7.28。Go Gateway 和 Executor 不依赖 Python；只有 Gateway 所在机器需要 Brain 环境。

登录页面后，在侧栏「模型设置」填写 API 地址、模型名称和 API 密钥并保存。密钥输入框不回显已保存的值，留空保留原密钥；首次保存必须填写。高级参数仍可通过配置文件调整，页面保存会保留这些参数：

```json
{
  "base_url": "https://api.openai.com/v1",
  "api_key": "填写服务商密钥",
  "model": "填写支持工具调用的模型名",
  "max_model_calls": 20,
  "request_timeout": 90,
  "max_output_tokens": 4096
}
```

`base_url` 填 API 基址，通常以 `/v1` 结尾，不要追加 `/responses`。代码通过 LangChain OpenAI 集成明确使用 **POST /v1/responses**；不会回退到 Chat Completions。服务需要支持 Responses 的 `function_call`、`function_call_output`、`store:false` 和 `parallel_tool_calls:false`。可选 `reasoning` 对象原样交给集成层；只在供应商支持时填写。

每一轮都会重新读取配置，填写后不需要重启 Gateway。页面的“模型已配置”只检查必要字段和 Python 路径，不代表已经连接模型成功。凭证通过已登录的管理接口保存到服务端，读取和保存响应均不返回密钥，也不传给 Executor。配置文件原子写入，权限为 0600。SDK 自动重试关闭，避免重复模型请求。

公网部署在同一页面完成配置，实际保存到 `/var/lib/stream-gateway-lab/model.json`；本地运行保存到 `brain/model.local.json`，不会自动同步到服务器。管理 API 为 `GET /api/model-config` 与 `PUT /api/model-config`，沿用管理令牌认证。服务配置、更新方式见 [homelab 部署说明](homelab-deployment.md)。

## 会话状态机

| 状态 / 事件 | 页面和服务端行为 |
| --- | --- |
| 创建会话 | 必须选择在线且空闲的 Executor；绑定保存后不能切换 |
| 就绪 | 可以发送消息；每轮启动独立 Python Brain 并预占绑定 Executor |
| 回复中 | 展示处理状态和工具记录；同一会话不能并发提交，可停止 |
| Executor 正在处理其他会话 | 当前会话保留历史，等待机器空闲后再提交 |
| Executor 离线 | 已有会话继续接收聊天消息，模型请求不提供机器工具 |
| 执行中掉线 | 旧调用结果视为未知，本轮永久降为仅聊天；不重试、不重放 |
| Executor 重新上线 | 下一轮恢复工具能力；当前降级轮不会恢复或补做命令 |
| 停止 / Gateway 重启 | 停止当前轮与远端命令，保存已完成轮；中断轮不会自动恢复执行 |

会话可共用同一台机器，但在线机器一次仅执行一轮，不排队。完全访问是权限策略；仅聊天是连接能力，两者独立。心跳判定与网页轮询有短暂延迟，后端仍会根据连接和 epoch 拒绝失效调用。

## 代码结构与执行路径

```text
浏览器聊天页 → Go/Gin control → 每轮 Python Brain / DeepAgents → Responses API
                                      ↓ tool call
                         GatewayExecutorBackend → RemoteExecutor
                                      ↓ WebSocket
                         Go Gateway 按固定绑定和 epoch 转发
                                      ↓
                         选定的 Go Executor → 文件系统 / shell
```

- `internal/control/sessions.go`：会话、固定绑定、派生状态、磁盘持久化。
- `internal/control/agent.go`：启动 / 停止 Python 子进程，接收 JSONL 事件，管理 Executor 预占。
- `internal/control/model_config.go`：模型连接配置的校验、密钥保留与原子保存。
- `brain/gateway_brain/__main__.py`：模型配置、DeepAgents 工具循环、对话上下文、工具串行与离线门控。
- `brain/gateway_brain/backend.py`：把 DeepAgents 的文件和执行接口转换为远程工具调用。
- `brain/gateway_brain/wire.py`：与 Go 端一致的 WS 协议，绑定、epoch、超时与结果匹配。
- `internal/endpoint/machine_tools.go`：文件读写 / 编辑 / 搜索、shell 执行与进程组清理。
- `internal/control/web/`：无前端构建依赖的聊天 UI，每秒更新状态和步骤。

DeepAgents 的子 Agent 关闭。上下文压缩采用 LangChain 的纯消息摘要，达到约 24k tokens 时保留近期 12 条消息；摘要不读写 Executor 文件，离线也能继续聊天。机器工具从不回落到 Brain 主机本地执行。

Gateway 数据目录保存 `sessions.json`、`turns/<id>.json` 和 `histories/<session-id>.json`。完整 UI 轮记录在结束时落盘；模型上下文在每个图状态更新后原子保存。进程被硬杀时，正在进行的 UI 步骤可能不完整；下一轮将未配对工具调用标为中断，不重发请求。

## 真实模型验收

1. 更新 Executor 到这一版（旧版只有 echo / wait）；已安装机器可用原安装目录再次安装并保留身份。
2. 将 `scripts/examples/broken.py` 复制到 Executor 上一个实际目录，确保该机器安装了 Python 3。
3. 在页面选择在线机器和该工作目录创建会话，发送：

   > 请读取 broken.py，用 python3 执行，分析实际报错并修复，再执行一次验证。只修改这个实验脚本，报告错误原因和最终输出。

4. 应看到读取、失败执行、编辑、成功执行和最终解释；成功证据是实际 `exit_code: 0` 与 `VERIFIED:4`。
5. 停止该 Executor，再发送一条普通聊天；页面应显示仅聊天，仍能回复，不产生新的机器调用。重启同一 Executor 后，下一轮可重新调用工具。

## 已验收与边界

自动集成测试使用本地确定性 Responses 测试服务，驱动真实 DeepAgents、真实 Go Gateway、两个真实 Go Executor 和真实文件 / 子进程。6 项集成测试已验证读文件 → ZeroDivisionError → 编辑 → 再执行 VERIFIED:4、多轮上下文、不串机器、离线聊天、重连后恢复、停止进程、重启后历史保留、模型思考期间掉线后拦截迟到工具调用，以及离线压缩上下文并跨轮保留摘要。浏览器验收覆盖创建绑定、工具轨迹、结果展开、离线提示和继续聊天。

2026-10-18 已使用页面配置的 `glm-5.3-flash` 和在线 `my-mac3344` 完成一次真实模型修复验收：实际复现 ZeroDivisionError、修改一行、4 条原有断言通过并输出 VERIFIED:4。工具顺序、执行机器和证据边界见 [真实模型验收记录](real-model-acceptance.md)。前述确定性测试与本次真实模型验收分别保留，不以测试 fixture 代替真实模型能力证据。

模型配置 API 测试覆盖未授权拒绝、首次密钥必填、留空保留、替换密钥、响应不泄露密钥、地址校验、保留高级参数、文件权限及重启持久化。浏览器已验证修改模型名并保存后，无需重启即可完成下一轮修复链路。

```sh
go test -race ./...
go vet ./...
brain/.venv/bin/pytest -q brain/tests
brain/.venv/bin/ruff check brain
node --check internal/control/web/app.js
```

本版聚焦短脚本：每条命令最多 12 秒，stdout / stderr 各保留 4 KiB，后台子进程会被清理；每轮最多 10 分钟、默认 20 次主循环模型调用。文本读取最多 1 MiB，分页 200 行 / 8 KiB，直接写入最多 32 KiB。工具结果和模型回复按步骤更新，暂未实现 token 级流式输出。没有文件树、交互终端、附件、分支会话或任务队列。

早期 Go echo → wait Brain 和 `/api/tasks` 继续保留作连接回归实验；新的聊天页使用 `/api/sessions`，新增机器操作都经过固定 Executor。
