---
description: 审计当前实现与目标架构的差距并直接修复高优先级问题
---

对当前仓库做一次全量实现审计。参考 `.omp/AGENTS.md`、`RULES.md`、`docs/implementation-status.md` 和 ADR，但以实际代码/数据库/测试为准。

检查：
- CPA 边界是否泄漏；
- 一个 Key 多模型是否真实 E2E 可用；
- Wallet/Usage/Payment 幂等与并发；
- route/scheduler 是否可解释；
- secret 是否安全；
- 统计是否来自同一事实数据；
- admin/user 权限；
- stream cancel/lease；
- migrations；
- observability；
- production deploy/backup/rollback；
- TODO/FIXME/临时 mock 是否进入关键路径。

先生成按 P0/P1/P2 排序的 gap list，然后在本次工作中直接修复能够安全完成的 P0/P1，并跑验证。不要只写审计报告。更新 status；剩余 P0 必须显式标成 release blocker。
