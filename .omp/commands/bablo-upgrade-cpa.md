---
description: 安全升级 CPA SDK：差异审计、兼容测试、Canary 与回滚
---

将 CPA SDK 从当前锁定版本升级到 `$1`。如果未提供目标版本，只做候选版本评估，不修改生产依赖。

## 流程

1. 记录 old/new tag、Go version、module major。
2. 阅读 release/changelog 与 old..new diff，重点检查 `sdk/cliproxy`、`sdk/cliproxy/auth`、executor、handlers、translator、config、usage/session。
3. 禁止直接 import 新的 `internal/*` 来修编译。
4. 只修改 `internal/inference/cpa` 适配层；若业务层必须变化，先写 ADR 解释为何边界失效。
5. 跑完整 compatibility suite：Chat/Responses/Messages（项目启用的协议）、stream、tools、reasoning、cancel、429/401/5xx、session、credential refresh。
6. 跑 regression + race。
7. 更新 `docs/upstream-compatibility.md`。
8. 给出 canary 和 rollback 条件；没有验证环境时不得声称已生产验证。

如果目标 tag 引入无法接受的回归，应保持旧版本并形成阻塞报告，而不是硬改业务绕过。
