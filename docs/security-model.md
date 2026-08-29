# Bablo 安全模型

> 目标：生产级多用户 AI Gateway 的控制面、推理面和账务安全基线
> 日期：2026-08-29
> 当前状态：设计已确定，代码尚未实现

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

### Web 管理面

- 登录使用 Argon2id，参数和版本写入 `password_params_version`，参数升级采用登录时 rehash；
- Session 使用 CSPRNG 高熵 token，数据库只存 hash；Cookie `HttpOnly`、生产 `Secure`、合理 `SameSite`、有限 TTL，支持单个/全部注销；
- 登录、MFA、密码变更和危险管理操作使用 request rate limit；失败锁定需有上限、恢复和管理员审计，不能造成永久 DoS；
- 管理员至少 TOTP MFA，绑定必须二次确认，恢复码 hash 化、单次使用、行锁/条件更新防重放；生产策略可强制全体管理员 MFA；
- 状态变更使用 CSRF token（SameSite 不是唯一防线）；CORS 只允许显式 origin；
- RBAC 至少 admin/user。UI 隐藏不是授权；每个 service/use-case 重新判断 actor、tenant/user scope、资源状态和危险操作权限。

没有可靠邮件基础设施时，不实现伪造的邮件重置；采用受审计的管理员重置或一次性恢复流程，并在发布清单标明限制。

### 推理面

- 只接受 `Authorization: Bearer`（兼容其他 header 前必须单独设计并测试）；
- Key 由 CSPRNG 生成，创建响应只返回一次明文；持久层 `secret_hash + prefix + metadata`；
- 每次请求检查 active/revoked/expired、owner/policy、model entitlement、IP、RPM/TPM 和预算；上下文只放内部 user/key ID，不放明文 Key；
- policy 采用默认拒绝；Key -> policy/entitlement -> model route，绝不使用单一 group/provider 绑定；
- 轮换的旧 Key 窗口若启用必须有明确 TTL、同时审计和撤销语义。

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