# 讨论 003：epoch 的发音，以及可能记混的技术词

- 日期：2026-10-18（Asia/Shanghai）。
- 延续：[讨论 002](2026-10-18-002-epoch-meaning.md)。
- 状态：发音与候选词的技术含义已核对来源；用户记忆中的具体词尚未确认。

## 用户问题与线索

用户询问 epoch 的音标，并回忆在“可观测系统的 OS 层日志收集手段”和“云原生网关的网络协议”中见过拼写相似的词，希望识别可能的名称。

本轮需要分别处理两件事：epoch 如何读是可以核对的词典事实；用户过去看到什么，只能根据线索提出候选。

## 发音事实

| 读法 | 音标 | 辅助理解 |
| --- | --- | --- |
| 英式 | `/ˈiː.pɒk/` | `EE-pok`，首音节是长音。 |
| 美式的一种 | `/ˈep.ək/` | 首音节重读，第二音节含弱元音 `/ə/`。 |
| 美式的另一种 | `/ˈiː.pɑːk/` | 首音节同样是长音。 |

这些读法均为两个音节，重音在第一个音节；`ch` 读 `/k/`。来源：[Cambridge 发音页与音频](https://dictionary.cambridge.org/pronunciation/english/epoch)、[Oxford Learner's Dictionaries](https://www.oxfordlearnersdictionaries.com/definition/english/epoch)。Cambridge 正文直读受限，本轮通过其搜索索引中的音标和 Oxford 词典结果交叉核对。

## 候选词：按什么线索判断

| 候选 | 为什么可能是它 | 实际是什么 | 识别时可回想的同页关键词 |
| --- | --- | --- | --- |
| **eBPF** | 与“OS 层观测”和“云原生网络”两个场景都吻合，是按场景优先考虑的候选。 | 可在内核等特权上下文运行受验证程序的技术，可挂接系统调用、函数或网络事件等观测点；也用于网络处理。 | `kprobe`、`uprobe`、`bpftrace`、`Cilium`。 |
| **epoll** | 与 epoch 的拼写最接近，都以 `epo` 开头；也出现在 Linux 网络 I/O 实现中。 | Linux I/O 事件通知机制，监视多个文件描述符是否就绪，不负责解释业务日志或网络协议。 | `select`、`poll`、`epoll_wait`、边沿触发、水平触发。 |
| **Envoy** | 符合“云原生网关”场景，但拼写相似度较低。 | 网络代理软件，可用于边缘代理、服务通信等场景；是组件名。 | 代理、路由、控制面、`xDS`。 |
| **xDS / EDS** | 如果用户明确记得是网关的配置发现协议，可作为补充候选；拼写与 epoch 相似度较低。 | Envoy 的动态资源发现服务与 API；EDS 为 Endpoint Discovery Service，负责端点相关资源。 | `LDS`、`RDS`、`CDS`、`gRPC`、动态配置。 |

技术事实来源：[eBPF 官方介绍](https://ebpf.io/what-is-ebpf/)、[Linux epoll 手册](https://man7.org/linux/man-pages/man7/epoll.7.html)、[Envoy 官方介绍](https://www.envoyproxy.io/docs/envoy/latest/intro/what_is_envoy)、[Envoy xDS 协议](https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol)。候选排序是助手根据用户线索的推测，不是来源证明用户读过这些内容。

## 为什么两个场景可能出现同一个词

eBPF 官方介绍同时列出了可观测性和网络处理用途；它能挂接系统调用、内核跟踪点与应用探针。因此，如果“OS 层日志收集”指的是观察系统／应用事件，eBPF 很符合这个描述。它不是专指读取日志文件的机制。[官方介绍](https://ebpf.io/what-is-ebpf/)

Cilium 的 Gateway API 文档还会同时出现 eBPF 与 Envoy：流量处理涉及 eBPF，网关流量经过 Envoy 代理。因此用户也可能在同一套架构的不同层次看到这两个名称。[Cilium Gateway API 文档](https://docs.cilium.io/en/stable/network/servicemesh/gateway-api/gateway-api/)

这只能解释为何候选与线索吻合，不能据此确定用户当时看到的是哪一个。它们也不是 epoch 的不同拼法：epoch 描述代次／时间参考，eBPF 是技术机制，epoll 是系统接口，Envoy 是代理组件，xDS 是配置发现 API／协议体系。

## 后续确认方式与本轮变更

若用户能想起全小写 `epo…`，优先核对 epoll；若记得大小写混写、内核探针或 Cilium，优先核对 eBPF；若记得代理与配置下发，核对 Envoy 与 xDS。

本轮将发音补入 epoch 专题，并记录上述候选。未修改实验实现，也不把本次资料检索当作该工程使用这些技术的证据。
