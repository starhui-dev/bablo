---
description: 实现 OpenAI-compatible 推理数据面并接入 CPA Adapter
---

实现公开推理数据面。先根据真实目标客户端确定最小协议，不要假装所有 Provider 协议完全一致。

## 最小接口

至少实现/验证：
- `GET /v1/models`
- `POST /v1/chat/completions`
- `POST /v1/responses`

如果项目需要 Claude Code/Anthropic 客户端，再加入 `/v1/messages` 兼容面；需要 Gemini 再按 CPA 支持能力增加。每增加一种协议都要有契约测试。

## 请求流水线

request id -> API Key auth -> entitlement -> budget precheck -> model route -> scheduler -> CPA adapter -> stream/non-stream response -> usage finalization -> settlement -> metrics/log.

- 禁止把完整请求体写普通日志。
- Streaming 必须正确处理客户端取消、上游断流、首包前错误、首包后错误。
- 保留必要的 upstream headers 但必须过滤敏感/header injection 风险。
- 错误映射保持 OpenAI-compatible 可读结构并带 request_id。
- 客户端取消后仍需在可获得 Usage 时完成结算；无法获得时标为 reconcile-needed，不能静默丢账。

建立 golden/contract tests，与一个可控 fake CPA provider 比较 JSON/SSE 行为。
