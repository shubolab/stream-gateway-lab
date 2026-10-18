# 第二阶段：共享 Executor 池与 Web 调度

本阶段沿用 echo → wait 流程。操作者先在页面选择一个已注册、在线且没有分配任务的 Executor，再由 Gateway 启动独立 Brain 子进程。所有机器属于同一个共享池，没有用户、租户或分组隔离。

## 职责与状态

```text
浏览器 → Gin 管理 API → 预占指定 Executor → 启动独立 Brain 进程
                                               ↓
                          Brain → WS Gateway → Executor → echo / wait
                                               ↑
                          Linux/macOS 安装助手 → 注册身份 → 长期在线等待任务
```

- `internal/control`：Gin 路由、共享管理鉴权、一次性注册码、执行池、进程启动/回收、控制台和安装包下载。
- `internal/gateway`：继续校验外层消息并原样转发。池成员各有独立 `PairRoute`，以 Executor ID 作为稳定寻址标识；任务创建前设置临时 Brain 凭证，结束后撤销。
- `internal/endpoint`：复用工具调用接口和执行槽。新增池客户端允许没有 Brain 时长期空闲；静态实验仍保留原先 30 秒等待期限。
- `internal/app`：命令装配、注册配置读写，以及已有的同一份 Brain 决策流程。
- `internal/transport`：协议、心跳与有限超时写入。

创建任务先在控制层锁内检查/占用成员，再启动进程；同一成员的并发请求只有一个成功。离线或已有任务返回 409，不排队，也不改派其他机器。每项任务有自己的 PID 和临时凭证，Brain 子进程不继承平台管理密钥或其他成员的凭证。

任务结束的顺序为：等待进程退出 → 撤销临时 Brain 凭证并清理旧连接 → 更新终态 → 释放调度名额。取消通过停止 Brain 进程触发原有断线取消链路；不新增工具 cancel 协议。执行端的槽仍直到工具真正退出才释放，取消不代表真实操作可以回滚。

控制台保留运行中任务与最近 100 个已结束任务，单个进程的 stdout、stderr 各最多 64 KiB。这些结果仅用于本次 Gateway 会话的页面展示，不持久化、不重放。注册身份和凭证写入数据目录的 `executors.json`（0600）；重启 Gateway 后成员仍存在，但在线连接和任务历史重新建立。

## 构建与启动

```sh
sh scripts/build-distribution.sh
./dist/stream-gateway-lab gateway -listen 0.0.0.0:8787
```

构建产生当前主机可运行的 Gateway 程序，以及无 CGO 依赖的 Linux/amd64、Darwin/arm64 程序和 `dist/install-executor.sh` 安装助手。Linux 构建也包含 Gateway/Brain 入口，可直接部署到 Linux 主机。默认从 `dist/` 提供下载。也可以显式设置 `-downloads /absolute/distribution/path`。

首次启动在 `data/admin-token` 生成共享管理密钥；在主机上读取该文件后填入页面。也可以通过 `LAB_ADMIN_TOKEN` 环境变量设置。可使用 `-data-dir` 更改数据目录，务必保留该目录，否则已安装的 Executor 身份无法继续认证。默认监听 127.0.0.1；跨主机实验需要可访问的监听地址及网络路由。

页面入口为 Gateway 根地址。安装助手默认采用页面的访问 origin，也可以在弹窗中手动填写对执行端可达的地址。支持 HTTP/WS 和 HTTPS/WSS；部署 HTTPS 时由反向代理转发 `/ws` 的升级请求。当前地址格式为 origin，不支持把控制台挂到 URL 子路径。

Gin 使用 v1.11.0，与本项目现有 Go 1.24 版本兼容。WS 路由的连接接管适配位于 `control/http.go`；工具转发算法不依赖 Gin。

## 安装 Linux/amd64 或 macOS/arm64 机器

1. 页面点击「接入 Executor」，选择平台，填写机器名称和 Gateway 地址。
2. 生成一次性安装命令，在目标机器运行。注册码 15 分钟有效，成功注册后失效；安装命令不包含共享管理密钥。
3. 安装助手自动检测 Linux/x86_64 或 Darwin/arm64，下载 gzip 压缩包、解压程序并检查 SHA-256，注册后把私有配置保存在 `~/.local/share/stream-gateway-executor/executor.json`。
4. macOS 有登录桌面会话时安装用户 LaunchAgent，在用户登录后运行。Linux 有可用 systemd 时安装并启动服务；普通用户采用 user service，root 且存在系统管理器时采用 system service。没有服务管理器时以前台方式运行。
5. 回到页面，机器显示「可用」后，选择它创建任务。

安装要求：POSIX shell、curl、gzip，以及 Linux 的 sha256sum 或 macOS 自带的 shasum，以及可访问 Gateway 的网络。源码和 Go 编译器不需要放到执行端。

页面生成的命令携带 `--platform darwin-arm64` 或 `--platform linux-amd64`，安装助手会在下载前核对当前终端的系统与架构，避免平台选择与执行环境不一致。省略该参数时自动识别。

安装命令可追加 `--foreground` 强制前台启动，或追加 `--no-start` 只安装注册。`--dir /absolute/path` 可选择安装目录。同一路径再次安装会复用注册身份；如需安装同一主机的第二个独立成员，使用另一个目录。重复运行前台进程会被网关拒绝为重复连接。

普通用户服务要在退出登录后继续运行，需要该用户启用 lingering。服务操作：

```sh
systemctl --user status stream-gateway-executor
journalctl --user -u stream-gateway-executor -f
systemctl --user stop stream-gateway-executor
```

macOS 后台服务使用 `com.shubolab.stream-gateway-executor`，安装目录仍为 `~/.local/share/stream-gateway-executor`：

```sh
launchctl print gui/$(id -u)/com.shubolab.stream-gateway-executor
tail -f ~/.local/share/stream-gateway-executor/logs/stderr.log
launchctl bootout gui/$(id -u)/com.shubolab.stream-gateway-executor
```

LaunchAgent 文件位于 `~/Library/LaunchAgents/com.shubolab.stream-gateway-executor.plist`。后台服务在登录会话中运行，注销后停止；前台分支则跟随终端。每个用户只有一个同名后台服务，多实例请另选目录并使用 `--foreground`。

Linux 系统服务去掉 `--user`。卸载时先停止并禁用对应服务，再删除该安装目录和对应 unit 文件；本阶段不提供远程删除机器或自动卸载功能。

池客户端仍保留原有有限拨号策略：首次之外再尝试 3 次，间隔 5 秒。systemd 的 `Restart=on-failure` 与 LaunchAgent 的失败重启可在进程退出后重启执行端服务；它只恢复连接，不恢复已失败任务。前台模式耗尽额度后需要重新启动。

## 管理 API

| 方法与路径 | 作用 | 凭证 |
| --- | --- | --- |
| GET `/api/state` | 成员状态、任务结果、安装包是否就绪 | 共享管理 Bearer token |
| POST `/api/enrollments` | 生成一次性注册码 | 共享管理 Bearer token |
| POST `/api/register` | 注册机器并返回长期 ID/token | 一次性注册 Bearer token |
| POST `/api/tasks` | 指定 executor_id、message、wait_ms 创建任务 | 共享管理 Bearer token |
| POST `/api/tasks/:id/cancel` | 停止对应 Brain 进程 | 共享管理 Bearer token |
| GET `/ws?pair=...&role=...` | WS 工具通道 | 执行端长期凭证或任务临时凭证 |
| GET `/install.sh`、`/downloads/...` | 安装助手与 Linux/macOS 程序 | 公开下载 |

管理 token 只存于当前浏览器标签页的 sessionStorage，通过 Authorization 请求头发送；没有用户账户或多租户权限模型。注册与管理响应均不缓存。状态接口不返回任何端点凭证。

## 与第一阶段的关系

本阶段明确扩充了此前要求中的静态绑定、网关不管理任务、无调用展示历史和有限对端等待四项边界。新的任务管理和内存结果展示属于控制层；WS 消息仍不排队、不缓存重发、不因重连恢复旧调用。工具接口、epoch 校验、迟到结果丢弃和单次调用 15 秒上限保持不变。池模式的 Gateway 与 Executor 使用 5 秒心跳间隔、10 秒 Pong 超时，以容忍公网代理链路；静态实验保留原来的 1 秒间隔、2 秒超时。显式断线仍立即失效，静默失联最多约 15 秒识别。

默认 `gateway` 启动池控制台。第一阶段的静态绑定实验改为显式启动：

```sh
./dist/stream-gateway-lab gateway -config bindings.example.json
```

旧的 Brain/Executor `-config`、`-pair` 入口和本地 Brain 模式保留。测试仍集中在 `test/`。
