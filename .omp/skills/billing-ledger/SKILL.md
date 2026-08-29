---
name: billing-ledger
description: 设计或审查 Usage、钱包、价格、充值、订单和结算时使用；确保幂等、不可变账本、并发一致性
---
# Billing & Ledger

- 金额 NEVER 使用 float。
- UsageEvent 和 Ledger 是事实，Dashboard 不是事实。
- 每次结算绑定实际 resolved provider/model/route/credential 和 price_version。
- 所有入账/扣账必须有稳定 idempotency key。
- Wallet balance 是可缓存派生值，必须能从 ledger/reconciliation 恢复。
- Payment webhook 必须验签、金额核对、防重放；客户端成功页不能入账。
- 管理员调账用 adjustment entry，不 UPDATE 历史 ledger。
- Streaming/client cancel/迟到 usage 必须有明确 settlement/reconcile 状态。
- 使用数据库事务/锁证明并发下不会重复到账或非法透支。
