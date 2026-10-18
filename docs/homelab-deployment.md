# homelab 公网部署

入口：<https://gateway-lab.shubolab.com>。Gateway 与 Brain 在 `ssh homelab` 的 Debian Linux/amd64 主机运行；执行端从外部机器通过 WSS 主动连接。

## 服务与文件

| 项目 | 位置 |
| --- | --- |
| Gateway 服务 | `stream-gateway-lab.service` |
| 独立 Cloudflare 隧道服务 | `cloudflared-gateway-lab.service` |
| 隧道名 / ID | `gateway-lab` / `19f6c587-117d-4bc6-ac49-0c82168d1f7a` |
| Gateway 监听 | `127.0.0.1:8787` |
| 当前发行版 | `/opt/stream-gateway-lab/current`，指向 `releases/` 中的目录 |
| 注册身份与管理密钥 | `/var/lib/stream-gateway-lab/` |
| Responses API 模型配置 | `/var/lib/stream-gateway-lab/model.json` |
| Python Brain / 虚拟环境 | `/opt/stream-gateway-lab/current/brain/` |
| 会话与上下文 | 数据目录内 `sessions.json`、`turns/`、`histories/` |
| 隧道配置与凭证 | `/etc/cloudflared/gateway-lab/` |
| systemd 模板 | `scripts/deploy/` |

Gateway 与 cloudflared 使用各自的系统用户，两个服务均设置开机启动。Cloudflare 只将该域名转发到 Gateway，默认规则返回 404。原有 `cloudflared-homelab-ssh.service` 和 `aa-gateway.service` 不参与本次部署。

域名由独立隧道的 CNAME 路由承载；配置方式参考 [Cloudflare 官方本地管理隧道文档](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/create-local-tunnel/)。HTTPS 在 Cloudflare 边缘终止，经加密隧道转发至本机 HTTP 监听，WebSocket 升级走同一域名的 `/ws`。

## 登录与安装本机 Executor

在 Mac 终端复制共享管理密钥到剪贴板：

```sh
ssh homelab 'sudo cat /var/lib/stream-gateway-lab/admin-token' | pbcopy
```

打开控制台粘贴登录。点击「接入 Executor」，选择 macOS / arm64，填写机器名称，再复制一次性安装命令到本机终端运行。安装助手不依赖 Go，使用系统 curl 与 shasum；默认创建登录用户的 LaunchAgent。只想进行终端实验时，在生成的命令末尾追加 `--foreground`。

本机构建文件：`dist/stream-gateway-executor-darwin-arm64`、`dist/install-executor.sh`。在线安装入口：`/install.sh`；两个平台的二进制文件、gzip 压缩包和 SHA-256 均通过 `/downloads/` 提供。

## 运维

```sh
ssh homelab 'sudo systemctl status stream-gateway-lab cloudflared-gateway-lab --no-pager'
ssh homelab 'sudo journalctl -u stream-gateway-lab -n 80 --no-pager'
ssh homelab 'sudo journalctl -u cloudflared-gateway-lab -n 80 --no-pager'
ssh homelab 'sudo systemctl restart stream-gateway-lab'
```

第三阶段增加 Python + DeepAgents Brain。在页面侧栏「模型设置」填写 API 地址（通常以 `/v1` 结尾）、模型名称和密钥并保存，下一轮自动生效。已保存的密钥不会回显，留空保留原值。服务端配置为 `/var/lib/stream-gateway-lab/model.json`，不需要 SSH 编辑。高级参数仍可在该文件中维护。使用 Responses API，具体配置与验收见 [第三阶段说明](agent-chat.md)。现有 Executor 也需要更新下载产物才能使用机器工具；在原安装路径重新运行安装助手会保留身份，不必重新注册。

升级时构建新的 Linux/amd64 程序与双平台下载产物，将 `brain/` 源码、`pyproject.toml` 和 `uv.lock` 放入新发行目录，用 uv 在该目录安装锁定的非开发依赖。不要复制本机 `.venv`。配置归系统用户所有，并保持模型配置在独立的数据目录，升级不覆盖它。更新 systemd 模板、切换 `current` 后重启 Gateway。Cloudflare 隧道保持原配置。

回滚时将 `current` 指回旧目录并恢复对应的 service 文件后重启。Gateway 重启会终止运行中轮次，新会话的已完成记录和模型上下文保留，中断操作不自动重放；旧 echo 实验任务仍只保留内存历史。注册身份一直保留。

## 2026-10-18 发布记录

模型设置发布目录为 `releases/agent-chat-20261018-02`，包含聊天工作台和页面模型设置。Python 3.13 环境在 homelab 上根据锁定依赖独立安装；机器工具未变化，Executor 下载产物与 `agent-chat-20261018-01` 相同。旧发行目录和 systemd 模板保留用于回滚。

Go race 测试和静态检查通过；6 项真实进程集成测试在 macOS 和 homelab Linux 上通过。页面模型配置的校验、留空保留密钥、保存后下一轮生效均已验证。公网页面、模型配置管理接口和双平台下载可用，原有 2 个 Executor 注册身份保留。该版本发布时尚未填写真实模型凭证。

验收细节见 [第三阶段说明](agent-chat.md) 和 [第二阶段验收记录](pool-acceptance-results.md)。

后续发布 `releases/agent-chat-20261018-03` 增加 Markdown 渲染，保留既有模型配置、Executor 和会话。真实模型 `glm-5.3-flash` 已在在线 Mac Executor 上完成脚本修复闭环，证据见 [真实模型验收记录](real-model-acceptance.md)。

## 公网 TLS / QUIC 连接排查记录

2026-10-18 深夜，本机访问域名出现 TLS 超时 / EOF 和浏览器 `ERR_QUIC_PROTOCOL_ERROR`。homelab 上两个服务均处于 active 状态，Cloudflare 隧道有 4 条连接，源站资源正常；其他出口的 TLS 探测也成功。进一步检查验收 Mac 的 Clash `/connections`，失败流量命中了 `DOMAIN-KEYWORD,shubolab.com,DIRECT`。这是该客户端的直连路径问题，不能仅凭浏览器错误码判定 Gateway、证书或 cloudflared 的 QUIC 隧道故障。

修复是在当前 Clash 订阅的持久规则覆写中加入精确域名例外，放在其他 `shubolab.com` 规则之前：

```yaml
prepend:
  - DOMAIN,gateway-lab.shubolab.com,Others
  # 后面保留现有规则
```

本机覆写文件位于 Clash Verge 配置目录的 `profiles/rwCsZkz397ZX.yaml`，修改前已备份。通过「重新激活订阅」应用后，用 [Mihomo API](https://wiki.metacubex.one/api/) 的 `/rules` 和 `/connections` 确认新规则生效，TCP、UDP 流量均进入现有 `Others` 出口。本机后台服务使用独立运行目录；直接通过 API 请求重载桌面目录下的配置会被路径校验拒绝，应由 Clash Verge 生成并应用配置。

修复后普通 HTTPS、校验证书的 TLS 1.3 探测和公网页面均成功，完整静态资源与发布内容一致，真实模型会话及 Markdown 显示已在公网浏览器复核。这次没有调整服务器、Cloudflare 或应用代码，线上仍为 `agent-chat-20261018-03`。上述客户端规则依赖本机已有的 `Others` 代理组；其他机器排查时应先确认自己的实际连接路径。
