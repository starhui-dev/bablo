# ADR 0003：自建 UsageEvent 与 Wallet Ledger 作为计费事实

- 状态：Accepted（P0 Billing 已实现）
- 日期：2026-08-29
- 实现更新：2026-08-31
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
10. reservation、settlement、ledger 和 billing outbox 均在 PostgreSQL 事务内提交。Redis、CPA usage manager 和 Dashboard 聚合均不参与真钱事实判定。

## 实现映射

- `migrations/000011_billing_integrity.sql`：reservation、settlement、显式 ledger delta/balance snapshot、跨表 owner/price/usage consistency、append-only guard 和查询索引；
- `internal/billing`：精确 Quote、Reserve、Settle、Release、Credit、GetWallet、RebuildBalance；
- `internal/proxy`：强制配置 Billing coordinator，解析最大输出 token，先 reserve 再调用 CPA，UsageEvent 后 settle；
- `cmd/bablo`：生产数据面装配 Billing repository/service；
- 钱包用户/管理员 HTTP 查询与调账接口仍按 `bablo-admin` / `bablo-user` 阶段实现；支付订单和 webhook 归 `bablo-payment`，不在本 ADR 中伪造 Provider。

## 后果

正面：账务可审计、可对账、可从 ledger 恢复；预算和余额并发语义明确；无 usage、重试和结算不足都不会静默免费。代价：需要 settlement retry/reconciliation worker、账本保留策略、真实币种/价格策略和支付通道验证；估算规则变更属于财务兼容变更，必须版本化并回归历史样本。

## 不采用

1. 读取 CPA 内存统计直接扣余额：进程崩溃、无 plugin 或异步丢失会免费或错账；
2. 用 Dashboard 聚合值作为账本：不可追溯且无法证明并发正确性；
3. UPDATE 历史 ledger/Usage“修正”：破坏审计链，应追加 adjustment/reconciliation；
4. 按请求 alias 或预估 provider 计费：必须绑定 resolved target 和实际 `price_version_id`；
5. 各 token 维度分别向上取整：会系统性多收；P0 只在总额转换为最小货币单位时取整一次。

## 验收不变量

- 同一 request/reservation/settlement/ledger idempotency key 重试只产生一份事实；
- 并发 reservation 不会使 available/reserved 为负或突破 daily/monthly budget；
- 每笔 ledger delta 可重建当前 available/reserved，且历史 entry 不可 UPDATE/DELETE；
- 非零 reservation 和 UsageEvent 必须绑定同一已发布 price version、wallet、request 和 owner；
- stream cancel、missing usage、结算余额不足和数据库短暂失败最终处于 settled 或明确 pending/reconcile 状态；
- price version 切换不重写历史，金额计算不使用 float。
