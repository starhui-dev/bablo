---
description: 锁定并接入 CPA SDK，建立唯一 CPA Adapter 与兼容测试
---

实现 CPA SDK 集成层。可通过 `$1` 指定要测试的 CPA tag；未指定时选择当前稳定版本，但必须先验证，不得直接 `@latest`。

## 调研与锁定

1. 核对官方仓库、pkg.go.dev、tag 源码、examples/custom-provider。
2. 记录：module major、Go 版本、锁定 tag、commit、公开 sdk 包、已验证接口。
3. 如果 `docs/sdk-usage.md` 与当前 tag 不一致，以锁定 tag 的公开源码/pkg.go.dev 为准，并在文档标注。

## 集成边界

- 只有 `internal/inference/cpa/**` 可 import CPA。
- 对业务暴露项目自己的 `InferenceEngine`、`Capability`、`ExecutionResult`、`StreamEvent`、`ResolvedRoute` 等领域类型。
- 不允许任何 CPA struct 泄漏到 handler/service/repository。
- 不 import CPA `internal/*`。

## 生命周期

实现：初始化、启动、ready、优雅停止、错误映射、版本信息、capability probe。优先使用公开 SDK；如某能力只能通过 CPA embedded HTTP 稳定暴露，则可在 `internal/inference/cpa` 内采用 loopback/Unix-socket 适配，但必须写 ADR，且对业务层保持同一接口。

## 验证

建立 CPA compatibility tests，至少验证：
- Service/Manager 能构造并优雅退出；
- 一个 mock/test provider 的非流式与流式路径；
- cancellation；
- upstream 4xx/429/5xx 错误分类；
- request id 传播；
- SDK 升级时的编译契约。

不要在此阶段做用户钱包或支付。完成后更新 `docs/upstream-compatibility.md` 和状态文档。
