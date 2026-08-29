---
description: 执行最终生产上线门禁，修复阻塞项并生成 Release Checklist
---

执行最终 Production Readiness Review。目标不是“看起来差不多”，而是确定当前 commit 是否可上线。

## Gate

必须逐项验证并给证据：
- clean build；全部必要测试通过；race 无关键问题；
- migration 在空库和升级库验证；
- 一个 Key 多模型 E2E；
- CPA SDK 锁定且 compatibility suite 通过；
- Wallet/Usage/Payment 幂等和并发测试；
- 真实支付 Provider 若计划首发，已在官方 sandbox/真实测试环境完成端到端；否则不得把充值标为生产可用；
- Critical/High 安全问题为 0；
- secrets scan clean；
- health/ready/metrics/log 正常；
- backup + restore drill 已完成；
- rollback runbook 可执行；
- 监控与告警就绪；
- resource commercial policy 已配置；
- 文档/用户调用示例准确。

先修复可修的 blocker，再重新跑 gate。产出 `docs/release-readiness.md`，明确 `GO` 或 `NO-GO`。任何未验证真实条件必须导致对应能力 NO-GO，禁止凭感觉放行。
