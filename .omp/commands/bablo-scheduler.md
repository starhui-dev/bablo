---
description: 实现可解释 Scheduler：优先级、权重、并发、配额、重置时间、健康与粘性
---

实现独立 Scheduler 领域模块。第一版必须正确、确定、可解释；不要先追求复杂数学。

## 两阶段算法

### Eligibility 硬过滤
排除：disabled/revoked、模型不支持、资源 policy 不允许、cooldown、quota exhausted、并发租约无法获取、地区/代理要求不满足。

### Selection
实现可插拔 strategy，至少：
- round-robin
- weighted-round-robin
- fill-first
- quota-aware（考虑 quota_remaining/reset_at）

session affinity 是选择修正而不是绕过硬过滤；绑定 Credential 不可用时必须安全 fallback，并记录原因。

## 分布式状态

- Redis 存并发 lease、短 TTL affinity、RR cursor/必要瞬态状态。
- 所有 lease 必须 TTL + finally/recovery 释放，避免进程崩溃永久占槽。
- Quota snapshot 来自异步 poller，带 observed_at/staleness；过旧时使用保守策略。

## Decision Log

每个请求记录候选、排除原因、score/priority、selected credential、fallback chain。避免泄漏 secret。

## 测试

做 deterministic tests、并发租约测试、429 cooldown、配额重置、stale quota、session affinity failover、多实例模拟。加入 property/fuzz test 验证“不可用 Credential 永不被选中”等不变量。
