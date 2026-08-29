---
description: 规划 Bablo 生产级 AI Gateway 的架构、范围、ADR 与实施顺序
---

你现在是本项目的技术负责人。基于当前仓库实际状态，为 **Bablo**（自研控制面 + CPA SDK inference engine）的生产级多用户 AI Gateway 完成可执行架构规划。附加约束：$ARGUMENTS

## 必做

1. 先审计仓库：已有代码、语言、目录、数据库、前端、CI、部署文件、未提交修改。不得覆盖已有有效工作。
2. 核对当前 CPA：
   - 官方仓库当前 major、`go.mod` Go 版本；
   - pkg.go.dev 的公开 `sdk/*` 包；
   - 当前稳定 tag / release；
   - `docs/sdk-usage.md` 与实际源码是否版本漂移。
   记录真实 URL、tag、日期。不得凭记忆写 SDK API。
3. 产出并落盘：
   - `docs/product-scope.md`
   - `docs/architecture.md`
   - `docs/data-model.md`
   - `docs/api-surface.md`
   - `docs/security-model.md`
   - `docs/upstream-compatibility.md`
   - `docs/implementation-status.md`
   - `docs/adr/0001-cpa-sdk-boundary.md`
   - `docs/adr/0002-postgres-source-of-truth.md`
   - `docs/adr/0003-usage-ledger-billing.md`
   - `docs/adr/0004-model-routing-and-scheduler.md`
4. 明确模块边界：user/auth、apikey、model、provider、credential、route、scheduler、usage、pricing、wallet、payment、stats、audit、`inference/cpa` adapter。
5. 明确最小生产版本和后续增强，禁止把所有高级特性挤进第一阶段。
6. 给出数据库核心实体关系和关键唯一约束/幂等键，但此命令暂不创建业务表。
7. 给出完整实施顺序与每阶段验收标准；顺序应与本提示词包命令对应。

## 关键决策

- API Key 不绑定单一 Group；Key -> policy/entitlement -> model route。
- CPA 只存在于 `internal/inference/cpa` 适配器边界；公共产品和业务类型统一使用 Bablo 领域语义。
- Billing 只依赖自己的 UsageEvent/Ledger，不依赖 CPA 内存统计作为最终账。
- Scheduler 先实现简单、确定性、可解释策略，再做 quota-aware 高级策略。
- 第一阶段可单实例上线，但数据模型和 Redis 协调接口不得堵死未来 HA。
- 原始 Prompt/响应正文默认不持久化。

## 完成时

实际创建/更新上述文档，检查相互一致性。最后输出：当前仓库状态、已确定决策、仍需要外部凭据/业务决定的阻塞项、下一条建议命令。不要只在聊天中给方案而不落盘。
