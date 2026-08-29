---
description: 实现可靠 UsageEvent 捕获、流式结算事实、幂等与对账
---

实现自己的 Usage 事实层，不能依赖 CPA usage queue 作为唯一来源。

## UsageEvent

设计 append-only/immutable event，至少包含 request/user/key/model/route/provider/credential、token breakdown、status/error、latency/TTFT、started/finished、price_version、source/provenance。

- request 完成结算必须有唯一幂等键。
- Streaming 可使用临时 execution state，最终只生成一次 settle event；必要时允许 adjustment event，不能改写旧事件。
- 明确“上游没返回 usage”策略：可使用可验证 tokenizer 估算并标注 `estimated=true`，或进入 reconcile queue；不能伪装成精确数字。
- CPA usage queue 可作为 reconcile signal：发现差异则记录 reconciliation，不直接覆盖账本。

## Outbox

关键 Usage/结算事件与数据库事务配合使用 transactional outbox 或等价可靠机制，避免“请求成功但事件丢失”。

## 隐私

原始 Prompt/响应默认不存；只存长度/token/metadata。Debug capture 必须脱敏、有 TTL、管理员显式开启。

实现重复回调、进程崩溃恢复、stream cancel、无 usage、迟到 adjustment 的测试。
