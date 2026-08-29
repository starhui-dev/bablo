---
description: 实现 PostgreSQL 核心数据模型、迁移、Repository 与事务边界
---

根据 `docs/data-model.md` 实现生产数据库基础。先核对现有 migration，不得重建已上线表。

## 核心实体至少覆盖

- users / roles 或等价 RBAC
- user_sessions / mfa 状态
- api_keys
- providers
- credentials 与 encrypted secret metadata
- models
- model_routes / route_targets
- credential_pools 或等价资源池
- quota_snapshots
- price_versions / model_prices
- usage_events
- wallets / wallet_ledger
- payment_orders / payment_events
- scheduler_decisions
- audit_logs

## 约束

- 主键统一使用项目确定的 UUIDv7/等价有序 ID 策略。
- 时间统一 UTC；展示层再转时区。
- 金额不得 float。
- API Key 只存 hash + prefix + metadata。
- secret ciphertext、nonce/key version 与业务字段分离。
- usage/payment/ledger 必须有明确 idempotency/unique key。
- 软删除只用于确实需要恢复的业务实体；账本、Usage、Audit 不做破坏式删除。
- 索引必须针对真实查询：user/time、model/time、provider/time、credential/time、request_id、order_no 等。

实现 repository/service transaction boundary；禁止 handler 自己拼 SQL。写 migration 测试：空库 up、连续升级、重复启动安全、关键约束测试。更新 ER 文档和状态。
