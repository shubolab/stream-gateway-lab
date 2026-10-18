# 把抽象术语换成具体问题

## 一张总对照表

“建议心中读法”帮助阅读，不代表已经修改变量名。

| 原词／代码 | 建议心中读法 | 具体含义与位置 | 生命周期／是否上网传输 |
| --- | --- | --- | --- |
| `pair` | 配对名 | A 或 B，指定固定的 Brain ↔ Executor 关系；`binding.Pair`、`client.pair`、`pairRoute.pair`。 | 启动配置确定；放在接入 URL 中，不在每条工具消息里重复。 |
| `role` | 这一端的角色 | `brain` 或 `executor`。与 pair 组合成逻辑端点身份。 | 一次端点运行期间固定；接入 URL 携带。 |
| `token` | 该端的接入凭证 | 每个 `(pair, role)` 各一份；pair 名本身不是密码。 | 经 Authorization 头传给网关。 |
| `peer` | 网关中的接入记录 | `gateway.go` 的结构体，里面持有这一端的 WS；也充当旧、新接入的身份标记。 | 从接入占位到清理；对象仅在网关内。 |
| `conn` / socket | 本端的一条 WS 连接 | `*websocket.Conn`。Brain→Gateway 与 Executor→Gateway 是两条连接。 | 一次连接；Go 指针不跨进程传递。 |
| `epoch` | 本轮配对通道标识 | 网关在双方配齐后用 `newID()` 生成的随机字符串。 | 一轮有效通道；`state`、`data` 外层都携带。 |
| `channel` | 本端对本轮可用通道的句柄 | `client.go` 自定义结构体：`conn + epoch + ctx + stop`。 | 一条本端 WS 上的一个 epoch；对象不跨进程。 |
| `ctx` | 这段工作的取消／期限信号 | Go 的 `context.Context`；含义取决于创建处与父 context。 | 本地对象，不被序列化到协议里。 |
| `stop` / `cancel` | 请求结束这个 context | 取消函数；部分版本可携带具体原因。 | 本地函数；名字相似，作用范围要看其对应的 ctx。 |
| `id` / `pending.id` | 本次工具调用编号 | 协议内实际字段叫 `id`，文档有时称 `request_id`。 | 每次远程 Call 新生成；call/result 的 body 携带。 |
| `pending` | Brain 当前等着的那一次调用 | `RemoteExecutor.pending` 保存 id、原 channel 和结果收件箱。 | Call 登记到返回；不是历史调用表。 |
| `result chan toolMessage` | 当前调用的本地收件箱 | 容量为 1，让读消息的 goroutine 把结果交给等待的 Call。 | 仅当前调用；不是离线结果缓存。 |
| `slot` | 真实工具执行名额 | 执行端的一把 mutex；拿不到就回复忙碌。LocalExecutor 也有自己的 slot。 | 端点对象持有；工具真正返回才释放，跨重连保留。 |
| `current` | 当前仍有效的通道句柄 | `client.current`，可能是 nil。 | 随状态消息和连接失效更新。 |
| `snapshot()` | 取当前有效通道的引用 | 在锁内读取 current 并检查 ctx；返回同一个对象的指针。 | 不是深拷贝，也不保证下一刻仍有效。 |
| `changed` | 状态变化通知铃 | `chan struct{}`；关闭旧通知 channel 唤醒等待者，再创建下一个。 | 本地同步用途，不装工具消息。 |
| `mu` | 保护这份共享状态的锁 | 避免读循环、调用和清理并发改写同一份数据。 | 本地同步对象；不是协议字段。 |
| `envelope` | 给通信层看的外层信封 | `type`、`epoch`、`body`；网关只解释外层。 | JSON 在线传输。 |
| `toolMessage` | 给两端看的工具内容 | `kind`、`id`、工具参数或结果。 | 放在 envelope.body 中；网关不解释工具语义。 |

## 用 A 组的一次重连区分四种身份

下面 E1/E2、R1/R2 都是便于讲解的代号，实际 epoch 和 id 是随机字符串，不是递增数字。

| 时刻 | pair | Brain 的 WS | Executor 的 WS | epoch | 调用 id |
| --- | --- | --- | --- | --- | --- |
| 两端已就绪，调用 echo | A | B-conn-1 | E-conn-1 | E1 | R1 |
| echo 结束，调用 wait | A | B-conn-1 | E-conn-1 | E1 | R2 |
| Brain 断线，wait 尚未结束 | A | 失效 | E-conn-1 仍健康 | 网关清空 | R2 失败结束 |
| Brain 重连，双方重新就绪 | A | B-conn-2 | E-conn-1 继续使用 | E2 | 暂无调用 |
| 操作者显式发起新调用 | A | B-conn-2 | E-conn-1 | E2 | R3 |

由此可以逐个问：

- **pair：我和谁固定配对？** A 不因断线改变，也不会自动改找 B。
- **conn：我现在通过哪条连接到网关？** 一侧换连接，另一侧可能不换。
- **epoch：这条消息属于哪一轮有效通道？** Executor 的 conn 没变，epoch 也必须跟着整组变化。
- **id：这是这一轮里的哪一次调用？** 只检查 epoch 无法区分同一轮中先后两次调用。

`epoch` 在这里不表示时间戳、训练轮数，也没有“大于／小于”的比较。代码只判断空值和相等／不等。生成位置是 `pairRoute.ServeHTTP`；比较位置包括网关转发和 `client.readConnection`。

更口语的读法是“本次连线的场次号”：固定搭档不变，断线后重新配齐就开始新的一场。词义、NTP／DTLS／Kafka／Redis Cluster 的协议对照，以及这个比喻的边界见 [epoch 专题](06-epoch-explained.md)。

## 两个 channel 不是一回事

```go
type channel struct { /* conn, epoch, ctx, stop */ } // 项目自定义通道句柄
result := make(chan toolMessage, 1)                // Go 语言的 channel
```

前者描述“这轮网络通信还是否有效”；后者是 goroutine 间交接数据的语言机制。`ctx.Done()` 返回的也是 Go 的通知 channel，关闭意味着取消或到期。

此外，Brain 与 Executor 的 `*channel` 是各自进程中的两个对象。它们共享的是网关下发的 **epoch 字符串**，不是同一段内存。

## 同样的短变量，在不同函数里含义不同

| 写法 | 在什么位置 | 读成什么 |
| --- | --- | --- |
| `g *gateway` | `gateway.ServeHTTP` | 全部配对的入口。 |
| `g *pairRoute` | `pairRoute.ServeHTTP`、`remove` | 某一组配对的路由；此处的 `g` 已不是全局网关对象。 |
| `e *RemoteExecutor` | `executor.go` | Brain 侧远程调用代理。 |
| `e *executorEndpoint` | `executor_endpoint.go` | 实际执行工具的端点。 |
| `c *client` | `client.go` | 当前这一端的通信客户端，可能属于 Brain，也可能属于 Executor。 |
| `s *channel` | `Call`、`send`、`receive` | 本次操作绑定的原通道句柄，别自动读成“最新通道”。 |
| `p := &peer{}` | 网关接入时 | 这一次接入记录，握手前就先占位。 |
| `p := &pendingCall{...}` | 远程 Call 时 | 这一次等待结果的调用记录。 |

像 `e.pending.channel == s` 这样的比较，检查的是**本进程内是不是原来那一个通道对象**。它不比较网络对端的指针。

## 翻译三句关键判断

```go
if e.pending != nil && e.pending.channel == s && msg.ID == e.pending.id
```

“Brain 确实还在等一个调用；结果来自它当时使用的通道；结果编号也是它要等的那个。”三项都满足才放进收件箱。出处：`RemoteExecutor.receive`。

```go
current := c.current == s && c.conn == s.conn
```

“我要清理的，仍然是当前这轮通道和当前连接吗？”不满足就不关闭连接，避免旧调用的迟到清理伤到新通道。出处：`client.disconnect`。

```go
if !e.slot.TryLock()
```

“真实执行名额是否已被占用？”拿不到立即忙碌，不排队等待。出处：`executorEndpoint.receive`。

## 读旧文档时怎样对照

| 概念性名称 | 当前实现 |
| --- | --- |
| 通道代次、channel generation、旧讨论中的 channel_id | 当前线上字段是 `epoch`。 |
| request_id | 当前工具消息字段是 `id`。 |
| 连接身份、connection_id | 当前靠 `*peer`、`*websocket.Conn` 和 `*channel` 对象身份检查；没有单独线上字段。 |
| revision / 状态版本序号 | 当前没有这个字段；依赖每组锁、状态发送顺序和同一 WS 的有序读取。 |
| BUSY | 当前实际文本为 `executor busy`；远程收到后通常包装成 `tool failed: executor busy`。 |
| cancel 消息 | 当前未实现；等待中的调用超时／取消时，通过关闭原通道 WS 触发对端失联取消。 |

判断依据是当前 [`protocol.go`](../../protocol.go)、[`client.go`](../../client.go)、[`gateway.go`](../../gateway.go) 与 [`executor.go`](../../executor.go)，而非术语在其他系统里的通用用法。
