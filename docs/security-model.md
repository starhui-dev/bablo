# Bablo 安全模型

> 目标：生产级多用户 AI Gateway 的控制面、推理面和账务安全基线
> 日期：2026-08-29
> 当前状态：P0 Web Session、CSRF、RBAC、管理员 TOTP/恢复码已实现；推理 Key、Credential 和支付安全仍按后续阶段落地

## 1. 资产与信任边界

| 资产 | 保护目标 | 边界 |
|---|---|---|
| Web Session、密码、MFA | 机密性、抗重放、可撤销 | 浏览器 -> 管理 API -> PostgreSQL |
| Bablo API Key | 机密性、最小权限、可撤销 | 客户端 -> 推理 API -> apikey/policy |
| OAuth refresh token / upstream API Key | 机密性、轮换、最小暴露 | 管理员输入 -> 加密存储 -> CPA adapter |
| Wallet/Ledger/Usage/Payment | 完整性、幂等、可审计 | API/webhook -> PostgreSQL transaction |
| Route/Price/Policy | 完整性、版本一致性 | admin service -> route/scheduler |
| Prompt/response | 默认机密性、最小留存 | 请求内存 -> CPA/upstream，不进普通日志 |
| Redis lease/rate state | 可用性、不可升级为事实 | 运行时协调；丢失可重建 |
| CPA management/control | 管理面隔离 | 仅 loopback/内网，不公网开放 |

攻击者模型包括：泄漏/伪造 API Key 的客户端、被盗 Web Session、越权普通用户、恶意/被攻陷管理员、伪造支付 webhook、恶意 Provider/URL 配置、日志/备份读取者、Redis/PostgreSQL 网络攻击者和供应链依赖漏洞。

## 2. 身份认证与授权

### Web 管理面（P0 已实现）

- 密码使用 Argon2id PHC 格式；当前参数版本 `argon2id-v1-m19456-t2-p1` 对应 19 MiB、2 iterations、1 lane、16-byte salt、32-byte key。`users.password_params_version` 记录策略版本，成功登录时按当前参数自动 rehash；密码长度为 12–1024 UTF-8 bytes；
- Session token 与 CSRF token 各由 CSPRNG 生成 32 bytes。浏览器只收到一次明文；PostgreSQL 只存 SHA-256 hash。Session 默认 TTL 12h，可配置范围 5m–7d；登录、MFA 成功均旋转 Session，密码变更/重置和 logout-all 撤销全部 Session；
- `bablo_session` Cookie 为 `HttpOnly`、`SameSite=Lax`、Path `/api/v1`，生产环境强制 `Secure`；`bablo_csrf` 为前端可读、Path `/`、同样有限 TTL。所有状态变更同时校验显式 `Origin`、CSRF Cookie、`X-CSRF-Token` 和 Session 内绑定 hash，SameSite 不是唯一防线；
- 登录和 MFA 使用有界单实例 fixed-window limiter：默认每个 email + source IP 5 分钟 8 次，窗口到期自动恢复，不写永久账号锁。P0 单实例可用；HA 前必须替换/补充 Redis 协调实现，仍不得把 Redis 当身份事实源；
- RBAC 至少 `admin`/`user`。授权在 auth service 内重新判断，不依赖 UI。生产 `BABLO_AUTH_REQUIRE_ADMIN_MFA` 不能关闭；管理员操作要求 admin role 且当前 Session 已完成 MFA。未绑定 MFA 的管理员只允许登录和进入绑定流程，不能执行管理员密码重置；
- TOTP 使用 30 秒、6 digits、HMAC-SHA1 兼容配置，允许 ±1 period，`last_totp_counter` 在行锁事务内前进以拒绝重放。绑定先写 pending factor，再用有效 TOTP 二次确认；成功后原子生成并 hash 存储 10 个 80-bit 恢复码、旋转 Session；恢复码通过条件 UPDATE 单次消费；
- TOTP secret 使用 AES-256-GCM，主密钥由 `BABLO_AUTH_ENCRYPTION_KEY` 以 32-byte base64 注入，ciphertext/nonce/key_version 写 PostgreSQL，AEAD AAD 绑定 factor ID、user ID 和 key version。当前只读取活动 key version；多版本解密和后台 re-encrypt 属于 credentials/security 阶段上线阻塞；
- `bablo auth create-admin --email ...` 与 `bablo auth reset-password --email ...` 是本地可信运维入口，密码从无回显终端或 stdin 读取，不进入 argv；重置在单事务内更新 hash、撤销全部 Session、写 audit。没有可靠邮件基础设施时不伪造邮件重置；
- 登录成功/失败、MFA 开始/启用/验证/恢复码重建、密码变更/重置和注销写 `audit_logs`；不记录密码、TOTP secret、恢复码、Cookie 或请求正文。

### 推理面（API Key P0）

- 只接受严格的单一 `Authorization: Bearer`（兼容其他 header 前必须单独设计并测试）；Key 格式为 `bablo_sk_` + 32-byte CSPRNG base64url；
- 创建和轮换响应只返回一次明文；持久层只保存完整 Key 的 SHA-256、短 prefix 和 metadata。256-bit 随机熵使离线猜测不可行，同时避免可逆 secret 存储；日志、audit、错误和 context 均不得含 raw key/hash；
- 身份中间件每次检查 active/revoked/expired、active owner 和 canonical CIDR IP allowlist，只信任直连 `RemoteAddr`，不信任客户端提供的 forwarded headers；上下文只放内部 user/key ID、prefix、secret version 和限额，授权阶段再次比对当前版本，使 rotate 提交前取得的陈旧 Principal 失效；
- 推理 handler 在解析 requested model 与 token 估算后必须调用授权服务：policy default deny，显式 deny 优先、allow 次之，一个 Key 可允许多个模型；随后执行 RPM/TPM 固定 UTC 分钟窗口门禁；
- PostgreSQL 是 Key、policy、entitlement、撤销和轮换事实源。配置 Redis 时 Lua 原子计数且错误 fail closed；未配置 Redis 只允许 P0 单实例使用有界进程内计数，不能作为 HA 方案；
- daily/monthly budget 阈值已保存并进入 Principal，但真实消费门禁必须等 Usage/Billing 提供可核验消费事实；在此前不得假装预算已经执行；
- P0 轮换原子替换同一 Key 的 hash，旧 Key 立即失效并写 audit；不提供难以证明撤销边界的双 Key 并行窗口。

## 3. Secret 与加密

- Credential secret 在应用层使用成熟 AEAD（AES-256-GCM 或等价方案），存 ciphertext、nonce、key_version、secret kind；
- 主密钥仅来自环境变量/secret manager，不进仓库、镜像、数据库普通字段或日志；
- 新写入使用当前 key version；后台分批 re-encrypt，保留失败重试和审计；轮换期间读取按 key_version 解密，轮换完成后可撤销旧 key；
- secret metadata 与业务 Credential 分离；API/UI 只返回类型、外部标识、更新时间、key version、健康，不回显完整 token；
- 备份、dump、crash dump、trace/span attributes、错误消息都按 secret 处理；定期 secret scan。

## 4. 路由、上游与 SSRF

- Provider endpoint、proxy URL、region、management URL 只允许配置层受控字段；解析后拒绝 loopback、link-local、RFC1918、云 metadata、Unix socket、非允许 scheme/端口，除非显式内部 allowlist；
- 禁止用户通过请求字段选择任意 URL、任意 proxy 或 CPA management；HTTP client 禁止跟随不受控跨协议重定向，重新校验每次 redirect；
- 统一 timeout、最大 body、连接池和响应 header allowlist；拒绝 CRLF/header injection；
- route preview 与真实执行复用同一 policy/entitlement 逻辑；route snapshot 固定后不受后台修改影响；
- Provider/resource type 明确区分 `official_api`、`enterprise_api`、`subscription`、`third_party`；`subscription` 默认 `commercial_allowed=false`，未作出业务/条款决定不得公开商业转售；
- OAuth/上游 401 不按普通 429 无限重试，credential 进入需要人工/刷新处理的状态。

## 5. 账务、支付与防重放

- PostgreSQL 是唯一事实源；UsageEvent、Wallet Ledger、PaymentEvent 追加式、不可变；
- 钱包预留/扣费在事务中锁定 wallet 行或使用已验证的原子更新；重复 request settle、worker retry、webhook retry 都由唯一幂等键收敛；
- 支付 webhook 必须按 provider 当前官方规范验签，并验证 merchant/app ID、订单号、金额、币种、状态和时间窗；provider event/trade ID 唯一；
- webhook 事务只更新订单、写 event、充值 ledger 和 outbox；客户端成功页不入账；
- 管理员调账只新增 adjustment/grant/refund entry，不修改历史 ledger；
- 没有真实 sandbox/商户凭据时，支付只能是 disabled、fixture 或 runbook 状态，对外标记 NO-GO。

## 6. 日志、隐私与数据最小化

默认日志字段：request_id、内部 user/key/route/provider/credential ID、status、error_class、latency、TTFT、版本。禁止完整 Key、Authorization、Cookie、OAuth token、支付密钥、Prompt、响应正文和上游敏感 body。Metrics 不将 user_id、key_id、request_id 作为 label。

Usage 只保存 token/长度/协议、模型、路由和状态元数据；Prompt/响应 debug capture 必须管理员显式开启、脱敏、短 TTL、访问审计并有开关。备份和导出同样执行字段最小化与访问控制。

## 7. 网络、容器与供应链

- PostgreSQL/Redis 只在私网/容器网络，禁止公网监听；TLS/反向代理由部署环境提供并强制生产配置；
- 管理 API 与推理 API 可使用不同 listener/ingress policy；CPA management 仅 loopback/内网；
- 容器非 root、只读文件系统（可行处）、最小 capability、无 Docker socket，secret 运行时注入；
- Go/frontend 依赖锁版本、许可证和漏洞扫描；CPA 更新只能通过 PR + compatibility suite，不自动合并到生产；镜像 tag 使用不可变版本和 git SHA，并生成 SBOM/摘要；
- SQL 全部参数化，输入校验、最大页大小、body/timeout 限制，数据库账号最小权限。

## 8. 审计与响应

以下动作必须写不可变 audit：登录失败/成功、MFA 变更、Key 创建/轮换/撤销、用户/RBAC 变更、Credential 写入/禁用/轮换、route/model/price 变更、钱包 adjustment/refund、支付 webhook 验签结果、敏感 debug capture、管理员导出。记录 actor、action、target、request ID、结果和脱敏前后摘要，不记录 secret/body。

上线前完成 API Key 泄漏、越权、CSRF/XSS、SSRF、wallet race、webhook forged/replay、日志泄密、CPA 暴露、Redis/PG 暴露和依赖漏洞演练。`Critical/High=0`、secret scan clean、备份恢复和回滚可执行，才允许进入 ship gate。