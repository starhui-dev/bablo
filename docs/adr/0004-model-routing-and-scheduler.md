# ADR 0004：模型路由与确定性可解释调度

- 状态：Accepted
- 日期：2026-08-29
- 决策者：Bablo 技术负责人

## 背景

同一个 public model alias 可能由多个 Provider、upstream model 和 Credential Pool 服务。Key 不能绑定单一 Group；路由越权、不可重现的随机选择和 stale quota 都会造成错误计费、不可排障和资源超卖。

## 决策

请求顺序固定为：

```text
API Key -> policy/entitlement -> public model -> route version snapshot -> candidate targets -> scheduler -> CPA adapter
```

Scheduler 两阶段执行：

1. **硬过滤**：policy/resource commercial policy、target/credential enabled、revoked/disabled、model capability、cooldown/health、quota available/staleness policy、地区/代理要求、concurrency lease；
2. **选择**：P0 使用 priority + 稳定 round-robin（稳定 target ID 作为 tie-breaker），不使用隐式随机；每次输出 selected/fallback/reason。P1 再加入 weighted-round-robin、fill-first、quota-aware 和有限 session affinity。

每次决策写 `scheduler_decisions`：候选集、排除原因、priority/weight/score、selected credential、fallback chain、strategy version、request/attempt。session affinity 只能修正排序，不能绕过硬过滤；绑定 credential 不可用时安全 fallback 并记录原因。

Redis 只存带 TTL 的 concurrency lease、affinity、RR cursor；quota snapshot 来自带 `observed_at`/confidence 的 poller，stale/missing 按保守策略处理。所有 lease 使用 owner token finally/recovery 释放。

## 后果

正面：选择可解释、可测试、可复现；先保证正确性再优化利用率。代价：P0 可能不能充分利用 quota/成本差异；Decision Log 和 snapshot 写入有存储成本。

## 不采用

1. Key -> Group/Provider：阻止一 Key 多模型，耦合控制面和上游；
2. 先做 quota-aware/复杂随机优化：quota stale 和不可重放会放大错误；
3. 只返回最终 Provider、不记录排除原因：无法排障、审计和验证调度不变量。

## 验收不变量

- revoked/disabled/不支持模型/cooldown/quota 不可用/无租约 credential 永不被选；
- 同一输入、配置版本和 seed（P0 无随机）得到同一选择；
- route/price 版本在请求期间固定；
- 并发 lease TTL 后可恢复，不永久占槽；
- 一个 Key 连续调用多个 public model 进入各自正确 route/pool；
- 429/401/5xx 分类、cooldown、fallback 与 Decision Log 一致。
