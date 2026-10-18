# 真实模型与远端 Executor 验收

2026-10-18（Asia/Shanghai），在公网 Gateway 上使用用户通过页面保存的模型配置完成验收。此次调用真实供应商服务，没有使用 Responses fixture。

## 环境与范围

| 项目 | 值 |
| --- | --- |
| Gateway | `https://gateway-lab.shubolab.com`，homelab Linux/amd64 |
| 模型 | `glm-5.3-flash`，通过 Responses API 集成 |
| Executor | `my-mac3344`，macOS/arm64 |
| 执行主机 | `wangshubodeMacBook-Pro.local` |
| 会话 ID | `CLOLAPAYBOYE43J43QCXSQQVSV` |
| 修复轮 ID | `SBDWL5WWNON4ZFHGMO55MTBP2D` |
| 修复轮时间 | 23:40:32–23:41:31（Asia/Shanghai） |
| 实验文件 | `/tmp/gateway-real-model-20261018-e935aed2/broken.py` |

先让模型通过绑定的 Executor 创建独立临时目录并原样写入故障脚本，本轮不运行、不修复；再发送修复要求。整个过程的机器操作均来自模型工具调用，未通过本地文件工具或 SSH 代替 Executor 修改测试脚本。

## 实际工具轨迹

修复轮成功结束，未降级到仅聊天，按顺序产生 5 次工具调用：

| 顺序 | 工具 | 观察结果 |
| --- | --- | --- |
| 1 | `read_file` | 读取 8 行原始脚本，分母为 `len(values) - 3` |
| 2 | `execute` | Python 实际抛出 `ZeroDivisionError: division by zero`，脚本退出码 1 |
| 3 | `edit_file` | 仅替换 1 处，将分母改为 `len(values)` |
| 4 | `execute` | 输出 `VERIFIED:4`，脚本退出码 0，stderr 为空 |
| 5 | `read_file` | 回读完整文件，确认 4 条断言和成功输出语句保持原样 |

两次执行的工具结果均返回上述 Executor 主机名。模型使用了 `python3 <path>; echo "EXIT_CODE=$?"`，因此复现错误时整个 shell 的 `exit_code` 也是 0；Python 的真实失败由 stderr 堆栈和 stdout 的 `EXIT_CODE=1` 共同确认。不能把最后一个 `echo` 的退出码当成脚本成功。

唯一修改：

```diff
 def average(values):
-    return sum(values) / (len(values) - 3)
+    return sum(values) / len(values)
```

以下断言全部保留并通过，覆盖多个输入和单元素列表，未硬编码测试结果：

```python
assert average([2, 4, 6]) == 4
assert average([3, 6, 9]) == 6
assert average([-3, 0, 3]) == 0
assert average([5]) == 5
print("VERIFIED:4")
```

会话和工具记录保留在 Gateway 页面中；实验文件保留在上述临时目录，便于复核。该结果证明当前配置下的一次真实读文件、执行、依据报错修改、再次验证闭环，不代表所有模型或复杂长任务都已经验收。

## Markdown 显示验收

聊天回复改为完整 Markdown / GFM 渲染，覆盖标题、段落、粗体、强调、列表、引用、行内代码、代码块、表格、任务列表和链接。工具参数与原始输出继续作为代码文本显示。

本地浏览器检查确认上述结构正常生成，代码缩进保留，表格有横向滚动容器，任务复选框不可修改；脚本、图片事件和 JavaScript 链接不会进入可执行 DOM。真实模型返回的验收报告也在同版本本地页面中完成排版检查。

## 发布复核

Markdown 修复已发布到 homelab 的 `agent-chat-20261018-03`。Gateway、Cloudflare 隧道服务正常，服务器本机返回的静态资源与构建内容一致，模型配置和成功的验收会话在重启后保留。

首次发布后的公网复核曾遇到 TLS 连接超时 / EOF 和浏览器 `ERR_QUIC_PROTOCOL_ERROR`，当时只完成本地页面和部分公网资源验证。该连接问题随后完成排查和修复，详见下节。

## 公网页面补充验收（2026-10-18 深夜至 10-19 零点）

问题位于验收 Mac 的 Clash 路由：`DOMAIN-KEYWORD,shubolab.com,DIRECT` 抢先匹配 Gateway 域名，失败连接实际走 `DIRECT`。在当前订阅的持久规则中添加更高优先级的 `DOMAIN,gateway-lab.shubolab.com,Others`，通过 Clash Verge「重新激活订阅」生成并应用运行配置。其他域名的直连规则保留。Gateway、Cloudflare 配置及 TLS 校验均未调整。

修复后的实测结果：

- 普通 HTTPS 请求返回 200，证书校验成功；独立 Python SSL 探测在 2026-10-19 00:00:06 使用 TLS 1.3 成功取得 `HTTP/1.1 200 OK`。
- Clash 运行时首条规则匹配上述精确域名，浏览器 TCP、UDP 连接均走 `Others → VLESS → VLESS-REALITY-443`。
- 首页、`app.js`、`style.css`、`vendor/marked.umd.js`、`vendor/purify.min.js` 从公网完整下载，与仓库对应文件逐字节一致。
- 在 `https://gateway-lab.shubolab.com/` 实际登录工作台并打开本记录中的真实模型会话，页面可读、可展开工具结果，未再出现 TLS / QUIC 错误页。
- 展开的修复后 `execute` 结果显示执行机器 `wangshubodeMacBook-Pro.local`、`VERIFIED:4`、`EXIT_CODE=0`，stderr 为空。
- 最终报告在公网 DOM 中生成 9 个标题、4 个有序步骤、5 个代码块、26 处段落或列表中的行内代码及 6 处粗体；浏览器截图确认标题、列表、代码缩进和代码块背景正常显示。

这次补充验收复核的是前文已完成的真实模型轮次，没有重新执行机器任务。复核时 2 台已注册 Executor 均离线，页面正确显示「仅聊天」并禁用机器工具；之前的执行记录仍可查看。截图保存在本地 `dist/real-model-markdown-public.png` 与 `dist/real-model-tool-result-public.png`。
