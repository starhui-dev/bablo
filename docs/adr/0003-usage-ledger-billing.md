# ADR 0003：自建 UsageEvent 与 Wallet Ledger 作为计费事实

- 状态：Accepted（P0 Billing 与 Payment 内核已实现）
- 日期：2026-08-29
- 实现更新：2026-09-04
- 决策者：Bablo 技术负责人

## 背景

CPA 的 usage aggregation/queue 是异步、进程内的观测机制，不提供 Bablo 所需的不可变结算、价格版本、钱包并发和支付幂等保证。成功推理不能因为 queue 丢失而免费，重复 webhook/settle 不能重复到账或扣费。

## 决策

1. 每个逻辑请求最终生成一次 immutable `UsageEvent`，记录 request/user/key、requested/resolved model、provider/provider model、route version、credential、price version、wallet、token breakdown、status/error、latency/TTFT 和 provenance/estimated；
2. 只有 Pricing service 解析出的已发布且在生效区间内的 `price_version` 可创建非零 reservation。`unit_price` 的 P0 语义是“主货币单位/一个维度单位”，以 decimal string + PostgreSQL `numeric(30,12)` 表示；Go 端使用整数/`math/big`，所有维度汇总后一次向上取整到货币最小单位，禁止 float；
3. input/output 总量可能包含 cache/reasoning breakdown。存在专属价格时，已包含于总量的部分先从 base token 扣除再按专属价计费；breakdown 大于总量时视为 provider 的独立计数。缺少可选维度专属价时回退到 input/output 基础价，不将已观测 token 静默计为免费；
4. `wallet_ledger` 是追加式账本。`available_delta_minor` / `reserved_delta_minor` 是余额重建的权威变动，`wallets.available_balance_minor` / `reserved_balance_minor` 只是事务维护的投影；数据库 trigger 拒绝 ledger UPDATE/DELETE；
5. 在已完成 entitlement、route、scheduler 和 resolved target 价格解析后、执行上游请求前创建 `wallet_reservations`。reservation 固定 request、wallet、API Key、route/provider/credential、price version、预估 token 与金额；同一 `request_id` 的不同载荷返回冲突；
6. API Key daily/monthly budget 使用 API-key advisory transaction lock 串行化，消费额为周期内已结算 charge + 当前 active/pending reservation。随后锁 wallet 行并把 available 原子转入 reserved；余额不足或预算超限在上游执行前失败；
7. UsageEvent 提交后在独立 PostgreSQL transaction 内执行 settlement：实际金额消费 reserved，少于预留的差额追加 release，多于预留的差额从 available 扣除。余额不足时保留 reservation，写 `billing_settlements.status=pending`、错误分类与 transactional outbox，后续补款后可用相同 UsageEvent 幂等重试；
8. 上游未返回可验证 usage 时，不释放为免费请求：UsageEvent 标记 `estimated=true` / `reconcile_needed`，按已预留金额结算；迟到事实通过 `usage_reconciliations` 和新的 adjustment entry 修正，不 UPDATE 旧 Usage/Ledger；
9. recharge、refund、grant、bonus、adjustment、admin_adjustment、expiration 统一追加 ledger entry，并要求 wallet-scope idempotency key。管理员调账必须携带 operator 并在同一事务写 sanitized audit；
10. reservation、settlement、ledger、payment event 和 outbox 均在 PostgreSQL transaction 内提交。Provider API 调用不包在数据库事务内，使用稳定 idempotency key、短租约和 pending/retryable 状态恢复；Provider operation 固定 merchant identity 与 live/test mode，重试时不允许切换商户；
11. 退款调用前先追加 hold ledger 把 available 转入 reserved，验签 success event 消费 reserved，确定失败 event 释放，未知结果不提前释放。Provider 回调提供的 order/trade/refund/payment-intent/charge 标识只要本地已有对应值就必须全部一致，不能把多个标识当作互相替代的宽松匹配；
12. Provider 控制台或争议流程产生的外部退款/charge dispute 也必须进入 Bablo 账务：先以 `wallet_liabilities` 原子回收现有 available，不足部分设置 `financial_hold` 并阻止新 reservation；后续充值按 FIFO 回收 liability，独立 recovery worker 以租约重试 pending settlement。争议胜诉追加反向 ledger，败诉保留已回收事实；
13. 支付 webhook 只有在 Provider adapter 完成签名、时间、API version、merchant 和 live mode 校验后才持久化；无效签名不写 PaymentEvent。浏览器 success/cancel redirect 永远不能入账；
14. Redis、CPA usage manager、Provider browser redirect 和 Dashboard 聚合均不参与真钱事实判定。

## 实现映射

- `migrations/000011_billing_integrity.sql`：reservation、settlement、显式 ledger delta/balance snapshot、跨表 owner/price/usage consistency、append-only guard 和查询索引；
- `migrations/000012_payment_integrity.sql` 至 `000015_payment_merchant_mode.sql`：order/event/voucher 状态、退款 hold、Provider operation lease、merchant/live-mode identity、terminal constraints 和迁移时禁止猜测历史商户身份；
- `migrations/000016_payment_financial_recovery.sql`：pending settlement 恢复租约、wallet financial hold、`wallet_liabilities` 和不可变人工充值操作；
- `migrations/000017_payment_provider_recovery.sql`：PaymentIntent/Charge/Dispute 标识、外部退款事实、争议状态机及跨表账务约束；
- `migrations/000018_billing_liability_integrity.sql`：Provider financial reference 的 liability 全局唯一约束；
- `migrations/000019_quota_observation.sql`：quota snapshot 的 provider/model/observation key、metadata、幂等唯一约束、append-only guard 和可重建 probe state；quota 只提供资源观测，不参与 Usage/Ledger 事实计算。
- `internal/billing`：精确 Quote、Reserve、Settle、Release、Credit、payment refund hold/consume/release、liability open/recover/reverse、pending settlement recovery、GetWallet 与 RebuildBalance；
- `internal/payment`：Provider-neutral order/refund state machine、Stripe Checkout/webhook/refund/外部退款/争议 adapter、merchant/live-mode 绑定、fixture test provider、voucher/admin credit、operation/expiration/reconciliation worker；
- `internal/proxy`：强制配置 Billing coordinator，解析最大输出 token，先 reserve 再调用 CPA，UsageEvent 后 settle；
- `cmd/bablo`：装配 Billing、Payment、Webhook/管理/用户 HTTP 与恢复 worker；任何已启用支付能力都要求 PostgreSQL。

## 后果

正面：账务可审计、可对账、可从 ledger 恢复；预算、余额、外部退款和争议并发语义明确；无 usage、重试、结算不足或 Provider 控制台退款都不会静默免费。代价：需要 outbox 告警、真实币种/价格策略、商户级对账与支付通道验证；历史支付数据升级到 v15 前必须由运营从权威 Provider 回填 merchant/live mode，禁止自动猜测。

## 不采用

1. 读取 CPA 内存统计直接扣余额：进程崩溃、无 plugin 或异步丢失会免费或错账；
2. 用 Dashboard 聚合值作为账本：不可追溯且无法证明并发正确性；
3. UPDATE 历史 ledger/Usage“修正”：破坏审计链，应追加 adjustment/reconciliation；
4. 按请求 alias 或预估 provider 计费：必须绑定 resolved target 和实际 `price_version_id`；
5. 各 token 维度分别向上取整：会系统性多收；P0 只在总额转换为最小货币单位时取整一次。

## 验收不变量

- 同一 request/reservation/settlement/ledger idempotency key 重试只产生一份事实；
- 同一 payment order、Provider event、voucher redeem、admin credit、refund、external refund 或 dispute event 重试不重复到账/退款/追偿；失败与未知退款不会错误释放 held balance；
- 每个已认证 PaymentEvent 的全部非空 Provider 标识、merchant 和 live mode 必须与同一订单一致；同 event ID 异载荷重放被拒绝；
- 并发 reservation 不会使 available/reserved 为负或突破 daily/monthly budget；存在 pending settlement/open liability 时 `financial_hold` 阻止新消费；
- 每笔 ledger delta 可重建当前 available/reserved，且历史 entry 不可 UPDATE/DELETE；
- 非零 reservation 和 UsageEvent 必须绑定同一已发布 price version、wallet、request 和 owner；
- stream cancel、missing usage、结算余额不足和数据库短暂失败最终处于 settled 或明确 pending/reconcile 状态；充值与恢复 worker 最终清理 liability/pending settlement，未清理前保持 financial hold；
- price version 切换不重写历史，金额计算不使用 float。
