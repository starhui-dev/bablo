---
description: 实现日志、Metrics、Tracing、健康检查、告警与隐私保留策略
---

实现生产可观测性。

## Logs

结构化 JSON；字段至少 request_id、user_id（内部 ID）、api_key_id、route/provider/credential 内部 ID、status、error_class、latency。绝不记录完整 key/token/cookie/支付密钥/Prompt 正文。

## Metrics

Prometheus-compatible 或当前项目已选标准：request rate/error/latency/TTFT、active streams、scheduler selections/fallback/cooldown、credential health/quota staleness、billing failures、payment webhook failures、DB/Redis pool、worker queue。

避免高基数 label：不要直接把 user_id/api_key_id/request_id 当 Prometheus label。

## Tracing

如采用 OpenTelemetry，建立 Bablo -> CPA adapter -> upstream 的 span；敏感 body 不入 span attributes。

## Health

- `/healthz`：进程活着，不做昂贵依赖检查。
- `/readyz`：必要 DB/Redis/CPA engine 就绪。
- worker readiness 单独可诊断。

给出告警规则/Runbook：持续 5xx、429 激增、结算失败、payment webhook 验签失败、quota stale、DB connection exhaustion。更新运维文档。
