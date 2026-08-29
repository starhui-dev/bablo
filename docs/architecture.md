# Bablo 总体架构

> 版本：架构规划基线
> 日期：2026-08-29
> 相关 ADR：`docs/adr/0001-cpa-sdk-boundary.md`、`docs/adr/0002-postgres-source-of-truth.md`、`docs/adr/0003-usage-ledger-billing.md`、`docs/adr/0004-model-routing-and-scheduler.md`

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
- Redis：API Key 限流、预算快速门禁、credential concurrency lease、短 TTL affinity/cursor；丢失后可重建；
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
| `auth` | 登录、Session、密码、MFA、RBAC/CSRF policy | session principal、authorization decision | 把 Web Session 当推理 API Key |
| `apikey` | Key 生成/hash、撤销、过期、轮换、限额 | key principal、policy ID | 保存明文 Key、绑定单一 group/provider |
| `model` | public model、能力、visibility、billing class | model capability、provider model | 散落模型字符串、直接覆盖人工目录 |
| `provider` | Provider 元数据、资源政策 | provider ID、resource type | 处理 OAuth secret 明文 |
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

## 4. 稳定领域接口

以下是边界形状，不是对 CPA API 的猜测；实际 CPA 适配以 `docs/upstream-compatibility.md` 锁定 tag 源码为准：

```go
type InferenceEngine interface {
    Execute(context.Context, InferenceRequest) (ExecutionResult, error)
    ExecuteStream(context.Context, InferenceRequest) (Stream, error)
    Capabilities(context.Context) (Capabilities, error)
    Shutdown(context.Context) error
}

type InferenceRequest struct {
    RequestID       string
    ResolvedModel   string
    ProviderID      string
    CredentialID    string
    SourceFormat    string
    ResponseFormat  string
    Headers         map[string]string // 已过滤敏感头
    Body            []byte             // 仅适配器边界；不进入普通日志/持久化
    Stream          bool
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
