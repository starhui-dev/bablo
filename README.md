# Bablo

Bablo 是面向多用户、多 API Key、多模型的生产级 AI Gateway：自研控制面，使用隔离的 CPA SDK adapter 作为 inference engine。当前已完成架构规划、基础工程、身份与目录控制面、路由调度、推理 Proxy、Usage/Billing/Payment 内核和 quota 观测层；真实上游、支付和发布门禁仍未放行。


## 已确定的架构边界

- 一个 API Key 通过 `policy/entitlement -> model route` 访问多个模型，不绑定单一 Group 或 Provider；
- PostgreSQL 是用户、授权、路由、价格、Usage、钱包、支付和审计的唯一业务事实源；
- Redis 只保存限流、并发租约、短期 affinity/cursor 等可重建运行时状态；
- CPA 只允许存在于 `internal/inference/cpa`，业务层使用 Bablo 自有领域类型；
- 计费只依赖 immutable `UsageEvent` 和 append-only `Wallet Ledger`；
- Scheduler 先硬过滤，再使用确定性、可解释的选择策略并写入 Decision Log；
- 原始 Prompt 和响应正文默认不持久化。

## CPA 基线

当前核验的稳定版本为：

```text
github.com/router-for-me/CLIProxyAPI/v7 v7.2.149
```

该版本的 module 要求 Go 1.26.0。CPA 版本、公开 SDK 包、源码核验结果和 `docs/sdk-usage.md` 漂移记录见 [`docs/upstream-compatibility.md`](docs/upstream-compatibility.md)。

## 文档

- [产品范围与分期](docs/product-scope.md)
- [总体架构](docs/architecture.md)
- [数据模型](docs/data-model.md)
- [API Surface](docs/api-surface.md)
- [安全模型](docs/security-model.md)
- [CPA 兼容性](docs/upstream-compatibility.md)
- [实施状态与验收顺序](docs/implementation-status.md)
- [ADR：CPA SDK 边界](docs/adr/0001-cpa-sdk-boundary.md)
- [ADR：PostgreSQL 事实源](docs/adr/0002-postgres-source-of-truth.md)
- [ADR：Usage/Ledger 计费](docs/adr/0003-usage-ledger-billing.md)
- [ADR：模型路由与 Scheduler](docs/adr/0004-model-routing-and-scheduler.md)
- [ADR：Web Session 认证](docs/adr/0005-web-session-authentication.md)

项目按 `docs/implementation-status.md` 中的阶段顺序推进。Quota 已实现 PostgreSQL immutable snapshot、被动响应头观测、受控 probe/health worker、Redis TTL credential lease、staleness 计算和管理员查询 API；统计聚合是下一阶段。真实 Provider/OAuth/支付、备份恢复和回滚验证仍属于发布门禁。

下一阶段：`/bablo-stats`。

## 安全与上线

没有真实外部凭据时不伪造 OAuth、支付或生产 E2E 结果。支付 Provider、外部 Credential、TLS/域名、备份恢复和回滚验证必须在发布门禁中提供真实证据；未验证能力保持 NO-GO。

## License

[MIT License](LICENSE)
