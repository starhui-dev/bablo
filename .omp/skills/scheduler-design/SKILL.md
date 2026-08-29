---
name: scheduler-design
description: 设计模型路由、资源池、配额感知调度或 Session Affinity 时使用；要求可解释和并发安全
---
# Scheduler Design

先硬过滤，后选择：

1. entitlement/resource policy
2. enabled/revoked
3. model support
4. cooldown/health
5. quota available/staleness
6. concurrency lease
7. strategy score/priority/weight
8. bounded session affinity adjustment

每次选择写 decision log：候选、排除理由、score/priority、selected、fallback。

Redis 中的 lease/affinity/cursor 都必须可重建并有 TTL。不能让 stale quota 伪装成最新值。任何随机策略必须可控 seed 或明确无需复现；生产默认优先确定性。
