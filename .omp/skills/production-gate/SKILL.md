---
name: production-gate
description: 准备部署、发布或声称“可上线”时使用；以测试、恢复演练、安全和外部依赖实证作为放行条件
---
# Production Gate

“可编译”不是可上线。必须有证据：

- 关键单测/集成/E2E/race 通过；
- migration 空库和升级库通过；
- CPA compatibility suite 对锁定 tag 通过；
- 一个 Key 多模型 E2E；
- wallet/payment/usage 幂等与并发通过；
- Critical/High security findings = 0；
- secret scan clean；
- health/readiness/metrics/log 可用；
- backup restore drill 实际成功；
- rollback runbook 可执行；
- 计划首发的真实支付通道已在官方 sandbox/真实环境 E2E；
- 上游资源的商业使用策略明确。

任何没有实际验证的外部条件必须明确 NO-GO 对应能力。
