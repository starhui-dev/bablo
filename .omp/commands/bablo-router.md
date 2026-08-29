---
description: 实现 public model -> route target -> provider/pool 的确定性模型路由
---

实现模型路由层，让一个 API Key 通过请求里的 `model` 自动选择正确资源。

## 路由模型

支持：
- exact model match；
- 明确受控的 prefix/regex（若需要，优先 exact/prefix，避免任意 regex 带来的复杂性）；
- public model -> 多个 route target；
- target 包含 provider、upstream_model、credential_pool、priority、weight、enabled、商业策略。

路由先做 Key entitlement，再做模型/目标匹配，再把候选交给 Scheduler。

## 关键要求

- 同一个 public alias 可以落到多个 Provider，但 Usage 必须记录最终 resolved target。
- Route 配置修改要有版本/审计，正在处理的请求使用开始时快照。
- 无路由、路由被禁用、资源政策不允许时返回稳定、可诊断错误，不随机 fallback 到未知 Provider。
- 提供 dry-run/preview API：给定 user/key/model 显示候选 route，但不真正发请求。

用 DeepSeek/GLM/Kimi/MiMo/Codex/Grok 这类“多个协议可能同为 OpenAI compatible”的抽象测试数据验证：它们能依据 model/route 进入不同 pool，而不是因为协议同为 OpenAI 就混在一起。
