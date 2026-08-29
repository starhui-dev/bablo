---
description: 将现有 Sub2API + CPA 体系安全双跑迁移到 Bablo，并完成余额、Key、路由和 Usage 对账
---

为当前现网从 **Sub2API + CPA** 迁移到 **Bablo + CPA SDK** 设计并实际实现可回滚迁移。附加信息：$ARGUMENTS

## 第一原则

- 不猜旧系统数据库 schema、API 或余额语义；先读取真实版本、配置、导出能力和数据库结构。
- 不直接在生产数据库上做不可逆试验。
- 迁移必须可暂停、可重跑、可对账、可回滚。
- 迁移期间只能有一个明确的财务主账本；双写必须有 idempotency key 和 reconciliation。

## 必做盘点

记录并确认：

- 当前 Sub2API 版本、数据库、Redis、用户数量、API Key 数量；
- 用户余额/赠送余额/套餐/充值订单的真实语义；
- Group、Model、Account、Platform/Provider、倍率与价格配置；
- 当前 CPA 版本、Credential/OAuth 账号与 Sub2API 到 CPA 的上游关系；
- 需要保留的请求/Usage 历史范围；
- 客户端当前 Base URL、Key 发放方式和必须保持的兼容接口。

不得因为名称相似就假设 Bablo 表字段和旧表一一对应。

## 迁移设计

至少设计：

1. **Identity mapping**：旧 user/key -> Bablo user/key；是否保留旧 Key 必须通过安全评估。
2. **Balance opening**：将旧系统最终对账余额作为 Bablo opening ledger entry，并保存来源快照/审计证据。
3. **Model/route mapping**：旧 Group/Account 转成 Bablo Model Route / Provider / Credential Pool，而不是复制“Key -> Group”模型。
4. **Credential migration**：CPA OAuth/Secret 不得明文落日志；能继续由 CPA SDK/安全存储读取时避免不必要的明文导出。
5. **Usage history**：历史 Usage 与新的实时 UsageEvent 分表/标记来源，禁止重复扣费。
6. **Payment migration**：未完成订单、退款、回调幂等键必须有明确处理方案。

## 双跑阶段

实现可控的：

```text
Client cohort A -> old gateway
Client cohort B -> Bablo
```

必要时支持 shadow/compare，但绝不能对同一真实付费请求重复向上游执行。

对账至少覆盖：

- 请求数；
- 输入/输出/cache token；
- 用户消费；
- 模型/Provider 分布；
- 错误率；
- 余额变化；
- Scheduler route 结果；
- p50/p95 latency 与 TTFT。

给每一项定义允许误差和阻塞阈值。

## Cutover

产出并实际维护：

- `docs/migration/source-inventory.md`
- `docs/migration/mapping.md`
- `docs/migration/reconciliation.md`
- `docs/migration/cutover-runbook.md`
- `docs/migration/rollback-runbook.md`

切换前创建数据库/配置备份并实际验证恢复流程。切换后保留旧系统只读/可回滚窗口，达到既定稳定期后再停机。

完成时输出已验证事实、尚未迁移的数据、对账差异和是否具备 cutover 条件；没有真实验证不得宣称迁移完成。
