---
description: 实现 Credential 配额窗口采集、健康探测、staleness 与 Scheduler 输入
---

实现上游 Credential 的 quota/health 观测层，用于订阅窗口与按量资源调度。

## 抽象

定义 provider-specific `QuotaProbe`/`HealthProbe` 接口，不让 Scheduler 解析供应商原始 JSON。

标准化 snapshot 至少包含：credential_id、window kind、used/remaining/limit（可得时）、reset_at、observed_at、source、confidence、error。

- 仅使用上游公开/CPA 已暴露且合法可用的信息；禁止通过规避限制的手段探测。
- 采集失败不能把旧值当新值，必须保留 observed_at/stale。
- Scheduler 对 stale/missing snapshot 采用保守且可配置策略。
- 429/401/403/5xx 进入标准 health/error class；401 不应被当普通 cooldown 无限重试。

实现 poll worker、指数退避+jitter、单 credential 防并发探测、管理 UI API。为 Codex/Claude/Gemini/Grok/通用 API 仅实现当前真实可支持的 Probe，不猜接口。
