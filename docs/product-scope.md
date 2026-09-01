# Bablo 产品范围与分期

> 状态：架构规划基线
> 观察日期：2026-08-29
> 产品身份：Bablo / `bablo` / AI Gateway

## 1. 当前仓库事实

本次审计的工作目录只包含 `.omp/` 提示词、命令、代理和技能文件；未发现 Go module、Go 源码、Vue/TypeScript/Vite 工程、数据库 migration、CI、Docker/Compose、README 或 `docs/`。工作目录也不是 Git 仓库：`git status --short --branch` 返回“不是 Git 仓库”。因此本文件是从零开始的架构基线，不覆盖已有业务实现；所有实现状态以 `docs/implementation-status.md` 为准。

## 2. 产品目标

Bablo 是面向多用户、多 API Key、多模型的 OpenAI-compatible AI Gateway：

- 控制面自研用户、认证、Key、模型、Provider、Credential、路由、调度、Usage、价格、钱包、支付、统计和审计；
- CPA（CLIProxyAPI）仅作为 `internal/inference/cpa` 内的 inference engine，通过锁定版本的公开 SDK 接入；
- 一个 API Key 通过 policy/entitlement 访问多个模型，不能绑定单一 Group、Provider 或上游账号；
- PostgreSQL 是业务唯一事实来源，Redis 只保存可重建的限流、租约、粘性和短期调度状态；
- 每次请求能够追溯实际 resolved model/provider/route/credential、价格版本、token、费用、错误和调度原因；
- 默认不持久化原始 Prompt/响应正文。

## 3. 用户与使用场景

### 3.1 普通用户

- 登录控制台，创建/撤销/轮换多个 API Key；
- 用同一个 Key 调用多个有权限的 public model；
- 查看可用模型、价格、Usage、钱包账本和充值状态；
- 管理会话、密码和可选的个人 MFA。

### 3.2 管理员/运营

- 管理用户、RBAC、模型目录、Provider、Credential、Credential Pool 和路由；
- 预览给定 user/key/model 的候选路线；
- 查看 Scheduler Decision Log、Credential 健康/配额、Usage、Ledger、Payment 和 Audit；
- 通过追加式 adjustment/grant 处理人工账务，不修改历史账本。

### 3.3 客户端开发者

- 使用标准 `Authorization: Bearer <bablo-key>`；
- 首发使用 `/v1/models`、`/v1/chat/completions`、`/v1/responses`；
- 依赖稳定的 OpenAI-compatible 错误结构、request ID 和 SSE streaming 行为。

## 4. 明确边界

### 4.1 第一阶段最小生产版本（P0）

P0 是“可控用户、预充值/管理员授信、单实例”的生产版本，不等于所有高级能力完成：

1. Go 服务骨架、配置、结构化日志、优雅退出、`/healthz`、`/readyz`、metrics；
2. PostgreSQL migration/repository/事务边界，Redis 运行时接口；
3. 用户登录 Session、RBAC（至少 admin/user）、管理员 TOTP MFA、CSRF；
4. API Key CSPRNG 生成、只显示一次、hash+prefix 存储、撤销/过期/轮换；
5. policy/entitlement，使一个 Key 可访问多个 public model；
6. Provider、Model、Credential、Credential Pool、加密 secret metadata；
7. public model 到 route target 的 exact match 路由；
8. 确定性、可解释的 Scheduler（优先级 + 稳定 round-robin；硬过滤和 Decision Log 必须完整）；
9. CPA v7.2.145 adapter 的非流式、流式、取消和错误映射兼容测试；
10. `/v1/models`、Chat Completions、Responses，包含 stream/non-stream、首包前/后错误和客户端取消；
11. 自有 immutable UsageEvent、价格快照、预算预检/预留、Wallet Ledger、结算和失败重试；
12. 管理员 credit/adjustment 与一次性 voucher 已实现为 P0 资金入口；Stripe Checkout/webhook/refund adapter 已建立，但没有真实 test-mode E2E 证据时 self-service Stripe 支付不得标为生产可用；
13. 基于 PostgreSQL 的基础统计、审计日志、敏感字段过滤；
14. Docker/Compose 示例、migration release step、备份恢复和回滚 runbook。

P0 上线门禁仍要求真实 PostgreSQL/Redis、CPA compatibility suite、一个 Key 多模型 E2E、Usage/Ledger 幂等并发测试、secret scan、restore drill 和安全审计证据；未验证项对应能力必须 NO-GO。

### 4.2 P1 增强

- self-service 支付 Provider（按官方规范验签、金额核对、防重放、幂等）并完成 sandbox E2E；
- quota snapshot poller、staleness/conservative policy、429 cooldown；
- weighted/fill-first/quota-aware scheduler；
- Claude Messages、Gemini 等额外协议，逐协议建立契约测试；
- route 版本生效时间、灰度/Canary、请求级 fallback trace；
- 管理员和用户更完整的 Usage/账务页面；
- OTel traces、告警、日/小时 rollup 与 retention job；
- CPA credential refresh、重加密轮换 worker 和更完整的 provider health probe。

### 4.3 P2 增强

- 多实例 HA、Redis lease/affinity/cursor 的跨实例压测；
- 多区域、跨区域灾备和流量切换；
- 更精细的 session affinity、quota 预测、成本/延迟优化；
- 组织/团队/项目层级、SSO/SCIM、细粒度 ABAC；
- 大规模统计存储。只有 PostgreSQL rollup 的真实性能证据不足时，才评估额外分析基础设施；默认不引入 ClickHouse、Kafka、Nacos。

## 5. 不在 P0 的内容

不在第一阶段实现或承诺：任意 Provider 全协议兼容、自动抓取/规避上游限制、将 subscription 资源默认商业转售、无审计的后台手改、CPA 内存 usage queue 作为账本、原始 Prompt/响应长期留存、无真实凭据的支付成功、Active-Active 多区域和不可解释的随机调度。

## 6. 发布资格原则

- 任何 P0 能力必须有对应代码、migration、测试和运维路径；
- 真实支付、外部 OAuth、域名/TLS、邮件等缺少凭据时，只做到配置后可启用，并把 E2E 标为 blocker；
- `Critical/High` 安全问题为零；
- 费用只来自 UsageEvent + Wallet Ledger，统计可以聚合但必须可回溯；
- 旧版本升级必须先通过锁定 CPA tag 的兼容套件，不得以导入 CPA `internal/*` 解决编译问题。
