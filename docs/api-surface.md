# Bablo API Surface

> API 规划基线；实现时以 OpenAPI 为契约
> 日期：2026-08-31

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

- `stream=true` 使用 SSE，正确返回 `Content-Type`、终止事件和 request ID；JSON/SSE 响应均强制 `Cache-Control: no-store`，不接受上游覆盖；
- 首包前上游错误返回标准 JSON error；首包后错误发送可识别的 SSE error/终止事件，不能伪造完整成功；
- 客户端取消必须取消 context、释放 concurrency lease；若可取得 usage，仍生成 UsageEvent；无法取得时标为 `estimated + reconcile_needed` 并按 reservation 金额结算，不静默免费；
- 首个 payload 已发出后不做透明 fallback，所有尝试、UsageEvent 和 Billing settlement 状态进入 request trace。

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

### 用户 API Key / policy（P0 已实现）

- `GET /api/v1/me/api-keys`
- `POST /api/v1/me/api-keys`
- `PATCH /api/v1/me/api-keys/{id}`
- `POST /api/v1/me/api-keys/{id}/rotate`
- `POST /api/v1/me/api-keys/{id}/revoke`
- `GET /api/v1/me/models`（已由模型阶段实现为 `/api/v1/models`）
- `GET /api/v1/me/usage`（stats/user 阶段）
- `GET /api/v1/me/wallet`、`GET /api/v1/me/wallet/ledger`（Billing 领域服务已实现；HTTP surface 后置到 user/admin 阶段，当前未挂载）
- `GET /api/v1/me/payment-orders/{order_no}`（payment 阶段）

以上 Key 管理接口只接受现有 Web Session：所有方法要求 full session，POST/PATCH 继续要求精确 Origin、CSRF Cookie 与 `X-CSRF-Token`。用户查询和修改条件始终包含 session user ID；其他用户的 Key 返回 `not_found`。响应统一 `Cache-Control: no-store`。

创建请求：

```json
{
  "name": "development",
  "expires_at": "2026-12-31T23:59:59Z",
  "allowed_models": ["gpt-example-a", "gpt-example-b"],
  "ip_allowlist": ["203.0.113.0/24", "2001:db8::/32"],
  "rpm_limit": 60,
  "tpm_limit": 100000,
  "daily_budget_minor": 10000,
  "monthly_budget_minor": 200000
}
```

`allowed_models` 只接受 enabled/public/non-deleted catalog ID，可为空以建立默认拒绝且暂不可用的 Key。IP 可传 CIDR 或单 IP，服务端保存 canonical CIDR；空 allowlist 允许任意直连源。RPM/TPM 必须为正数，预算阈值不得为负。PATCH 支持同名字段；`expires_at`、RPM/TPM、daily/monthly budget 传 `null` 表示清除，`allowed_models: []` 清空授权，`ip_allowlist: []` 清空 IP 限制。未知字段、空 patch、过去到期时间和无效模型/IP/限额返回 400。

创建成功返回 201，轮换成功返回 200；只有这两个响应包含一次性 `secret`。普通 Key DTO 只含 `id/name/prefix/status/expires_at/allowed_models/ip_allowlist/limits/last_used_at/timestamps/secret_version`，从不含 `secret` 或 `secret_hash`。P0 rotate 原子替换同一记录，旧 Key 在事务提交后立即无效；revoke 幂等并立即无效。

稳定错误 code 包括 `invalid_api_key`（401）、`ip_not_allowed`/`model_not_allowed`（403）、`rate_limited`（429，`Retry-After: 60`）、`rate_limit_unavailable`（503）、`insufficient_funds`/`budget_exceeded`（402）、`not_found`（404）、`conflict`（409）和 `invalid_request`（400）。Key API 不提供 `group_id`/`provider_id` 绑定字段；多模型授权只通过 policy/entitlement 管理。daily/monthly budget 阈值由数据面 Billing 以已结算 charge + active/pending reservation 执行。

### 数据面 Billing（P0 已实现）

- 数据面顺序固定为 entitlement -> route snapshot -> scheduler selection -> resolved price snapshot -> wallet reservation -> CPA execution -> immutable UsageEvent -> wallet settlement；
- `max_output_tokens`、`max_completion_tokens`、`max_tokens` 中出现的正整数取最大值作为输出预估；显式 0、负数或非整数返回 `invalid_request`。均未提供时，非免费模型 P0 默认预留 4096 output tokens；超过服务上限或无法精确计价返回 `invalid_request`/`price_unavailable`，不会调用上游；
- 非零 reservation 只接受 active/retired 且当前 effective 的 price version，并绑定实际 provider model/route/provider/credential；daily/monthly budget 和余额不足在上游前分别返回 402 `budget_exceeded` / `insufficient_funds`；
- 正常完成按 UsageEvent 实际 amount settle；少于预留自动 release，多于预留补扣 available。补扣不足写 durable pending settlement/outbox，不修改历史 Usage/Ledger；
- 上游未返回完整 usage 时，UsageEvent 为 `estimated=true`、`reconcile_needed`，按 reservation 金额结算；迟到差异只能追加 reconciliation/adjustment；
- 金额响应和未来 Wallet API 只使用整数 `amount_minor` + 三字母 currency，不返回 float；`unit_price` 是主货币单位/一个 token 或 request，所有维度汇总后一次向上取整到最小货币单位。


### 管理资源

- `GET/POST/PATCH /api/v1/admin/users`
- `GET/POST/PATCH /api/v1/admin/roles`
- `GET /api/v1/models`：已登录用户可见的 enabled/public 模型与 canonical capabilities/aliases；推理面的 `/v1/models` 已由 Proxy 按 API Key entitlement 返回当前 Key 可访问模型，不泄漏其他租户或 Credential。
- `GET/POST /api/v1/admin/models`、`GET/PATCH /api/v1/admin/models/{id}`：模型目录、别名、visibility、billing class、能力和启停；
- `GET/POST /api/v1/admin/providers`、`GET/PATCH /api/v1/admin/providers/{id}`：Provider 资源类型、商业政策和启停；subscription 在 P0 强制 `commercial_allowed=false`；
- `GET/POST /api/v1/admin/provider-models`、`GET/PATCH /api/v1/admin/provider-models/{id}`：上游模型映射、协议、能力和审核状态；列表必须按 `provider_id` 限定；
- `POST /api/v1/admin/providers/{id}/reconcile`：提交一次完整发现快照；新增上游模型为 pending/disabled，消失只标记 discovery missing，不覆盖已批准业务配置；
- `GET/POST /api/v1/admin/prices`、`GET /api/v1/admin/prices/{id}`：创建 draft 价格版本并查询完整条目；scope 为 global/model/provider_model；
- `POST /api/v1/admin/prices/{id}/activate`、`POST /api/v1/admin/prices/{id}/retire`：发布/结束价格区间；发布后价格条目和版本身份不可修改；
- `GET/POST /api/v1/admin/credentials`：分页列出或创建 Provider Credential；创建 secret 只接受一次性请求正文，响应仅返回 descriptor，不返回值。
- `GET/PATCH /api/v1/admin/credentials/{id}`：查询或修改 region/proxy/非敏感 metadata/status；Provider、external stable ID、source kind 不可变。
- `POST /api/v1/admin/credentials/{id}/rotate`：按 `kind` 原子轮换 secret，旧版本只保留 ciphertext/history descriptor。
- `POST /api/v1/admin/credentials/{id}/reencrypt?kind=...`：按当前应用密钥重加密 active secret，保留版本历史；允许对 disabled/error Credential 执行，revoked 仅作为密钥迁移对象。
- `GET /api/v1/admin/credentials/{id}/health`：返回 last success/error class、cooldown 和 observed timestamp，不返回 secret。
- `GET/POST /api/v1/admin/credential-pools`、`POST/DELETE /api/v1/admin/credential-pools/{id}/members`：Provider-owned pool 与成员 priority/weight/enabled 管理；数据库拒绝跨 Provider 成员。
- `GET/POST /api/v1/admin/routes`：按 `model_id`、opaque cursor 分页查询或创建 Route；P0 只接受 `exact`，创建时同时提交至少一个启用 target。
- `GET/PATCH /api/v1/admin/routes/{id}`：查询 active route snapshot，或仅修改 Route metadata/enabled；已发布 version/target 不原地修改。
- `GET/POST /api/v1/admin/routes/{id}/versions`：查询 immutable version history，或关闭旧 active version 并原子发布新的 target snapshot。
- `GET /api/v1/admin/routes/preview?model={public_id_or_alias}`：管理员 dry-run；返回当前匹配的 route version 和所有 candidate target，不执行 scheduler、不触发 Credential 解密或上游请求。数据面仍必须先完成 API Key entitlement，再调用 Route resolver。
- `GET /api/v1/admin/usage`
- `GET /api/v1/admin/requests/{request_id}`
- `GET /api/v1/admin/scheduler/decisions`
- `GET /api/v1/admin/credentials/{id}/quota`
- `POST /api/v1/admin/wallets/{user_id}/adjustments`
- `GET /api/v1/admin/payment-orders`
- `POST /api/v1/admin/payment-orders/{order_no}/close`
- `POST /api/v1/admin/payment-orders/{order_no}/refund`
- `GET /api/v1/admin/audit-logs`
- `GET /api/v1/admin/system`


请求 alias 必须先由 model service 解析为 canonical public model ID，再执行 Key entitlement、route 和价格解析；alias 本身不成为独立计费维度。

模型/Provider/价格写操作均要求 Web Session、CSRF、admin RBAC 和生产 MFA；发现是信号，管理员批准/映射才会产生可路由的 provider model。价格金额使用 decimal string，`unit_price` 表示主货币单位/一个维度单位，缺少 input/output 或 request 必需维度时解析失败，不按 0 收费；可选 cache/reasoning 维度缺专属价格时使用基础 input/output 价格。

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
