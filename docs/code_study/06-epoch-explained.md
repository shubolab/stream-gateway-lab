# epoch：从“纪元”到“本次连线的场次号”

- 整理日期：2026-10-18。
- 源码基线：`a3ae18f`；本轮重新核对 `protocol.go`、`gateway.go`、`client.go` 和 `executor.go`。
- 对应讨论：[讨论 002](discussions/2026-10-18-002-epoch-meaning.md)。

## 这个词本来是什么意思

`epoch` 常译作“纪元、时代”。它既可以表示一个新时期的开端，也可以表示有明显特征的一段时期；在天文等领域，还可以表示选定的时间参考点。词源经过拉丁语追溯到希腊语，与“固定点、停顿”有关。[Merriam-Webster：epoch](https://www.merriam-webster.com/dictionary/epoch)

发音是两个音节，重音在第一个音节，末尾 `ch` 发 `/k/`。英式为 `/ˈiː.pɒk/`；美式可读 `/ˈep.ək/`，也有 `/ˈiː.pɑːk/`。英式可用 `EE-pok` 辅助记忆，美式前一种的第二音节较轻。[Cambridge 发音与音频](https://dictionary.cambridge.org/pronunciation/english/epoch)

与它容易在记忆中混淆的技术词，见[讨论 003：发音与相近词](discussions/2026-10-18-003-pronunciation-and-similar-terms.md)。其中 `epoll` 拼写相近，`eBPF` 更符合内核可观测性与云原生网络两个场景；候选尚待用户确认。

为了理解工程代码，可以把它的常见技术用法分成两类：

| 用法 | 它回答的问题 | 例子 |
| --- | --- | --- |
| 时间参考点 | 从哪个时刻开始计时？ | NTP 的 prime epoch。 |
| 状态的代次 | 这条消息属于哪一代有效状态？ | 密钥代次、领导权代次、配置代次，以及本实验的通信代次。 |

这个分类是便于阅读的概括，不是所有协议对 epoch 的统一定义。看到这个词，仍须确认“具体是哪一种状态在换代”。

## 哪些网络／服务协议实际用了它

这里把标准网络协议与分布式服务协议分别标明，避免把不同层面的字段当成同一种机制。

| 协议／系统 | 实际用词 | 通俗含义与用途 | 官方依据 |
| --- | --- | --- | --- |
| NTPv4：网络校时协议 | `prime epoch` | 时间计算的参考起点；era 0 的基准是 1900-01-01 00:00:00 UTC。属于“从何时计时”的用法。 | [RFC 5905 §6](https://www.rfc-editor.org/rfc/rfc5905.html#section-6) |
| DTLS 1.3：数据报安全协议 | `epoch` | 标识记录所使用的密码状态。包可能丢失或乱序，接收方据此选择对应的密钥材料。 | [RFC 9147 §6.1](https://www.rfc-editor.org/rfc/rfc9147.html#section-6.1) |
| Kafka：消息服务协议 | `current_leader_epoch` 等 | 标记分区领导权相关的代次，用于识别持有过期元数据的请求；请求中的代次更旧时，相关接口可返回 `FENCED_LEADER_EPOCH`。 | [Kafka 4.1 协议说明](https://kafka.apache.org/41/design/protocol/) |
| Redis Cluster：节点间集群协议 | `currentEpoch`、`configEpoch` | 给集群事件与配置加上版本。节点宣称冲突的槽位归属时，配置代次帮助识别应采用的状态。 | [Redis Cluster specification](https://redis.io/docs/latest/operate/oss_and_stack/reference/cluster-spec/#configuration-handling-propagation-and-failovers) |

可以把后三者的共同思路概括为：**状态会更新，消息需要说明自己依据的是哪一代状态。** 这是类比，不表示它们有相同的更新、接收或丢弃规则。

例如 DTLS 的 epoch 用来选择密码状态，并不等同于本系统“只接受当前通道 epoch”的规则；Kafka、Redis 的相关代次可比较大小，本系统的随机字符串只判断是否相同。

## 在本实验里，把它读成“本次连线的场次号”

假设 Brain A 和 Executor A 是固定搭档。它们通过网关连线，可以前后进行很多场：

```text
固定搭档：Brain A ↔ Executor A                     pair 始终是 A

第一场连线 E1：  echo R1 → wait R2 → ……
                                  │
                              Brain 断线
                                  │
                         E1 作废，旧调用失败
                                  │
                         Brain 重新接入并配齐
                                  │
第二场连线 E2：  新调用 R3 → ……

若 E1 的旧结果迟到：它属于上一场，不能进入 E2 的新调用。
```

E1、E2、R1 等只是讲解代号。实际 epoch 由网关调用 `NewID()` 生成随机字符串；它不是 1、2、3 的递增计数，也不是时间戳。

可以用三个问题记住区别：

| 字段 | 问题 | 例子 |
| --- | --- | --- |
| `pair` | 哪一对固定搭档？ | A 组。 |
| `epoch` | 这对搭档的哪一场有效连线？ | E1 场、E2 场。 |
| `id` | 这一场中的哪一次工具调用？ | echo 的 R1、wait 的 R2。 |

`pair=A` 只能说明结果来自这组通信，不能说明它是否还属于当前这一场。epoch 把这种有效期明确地写进消息。

同一 epoch 内又可以有多个先后发生的调用，因此还需要 id。当前代码同时使用通道对象身份与 id 匹配结果，不能因为 id 通常不同，就把整个通道的换代处理省掉。

## 为什么 Executor 的连接没断，epoch 也会换

因为 epoch 属于**双方配成的一轮逻辑通道**，而 `conn` 只是一端到网关的物理连接对象。

```text
第一场：Brain 的连接 B1 ── 网关 ── Executor 的连接 X1     epoch=E1
第二场：Brain 的连接 B2 ── 网关 ── Executor 的连接 X1     epoch=E2
```

第二场中 Executor 仍使用 X1，但它已经在和重新接入的 Brain 通信。新场次开始，旧场次的执行上下文应取消，旧结果不能再按当前场次交付。

取消仍是尽力而为；如果旧工具没有退出，它会继续占用执行槽，新场次上的调用可能收到忙碌。因此“新场次有效”描述通信资格，不保证工具执行资源已经空闲。

## 对照实际代码看一遍

| 步骤 | 代码位置 | 用场次语言理解 |
| --- | --- | --- |
| 双方配齐：`g.Epoch = NewID()` | [`gateway.go`](../../internal/gateway/gateway.go)：`PairRoute.ServeHTTP` | 网关给新一场连线发一个新标识，先通知 Executor，再通知 Brain。 |
| 转发检查：`msg.Epoch == g.Epoch` | 同函数的 data 转发分支 | 消息必须属于当前这一场；同时还须满足当前 peer、对端在线等条件。 |
| 断线：`g.Epoch = ""` | `PairRoute.Remove` | 当前已没有有效场次，通知仍在线的另一端。 |
| 收到不同 epoch 时 `invalidateLocked(...)` | [`client.go`](../../internal/endpoint/client.go)：`readConnection` | 上一场失效，先取消旧 Channel.Ctx，再接受新的非空 epoch。 |
| 以原 channel 发送，按 channel 和 id 交付 | `Client.Send`、[`RemoteExecutor.Receive`](../../internal/endpoint/executor.go) | 旧执行不能借新场次发结果，旧结果不能交给新的等待者。 |

`epoch` 这个字段来自本工程自己定义的 [`Envelope`](../../internal/transport/protocol.go)，不是 WebSocket 库自动生成的字段。是否进入新场次也不是单看 Executor 自己重拨了几次，而是由网关的配对状态决定。

## 本轮建议的中文叫法

正式写文档时用 **“通道代次标识”**；讲流程时用 **“本次连线的场次号”**。

它强调的是：**还是同一对搭档，但只有属于当前这一场的消息和调用才继续有效。**

这是助手提出的解释方式，尚不代表用户已决定重命名源码。相关故障时序可接着看[断线与恢复](04-call-and-recovery.md)。本轮只核对源码和外部资料，没有重新运行 Go 测试；既有测试证据见[核验记录](05-reading-and-evidence.md)。
