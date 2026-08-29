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

### Session / identity

- `POST /api/v1/auth/login`
- `POST /api/v1/auth/logout`
- `POST /api/v1/auth/logout-all`
- `GET /api/v1/auth/session`
- `POST /api/v1/auth/password`
- `POST /api/v1/auth/mfa/totp/bind`
- `POST /api/v1/auth/mfa/totp/confirm`
- `POST /api/v1/auth/mfa/recovery/regenerate`

登录限速、Session fixation 防护、有限 TTL、Secure/HttpOnly/SameSite Cookie、CSRF token 由 auth 模块统一处理。没有邮件基础设施时不伪造密码重置邮件流程。

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
