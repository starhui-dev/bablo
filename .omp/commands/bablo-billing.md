---
description: 实现价格快照、预算预留、Wallet Ledger、结算与退款/调整
---

实现可真实扣费的钱包/计费模块。

## 核心原则

- Ledger 是事实，balance 是可事务维护的派生/缓存。
- 金额绝不 float。
- 每次推理开始做 budget precheck；对高成本/长上下文请求做合理 reservation，结束后 settle 差额。
- Usage 绑定请求开始时/实际路由对应的 price_version。
- 同一个 alias 落到不同 Provider/模型时按 resolved target 价格结算。

## Ledger 类型至少支持

recharge、usage_charge、reservation、reservation_release、refund、adjustment、grant/bonus、admin_adjustment。每条有唯一 reference/idempotency key、operator/source、余额前后或可重建信息。

## 并发

同一 Wallet 多并发请求必须保证不会透支到策略之外。使用 PostgreSQL transaction/row lock 或经过验证的原子方案，不得“先读余额再普通 UPDATE”。

## 异常

- 结算失败进入可重试状态并报警，不能让成功推理永久免费。
- 重试必须幂等。
- 管理员调账只能新增 adjustment，不修改历史 ledger。

写金额边界、并发 100+ 请求、重复 settle、退款、价格切换、负余额策略测试。
