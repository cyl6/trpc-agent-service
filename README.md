# 多租户 IM Agent Platform

这是基于 `trpc-agent-go` 的赛题完整交付：一个可直接运行的多租户 Agent Gateway/Worker 示例，以及面向生产环境的架构、数据、一致性、安全、可观测性和部署设计。

默认使用确定性 mock Agent，因此不需要模型 API Key 就能走通：

```text
HTTP / Telegram / Slack / 企业微信
  → webhook 原文验签
  → 可信 channel binding 解析 tenant
  → 消息规范化、用户权限和租户预算 Filter
  → 幂等 Inbox/结果 Outbox、同 Session 串行
  → 租户 Runtime/Runner 池
  → trpc-agent-go Runner + Session/Memory/Artifact
  → ToolFilter + ToolPermissionPolicy + 参数绑定审批
  → IM 分片投递、审计、Metrics、OpenTelemetry
```

## 已实现能力

- 租户模型覆盖应用、模型、工具、Telegram/Slack/企业微信、Session/Memory/Summary/Artifact/Knowledge/Audit 后端、审计和预算策略。
- `(channel_type, binding_id)` 是租户身份唯一可信来源；外部消息不能指定 `tenant_id`。
- Telegram secret token 验证和 Slack HMAC-SHA256 + 5 分钟重放窗口验证；Slack 再绑定 `team_id/api_app_id`，阻断共享 App secret 下的跨工作区路由。
- 企业微信回调 AES-256-CBC 解密 + SHA1 验签 + URL echostr 验证；receiveID/corpid 与 AgentID/agentid 双重绑定，阻断跨企业、跨应用路由；单聊/群聊回复分流，access_token 缓存与 40014/42001 刷新重试；2048 字节按 rune 边界拆分。
- 私聊、群聊、群话题使用确定性租户隔离 ID；群内不同成员共享同一个 Runner group principal，真实发送者单独审计。
- 按租户、配置版本和配置摘要缓存不可变 Runner；不同 revision 可交错复用，后端构建用 singleflight 隔离慢连接且不持有全局缓存锁。Session 与 Memory 均支持 InMemory/Redis。
- 以 `agent.WithAppName` 强制 Session/Memory namespace；应用名为无 `/` 的 opaque hash，避免 EventFilterKey 层级污染。
- `agent.WithToolFilter` 缩小模型可见工具面，`agent.WithToolPermissionPolicyFunc` 在最终参数产生后再次强制鉴权。
- 危险工具审批绑定 tenant、配置版本、用户、session、tool 和规范化参数 hash，五分钟过期且一次性消费。
- 消息 Claim + pending result Outbox：IM 投递失败时重放已有结果，不重复运行 Agent。
- InMemory/Redis 同 Session 锁、租户 RPM/输入大小/月度成本预留、PII/secret 审计脱敏。
- Prometheus 文本指标、框架 OTLP MeterProvider（模型 TTFT/token/耗时与 Agent/工具 histogram）和跨异步队列的 W3C trace context；LLM/Agent/Tool/Workflow 的正文、参数和结果等敏感 trace 属性由服务端 SpanAttributePolicy 在源端 Drop，OTel Collector 再做一次统一删除兜底。
- Admin API 支持配置重载和租户 revision 回滚。

生产方案在上述最小实现之上使用持久 inbox/Outbox、按完整 Session Key 分区的消息队列、SQL fencing、Secret Manager、独立 Channel Sender 和外置 Summary/Memory 任务。两者边界在 [架构设计](docs/architecture.md) 与 [数据一致性](docs/data-consistency.md) 中明确说明；不会把进程内队列或普通 Redis lease 描述成 exactly-once。

## 赛题需求完成情况

对照 `赛题.md` 的 7 条验收标准逐项审查（详细矩阵见 [docs/acceptance.md](docs/acceptance.md)）：

| # | 验收标准 | 状态 | 证据 |
| --- | --- | --- | --- |
| 1 | 架构方案覆盖多租户、节点化部署、数据同步、多后端、IM 接入、治理监控、故障恢复 | 已覆盖 | 四篇设计文档 + acceptance.md 逐项矩阵 |
| 2 | 数据模型表达 tenant、agent、channel binding、session、event、memory、summary、audit log | 已交付 | [migrations/001_schema.sql](migrations/001_schema.sql)、[数据一致性 §10](docs/data-consistency.md) |
| 3 | 至少两种 IM 通道接入差异，其中至少包含微信或企业微信 | 已满足 | Telegram/Slack/企业微信三通道已实现，差异（明文签名头 vs HMAC vs AES 加密信封、rune vs 字节拆分、单聊/群聊 API 分流）见 [IM Adapter §2](docs/im-adapters.md) |
| 4 | 至少三类后端的存储与同步策略 | 已覆盖（设计层） | [数据一致性 §7](docs/data-consistency.md)：Redis、SQL、向量库、对象存储逐一取舍 |
| 5 | 完整消息链路时序，`trace_id` 贯穿 | 已覆盖 | [架构 §4](docs/architecture.md) Mermaid 时序图；[安全 §5](docs/security-operations.md) trace 链路 |
| 6 | 至少 8 个生产风险及缓解措施 | 已覆盖 | [安全 §7](docs/security-operations.md) 11 项故障/风险缓解表；acceptance.md §8 P0 缺口 |
| 7 | 明确哪些能力复用 tRPC-Agent-Go、哪些是平台层新增 | 已覆盖 | [架构 §6](docs/architecture.md) 集成边界 |

交付物对照：架构设计文档、系统架构图（最小/生产两张拓扑）、核心时序图（Telegram/Slack/企业微信完整链路）、数据模型 SQL、同步与幂等策略、多后端适配方案、风险清单、可运行代码（`go vet`、`go test`、smoke 全部通过）均已交付。

已知差距（按影响排序，与 acceptance.md 状态一致）：

1. 生产数据面为设计而非实现：持久 SQL Inbox/Outbox、session 分区持久队列、CAS/fencing token、Redis→SQL 迁移执行器（acceptance.md §8 P0 清单）。
2. Summary/Knowledge 后端尚未装配到 Runtime：Summary 随 Session 存储走，Knowledge 当前仅支持 `disabled` 配置。
3. 水平扩展受限：消息队列、审批、预算、配置历史均为进程内状态，Redis 只共享 Session/Memory/协调键；随附 K8s 清单有意保持单副本。
4. 限流与预算为单进程内存固定窗口，多节点下不原子，重启清零。
5. 通道出站能力为纯文本：无出站附件、卡片、编辑/撤回；企业微信 MediaId 仅入站引用。

## 目录

```text
cmd/trpc-service/      单二进制入口（Gateway + Worker + Admin）
trpcservice/           服务库根（版本号在此）
trpcservice/channels/  Telegram、Slack、企业微信 Adapter
trpcservice/web/       Web 管理页、Webhook、Admin、直接验收 API
trpcservice/worker/    Runner 调用、幂等、Outbox、投递
trpcservice/agent/     按租户/revision 的 Runner 与后端池
trpcservice/tenant/    租户 Registry 与治理 Filter
trpcservice/tenant/governance/ 用户、预算、工具、审批 Filter
trpcservice/coordination/     InMemory/Redis 幂等与 Session lane
trpcservice/log/       租户路由、JSONL、脱敏审计
trpcservice/metrics/   低基数 Prometheus 指标
trpcservice/skill/     Agent Skills 仓库（SKILL.md 加载）
trpcservice/domain/    InboundMessage 等领域模型
trpcservice/tool/      平台内置工具面
trpcservice/queue/     进程内异步队列
trpcservice/config/    配置加载、校验、Secret 引用
trpcservice/workspace/ Agent 运行时工作区（本地开发）
config/                两租户示例
data/                  本地运行的 PID 与日志
api/                   OpenAPI 契约
migrations/            生产控制面和数据面 SQL
deploy/                Dockerfile、Compose 与 Kubernetes
scripts/               冒烟测试
observability/         OTel Collector 和 Prometheus
docs/                  赛题逐项设计及验收矩阵
build.sh 等            build/start/stop/clean/coverage/format/lint 脚本
```

依赖上游发布的 `trpc.group/trpc-go/trpc-agent-go`（见 `go.mod`），不携带任何本地框架补丁。敏感 Span 属性（正文、参数、结果、system instructions）在 OTel Collector 端统一二次删除（见 `observability/otel-collector.yaml`），与源端规则互为纵深防御。

## 零密钥快速运行

要求 Go 1.24+（见 `go.mod`）。

```bash
cd solution
export ADMIN_TOKEN=local-admin
go run ./cmd/trpc-service -config config/example.yaml
```

另一个终端执行两轮对话：

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/chat/acme \
  -H 'Authorization: Bearer local-admin' \
  -H 'Content-Type: application/json' \
  -d '{"message_id":"demo-1","user_id":"user-1","scope":"direct","text":"第一轮"}'

curl -sS -X POST http://127.0.0.1:8080/v1/chat/acme \
  -H 'Authorization: Bearer local-admin' \
  -H 'Content-Type: application/json' \
  -d '{"message_id":"demo-2","user_id":"user-1","scope":"direct","text":"第二轮"}'
```

响应中的文本应依次包含 `[mock turn 1]`、`[mock turn 2]`，且 `session_id` 相同。再次发送 `message_id=demo-2` 会返回 `duplicate=true`，不会再向 Session 追加事件。

健康和监控：

```bash
curl -sS http://127.0.0.1:8080/healthz
curl -sS http://127.0.0.1:8080/readyz
curl -sS http://127.0.0.1:8080/metrics
```

`/v1/chat` 是受 Admin Bearer Token 保护的本地验收入口，不是公网 IM 接口。生产消息只应进入 `/webhooks/{channel}/{opaque-binding}`。

## 接入真实模型

把租户模型配置改为：

```yaml
model:
  provider: openai
  name: gpt-4.1-mini
  variant: openai
  api_key_env: ACME_MODEL_API_KEY
  max_tokens: 2048
  temperature: 0.2
  streaming: true
```

然后仅通过环境变量或 Secret Manager 注入 `ACME_MODEL_API_KEY`。也可设置 `base_url` 使用 OpenAI-compatible 服务。模型 Key、IM Token、数据库 DSN 都只能在 YAML 中出现环境变量名，不能出现明文值。

## IM Webhook

### Telegram

在 Bot webhook 注册时设置 `secret_token`，并配置：

```bash
export ACME_TELEGRAM_WEBHOOK_SECRET='random-webhook-secret'
export ACME_TELEGRAM_BOT_TOKEN='bot-token-from-secret-manager'
```

Webhook URL 为：

```text
https://<host>/webhooks/telegram/acme-telegram
```

Adapter 先比较 `X-Telegram-Bot-Api-Secret-Token`，验签成功后才解析 JSON。`update_id` 是幂等键；文本、图片和文件都会转换为统一 `InboundMessage`。

### Slack

```bash
export ACME_SLACK_SIGNING_SECRET='signing-secret'
export ACME_SLACK_BOT_TOKEN='xoxb-...'
```

Events API Request URL：

```text
https://<host>/webhooks/slack/acme-slack
```

Adapter 校验 `v0:{timestamp}:{raw_body}` HMAC、拒绝超过五分钟的请求、处理 URL verification，并过滤 bot/subtype 消息防止回复环路。

### 企业微信

```bash
export ACME_WECOM_CORP_SECRET='self-built-app-secret'
export ACME_WECOM_CALLBACK_TOKEN='callback-token-configured-in-wecom-admin'
export ACME_WECOM_ENCODING_AES_KEY='43-char-encoding-aes-key'
```

配置中 `workspace_id` 填 corpid、`application_id` 填自建应用 agentid。接收消息服务器 URL：

```text
https://<host>/webhooks/wecom/acme-wecom
```

注册 URL 时企业微信发送 GET `echostr` 挑战，服务端验签、AES-256-CBC 解密并校验 receiveID 后回显明文。普通回调是仅含 `Encrypt` 字段的 XML 信封：先验 `SHA1(sort(token, timestamp, nonce, encrypt))` 与 ±5 分钟时间窗，再解密，并强制比较 receiveID/corpid 与 AgentID/agentid，防止跨企业、跨应用路由。带 `ChatId` 的回调按群聊处理。回复走主动 API：单聊 `message/send`（touser+agentid）、群聊 `appchat/send`（chatid），access_token 按 (corpid, secret) 缓存并在 40014/42001 时刷新重试一次。文本上限 2048 UTF-8 字节，按字节拆分且不切断多字节字符。

平台长度、限流、文件、异步回复和失败策略见 [IM Adapter 设计](docs/im-adapters.md)。

## Redis 与多节点

`globex` 示例租户选择 Redis Session：

```bash
export REDIS_URL='redis://127.0.0.1:6379/0'
```

若要验证跨 Runtime 状态共享，还应把顶层 `coordination.backend` 改为 `redis` 并配置 `redis_url_env: REDIS_URL`，同时让所有租户的 Session/Memory 都选择 Redis；对话状态路由因此不需要 HTTP sticky session。当前单体仍使用本进程队列、审批、预算和配置历史，所以随附 Kubernetes 清单有意保持单副本，不能仅靠 Redis 后端就安全横向扩容。生产严格顺序采用持久队列的 Session 分区，并让 SQL 写入校验 fencing token。

## 配置热更新与回滚

修改 YAML 时必须递增租户 `version`，再调用：

```bash
curl -X POST http://127.0.0.1:8080/admin/v1/reload \
  -H 'Authorization: Bearer local-admin'

curl -X POST http://127.0.0.1:8080/admin/v1/tenants/acme/rollback \
  -H 'Authorization: Bearer local-admin'
```

在途请求固定旧快照并继续完成，新请求获取新 Runtime；本进程见过的 revision 会缓存到停机，避免延迟任务在版本交错时反复重建连接。reload 只接受 tenant revision 变化；server、coordination 或 telemetry 变化返回冲突并要求重启。同一 tenant/version 的内容不可变，历史版本复用不同内容也会拒绝。生产环境用 SQL `tenant_config_revision` + active revision CAS，Admin API 只发布失效事件并为缓存设置容量/淘汰策略。

## 测试与构建

```bash
go mod tidy
go test ./...
go test -race ./...
CGO_ENABLED=0 go build -mod=readonly ./...
./scripts/smoke.sh   # 单测 + vet + 构建后起服务做端到端冒烟（含重复投递与 secret 泄漏检查）
```

测试覆盖严格 YAML/env 引用、双 IM 验签、企业微信 AES 验签解密/URL 验证/绑定校验/token 缓存与字节拆分、Slack workspace/app 绑定、重放窗口、Unicode 分片、私聊/群聊 ID、附件预算边界、跨租户隔离、同 Session 并发、重复投递、pending Outbox 重放与完成审计、历史 revision 不可变、参数绑定一次性审批和 secret/PII 脱敏。

## 设计文档

- [总体架构与节点拓扑](docs/architecture.md)
- [后端抽象、同步、一致性与迁移](docs/data-consistency.md)
- [Telegram / Slack / 企业微信接入](docs/im-adapters.md)
- [治理、安全、监控、故障、发布和容量](docs/security-operations.md)
- [赛题逐项验收矩阵](docs/acceptance.md)
- [生产数据模型](migrations/001_schema.sql)

## 最小实现与生产推荐的边界

本仓库的单二进制、InMemory queue、JSONL audit 和内存审批是可执行的最小部署，用于评审和本地验证。生产推荐部署不会依赖它们保证耐久性：Gateway 在持久 Inbox/dispatch Outbox 提交后快速 ACK；Worker 从按 Session 分区的持久队列消费；回复进入 delivery Outbox；Sender 独立重试并遵循平台 `Retry-After`；审批、预算和配置 revision 存 SQL/Redis；Artifact 使用 S3/COS；Telemetry 经 Collector 做二次敏感字段清理。
