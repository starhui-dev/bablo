---
description: 执行真实压测与容量基线，定位瓶颈并给出上线容量参数
---

为网关建立可重复压测，不使用真实付费上游烧额度；用 fake/mock upstream 模拟可控流式与延迟。

## 场景

- 非流式小请求
- 长流式 SSE
- 混合模型路由
- 同一用户多 Key / 多用户
- Scheduler 多 credential contention
- Redis 限流
- Usage + Ledger 写入高并发
- 429/5xx fallback storm
- DB/Redis 短暂抖动

测量吞吐、p50/p95/p99 latency、TTFT、CPU、RSS、goroutines、DB pool、Redis ops、ledger commit latency、错误率。

必须验证无 goroutine/connection/lease 泄漏。根据结果调整 pool、worker、batch/rollup 参数，并把硬件规格、命令、数据写入 `docs/performance-baseline.md`。不要伪造“生产 QPS”；只报告本次实际环境结果。
