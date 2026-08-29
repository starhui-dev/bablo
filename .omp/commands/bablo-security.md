---
description: 执行生产安全加固、威胁建模、依赖/Secret/越权/支付审计并修复
---

对当前项目执行一次真实安全审计并修复发现，不只是写 checklist。

## Threat model

覆盖：API Key 泄漏、Credential DB 泄漏、管理员账号接管、CSRF/XSS、SQL injection、SSRF/proxy abuse、模型路由越权、Wallet race、Webhook forged/replay、日志泄密、CPA Management 暴露、Redis/Postgres 暴露、供应链依赖。

## 检查

- `govulncheck` 或当前官方 Go 漏洞工具；前端依赖 audit。
- secret scan；检查 git tracked files。
- 所有 SQL 参数化。
- 所有 URL/proxy/admin 输入防 SSRF/内部网访问绕过。
- API Key endpoint 权限隔离。
- Credential encryption/key rotation。
- Cookie/CSRF/CORS/Trusted proxy 设置。
- rate limit 与登录防爆破。
- 支付 webhook 验签/幂等。
- Docker 非 root、只读 FS（可行处）、最小权限。
- CPA Management remote access 关闭或仅内部。

建立 `docs/security-review.md`，按 Critical/High/Medium/Low 记录证据、修复、剩余风险。Critical/High 未关闭不得进入 `bablo-ship`。
