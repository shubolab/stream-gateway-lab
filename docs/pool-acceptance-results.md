# 第二阶段验收记录

日期：2026-10-18，Asia/Shanghai。源码包含共享 Executor 池、Gin 管理 API、Web 调度、Linux/amd64 与 macOS/arm64 安装助手。

## 自动检查

环境：macOS/arm64，Go 1.24.3，Gin 1.11.0。

- `go test -race -count=1 ./...` 通过，最初测试包耗时 109.714 秒；加入公网心跳回归后最终复跑耗时 118.179 秒，无 race 报告。
- `go vet ./...`、`git diff --check`、前端 JavaScript 语法检查通过。
- ShellCheck 对安装助手和构建脚本的检查通过。
- 原有静态配对、断线、静默失联、超时、旧 epoch/迟到结果、无法取消工具和标准输入退出测试继续通过。
- 新增 `TestControlPool` 使用 Gin 服务、真实 WS、实际编译并启动的 Brain 子进程验证：管理鉴权、注册码一次使用、身份凭证不泄露、非法输入/离线拒绝、同一成员并发抢占只有一个成功、两个任务到达指定执行端、独立 PID、取消传播及释放、31 秒空闲后新任务成功、注册身份持久化、重启后没有旧任务复活，以及进程启动失败释放预占。

## Linux 安装与跨进程闭环

在两台独立的 Alpine 3.22 Linux/amd64 容器中运行 Gateway 提供的安装助手，容器通过 `host.docker.internal` 连接 Mac 上的 Gateway。Docker 宿主为 arm64，amd64 使用模拟执行；此证据不等同于独立公网物理主机验收。

| 检查 | 结果 |
| --- | --- |
| 下载、SHA-256 校验、注册、前台上线 | 两个容器均通过，登记为 linux/amd64 |
| 安装产物 | ELF x86-64，statically linked，无执行端 Go 编译器依赖 |
| 私有配置 | 两份 executor.json 权限均为 0600 |
| 重复安装 | 复用现有身份，池成员数保持 2 |
| 两台机器同时执行任务 | `linux-install-check-linux-amd64-a` / `-b` 分别返回指定消息及 300/600 ms 等待结果；Brain PID 为 39367/39368 |
| A 超时、B 正常 | A 的 18 秒 wait 触发 15 秒工具调用超时并记录失败；B 的 100 ms wait 正常完成 |
| 显式新任务 | A 超时后创建 `A-after-timeout` 成功，没有重跑旧任务 |
| Gateway 重启 | 身份保留，两台 Executor 自动重新连接，任务历史为空；新的两个任务均成功，Brain PID 为 39848/39849 |

## 浏览器操作

在 Codex 内置浏览器中实际完成：共享密钥登录、安装助手弹窗、覆盖 Gateway 地址并生成一次性命令、选择 Linux 执行端、创建任务、查看 echo/wait 结果，以及点击「停止」。

`web-to-linux-A` 由 Brain PID 39348 调度到 Linux A 并成功；`cancel-from-web` 的 PID 39672 被停止，页面显示「已取消」，Linux B 日志确认工具收到 context canceled 并丢弃旧结果。Gateway 重启后，页面重新读取机器和新任务状态；结果弹窗展示 `hello from linux-amd64-a` 与 `waited_ms: 200`。

页面检查中修复了不可用成员退出选项列表后选择框空白，以及状态不变时重复创建列表控件的问题。安装助手的临时下载目录在启动服务/前台进程前清理。

## 公网 homelab → macOS 验收

Gateway 已部署到 Debian Linux/amd64 的 homelab，入口为 <https://gateway-lab.shubolab.com>，独立 Cloudflare Tunnel 名为 `gateway-lab`。`stream-gateway-lab.service` 和 `cloudflared-gateway-lab.service` 均为 active、enabled；原有 SSH 隧道和 aa-gateway 保持 active。

最终发行目录为 `/opt/stream-gateway-lab/releases/pool-20261018-05`。两端均使用 Go 1.24.3，源码传输排除 macOS 归档元数据，最终本地构建和远端构建二进制 SHA-256 一致：

- Linux/amd64：`088c5328cbba9dbf20a0d3e57d7e227c92e3a222709add7e10e5693a67571558`
- Darwin/arm64：`276cd6e071e878bb7074dacb5640e8542a7b8f7144043cfe4e5ac285fd945187`

在本机 Mac（`Darwin/arm64`、`wangshubodeMacBook-Pro.local`）运行公网提供的安装助手，完成 gzip 下载、解压、SHA-256 校验、注册及 WSS 连接。注册 ID 为 `GHESGNTKHHJZW6DINDEVO36WTI`，名称为 `macOS-public-check`，临时 Executor PID 为 42949。私有配置权限为 0600，重复安装复用同一身份。

| 公网任务 | homelab Brain PID | 结果 |
| --- | --- | --- |
| `homelab-to-macOS-public` | 4139303 | echo 消息匹配，waited_ms = 500 |
| `public-cancel-check` | 4139313 | 已取消；远端 ps 确认该 Brain 的 PPID 为 Gateway PID 4139268 |
| `public-after-cancel` | 4139333 | echo 消息匹配，waited_ms = 200 |

公网匿名管理 API 返回 401；浏览器成功登录并显示 darwin/arm64 机器及真实任务结果。临时 Executor 验收后已停止，没有替用户安装长期后台进程。

实际公网测试推动两项调整：

- 压缩下载：macOS 包约 4.8 MB，Linux 包约 5.1 MB；下载允许最长 600 秒，解压后验证原始二进制摘要。
- 池心跳：原实验的 2 秒 Pong 超时在公网触发断线，池 Gateway/Executor 改为 5 秒间隔、10 秒超时。静态实验仍使用原值，工具调用仍限时 15 秒。新增真实 WS 的 3 秒延迟 Pong 测试。

页面选择平台现在会传入 `--platform`，脚本先输出实际系统/架构/主机名并校验匹配。本机执行线上助手确认识别 Darwin/arm64，故意指定 Linux 平台则在下载前拒绝；homelab 上反向指定 Darwin 平台同样在下载前拒绝。用户报告的本机显示 Linux 输出未复现，不能据此断定其原始终端环境。

## 尚未覆盖

- Gateway 与隧道已实际通过 systemd 托管并设置 enabled，但没有重启整台 homelab 验证开机过程。
- Executor 安装助手的 systemd 与 macOS LaunchAgent 分支已完成 ShellCheck / plist 校验；公网验收使用临时前台进程，不能据此宣称后台登录启动已验证。
- 未复验历史文档记录的 Go 1.26.4 SIGINT 行为，当前测试结果仅对应上述 Go 1.24.3 环境。

运行与安装说明见[第二阶段设计](pool-control-plane.md)。测试创建的临时 Gateway 和两台容器在验证结束后清理；构建产物保留于忽略跟踪的 dist/ 目录。
