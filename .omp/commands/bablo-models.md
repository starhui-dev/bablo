---
description: 实现 Provider/Model 目录、Alias、价格版本与能力声明
---

实现模型目录和 Provider 能力，不把“模型名字符串”散落在代码中。

## 模型目录

每个公开模型至少区分：
- public model id / alias
- canonical capability（chat/responses/messages、stream、tools、vision、reasoning 等）
- enabled/visibility
- billing class
- route policy

Provider/上游模型独立记录，允许多个 target 服务同一个 public model。

## 价格

- 价格必须版本化，有 `effective_from`，历史 Usage 绑定当时 price_version。
- 支持 input/output/cache read/cache write/reasoning/按次等实际需要维度。
- 金额使用安全 decimal/integer 表示。
- 价格缺失时生产策略必须明确：默认拒绝可计费用户请求，或明确标记 free/unpriced；绝不能静默按 0 收费。

## 模型同步

CPA 的 `/models`/registry 只能作为发现信号，不直接覆盖人工业务配置。建立 reconcile 流程：发现新增/消失模型 -> 待审核 -> 管理员启用。

实现管理 API、校验、测试和文档。
