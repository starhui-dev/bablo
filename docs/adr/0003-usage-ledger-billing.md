# ADR 0003：自建 UsageEvent 与 Wallet Ledger 作为计费事实

- 状态：Accepted
- 日期：2026-08-29
- 决策者：Bablo 技术负责人

## 背景

CPA 的 usage aggregation/queue 是异步、进程内的观测机制，不提供 Bablo 所需的不可变结算、价格版本、钱包并发和支付幂等保证。成功推理不能因为 queue 丢失而免费，重复 webhook/settle 不能重复到账或扣费。

## 决策

- 每个逻辑请求最终生成一次 immutable `UsageEvent`，至少记录 requested/resolved model、provider、route version、credential、price version、token breakdown、status/error、latency/TTFT、request/user/key 和 provenance/estimated 标记；
- 价格在请求开始/实际 resolved target 上形成 snapshot，历史 Usage 永远引用当时 `price_version_id`；
- 钱包用 append-only `wallet_ledger` 表达 reservation、release、usage_charge、recharge、refund、grant、adjustment；balance 只是事务维护的派生值；
- settlement key、ledger idempotency key、payment provider event ID 建立唯一约束；重复调用返回已存在结果；
- 预检/预留、usage finalize、reservation release/charge 和 transactional outbox 在 PostgreSQL transaction 内完成；wallet 行锁或等价原子约束保证并发不非法透支；
- streaming cancel/上游无 usage/迟到 usage 不覆盖旧事实：使用 estimated/reconcile/adjustment 新事件，并报警；
- CPA usage manager 只能提供 reconcile signal/观测，不直接入账。

## 后果

正面：账务可审计、可对账、可恢复，价格切换和并发语义明确。代价：需要处理流式终止、估算、重试、outbox 和 reconciliation；Usage 原始数据会增长，需要保留策略与 rollup。

## 不采用

1. 读取 CPA 内存统计直接扣余额：进程崩溃/无 plugin/异步丢失会免费或错账；
2. 用 dashboard 聚合值作为账本：不可追溯且无法证明并发正确性；
3. UPDATE 历史 ledger/Usage“修正”：破坏审计链，应追加 adjustment/reconciliation。

## 验收不变量

- 同一 request/settlement 重试只产生一个结算；
- 同一支付 event 只产生一次充值；
- 100+ 并发请求不会重复扣、免费或突破负余额政策；
- price version 切换不重写历史；
- stream cancel、进程崩溃和数据库短暂失败最终进入已结算或明确 reconcile-needed 状态。
