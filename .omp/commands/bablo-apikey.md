---
description: 实现一个 Key 多模型的 API Key、Policy、限额与鉴权
---

实现推理 API Key 系统，彻底避免“一个 Key 只能对应一个 Group”。

## 数据模型

API Key 至少具备：owner、name、prefix、secret_hash、status、expires_at、last_used_at、policy、可选 IP allowlist、RPM/TPM、日/月消费上限。

- Key 使用 CSPRNG 生成足够高熵 secret，只在创建时返回一次明文。
- DB 只存安全 hash 与短 prefix，不能可逆恢复完整 Key。
- 一个 Key 可访问多个模型；模型权限用 allow/deny entitlement 或 policy 表达。
- Key 不保存单一 `group_id`/`provider_id` 作为路由事实。
- 支持 revoke、rotate、新旧短暂并行窗口（如项目需要）。

## 鉴权

- 统一支持主要兼容入口需要的 Authorization Bearer；是否兼容其他 header 必须有明确文档。
- Redis 实现多实例可用的限速/预算快速门禁；数据库仍是长期事实来源。
- 鉴权结果传递稳定的 user_id/api_key_id，不把明文 Key 放 context/log。

## 测试

覆盖：错误 Key、撤销、过期、模型无权限、IP 限制、并发限速、Key 轮换、一个 Key 连续访问多个不同模型。更新 API 文档与状态。
