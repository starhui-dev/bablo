# Bablo 项目上下文

本仓库是 **Bablo**，一个可长期维护、可正式上线的多用户 AI Gateway。

项目身份固定为：

- 产品名：Bablo
- 仓库名：bablo
- 默认二进制/服务名：bablo
- 公共描述：AI Gateway

核心策略：**Bablo 自研业务控制面，CLIProxyAPI（CPA）仅作为 inference engine，通过公开 CPA SDK 隔离接入。**

## 产品目标

Bablo 必须支持：

- 一个用户可拥有多个 API Key；**一个 Key 可访问多个模型**，不得把 Key 与单一 Group、单一 Provider 或单一上游绑定。
- 用户、角色、API Key、模型目录、路由、Provider、Credential、配额窗口、钱包、充值、订单、套餐、价格版本、Usage、统计、审计。
- OpenAI-compatible 数据面，至少覆盖项目实际需要的 `/v1/models`、Chat Completions、Responses；根据真实客户端需求扩展 Claude Messages / Gemini 等入口。
- CPA 负责 OAuth、Provider Executor、协议转换、Streaming、Tool Calling、Reasoning、Credential Refresh 等底层能力。
- PostgreSQL 是业务唯一事实来源；Redis 只承担缓存、分布式限流、租约、短期会话/调度状态等可重建状态。
- 统计必须能回答：谁、哪个 Key、哪个模型、哪个 Provider、哪个 Credential、哪个 Route、多少 Token/缓存、多少钱、成功率、错误、延迟、TTFT、TPS、Fallback、调度原因。
- 调度必须可解释、可测试、可复现，后台可查看每次选择/排除 Credential 的原因。
- 支持生产部署、备份恢复、Canary/滚动升级、CPA SDK 安全升级、回滚和从旧网关迁移。

## 技术基线

- 后端：Go。Go 版本必须与当前锁定 CPA SDK 的 module 要求兼容；不得凭记忆写死版本。
- 数据库：PostgreSQL。
- 缓存/协调：Redis。
- 前端：Vue 3 + TypeScript + Vite；包管理器 pnpm。主 UI 框架一旦确定不得混用多套。
- API：JSON REST 管理面 + OpenAI-compatible 推理面。
- 部署：容器优先；必须有 healthz/readyz、migration、优雅退出、结构化日志和 metrics。

## 最重要的架构边界

### 1. CPA 隔离边界

只有 `internal/inference/cpa` 可以 import `github.com/router-for-me/CLIProxyAPI/...`。

业务层、数据库层、计费层、路由层、Scheduler、HTTP DTO 不得暴露 CPA SDK 类型。

Bablo 必须定义自己的稳定接口，例如：

```go
type InferenceEngine interface {
    Execute(ctx context.Context, req Request) (Response, error)
    ExecuteStream(ctx context.Context, req Request) (Stream, error)
    Capabilities(ctx context.Context) (Capabilities, error)
}
```

接口形状按实际需要设计，上例仅表达边界。

### 2. 不猜 CPA SDK API

CPA 迭代很快。实现/升级前必须：

1. 检查目标 tag 的 `go.mod` module major 与 Go 版本；
2. 检查对应 pkg.go.dev 公共包；
3. 检查目标 tag 源码和 examples；
4. 只使用该 tag 的公开 `sdk/*` API，业务代码禁止 import CPA `internal/*`；
5. 将锁定版本、commit、验证日期和实际使用的公共符号写入 `docs/upstream-compatibility.md`。

### 3. 钱包与 Usage 是财务数据

CPA usage aggregation / queue 只能用于观测或对账，不得作为真钱账本唯一事实来源。

必须自建：

- `usage_events`：不可变、幂等的请求结算事实；
- `wallet_ledger`：追加式账本；
- `payment_orders`：支付订单状态机；
- `price_versions`：价格快照/版本。

余额可以做事务内缓存，但必须可由账本重建。金额禁止 float，使用最小货币单位或明确精度的 decimal/numeric。

### 4. 请求结算记录实际路由

Usage 至少记录：requested_model、resolved_model、provider、route_id、credential_id、price_version_id、input/output/cache/reasoning token、upstream status/error class、latency/TTFT、request_id、user_id、api_key_id。

不能只按 public alias 计费。

### 5. API Key 与 Group 解耦

核心关系：

`API Key -> Policy/Entitlement -> Model Route -> Provider/Credential Pool`

而不是：

`API Key -> Group`

Key 可以限制模型、RPM/TPM、IP、到期时间和预算，但一个 Key 必须能够安全访问多个模型。

### 6. Scheduler 独立领域化

先硬过滤：disabled/revoked、不支持模型、cooldown、quota exhausted、并发槽位不可用、policy 不允许。

再评分/选择：priority、weight、reset_at、quota_remaining、current concurrency、recent 429/5xx、latency、cost、session affinity。

任何随机行为都必须显式、可配置、可测试；每次调度必须产生 Decision Log。

### 7. PostgreSQL 是唯一业务事实来源

禁止同时把 PostgreSQL、CPA config.yaml、auth files 和后台手改都当作可写主数据源。若 CPA 需要 config/auth runtime artifact，它们必须能由 Bablo 的主状态重新生成。

### 8. Secrets 与敏感数据

- Bablo API Key 生成后只展示一次；持久层存不可逆 hash + 短 prefix，不保存明文。
- 上游 OAuth refresh token/API Key 加密存储；主加密密钥只能从环境变量/secret store 注入。
- 默认日志不得记录 Prompt、响应正文、Authorization、Cookie、完整 API Key、OAuth token、支付密钥。
- 管理员敏感操作必须有 audit log。

### 9. 生产安全

- 管理面与推理面鉴权分离。
- 管理员至少支持并默认要求 MFA。
- Browser Session 使用 HttpOnly/Secure/SameSite Cookie，并做好 CSRF 防护。
- PostgreSQL/Redis 不对公网暴露。
- CPA Management API 不对公网暴露。
- 外部 webhook 必须验签、幂等、防重放。

### 10. 上游资源政策

数据模型明确区分 `official_api`、`enterprise_api`、`subscription`、`third_party` 等资源类型；商业可用性是资源级策略。不得实现规避上游使用限制、账号安全控制或 ToS 限制的能力。

## 工程规范

- 数据库结构修改全部 migration 化。
- 关键状态机具备单测、并发测试和幂等测试。
- 对外错误返回稳定 request_id，内部日志带 trace/request correlation。
- 对外 API 变更同步 OpenAPI/接口文档。
- 新增依赖先检查维护状态、许可证和必要性，避免无量化需求引入大型中间件。
- 外部 SDK/协议不得凭记忆实现，先核对该版本官方资料。
- “能编译”不等于完成；每阶段执行对应测试、lint、race/并发验证和必要集成测试。
- 不得删除已有正确业务能力来逃避测试；冲突时做最小且有说明的修正。

## 完成定义

每个 `/bablo-*` 命令完成后必须：

1. 代码/迁移/测试实际落地；
2. 执行当前环境可运行的验证；
3. 更新 `docs/implementation-status.md`：完成项、未决项、风险、验证命令、下一步；
4. 架构决策更新 `docs/adr/`；
5. 涉及 CPA 时更新 `docs/upstream-compatibility.md`；
6. 外部凭据/商户号/域名缺失时把代码做到配置后可启用，并明确上线阻塞，禁止伪造凭据或虚假声明验证成功。
