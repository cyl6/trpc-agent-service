# 赛题需求验收矩阵

本矩阵逐项对应 `赛题.md`。它同时回答两个不同问题：**当前最小实现能验证什么**，以及**生产目标如何设计**。状态定义：

- **已实现**：当前 `solution/` 有可运行路径，核心行为可由自动测试或接口验证。
- **部分实现**：已有主要接口/单机路径，但持久化、多节点、平台能力或生产闭环不完整。
- **生产设计**：本文档已经给出可实施方案，但当前代码没有对应生产 Adapter/基础设施；不得按已实现宣传。
- **不适用代码**：题目要求的是设计/取舍说明，文档本身即交付物。

文档入口：

- [architecture.md](architecture.md)：租户模型、拓扑、路由、隔离、部署与配置版本。
- [data-consistency.md](data-consistency.md)：统一数据抽象、并发顺序、幂等、迁移和表结构。
- [im-adapters.md](im-adapters.md)：Telegram/Slack 映射、验签、身份、平台限制和回复能力。
- [security-operations.md](security-operations.md)：Filter、指标、trace、审计、密钥、故障、灰度和容量。

## 1. 多租户与节点部署

| ID | 原始要求 | 当前状态与代码证据 | 验收方式 | 生产设计证据 |
| --- | --- | --- | --- | --- |
| A-01 | 租户模型至少含 tenant_id、应用、模型、工具权限、IM、数据后端、审计 | **已实现**：[config.go](../trpcservice/config/config.go) 的 `TenantConfig` 全部覆盖，另有 budget/version/enabled；未知 YAML 字段拒绝 | `TestDecodeDefaultsAndSecretReferences`、`TestDecodeRejectsUnknownField`；加载 [example.yaml](../config/example.yaml) | [架构 §2](architecture.md#2-租户模型) |
| A-02 | 组件部署拓扑及 Gateway/Worker/Channel/Storage/Admin/Telemetry 协作 | **部分实现**：这些模块已拆包但装配在单二进制 [main.go](../cmd/trpc-service/main.go) | 启动后检查 webhook、Admin、metrics；代码走查依赖装配 | [架构 §3](architecture.md#3-组件与部署拓扑) 给出 Mermaid 最小/生产拓扑 |
| A-03 | 多节点水平扩展，消息路由到正确 tenant/session | **部分实现**：binding 反推 tenant；session 派生稳定；Redis Coordinator/Session/Memory 可共享，但队列和配置历史仍本地 | `TestWebhookDerivesTenantFromVerifiedBinding`、`TestIdentityIsolationAndConversationRules`、`TestRedisSessionIsVisibleAcrossIndependentWorkerRuntimes` | [架构 §5](architecture.md#5-租户与-session-路由)，[数据 §4](data-consistency.md#4-同一-session-的并发一致性) |
| A-04 | 说明 sticky session | **部分实现**：单机 InMemory 不可横扩；Redis coordination/Session/Memory 模式不依赖 HTTP sticky，但尚非完整无状态生产栈 | `TestRedisSessionIsVisibleAcrossIndependentWorkerRuntimes` 用两个独立 Runtime 验证共享 Session/Memory | [架构 §5.3](architecture.md#53-是否需要-sticky-session) 明确生产不需要及例外 |
| A-05 | 配置、数据、工具、日志脱敏、密钥的租户隔离 | **部分实现**：binding 唯一、租户 hash namespace、Runtime 隔离、执行点工具策略、审计脱敏、secret env 引用均已有；SQL RLS/向量/IAM 尚无实现 | binding collision、identity、tool policy、audit redaction 测试 | [架构 §7](architecture.md#7-五层租户隔离)，[安全 §1–3](security-operations.md) |

## 2. 数据同步与多后端

| ID | 原始要求 | 当前状态与代码证据 | 验收方式 | 生产设计证据 |
| --- | --- | --- | --- | --- |
| D-01 | 不同租户选 InMemory、Redis、SQL、向量、对象或外部 Memory | **部分实现**：配置类型可表达；Session 与 Memory 实际支持 InMemory/Redis，Artifact 仅 InMemory，Knowledge 未装配；SQL/vector/object/external 是扩展契约 | `TestRedisSessionIsVisibleAcrossIndependentWorkerRuntimes`；用 acme/globex 配置验证不同构造；不得用配置字段冒充未实现 Adapter | [数据 §1、§7](data-consistency.md#1-当前能力真值表) |
| D-02 | 统一访问抽象，覆盖 Session/Memory/Summary/Artifact/Knowledge/Audit | **部分实现**：Runner 使用框架 Session/Memory/Artifact SPI，Audit Sink 独立；Summary 随 Session，`Data.Summary` 尚未接 Runtime；缺少完整生产 `BackendBundle` 和 capability 协商 | Runtime 构造测试/代码走查 | [数据 §2](data-consistency.md#2-统一访问抽象) 明确定义目标 Bundle 和 Scope |
| D-03a | 多节点并发写同一 session 的一致性 | **部分实现**：Coordinator 锁；单机并发测试通过；Redis 续约失败会取消 run，但后端无 fencing/CAS，不能证明取消竞态中无陈旧写 | `TestConcurrentMessagesAreSerializedPerSession`；生产需故障注入陈旧持锁者 | [数据 §4](data-consistency.md#4-同一-session-的并发一致性) 的 session 分区 + CAS/fencing |
| D-03b | Session event、state、summary 更新顺序 | **部分实现**：锁内由框架 Session 执行；未实现平台级 event/state/outbox 原子提交，未装配 summarizer | 连续 turn 测试只能验顺序，不能证明跨库事务 | [数据 §5](data-consistency.md#5-eventstate-summary-与-memory-的更新顺序) 给出原子提交/异步视图顺序图 |
| D-03c | Memory 写后跨节点可见 | **部分实现**：Redis Memory Adapter 按租户 namespace 共享数据；InMemory 模式仍只在当前进程可见；尚无平台级 visibility watermark/SLA | `TestRedisSessionIsVisibleAcrossIndependentWorkerRuntimes` 已用两个独立 Runtime 验证 Redis Memory 写后读；多副本配置不得保留 InMemory | [数据 §5.2](data-consistency.md#52-memory-跨节点可见性) 定义当前保证与 committed/eventual 水位强化 |
| D-03d | Redis→SQL、本地向量→远端向量迁移 | **生产设计**：当前没有迁移执行器 | 设计评审；实现后以可重入、shadow mismatch、回滚演练验收 | [数据 §8–9](data-consistency.md#8-redis--sql-迁移) 的 PREPARE→FINALIZE 状态机、双写/对账/tombstone |
| D-03e | IM 重复投递幂等 | **部分实现**：消息 hash claim；成功 completed；投递失败保存 pending result 后只重发不重跑；非事务且 TTL 后可再执行 | `TestProcessDedupSessionContinuityAndTenantIsolation`、`TestPendingOutboxReplaysDeliveryWithoutRerunningAgent` | [数据 §6](data-consistency.md#6-im-重投与端到端幂等) 的持久 Inbox/Outbox |
| D-04 | 说明各后端强/最终一致、延迟、成本和运维取舍 | **不适用代码** | 架构评审逐项核对表格 | [数据 §7](data-consistency.md#7-后端一致性取舍) |
| D-05 | 最小模型含 tenant/app/session/event/memory/summary/binding/audit | **不适用代码**；当前运行时数据沿用框架结构 | DDL 评审、迁移工具 dry-run | [数据 §10](data-consistency.md#10-最小关系数据模型) 含全部必需表及 Inbox/Outbox/Artifact |

## 3. IM 软件接入

| ID | 原始要求 | 当前状态与代码证据 | 验收方式 | 生产设计证据 |
| --- | --- | --- | --- | --- |
| I-01 | 至少支持两类 IM（含微信/企业微信） | **已实现**：[telegram.go](../trpcservice/channels/telegram.go)、[slack.go](../trpcservice/channels/slack.go)、[wecom.go](../trpcservice/channels/wecom.go) 注册到统一 Adapter Registry；企业微信覆盖 AES 加密回调验签解密、URL 验证、corpid/agentid 绑定、单聊/群聊分流与字节级拆分 | `TestTelegramVerifyAndParse`、`TestSlackVerifyParseAndRejectReplay`、`TestWeComVerifyParseDirectMessage`、`TestWeComParseGroupChatAndBindingChecks`、`TestWeComURLVerification` | [IM §1–2](im-adapters.md#1-adapter-契约与能力模型)（含企业微信章节） |
| I-02 | 外部消息→tRPC-Agent 输入；Agent Event→回复/流/卡片 | **部分实现**：规范消息→Runner；Event stream 聚合成最终文本；附件仅安全元数据提示；无增量编辑/卡片/附件出站 | Adapter 测试、`TestAttachmentMetadataIsPassedWithoutProviderCredentialedURL`、mock direct chat | [IM §3–4](im-adapters.md#3-入站规范化) 给出 ReplyOp/流式/card 降级 |
| I-03 | 账号租户绑定、URL/token/secret/验签/去重/身份映射 | **部分实现**：绑定、env secret、Telegram header、Slack HMAC+时间窗、企业微信 AES 验签解密+URL 验证、消息 claim、身份 hash 已有；去重不 durable | Gateway 签名先于 dispatch 测试、binding collision 测试、重复消息测试、`TestWeComURLVerification` | [IM §2、§5–6](im-adapters.md#2-webhook账号与租户绑定) |
| I-04 | 群聊/单聊 session_id 与跨群/跨租户隔离 | **已实现**：单聊按用户，群聊按 conversation/thread 共享 principal；tenant/app/binding/channel/scope 都入 hash；企业微信 `ChatId` 群聊与 userid 单聊同规则 | `TestIdentityIsolationAndConversationRules`、`TestGroupMembersShareOneRunnerSession`、`TestWeComParseGroupChatAndBindingChecks` | [IM §5](im-adapters.md#5-单聊群聊与身份隔离) |
| I-05 | 长度、频率、异步、图片/文件、撤回、失败重试 | **部分实现**：rune 拆分（Telegram/Slack）、字节拆分（企业微信 2048B）、租户入站 RPM、callback 后异步、本进程 3 次重试、附件元数据、企业微信 access_token 缓存与过期重试；无出站限频/Retry-After/内容下载/撤回 | `TestChunksUsesRuneLength`、`TestChunksUTF8BytesSplitsOnRuneBoundary`、`TestWeComDeliverCachesTokenSplitsBytesAndRoutesByScope`、`TestWeComDeliverRetriesOnceOnExpiredTokenWithoutLeakingSecret`、flaky Adapter 测试；其余做负向能力检查 | [IM §6–7](im-adapters.md#7-平台限制与降级矩阵) 完整能力/降级矩阵 |

## 4. 治理、监控和安全

| ID | 原始要求 | 当前状态与代码证据 | 验收方式 | 生产设计证据 |
| --- | --- | --- | --- | --- |
| G-01a | Filter：工具白名单 | **已实现**：模型可见性 Filter + 执行点 PermissionPolicy，deny 优先、未知默认拒绝 | governance 单测并增加 allow/deny 表驱动用例 | [安全 §2](security-operations.md#2-filter-治理链) |
| G-01b | Filter：敏感信息脱敏 | **部分实现**：审计 Reason/Error 和 secret 值脱敏，内容只写 hash；未实现入模 tokenization/出站 DLP | `TestAuditRedactsSecretsAndPII`；当前应负向验证 prompt/output 不承诺自动 PII mask | [安全 §2.2、§3](security-operations.md#22-生产-filter-顺序) |
| G-01c | Filter：预算限制 | **部分实现**：本进程 RPM、字符上限和月成本估算预留；多节点不原子、没有 actual usage settle | `TestInboundFilterEnforcesMonthlyCostReservation`；补充超 RPM/字符用例；重启清零为已知限制 | [安全 §2.3](security-operations.md#23-分布式预算) 的原子 reserve/settle |
| G-01d | Filter：危险工具二次确认 | **已实现（单机）**：nonce 绑定 tenant/revision/user/session/tool/规范参数 hash，5 分钟一次性 | `TestPermissionPolicyRequiresArgumentBoundOneTimeApproval`、`TestExtractApproval` | [IM §8](im-adapters.md#8-用户权限与危险操作确认) 的持久审批/card 方案 |
| G-01e | Filter：IM 用户权限 | **已实现**：binding 级 `allowed_users` 在 Runner 前校验 | allow/deny 用户集成用例 | [安全 §2](security-operations.md#2-filter-治理链) 的目录/角色强化 |
| G-02 | 请求、模型/工具耗时、投递、错误、token、成本、Session 延迟指标 | **大部分实现**：平台 `/metrics` 有 callback/request/worker latency/delivery/token/cost/tool permission；配置 OTLP 时框架 MeterProvider 另导出模型 TTFT/耗时/token 与 Agent/工具 histogram 到 Collector `:9464`；仍缺完整 Session/Memory backend histogram 和 SLO 告警 | GET `/metrics` 与 Collector `:9464/metrics`，验证 label 无用户/session/request | [安全 §4](security-operations.md#4-指标设计) 完整生产指标表 |
| G-03 | OTel trace 串 IM callback、Runner、Tool、Session/Memory、IM 回复 | **部分实现**：W3C 传播、OTLP、callback/signature/worker/send 显式 spans 和框架 Runner spans；队列/存储逐操作 span 不完整 | 配置 Collector，按 request 查 trace；检查无 LLM 正文 | [安全 §5](security-operations.md#5-opentelemetry-链路) 完整目标链及异步 link |
| G-04 | 审计至少含指定 11 类字段 | **已实现字段/部分生产耐久性**：`audit.Entry` 含所有必需字段及 request/revision/hash；策略随请求 revision 固定；入站与工具决策已接入；stdout/file 非 durable、写失败计指标但不 fail closed | `TestAuditRedactsSecretsAndPII`、`TestRouterUsesImmutableEntryPolicyAndCurrentReferencedSecret`；触发 allow/deny/tool ask 检查 JSONL | [安全 §6](security-operations.md#6-审计日志) 的 WORM、链式 hash、对账 |
| G-05 | IM/model/DB secret 不进日志/trace/error | **已实现最小策略/部分生产管理**：只存 env 引用、HTTP 错误归一、审计 redactor，Chat/Agent/Tool/Workflow payload span 属性源端 Drop 且 Collector 二次删除；未接 Vault/动态轮换 | `TestTelegramDeliveryErrorNeverLeaksBotToken`、审计脱敏测试；再做 secret canary trace/access-log 检查 | [安全 §3](security-operations.md#3-密钥管理与脱敏) |

## 5. 故障恢复与运维

| ID | 原始要求 | 当前状态与代码证据 | 验收方式 | 生产设计证据 |
| --- | --- | --- | --- | --- |
| O-01a | 节点故障降级 | **部分实现**：HTTP 优雅停机、队列 drain；但 202 后进程退出可丢内存任务 | 在压测中 SIGTERM 检查正常 drain；SIGKILL 证明已知丢失窗口 | [安全 §7](security-operations.md#7-故障恢复与降级) 的 durable queue/lease/CAS |
| O-01b | IM 重试降级 | **部分实现**：去重、最多 3 次短重试、pending result；无 Retry-After/DLQ | flaky Adapter 自动测试；注入 429/超时做负向验收 | [IM §7](im-adapters.md#7-平台限制与降级矩阵)、[安全 §7](security-operations.md#7-故障恢复与降级) |
| O-01c | DB 短暂不可用 | **生产设计为主**：当前返回错误并短重试，不会安全持久等待；不得退到 InMemory | 中断 Redis/DB，确认安全失败且不跨租户；生产环境验证 queue NACK/drain | [安全 §7](security-operations.md#7-故障恢复与降级) |
| O-01d | 模型超时 | **部分实现**：Worker context timeout，默认 90 秒；无 provider-aware fallback/账单对账 | fake slow model/取消测试（待补） | [安全 §7](security-operations.md#7-故障恢复与降级) 的可重试分类、备用模型与 reconciliation |
| O-01e | 工具执行失败 | **部分实现**：Runner 错误返回，permission 决策可审计；无独立熔断/副作用 ledger | fake idempotent/non-idempotent tool 故障测试（待补） | [安全 §2、§7](security-operations.md#7-故障恢复与降级) |
| O-02 | 灰度发布与租户级配置回滚 | **部分实现**：仅 tenant reload、每租户最多 10 个内存历史 revision、历史 version 内容不可变、Runtime revision cache；顶层变化要求重启，无跨节点控制面/稳定分桶/自动门禁 | 手工 reload v2→rollback v1；`TestApplyRejectsChangedPreviouslySeenVersion`；验证在途请求 revision 不变（待自动化） | [架构 §8](architecture.md#8-配置热更新灰度与回滚)、[安全 §8](security-operations.md#8-灰度发布与租户级回滚) |
| O-03 | 容量评估 | **不适用代码**；当前 queue/worker 数可配置 | 用真实压测参数代入并与饱和点校准 | [安全 §9](security-operations.md#9-容量评估) 含并发、节点、队列、token、Redis/SQL/IM 公式与样例 |
| O-04 | 最小与生产部署方案 | **部分实现/生产设计**：当前单二进制可运行；生产基础设施未实现 | 启动最小方案；生产按 readiness、HA、恢复演练验收 | [架构 §9](architecture.md#9-最小部署与生产部署)、[安全 §10](security-operations.md#10-部署与运行手册) |

## 6. 自动化验证基线

在 `solution/` 目录执行：

```bash
go test ./...
go test -race ./...
go vet ./...
```

最低自动化覆盖及其证明目标：

| 测试 | 证明什么 | 不能证明什么 |
| --- | --- | --- |
| config decode/default/unknown/collision | 配置严格、secret 使用引用、binding 不跨租户复用 | 多节点配置发布与 Vault 轮换 |
| Telegram/Slack/企业微信 Adapter | 原始请求验签、重放时间窗、规范消息映射、文本拆分（rune 与字节两路径）、企业微信 AES 回调/URL 验证、Telegram 与企业微信错误不泄露 token/secret | 真实平台限流/权限变化/所有事件类型 |
| Gateway binding/signature | tenant 来自已验证服务端 binding，验签失败不 dispatch | 持久 ACK；当前 queue 在内存 |
| identity/group tests | ID 稳定、群上下文共享、跨租户隔离 | HMAC 密钥轮换、账号合并 |
| worker continuity/concurrency/Redis | 单机相同 session 串行、不同租户状态分离、独立 Runtime 共享 Redis Session/Memory | Redis 租约失锁后的陈旧写防护 |
| dedupe/pending result | 重投不重跑，投递失败可复用输出 | 平台发送成功但响应丢失的 exactly-once |
| input budget/tool approval | 月成本预留、确认参数绑定、一次性、过期/篡改拒绝 | 多节点原子 settle、持久审批和实际副作用对账 |
| audit redaction | 常见 PII/secret 不出现在审计字段 | 任意第三方 SDK/Collector/代理日志均安全 |

发布前还应增加当前缺失的集成/故障测试：真实 Redis 双节点、锁租约丢失、持久 Inbox/Outbox crash-window、数据库恢复 drain、OTel 完整 span、模型超时与取消、工具副作用 ledger、429/Retry-After、附件 SSRF/病毒/大小限制、配置 canary/rollback 和备份 PITR。

## 7. 手工演示验收

1. 用 mock 模型启动单节点，访问 `/healthz`、`/readyz`、`/metrics`。注意当前 ready 只是进程级信号。
2. 通过 Admin 测试 API 对 tenant A 连续发送两条消息，确认 turn 连续；同外部 ID 重发确认 `duplicate=true`。
3. 用相同外部 user/message ID 调 tenant B，确认从 turn 1 开始，session/user hash 与 tenant A 不同。
4. 发送 Telegram/Slack 合法与非法签名夹具，分别确认 accepted 与 401，非法请求没有 Worker 调用。
5. 群内两个成员发送消息，确认共享群 session；另一个群、thread 或租户不共享。
6. 调用 allow 工具、deny 工具和 `require_confirmation` 工具；修改参数复用 nonce 必须失败；检查 tool audit 只含参数 hash。
7. 让 IM Adapter 首次投递失败，重投后回复文本相同且 Agent turn 只增加一次。
8. 超过输入、RPM、月预算和 allowed user 限制，确认在模型前拒绝，审计 decision/error_type 正确。
9. 配置 OTLP，按 trace ID 查看 callback→worker→Runner→send 已有 spans，并确认 prompt/response/secret 不出现；把缺失的存储 span 记录为已知差距。
10. reload 一个租户 v2 后 rollback，确认其他租户配置不变；重启后历史丢失是当前已知限制。

## 8. 上生产前的 P0 缺口

以下条目是从“完整赛题原型”走向“生产系统”的硬门槛，文档设计不能替代实现：

1. 持久 SQL Inbox/Outbox、durable session-partitioned queue 和 DLQ，做到持久化后 ACK。
2. Session 版本化原子提交/CAS 或 fencing token；处理租约丢失与副作用工具幂等 ledger。
3. SQL Session、共享 Memory/向量 Knowledge、对象 Artifact 和 durable Audit Adapter，以及 capability/integration test。
4. 分布式限流、预算 reserve/settle、审批 store、配置 revision 控制面和跨节点 cache invalidation。
5. 入模前 PII/tokenization、出站 DLP、附件安全流水线、资源级工具授权与沙箱。
6. 完整 Adapter/queue/model/tool/storage spans、histogram/SLO、依赖感知 readiness、审计 WAL/WORM 和恢复对账。
7. OIDC/RBAC/MFA Admin、Secret Manager/KMS、mTLS/NetworkPolicy、secret 双版本轮换。
8. Redis→SQL 与向量迁移执行器、shadow 比对、canary/cutover/rollback 演练和自动化数据校验。

只有 P0 中与实际部署范围相关的条目实现并通过故障演练后，才能把相应矩阵状态从“部分实现/生产设计”升级为“生产已实现”。
