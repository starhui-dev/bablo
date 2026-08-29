---
name: cpa-sdk-integration
description: 集成或升级 CLIProxyAPI/CPA SDK 时使用；强调版本锁定、公开 sdk 边界、兼容测试和文档漂移处理
---
# CPA SDK Integration

## Source of truth

按优先级：锁定 tag 的源码与 `go.mod` > 对应 pkg.go.dev > 对应 examples > README/docs。文档引用旧 major 时不得照抄。

官方参考：
- https://github.com/router-for-me/CLIProxyAPI
- https://pkg.go.dev/github.com/router-for-me/CLIProxyAPI/v7
- https://github.com/router-for-me/CLIProxyAPI/tree/main/sdk
- https://github.com/router-for-me/CLIProxyAPI/blob/main/examples/custom-provider/main.go
- https://github.com/router-for-me/CLIProxyAPI/blob/main/docs/sdk-usage.md

## Rules

- 精确 pin tag。
- 只有 `internal/inference/cpa` 可 import CPA SDK；其他 Bablo 包只能依赖自有接口。
- NEVER import CPA `internal/*` 作为长期集成方案。
- Adapter 对外只返回项目领域类型。
- Upgrade 必须跑协议/流式/cancel/错误/failover/credential compatibility tests。
- 把 SDK tag、commit、Go version、验证 API 写入 `docs/upstream-compatibility.md`。
