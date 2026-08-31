# ADR 0001：CPA SDK 仅通过 inference adapter 接入

- 状态：Accepted
- 日期：2026-08-29
> 当前状态：P0 Web Session、CSRF、RBAC、管理员 TOTP/恢复码、推理 API Key、上游 Credential AEAD/轮换、Route/Scheduler/Proxy 数据面已实现；真实上游、Usage/支付和生产安全门禁仍未放行。

## 背景

Bablo 需要复用 CLIProxyAPI 的 OAuth、executor、协议转换、streaming、tool/reasoning、credential refresh 等 inference 能力，但 CPA 迭代快，公开 wrapper 还存在 internal 类型 alias/leak。若业务层直接引用 CPA 类型，升级会把上游变更扩散到控制面、账务和 HTTP API，并破坏 Bablo 的产品语义。

## 决策

- 唯一 CPA import 边界为 `internal/inference/cpa/**`；
- Bablo 定义自己的 `inference.Engine`、`Request`、`ExecutionResult`、`Stream`、`Capabilities`、`ResolvedRoute`、`UpstreamError`；
- adapter 内部按精确锁定 tag 使用 CPA 的公开 `sdk/*` 包，当前基线为 `github.com/router-for-me/CLIProxyAPI/v7 v7.2.145`；
- 业务层不得 import CPA `internal/*`，不得把 CPA `Auth`、`executor.Response`、`StreamResult`、config alias、provider interface 透出到 handler/service/repository；
- adapter 负责 Build/Run/Shutdown、能力快照、协议/错误/stream/cancel/request ID/credential pin 映射，并在启动前拒绝 CPA config 中的 credential-bearing 字段和 `remote-management.secret-key`；CPA v7.2.145 无公开 readiness API，宿主 readyz 必须独立证明；CPA usage queue 只能是观测/reconcile 输入，不能是账本。
- Proxy 已按 request ID、Key entitlement、Route snapshot、Scheduler lease、CPA execution 顺序消费该边界；真实 Provider/OAuth/usage E2E 仍属于后续门禁。

## 后果

正面：业务模型稳定、CPA 可替换/升级、测试可用 fake engine、敏感类型集中审计。代价：需要维护一层映射和 CPA compatibility suite；部分 CPA 高级能力要等公开边界确认后才能启用。

## 实施约束

- CI 检查 CPA import 路径只出现在 `internal/inference/cpa`；
- `docs/upstream-compatibility.md` 记录 tag、commit、Go 版本、实际符号和漂移；
- 实现于 2026-08-30：`internal/scheduler` 已按上述边界落地；`migrations/000009_scheduler_integrity.sql` 记录 resolved route/provider/credential 并校验选择归属，`credentials.max_concurrency` 定义每个 Credential 的 TTL lease 槽位数。`internal/proxy` 已在每次请求中先调用 Route resolver、再调用 Scheduler 并执行 CPA；真实上游错误/配额反馈和 Usage 结算仍由后续阶段接入。
- 升级只改 adapter，除非新增 ADR 证明稳定边界确实不足。

## 不采用

1. 业务层直接调用 CPA core auth：泄漏 volatile API 和 internal 类型；
2. import CPA `internal/*`：不受公共兼容承诺，违反 Go internal 边界和项目规则；
3. 把 CPA 作为独立公网控制面：扩大攻击面，且无法满足 Bablo PostgreSQL source-of-truth。
