---
description: 实现用户控制台：统一 Key、多模型、用量、余额、充值与账单
---

实现普通用户控制台。

至少包含：
- Overview：余额、今日/本月消费、请求、Token、常用模型。
- API Keys：创建、只显示一次、复制、重命名、撤销、到期、模型权限、限额；强调一个 Key 可访问多个模型。
- Models：当前 Key/账号可访问模型、能力、公开价格、Base URL/调用示例。
- Usage：时间/Key/模型筛选、Token 与费用明细；不泄漏其他用户/内部 credential。
- Billing：Wallet ledger、充值订单、支付状态、退款/调整说明。
- Security：密码、Session、MFA（如普通用户开放）。

提供 OpenAI SDK/curl 等最小调用示例，示例只使用一个统一 Key。错误状态、空状态、移动端基本可用性要完成。补 E2E 测试。
