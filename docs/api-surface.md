# Bablo API Surface

> API 规划基线；实现时以 OpenAPI 为契约
> 日期：2026-08-29

## 1. 分离两套认证面

- **推理面**：`/v1/*`，使用 `Authorization: Bearer <Bablo API Key>`；Key 只代表 user + policy，不绑定单一 Group/Provider；
- **管理面**：`/api/v1/*`，浏览器 Session Cookie + CSRF，RBAC 在 service/policy 层再次检查；管理员默认要求 MFA；
- **运维面**：`/healthz`、`/readyz`、`/metrics`。不暴露 secrets；CPA management endpoint 仅内部网络且默认关闭。

所有响应带 `request_id`（header `X-Request-ID`，服务端生成并校验格式）。普通日志不记录 Authorization、Cookie、完整 Key、Prompt 或响应正文。

## 2. P0 推理面

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/v1/models` | API Key | 返回当前 Key 可访问的 public models，不泄漏其他租户/credential |
| POST | `/v1/chat/completions` | API Key | OpenAI-compatible chat，支持明确声明的 stream/tools/reasoning 能力 |
| POST | `/v1/responses` | API Key | OpenAI Responses 最小兼容面；能力不足时稳定返回 unsupported capability |

请求中的 `model` 是 public model ID。服务端执行 entitlement -> route snapshot -> scheduler；不能按 alias 直接计算价格或随机落到未知 Provider。

### Streaming 契约

- `stream=true` 使用 SSE，正确返回 `Content-Type`、终止事件和 request ID；
- 首包前上游错误返回标准 JSON error；首包后错误发送可识别的 SSE error/终止事件，不能伪造完整成功；
- 客户端取消必须取消 context、释放 concurrency lease；若可取得 usage，仍生成 UsageEvent；无法取得时标为 reconcile-needed；
- 首个 payload 已发出后不做透明 fallback，所有尝试和最终状态进入 request trace。

## 3. P0 管理面

### Session / identity（已实现）

| 方法 | 路径 | Session 要求 | 说明 |
|---|---|---|---|
| POST | `/api/v1/auth/login` | 无；要求可信 `Origin` | email/password 登录；成功设置 Session/CSRF Cookie；已绑定 MFA 时返回 `mfa_required=true` 的 partial Session |
| GET | `/api/v1/auth/session` | 有效 Session | 返回 user ID、email、roles、expiry、MFA enabled/required，不返回 token |
| POST | `/api/v1/auth/mfa/verify` | partial Session + CSRF | 接受 6 位 TOTP 或单次恢复码；成功旋转为 MFA-verified Session |
| POST | `/api/v1/auth/logout` | 任意有效 Session + CSRF | 撤销当前 Session 并清理 Cookie |
| POST | `/api/v1/auth/logout-all` | full Session + CSRF | 撤销当前用户全部 Session |
| POST | `/api/v1/auth/password` | full Session + CSRF | 校验当前密码，更新 Argon2id hash，撤销全部 Session |
| POST | `/api/v1/auth/mfa/totp/bind` | full、尚未启用 MFA 的 Session + CSRF | 返回一次性 `secret` 与 `provisioning_url`，数据库仅保存 AEAD ciphertext |
| POST | `/api/v1/auth/mfa/totp/confirm` | pending factor 的 full Session + CSRF | 二次 TOTP 确认、启用 factor、返回一次性恢复码并旋转 Session |
| POST | `/api/v1/auth/mfa/recovery/regenerate` | MFA-verified Session + CSRF | 原子作废旧恢复码并返回新码一次 |
| POST | `/api/v1/admin/users/{user_id}/password` | admin + MFA-verified Session + CSRF | 管理员重置密码并撤销目标用户全部 Session |

登录请求示例：

```json
{
  "email": "user@example.com",
  "password": "user-supplied password"
}
```

成功响应中的 `session` 只含公开身份状态；Session/CSRF 明文仅在 Cookie 中。`bablo_session` 为 `HttpOnly`、生产 `Secure`、Path `/api/v1`；`bablo_csrf` Path `/`，前端在状态变更时复制到 `X-CSRF-Token`。所有 mutation 还要求 `Origin` 精确匹配 `BABLO_WEB_ORIGIN`。请求正文上限 32 KiB，未知 JSON 字段被拒绝。

认证错误使用 `authentication_error` + 稳定 code：`invalid_credentials`、`invalid_session`、`csrf_failed`、`mfa_required`、`invalid_mfa_code`、`permission_denied`、`rate_limited`、`conflict`。限速响应为 429 并带 `Retry-After`。

没有邮件基础设施时不提供伪造的邮件重置 API。可信本机运维使用：

```text
bablo auth create-admin --email user@example.com
bablo auth reset-password --email user@example.com
```

密码从无回显终端或 stdin 读取，不进入命令行参数；操作更新 audit，密码重置撤销全部 Session。

### 用户 API Key / policy

- `GET /api/v1/me`
- `GET /api/v1/me/api-keys`
- `POST /api/v1/me/api-keys`：明文只在创建响应返回一次；
- `PATCH /api/v1/me/api-keys/{id}`：名称、到期和限额；
- `POST /api/v1/me/api-keys/{id}/rotate`
- `POST /api/v1/me/api-keys/{id}/revoke`
- `GET /api/v1/me/models`
- `GET /api/v1/me/usage`
- `GET /api/v1/me/wallet`
- `GET /api/v1/me/wallet/ledger`
- `GET /api/v1/me/payment-orders/{order_no}`

Key API 不提供 `group_id`/`provider_id` 绑定字段；多模型授权通过 policy/entitlement 管理。

### 管理资源

- `GET/POST/PATCH /api/v1/admin/users`
- `GET/POST/PATCH /api/v1/admin/roles`
- `GET/POST/PATCH /api/v1/admin/models`
- `GET/POST/PATCH /api/v1/admin/providers`
- `GET/POST/PATCH /api/v1/admin/credentials`
- `POST /api/v1/admin/credentials/{id}/rotate-key`
- `GET/POST/PATCH /api/v1/admin/credential-pools`
- `GET/POST/PATCH /api/v1/admin/routes`
- `POST /api/v1/admin/routes/preview`
- `GET /api/v1/admin/routes/{id}/versions`
- `GET/POST /api/v1/admin/prices`
- `GET /api/v1/admin/usage`
- `GET /api/v1/admin/requests/{request_id}`
- `GET /api/v1/admin/scheduler/decisions`
- `GET /api/v1/admin/credentials/{id}/quota`
- `GET /api/v1/admin/credentials/{id}/health`
- `POST /api/v1/admin/wallets/{user_id}/adjustments`
- `GET /api/v1/admin/payment-orders`
- `POST /api/v1/admin/payment-orders/{order_no}/close`
- `POST /api/v1/admin/payment-orders/{order_no}/refund`
- `GET /api/v1/admin/audit-logs`
- `GET /api/v1/admin/system`

所有管理变更执行影响范围预览、危险操作二次确认（服务端 nonce/再认证）、审计写入。Credential endpoint 只返回类型、标识、key version、健康和更新时间，不回显 secret。

## 4. 支付面（按 Provider 启用）

- `POST /api/v1/me/payment-orders`：创建订单并返回待支付信息；
- `GET /api/v1/me/payment-orders`、`GET .../{order_no}`：查询状态；
- `POST /webhooks/{provider}`：仅接收 provider webhook，验签/订单号/金额/币种/状态/时间窗后入库；
- `POST /api/v1/admin/payment-orders/{order_no}/refund`：权限、原订单状态和 provider 结果校验。

客户端“支付成功”页面永远不写充值账本。未配置并验证真实 Provider 时，订单 API 可保持 disabled 或仅供 fixture/sandbox，不能宣称充值生产可用。

## 5. 通用错误与分页

```json
{
  "error": {
    "type": "invalid_request|authentication_error|permission_denied|not_found|rate_limit|upstream_error|billing_error|internal_error",
    "code": "stable_machine_code",
    "message": "safe human-readable message",
    "request_id": "req_01..."
  }
}
```

不返回内部堆栈、OAuth/token、SQL、上游完整 body。列表接口统一 cursor/limit、最大 limit、稳定排序和过滤；管理员查询必须强制作用域和时间范围，避免无界扫描。

## 6. 版本与兼容策略

- 管理 API 版本在 `/api/v1`，推理协议沿 `/v1`；破坏性变化进入 v2；
- 将 OpenAPI schema、JSON/SSE golden tests 与客户端示例同一变更更新；
- `/v1/messages`、Gemini、WebSocket、批处理、embeddings 等不是 P0 默认承诺；每增加协议必须先确认真实客户端/CPA capability，再增加独立契约测试；
- API response 只出现 Bablo 领域语义，不出现 CPA `Auth`、`executor.Response` 或 SDK config 类型。
