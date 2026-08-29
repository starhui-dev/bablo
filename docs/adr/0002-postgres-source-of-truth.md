# ADR 0002：PostgreSQL 作为业务唯一事实源

- 状态：Accepted
- 日期：2026-08-29
- 决策者：Bablo 技术负责人

## 背景

Bablo 的用户、Key、policy、模型目录、route、credential metadata、价格、Usage、钱包、支付和审计需要跨请求、跨实例、可恢复且可审计。CPA config/auth 文件、进程内状态和 Redis 都不适合作为多用户业务主数据源。

## 决策

- PostgreSQL 保存所有业务事实和状态机：身份、授权、资源目录、route/price version、UsageEvent、Wallet Ledger、Payment、Scheduler Decision、Audit、outbox；
- Redis 只保存可重建的限流计数、预算快速门禁、concurrency lease、短 TTL session affinity/RR cursor 和 worker 协调状态；
- CPA 所需 runtime config/auth artifact 从 PostgreSQL 主状态生成；不把 CPA 文件或其内存统计反向当真相；
- 业务变更通过 repository/service transaction，handler 不直接拼 SQL；
- 关键事实使用 append-only/immutable 记录和唯一幂等键，统计是从 Usage/Ledger 派生的 rollup；
- 先支持单实例，但表结构、租约、outbox 和唯一约束不阻塞未来 HA。

## 后果

正面：恢复、审计、对账和跨实例一致性清晰；Redis 丢失不会改变财务。代价：需要设计索引、事务、outbox/retry、备份和 migration；高吞吐统计需先用 PostgreSQL rollup 证明瓶颈再演进。

## 关键约束

- DB migration 可重复、可升级、可回滚到兼容应用版本；
- 余额是 ledger 的事务维护派生值，必须可以重建；
- cache miss/stale、Redis outage、worker retry 都不能绕过 DB 权限或产生重复账；
- 备份包含 PostgreSQL 事实，恢复演练必须实际通过；Redis 仅按重建流程恢复。

## 不采用

1. CPA config.yaml/auth files 作为可写主库：绕过控制面和审计；
2. Redis 作为钱包/Usage 主账：丢失、过期或双写会造成错账；
3. 引入 Kafka/ClickHouse/Nacos 作为先决依赖：当前无真实性能证据，增加运维和一致性面。

## 当前实现

- 迁移采用精确锁定的 Goose `v3.27.3` SQL-first provider，配合 PostgreSQL session-level advisory lock；迁移文件位于 `migrations/`，已应用版本不得原地修改。
- `cmd/bablo-migrate`/Makefile 显式执行 up 或单步 down；应用启动只连接并 Ping PostgreSQL，不自动修改 schema。
- `internal/data.Store.WithTx` 统一 repository 事务边界，连接池由 pgx/v5 管理，数据库会话固定 `timezone=UTC`。
