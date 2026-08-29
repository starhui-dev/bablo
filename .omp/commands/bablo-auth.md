---
description: 实现生产级用户登录、Session、RBAC、管理员 MFA 与审计
---

实现管理面/用户面的 Web 身份认证，不要与推理 API Key 混为一套认证。

## 要求

- 用户名/邮箱按项目文档决定，密码使用当前推荐的 Argon2id 参数并记录参数版本，提供可升级 rehash 机制。
- Session 使用高熵随机 token；数据库仅存 hash；Cookie 必须 HttpOnly、Secure（生产）、SameSite 合理设置、有限 TTL，并支持主动注销/全部注销。
- 状态变更 Web 请求实施 CSRF 防护。
- RBAC 至少 admin/user；授权检查在 service/policy 层集中实现。
- 管理员必须实现 MFA：至少 TOTP，含绑定、验证、恢复码、重放防护；生产可配置强制启用。
- 登录限速、失败锁定策略不得造成容易被滥用的永久 DoS。
- 密码重置若没有邮件基础设施，不伪造流程；先实现管理员安全重置或一次性恢复流程并标明上线限制。
- 所有管理员对用户、余额、凭证、价格、路由的修改写 audit log。

补齐单测/集成测试，覆盖 Session fixation、CSRF、权限越权、MFA 恢复码单次使用、注销。更新安全文档。
