# Bablo 总体架构

> 版本：架构规划基线
> 日期：2026-08-29
> 相关 ADR：`docs/adr/0001-cpa-sdk-boundary.md`、`docs/adr/0002-postgres-source-of-truth.md`、`docs/adr/0003-usage-ledger-billing.md`、`docs/adr/0004-model-routing-and-scheduler.md`、`docs/adr/0005-web-session-authentication.md`

## 1. 架构结论

第一阶段采用**模块化 Go 单体**：同一 `bablo` 进程同时承载管理面、推理面和后台 worker；PostgreSQL 保存所有业务事实，Redis 保存可重建运行时状态，CPA 只在 `internal/inference/cpa` 适配器中出现。这样先降低部署和事务复杂度，同时让模块接口、幂等键和 Redis 协调契约可在未来拆分/扩展到 HA。

```mermaid
flowchart LR
    U[用户控制台] --> C[Control API]
    K[OpenAI-compatible 客户端] --> D[Inference API]
    C --> S[Domain Services]
    D --> S
    S --> PG[(PostgreSQL
业务事实)]
    S --> R[(Redis
限流/租约/短状态)]
    D --> SCH[Deterministic Scheduler]
    SCH --> CPA[internal/inference/cpa
Bablo InferenceEngine adapter]
    CPA --> UP[CPA SDK v7.2.145 / upstream]
    S --> O[Transactional Outbox + workers]
    O --> ST[Stats rollup / audit / reconciliation]
```

## 2. 进程与部署拓扑

### P0 单实例

- `bablo`：HTTP server、CPA service、scheduler、usage settlement、outbox/quota worker；
- PostgreSQL：唯一业务数据库；只允许私网/容器网络访问；
- Redis：API Key RPM/TPM 固定分钟窗口计数、后续 Usage/Billing 驱动的预算快速门禁、credential concurrency lease、短 TTL affinity/cursor；全部状态可重建，配置 Redis 后错误 fail closed；未配置时只允许 P0 单实例使用进程内计数；
- 反向代理/TLS：部署环境提供，域名不写死；
- CPA management endpoint：不暴露公网，优先不启用远程管理。

### HA 演进

多个 `bablo` 实例共享 PostgreSQL/Redis。所有跨实例状态必须有 TTL、租约所有者和恢复策略：

- Redis lease 用 token 校验释放，过期后自动回收；
- DB transaction 保证 ledger/usage/payment 幂等；
- outbox worker 用 claim/lease 避免重复副作用；
- CPA adapter 每实例拥有本地 engine，但控制面配置由 PostgreSQL 生成并可重放；
- 不把 CPA config/auth 文件、Redis 或进程内 map 当主数据源。

## 3. 模块边界

每个模块只能通过 service/use-case 接口调用其他模块；handler 不直接拼 SQL。下表是第一阶段必须保留的领域边界。

| 模块 | 负责 | 输入/输出 | 明确禁止 |
|---|---|---|---|
| `user` | 用户生命周期、状态、角色关系 | user ID、profile、role | 推理 Key 校验、账本 SQL |
| `auth` | `internal/auth` 已实现登录、Argon2id、Session、密码、TOTP/recovery、RBAC/CSRF；`bablo auth` 提供本地管理员维护 | session principal、authorization decision、一次性 Cookie/MFA enrollment material | 把 Web Session 当推理 API Key、在 handler 绕过 service 授权 |
| `apikey` | Key CSPRNG 生成/SHA-256 hash、一次性明文、撤销、过期、原子轮换、IP/RPM/TPM/预算阈值、policy entitlement | key principal、policy/model authorization decision | 保存明文 Key、把 Key 绑定单一 group/provider、把 Redis 当权限事实源 |
| `model` | public model、canonical alias、能力、visibility、billing class、route readiness | model capability、alias resolution | 散落模型字符串、直接覆盖人工目录 |
| `provider` | Provider 元数据、资源政策、上游模型发现/审核 | provider ID、provider model、discovery signal | 处理 OAuth secret 明文；把发现结果直接变成可路由配置 |
| `credential` | 加密 secret metadata、健康、pool membership | credential ID、lease input | 日志输出 token、让 subscription 默认商业可用 |
| `route` | public model 到版本化 target 的匹配/快照 | route snapshot、candidate targets | 未授权 fallback、请求中途变更快照 |
| `scheduler` | 硬过滤、确定性选择、租约、Decision Log | candidates、policy、quota snapshot | 隐式随机、选择 disabled/revoked credential |
| `usage` | request/attempt、immutable UsageEvent、reconcile/outbox | token/status/latency、settlement key | 依赖 CPA usage queue 作为最终账 |
| `pricing` | price version、resolved target 价格 | price snapshot | 用 float、重写历史价格 |
| `wallet` | reservation、charge、release、refund、adjustment | ledger entry、balance policy | UPDATE 历史 ledger、并发透支 |
| `payment` | order/event 状态机、webhook 验签边界 | provider event、idempotency key | 信任客户端支付成功 |
| `stats` | 从 Usage/Ledger 的查询和 rollup | filters、aggregates | 自建另一套计费公式 |
| `audit` | 管理员/敏感动作不可变记录 | actor/action/target/result | 记录 secret、Prompt/响应正文 |
| `inference/cpa` | CPA SDK lifecycle、请求/流、错误和 capability 映射 | Bablo `InferenceEngine` 类型 | 向业务泄漏 CPA 类型或 import CPA `internal/*` |


### Auth 已实现调用边界

`internal/auth.Handler -> auth.Service -> auth.Repository -> internal/data.Store`。Handler 只负责 JSON、Cookie、Origin/CSRF 传输校验和稳定错误；密码验证、Session rotation、管理员 MFA 与角色判断在 Service；所有身份事实、Session 撤销、MFA counter/recovery code 消费和 audit 在 PostgreSQL 事务内完成。前端只消费 Bablo Session DTO，不接触 hash、MFA ciphertext 或数据库类型。

管理员目录写接口统一经 `auth.Handler.ProtectRole(..., "admin")`，再由 model/provider/pricing service 复核输入、事务和 audit；普通 Web Session 只能访问用户模型目录，不能绕过 RBAC 写资源。

P0 登录/MFA limiter 是有容量上限和自动过期的进程内状态，符合单实例首发边界；HA 前必须迁移为 Redis 协调实现。PostgreSQL 仍是用户、角色、Session 撤销、MFA 和 audit 的唯一事实源，Redis 丢失不能恢复权限或 Session。

### API Key 调用边界

`internal/apikey.Handler -> apikey.Service -> apikey.Repository -> internal/data.Store`。Web Session `auth.Handler.Protect` 只为用户自助 Key API 提供 full-session、Origin 和 CSRF 边界；推理面只通过 `Authorization: Bearer` 进入 `apikey.Service.IdentityMiddleware`，上下文只携带内部 user/key ID、prefix、secret version 和限额，不携带 raw key/hash。实际推理 handler 在解析请求模型和 token 估算后必须继续调用 `Service.Authorize`，以当前 user/key/secret version 重新检查有效性，再完成 model entitlement 与 RPM/TPM 门禁，不能只依赖身份中间件。

每个用户创建的 Key 拥有 default-deny managed policy；一个 policy 可允许多个 public model，显式 deny 优先于 allow。轮换原子替换同一 Key 的 hash，旧 secret 立即失效；P0 不提供双 Key 并行窗口。PostgreSQL 保存 Key、policy、授权和撤销事实；Redis 仅保存带 TTL 的固定窗口计数，Redis 丢失不能恢复权限或撤销状态。daily/monthly budget 阈值已进入 Key principal，真正消费门禁必须等待 Usage/Billing 事实源，不能把“尚无消费数据”伪装为零消费。

## 4. 稳定领域接口

以下是边界形状，不是对 CPA API 的猜测；实际 CPA 适配以 `docs/upstream-compatibility.md` 锁定 tag 源码为准：

```go
type Engine interface {
    Execute(context.Context, Request) (ExecutionResult, error)
    ExecuteStream(context.Context, Request) (Stream, error)
    Capabilities(context.Context) (Capabilities, error)
    Shutdown(context.Context) error
}

type Request struct {
    RequestID      string
    ResolvedRoute  ResolvedRoute
    SourceFormat   string
    ResponseFormat string
    Headers        map[string][]string // 由 HTTP 层过滤敏感/hop-by-hop 头
    Metadata       map[string]any      // 仅内部提示，不记录正文
    Body           []byte              // 仅适配器边界；不进入普通日志/持久化
    Stream         bool
}

type ResolvedRoute struct {
    RouteID, RouteVersionID string
    ProviderID, CredentialID string
    RequestedModel, ResolvedModel string
}

type Stream interface {
    Next(context.Context) (StreamEvent, error)
    Headers() map[string][]string
    Close() error
}
```

`ExecutionResult`、`StreamEvent`、`Capabilities`、`ResolvedRoute` 都是 Bablo 自有领域类型。CPA SDK 任何 `Auth`、`executor.Response`、`StreamResult`、config alias 都不能出现在 handler/service/repository 的签名中。

## 5. 推理请求流水线

1. 入口生成/接收稳定 `request_id`，过滤 hop-by-hop 和敏感 header；
2. 以 Bearer Key hash 查找 `api_key`，检查 active、expiry、IP、RPM/TPM/预算；
3. 解析 user/policy/entitlement，拒绝无权限 public model；
4. 读取 model + route 配置，建立请求开始时的 route/price snapshot；
5. 预算预检：低成本请求可直接门禁，长上下文/高价请求写 reservation；
6. route 精确匹配得到 candidates；
7. scheduler 硬过滤 enabled、支持能力、resource policy、cooldown、quota freshness、concurrency lease，再按确定性策略选择；
8. 通过 CPA adapter 执行非流式/流式；所有上游错误映射到 Bablo error class；
9. 发送响应。流式首个 payload 发出后不得透明切换 credential；客户端取消仍须释放租约并尽力取得 usage；
10. 生成一次 immutable UsageEvent，按 resolved provider/model/route/credential/price version 结算；
11. 在同一事务写 settlement/outbox，异步更新 stats、health、reconciliation；
12. 记录不含 secret/body 的日志、metrics 和 scheduler decision。

## 6. 事务边界与失败语义

- **鉴权/路由**：读事务或一致性快照；一个请求使用固定 route/price version；
- **预算预留**：钱包行锁 + 幂等 reservation key；拒绝时不触发上游请求；
- **结算**：`usage_events`、reservation release/charge ledger 和 outbox 在一个 PostgreSQL transaction 内提交；失败进入 retry/reconcile，不静默免费；
- **支付**：webhook 验签、订单状态变更、payment event、recharge ledger/outbox 在一个 transaction 内完成，provider event ID 唯一；
- **Scheduler lease**：Redis `SET NX PX` + owner token，finally/recovery 释放；Redis 故障时进入保守失败，而不是超卖并发；
- **CPA 失败**：首包前允许有限、可记录的 fallback；首包后只上报错误并结算已知 usage；不得重复真实执行请求；
- **进程崩溃**：数据库事实可恢复；outbox/settlement worker 重新 claim；Redis 状态过期后重建。

## 7. 调度第一版

候选顺序固定为：route target 的 priority 升序、配置权重、稳定 target ID。先硬过滤：policy/resource、enabled/revoked、模型能力、cooldown/health、quota missing/stale 策略、concurrency lease。P0 只实现无隐式随机的 round-robin/priority 选择；每次写候选、排除原因、score、selected、fallback。P1 再加入 weighted/fill-first/quota-aware 和有限 session affinity，且 affinity 永远不能绕过硬过滤。

## 8. 可观测性与隐私

结构化 JSON 日志字段至少包含 request_id、内部 user/key/route/provider/credential ID、status、error_class、latency；Prometheus labels 不使用 user_id、key_id、request_id 等高基数值。默认不记录 Prompt/响应、Authorization、Cookie、完整 Key、OAuth token、支付密钥。调试采样必须显式开启、脱敏、短 TTL、可审计。

## 9. 代码与部署落地顺序

执行顺序和验收标准以 `docs/implementation-status.md` 为唯一进度表，严格对应 `.omp/commands/bablo-next.md`：先 `bablo-bootstrap` 建立骨架，再 CPA 边界、数据层、认证/Key、目录/路由/调度、代理、Usage/账务，最后可观测性、安全、测试、压测、部署、CI、审计和 ship 门禁。
