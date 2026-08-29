---
description: 实现上游 Credential、安全存储、资源分类、健康状态与生命周期
---

实现上游 Provider Credential 管理。

## 安全存储

- OAuth refresh token、API Key 等 secret 必须应用层 AEAD 加密（如 AES-256-GCM/等价成熟方案）。
- master key 从环境/secret store 注入；数据库只存 ciphertext、nonce、key_version 和非敏感 metadata。
- 设计 key rotation：新写使用当前 key version，后台可重加密旧记录。
- UI 永远不回显完整 secret；仅显示类型、标识、更新时间、健康状态。

## 资源分类

至少支持 `official_api`、`enterprise_api`、`subscription`、`third_party` 分类及 `commercial_allowed`/policy。默认不得把 subscription 自动开放给商业用户。

## CPA 同步

根据锁定 CPA SDK 的公开能力实现 CredentialSource/TokenClientProvider 或等价适配；若 CPA 仍要求 runtime auth artifact，则从 PostgreSQL 生成临时/运行时状态，不反向把文件当主数据源。

## 生命周期

支持 enabled/disabled/revoked、proxy/region metadata、health、last_success、last_error_class、cooldown、credential pool membership。不得把 OAuth token 写日志。

写加密、轮换、并发读取、CPA reconcile 测试。更新 upstream compatibility。
