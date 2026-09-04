# Bablo 实施状态

> 最后更新：2026-09-04
> 本次工作：完成 `bablo-quota` 的 provider quota/health 观测层、PostgreSQL immutable snapshot、Redis TTL credential lease、退避与 staleness、Scheduler 可选 freshness 输入、管理员 quota 查询 API 和请求路径被动响应头接线；修正 Scheduler 集成 fixture 未按 resolved provider/model 写入快照的问题；下一阶段进入 `bablo-stats`。

## 1. 仓库审计结果

| 项目 | 观察结果 | 证据/影响 |
|---|---|---|
| 根目录 | `.omp/`、`docs/`、Go/Vue bootstrap 文件 | 保留既有提示词与规划文档，新增实现文件 |
| 后端 | 已建立 Go bootstrap、CPA adapter、data layer、`internal/auth`、`internal/apikey`、`internal/model`、`internal/provider`、`internal/pricing`、`internal/credential`、`internal/route`、`internal/scheduler`、`internal/proxy`、`internal/usage`、`internal/billing`、`internal/payment`、`internal/quota` 与共享 `internal/audit`；`cmd/bablo` 已接线用户模型目录、管理员 catalog/Credential/quota、Route、Scheduler、CPA runtime reconcile、Usage、Wallet Billing 和 Payment 数据面 | Web Session 只保护管理面；admin catalog 强制 RBAC/MFA/CSRF；CPA 仍只在 adapter 边界 import |
| 前端 | Vue 登录页已接通 Session/MFA 登录、CSRF header、路由守卫和退出；模型/Key/Credential/Quota/Wallet 管理 UI 留到后续前端阶段 | 当前目录 HTTP API 已可供后续 UI 使用，Dashboard/404 仍为业务壳 |
| 数据库 | 已落地 `000001`–`000019` migrations；`000019_quota_observation.sql` 增加 quota snapshot provider/model/observation key/metadata、幂等唯一约束、append-only trigger 和 rebuildable probe state | `cmd/bablo-migrate` 显式执行 up/down；应用启动不自动改 schema；quota 空 schema up、latest down、重放和 PostgreSQL transaction 测试已验证 |
| 文档 | 架构规划、ADR、README、LICENSE、CPA compatibility 证据均已存在 | API/data/security/architecture/status 已同步 Billing 实际契约与后置 HTTP surface |
| Git | 当前工作目录未检测到 `.git` 元数据，不能独立报告分支/未提交 diff | 未执行破坏性覆盖；保留既有文件并增量落盘 |
| CPA 本地使用 | `go.mod` 精确 pin `v7.2.149`；adapter Build/Run/Shutdown、Manager Execute/Stream、Credential runtime 注册和 quota/health response observation 已接线 | 真实 Provider/OAuth E2E 仍缺外部凭据 |

## 2. 已落盘的规划

- [x] `docs/product-scope.md`：目标、P0/P1/P2、非目标和上线门禁；
- [x] `docs/architecture.md`：模块边界、请求流水线、单实例/HA、事务与失败语义；
- [x] `docs/data-model.md`：ER、实体、唯一约束、幂等键、账务不变量；
- [x] `docs/api-surface.md`：管理面、推理面、支付面、错误和 streaming 契约；
- [x] `docs/security-model.md`：威胁边界、密钥、认证、SSRF、支付、隐私、部署；
- [x] `docs/upstream-compatibility.md`：CPA tag/commit/Go/package、漂移和 adapter 规则；
- [x] 五份 ADR：CPA boundary、PostgreSQL source-of-truth、Usage/Ledger billing、routing/scheduler、Web Session authentication；
- [x] 本状态文件。

## 3. 已确定的硬决策

1. 产品语义固定为 Bablo，CPA 不成为公共产品名；
2. 只有 `internal/inference/cpa` import CPA；已精确 pin `github.com/router-for-me/CLIProxyAPI/v7 v7.2.149`；
3. CPA module major 为 v7，tag `go 1.26.0`；main 当前与 tag 相同但不作为未审计浮动依赖；
4. API Key -> policy/entitlement -> model route，一个 Key 可访问多个模型；
5. PostgreSQL 是业务唯一事实源，Redis 只存可重建运行时状态；
6. UsageEvent + Wallet Ledger 是唯一计费事实；CPA usage queue 不入账；
7. route snapshot 和 price version 按请求固定；scheduler 先硬过滤、再确定性选择、每次写 Decision Log；
8. 原始 Prompt/响应默认不持久化；subscription 与 official/enterprise/third_party 资源政策分离且默认不商业开放；
9. P0 单实例、邀请制/预充值或管理员授信；支付 Provider 未完成官方 sandbox/真实 E2E 前为 NO-GO。
10. Web Session 与推理 API Key 完全分离；生产管理员操作必须 MFA，Session/CSRF 只存 hash，TOTP secret 使用版本化 AES-256-GCM ciphertext。
11. API Key 固定为 `bablo_sk_` + 32-byte CSPRNG base64url；数据库只保存完整 Key SHA-256、展示 prefix 和 metadata；P0 rotate 原子替换且旧 secret 立即失效，不提供双 Key 并行窗口。
12. Billing `unit_price` 固定为主货币单位/一个维度单位的 decimal；Go 使用整数/`math/big` 汇总后一次换算最小货币单位，禁止 float。非零 reservation/Usage 必须绑定同一已发布 price version、wallet、request 和 resolved route。

## 4. 上游 CPA 核验摘要

观察日期 2026-09-04：官方稳定 release `v7.2.149`，tag/main commit `2a6b87aca083a5bf498ac1f68a1b636c500d7aaa`，发布于 2026-09-03 13:30:35Z；module `github.com/router-for-me/CLIProxyAPI/v7`，要求 `go 1.26.0`。Bablo 已精确 pin 该版本并完成本地 compile/test/race/vet/build；adapter 只 import public `sdk/auth`、`sdk/config`、`sdk/cliproxy`、`sdk/cliproxy/auth`、`sdk/cliproxy/executor`、`sdk/translator`。v7.2.149 的 `sdk/cliproxy/auth/quota_signals.go` 明确只支持 Claude/Codex 被动 quota signals；上游 `docs/sdk-usage.md` 仍保留 v6/internal/config/option/stream 漂移，详见 `docs/upstream-compatibility.md`。

## 5. 完整实施顺序与阶段验收

严格遵循 `.omp/commands/bablo-next.md` 的顺序。每阶段完成代码、测试和必要文档后才勾选；没有实现时不能标记完成。

| 序 | 命令/阶段 | 进入条件 | 最低验收标准 |
|---:|---|---|---|
| 1 | `bablo-plan` | 完成 | 范围、ADR、数据/API/安全、CPA 核验一致 |
| 2 | `bablo-bootstrap` | 完成 | Go/Vue 骨架、配置、日志、优雅退出、health/ready、Makefile/lockfile、无真实 secret |
| 3 | `bablo-cpa` | 完成（真实上游 E2E 后置） | pin/checksum、边界 adapter、fake provider non-stream/stream/cancel/error/shutdown/race |
| 4 | `bablo-data` | 完成 | migration 空库 up/升级/重复启动；核心约束、append-only 防护、repository 事务测试 |
| 5 | `bablo-auth` | 完成 | Argon2id 登录/rehash、Session hash/TTL/rotation/注销、Origin+CSRF、RBAC、管理员 TOTP/recovery、审计和本地 reset 均有测试 |
| 6 | `bablo-apikey` | 完成 | 一次性明文/SHA-256 hash、owner/CSRF、revoked/expired/IP/RPM/TPM、原子 rotate/revoke、一 Key 多模型和 Redis 并发 E2E |
| 7 | `bablo-models` | 完成 | public/upstream model、alias、capability、visibility、price version、Provider discovery/review、缺价拒绝和管理 API |
| 8 | `bablo-credentials` | provider/model policy | 完成：AEAD secret/key rotation、状态/health/pool metadata；不泄漏 token |
| 9 | `bablo-router` | models/credentials/policy | 完成：P0 exact 多 target、immutable version snapshot、preview、正确解析 provider model/pool target |
| 10 | `bablo-scheduler` | 完成 | target/member 硬过滤、RR/weighted/fill/quota 策略、TTL lease/cursor/affinity、Decision Log、并发/race/fuzz |
| 11 | `bablo-proxy` | 完成（真实 Provider/OAuth E2E 后置） | `/v1/models`、Chat、Responses；JSON/SSE、cancel、首包前后错误、request ID、header allowlist、Scheduler lease/health feedback |
| 12 | `bablo-usage` | proxy execution facts | 完成：immutable UsageEvent、settlement key、stream cancel/no-usage/reconcile/outbox 测试 |
| 13 | `bablo-billing` | 完成 | exact decimal quote、published price、reservation/charge/release、daily/monthly budget、pending retry、ledger rebuild/immutable、并发与重复 settle 均有 PostgreSQL 测试 |
| 14 | `bablo-payment` | payment business decision + provider docs | order state machine、验签 fixture、金额/币种/防重放/幂等；无真实凭据则 NO-GO |
| 15 | `bablo-quota` | provider-specific legal probe | 已完成：Claude/Codex CPA public response-header observation、immutable snapshot/state、staleness、指数退避+jitter、429/401/403/5xx health feedback、Redis TTL lease、admin quota view；Gemini/Grok/未知 Provider 不猜测接口 |
| 16 | `bablo-stats` | Usage/Ledger/outbox | 运营概览、趋势、受控维度日/小时 rollup、raw 对账、Usage trace、Scheduler/支付统计；不得自建计费公式 |
| 17 | `bablo-admin` | 管理 API | 管理用户/资源/route/price/ledger/audit；危险操作确认和权限复核 |
| 18 | `bablo-user` | 用户 API | Overview、Key、多模型、Usage、Wallet/Billing、空/错/移动端状态和 E2E |
| 19 | `bablo-observability` | 主要请求路径 | JSON logs、metrics、health、告警/runbook；无敏感正文和高基数 labels |
| 20 | `bablo-security` | 全部首发路径 | threat model 实审；依赖/secret scan；Critical/High=0；SSRF/CSRF/越权/支付修复 |
| 21 | `bablo-tests` | 功能冻结 | unit/integration/contract/E2E/race/fuzz；核心不变量全部通过 |
| 22 | `bablo-loadtest` | tests green + fake upstream | 小请求/流式/竞争/429/DB-Redis 抖动基线；无 goroutine/connection/lease 泄漏 |
| 23 | `bablo-deploy` | 容量和镜像参数 | 非 root 镜像、私网依赖、显式 migration、health rolling、实际 restore drill、rollback |
| 24 | `bablo-ci` | 部署资产 | PR CI 含 Go/frontend/migration/CPA/race/scan/Docker；immutable release/SBOM |
| 25 | `bablo-audit` | 代码与部署齐备 | P0/P1 gap list；修复可修 blocker；剩余 P0 明确 release blocker |
| 26 | `bablo-ship` | 前序证据齐全 | release gate 逐项有证据；支付/外部依赖未验证能力明确 NO-GO；GO/NO-GO |

## 6. 当前阻塞项与外部决定

### 必须由环境/业务提供

- Go module 与 CPA tag 编译契约已验证；CI 的固定 Go 1.26/1.27 toolchain matrix 仍待 `bablo-ci` 阶段建立；
- CPA SDK 没有公开 readiness probe；Bablo 必须在后续 wiring 中基于 adapter/依赖状态实现 readyz，而不是伪造 SDK readiness；
- 生产 PostgreSQL、Redis、TLS/域名、镜像 registry、secret manager、备份存储和恢复环境；生产现在强制 `BABLO_REDIS_URL`，Redis 故障对登录/MFA/API Key 运行时门禁 fail closed；
- Web 登录/MFA 的 trusted-proxy 解析已实现，但部署方仍必须提供直连反向代理 CIDR；推理 API Key IP allowlist 继续只读取 `RemoteAddr`，在数据面单独完成可信代理接线前不得信任客户端 forwarded header；
- CPA OAuth/Provider Credential、代理/地区和合法资源政策；
- Stripe 已选为首个 self-service adapter；是否首发仍需业务决定，并需 Connect account/merchant、test/live API key、endpoint signing secret、HTTPS return URL、公开 webhook endpoint 和官方 test-mode E2E；
- 生产计费币种、最终价格表、赠送/退款/负余额政策和缺失 usage 的估算业务规则仍需业务签字；P0 技术默认已明确为 ISO-style 最小货币单位、无负余额、缺失 usage 按 reservation 估算结算，改变规则必须版本化；
- 管理员 MFA 已固定为生产强制；仍需决定普通用户是否强制 MFA、邀请/自注册策略、邮件或外部 IdP、数据 retention/合规和首发协议范围；
- 目标用户客户端（是否必须 `/v1/messages`、Gemini 等）。

缺少上述信息不影响已完成的本地控制面、Route、Scheduler、Usage、Billing 与 Payment 内核实现和测试，但阻塞真实上游/Stripe E2E、数据面代理后客户端 IP allowlist、邮件自助恢复、备份恢复演练和最终生产 GO；不得伪造凭据或验证结果。

## 7. 下一阶段

```text
/bablo-stats
```

Quota 阶段已完成。当前 Scheduler quota policy 默认关闭；只有在 staging 观察到合法且足够稳定的 Provider signal 后，才通过配置启用 freshness/exhaustion 过滤。没有真实 Provider/OAuth 凭据时，quota-aware 生产能力仍保持 NO-GO。

## 8. Bootstrap 验收与验证证据

- `go test ./...` 通过；`go vet ./...` 通过；`go build -trimpath -o bin/bablo ./cmd/bablo` 通过；
- `pnpm install` 完成锁文件生成；首次安装的 esbuild 脚本按 pnpm 11 审批后重建成功；
- `pnpm lint`、`pnpm typecheck`、`pnpm test`（2 tests）、`pnpm build` 全部通过；
- 后端实际启动 smoke：`/healthz` 返回 200；`/readyz` 在 DB/Redis/Inference 未初始化时返回 503 和明确 checks；`/metrics` 返回 Prometheus 文本；
- 前端实际浏览器 smoke 验证 Dashboard、`/login` 和未知路径 404，页面标题、导航、空状态和错误页面可见；
- `git diff --check` 通过；当前数据阶段文件尚未提交，保留既有 CPA/Bootstrap 工作不覆盖。
- Bootstrap 阶段未创建业务表或真实 secret；CPA 依赖与 compatibility suite 已在本阶段落盘。

## 9. CPA Adapter 验收与验证证据

- `go.mod` 精确 pin `github.com/router-for-me/CLIProxyAPI/v7 v7.2.149`，`go.sum` 含 module/content checksum；
- `internal/inference` 定义 Bablo Request/ResolvedRoute/ExecutionResult/Stream/Capabilities/UpstreamError，不向业务层暴露 CPA 类型；
- `internal/inference/cpa` 实现 config load、Builder/Service lifecycle、Manager execute/stream、source/response format、request ID、provider/pinned credential、safe error 和 stream headers/cancel 映射；
- fake provider 覆盖 non-stream、stream、401/429/5xx、caller cancel、stream close、request ID、credential pin、capability copy、service build/shutdown；
- `go test -count=1 ./...` 通过；`go test -race -count=1 ./internal/inference/cpa` 通过；`go vet ./...` 通过；`go build -trimpath -o bin/bablo ./cmd/bablo` 通过；
- CPA import 搜索确认源码 import 只存在于 `internal/inference/cpa/**`；真实 Provider/OAuth、Chat/Responses provider golden、上游首包后错误/failover 仍待真实兼容环境验证，不能把 fake contract 当作真实上游兼容。

## 10. Data Layer 验收与验证证据

- `go.mod` 精确 pin `github.com/jackc/pgx/v5 v5.10.0` 与 `github.com/pressly/goose/v3 v3.27.3`；Goose v3.27.3 要求 Go 1.25.7，当前项目/CPA Go 基线为 1.26.0，实际环境 Go 1.27.0。
- `migrations/000001_initial_schema.sql` 覆盖 users/roles/sessions/MFA/API keys/policy/models/providers/credentials/pools/routes/quota/prices/requests/usage/wallet/payment/audit/outbox/stats；所有主键由应用 UUIDv7 提供。
- `migrations/000002_fact_table_guards.sql` 建立事实表 append-only trigger 和 provider/pool/route target 归属校验；PostgreSQL 错误码断言已纳入集成测试。
- `migrations/000003_wallet_payment_integrity.sql` 至 `000011_billing_integrity.sql` 依次补充账务/支付、Web Session/MFA、API Key、模型目录/价格、Credential、Route、Scheduler、Usage 和 Billing；`000012_payment_integrity.sql` 至 `000019_quota_observation.sql` 增加 Payment 状态机/验签处理、Stripe event、Provider operation recovery、merchant/live-mode、financial hold/liability、external refund/dispute、liability reference 唯一和 quota snapshot/state 约束；已应用迁移保持不可变。
- `internal/data.Open` 解析 pgxpool、固定会话 timezone=UTC/application_name=bablo 并执行真实 Ping；`Store.WithTx` 提供提交/回滚边界。
- `cmd/bablo-migrate` 与 Makefile `migrate`/`migrate-down` 可显式运行 schema 变更；应用启动不自动迁移。
- 真实 PostgreSQL 已验证完整 `0 -> 19`、latest `19 -> 18 -> 19`；从 v14 含历史 Provider 事实升级到 v15 会以 SQLSTATE `55000` 明确阻塞并要求运营从权威 Provider 回填 merchant/live mode，而不是猜测；reservation/settlement/payment/liability/quota 跨表约束、ledger immutable 和余额重建均有测试。

## 11. Auth 验收与验证证据

- `internal/auth` 已实现 Argon2id PHC hash/verify/login rehash、32-byte CSPRNG Session/CSRF token hash、Session rotation/fixation 防护、单个/全部注销和密码变更/重置撤销；
- `migrations/000004_auth_security.sql` 增加 `password_changed_at`、`csrf_token_hash`、`mfa_verified_at`、`last_totp_counter`、factor 唯一约束和恢复码可用索引；旧无 CSRF Session 在 upgrade 时主动撤销；
- 所有 mutation 同时校验 `Origin`、CSRF Cookie、`X-CSRF-Token` 和 Session-bound hash；生产配置要求 32-byte base64 MFA key、明确 Web origin、Secure Cookie，并禁止关闭管理员 MFA；
- TOTP secret 使用 AES-256-GCM + factor/user/key-version AAD；绑定二次确认、TOTP counter 防重放、10 个 80-bit hash 恢复码和单次消费在 PostgreSQL 行锁事务内；
- `bablo auth create-admin` 与 `bablo auth reset-password` 已实际运行；密码从终端/stdin 读取，reset 实际撤销全部 Session，浏览器验证旧密码被拒绝、新密码可登录；
- 真实 PostgreSQL 17-alpine：`BABLO_TEST_DATABASE_URL=... go test ./internal/data ./internal/auth -count=1` 通过，覆盖迁移 v4、Session fixation、CSRF、password change/logout、admin MFA、recovery replay 和 RBAC；
- 登录/MFA 使用独立 Redis namespace 的 account + source-address 双 fixed-window Lua 原子门禁；生产缺 Redis 配置直接拒绝启动，运行时 Redis 错误 fail closed。可信代理只在直连 peer 命中 `BABLO_TRUSTED_PROXY_CIDRS` 后解析 `X-Forwarded-For`，直连伪造 header 测试被忽略；
- 最新完整验证：真实 PostgreSQL/Redis 下 `go test -p 1 -count=1 ./...` 为 19 packages 通过、4 packages 无测试；`go test -p 1 -race -count=1 ./internal/payment ./internal/billing ./internal/auth ./internal/config ./internal/data ./cmd/bablo` 全部通过；`go vet ./...` 与两个 Go binary 构建通过；前端 lint、3 tests、typecheck/Vite build 和 Compose config 通过；
- 浏览器实际访问 Vue 登录页，经 Vite proxy 完成登录、路由守卫进入 Dashboard、HttpOnly Session/Path `/api/v1`、CSRF/Path `/` 和退出清理验证；开发环境 Cookie 非 Secure，生产强制 Secure 由配置测试覆盖；
- 当前限制：没有邮件/IdP 自助恢复；TOTP 只读取一个活动 key version，多版本解密/re-encrypt 是生产 key rotation blocker；生产 trusted proxy CIDR 必须由部署环境明确配置，管理员 MFA enrollment 管理 UI 未实现，但 API 与服务端强制已完成。

## 12. API Key 验收与验证证据

- `internal/apikey` 实现 `bablo_sk_` + 32-byte CSPRNG base64url、严格格式校验、完整 Key SHA-256、展示 prefix；Repository 只接收 hash/prefix，raw secret 只在 create/rotate 已提交响应返回一次；
- 每个用户 Key 在单事务内建立 metadata 标识的 default-deny managed policy；PATCH 原子替换该 policy 的多模型 allow entitlement，所有模型必须 enabled/public/non-deleted；授权执行 key+owner+secret version 有效性、显式 deny、allow、default action 的确定顺序，rotate 后陈旧 Principal 不能继续授权；
- 自助接口 `GET/POST /api/v1/me/api-keys`、`PATCH /api/v1/me/api-keys/{id}`、`POST .../rotate|revoke` 已接线；Web Session `Protect` 强制 full session，unsafe method 复用 Origin+CSRF；所有 owner 查询 fail closed，响应 `Cache-Control: no-store`；
- 数据面 `IdentityMiddleware` 严格解析单一 Bearer、只读取 `RemoteAddr`、context 只含内部 Principal；后续推理 handler 必须在解析 model/token 后调用 `Service.Authorize`；
- RPM/TPM 使用 UTC 固定分钟窗口；Redis v9 Lua 原子 `HINCRBY` + `PEXPIRE`，配置后故障 fail closed；无 Redis 时使用有容量上限的单实例内存实现。daily/monthly budget 阈值进入 Principal，并已由 Billing 使用 API-key advisory transaction lock 对已结算 charge + active/pending reservation 执行真实消费门禁；
- 真实 PostgreSQL 17-alpine + Redis 8-alpine：API Key 集成测试覆盖 owner 隔离、一次性明文/DB hash、普通与 IPv4-mapped IP canonicalization、expired/revoked、rotate 旧 secret/陈旧 Principal 失效及行锁等待跨过 expiry、PATCH 清除/替换、deny precedence、default allow 不越过 private visibility、一 Key 两模型、伪造 Principal owner、内存/Redis 100 goroutine 原子限流、HTTP CSRF/CRUD 和 Bearer context；
- 完整验证：`go test -count=1 ./...`、`go test -race -count=1 ./internal/apikey ./internal/auth`、`go vet ./...`、两个 Go binary `go build -trimpath` 全部通过；前端 `pnpm lint`、3 tests、typecheck、Vite build 全部通过；migration 实际 up/down/up 通过；
- 实际服务 smoke：Web Session 登录 200，Key create 201/list 200/patch 200/rotate 200/revoke 200，create `Cache-Control: no-store`，rotate `secret_version=2`，最终 status `revoked`；`/readyz` 显示 PostgreSQL/Redis `ok`、inference `not_initialized`，因此整体保持 NO-GO/503。

## 13. Models / Provider / Pricing 验收与验证证据

- `internal/model` 实现 canonical public ID、最多 100 个保留 alias、canonical capabilities、visibility、billing class、enabled 和 `route_configured`；用户 `GET /api/v1/models` 只返回 enabled/public/non-deleted 模型，alias 大小写归一且不能被其他模型重分配；
- `internal/provider` 实现 resource type/commercial policy、Provider model protocol/capability 映射和完整发现快照 reconcile；新发现固定 pending/disabled，missing 只改变 discovery signal，approved mapping/capabilities/enabled 不被发现结果覆盖；subscription P0 在 service 与数据库均禁止商业开放；
- `internal/pricing` 使用 decimal string + PostgreSQL `numeric(30,12)`，支持 global/model/provider_model 版本、input/output/cache read/cache write/reasoning/request 维度、draft/activate/retire；解析按 provider_model -> model -> global，billable 缺 input/output 或 request 价格即 `ErrPriceMissing`，free 显式返回 free snapshot；
- `migrations/000006_model_catalog_integrity.sql` 建立大小写不敏感且跨表互斥的 canonical ID/alias guard、Provider review/discovery guard、price scope guard、published entry/version 不可变和同 scope published 生效区间不重叠；迁移先修正存量 subscription/provider-model 数据，再 VALIDATE 约束；migration v6 真实 up、重复启动和 `6 -> 5 -> 6` 恢复测试通过；
- 目录更新在事务内串行化同一模型 capability 变更，并拒绝收窄已有 provider model 能力；Provider mapping 创建/更新也在同一 advisory key 下复核 public capability 子集，避免并发更新产生不可路由映射；
- 管理 API 已接线 model/provider/provider-model collection 的 GET/POST、resource GET/PATCH、Provider reconcile 和 price create/get/activate/retire；统一通过 `ProtectRole(..., "admin")` 执行 Session、CSRF、admin RBAC 和生产 MFA，普通登录用户只能读取 `/api/v1/models`；所有写入同事务写 sanitized audit；
- 真实 PostgreSQL 17-alpine 集成测试覆盖 alias promotion/冲突、discovery pending/approve/missing/no-overwrite、能力子集、模型能力收窄拒绝、缺价拒绝、Provider 级价格优先级、published mutation 55000、重叠区间拒绝、retired 历史解析与 replacement cutover；HTTP 端到端覆盖普通用户 403、管理员 model/provider/reconcile/approve/price activate 全链路；
- 完整验证：真实 PostgreSQL 下 `go test -count=1 ./...`、`go test -race -count=1 ./internal/model ./internal/provider ./internal/pricing ./internal/auth ./internal/httpapi ./cmd/bablo`、`go vet ./...`、两个 Go binary build 全部通过；追加能力约束后 `go test -race -count=1 ./internal/model ./internal/provider ./internal/pricing` 通过；前端 `pnpm lint/typecheck/test/build` 通过；实际 `bablo` 进程 smoke 验证 `/healthz` 200、`/api/v1/models` 和 `/api/v1/admin/models` 未登录均 401；
- 当前限制：CPA model registry 尚未接入自动 poller，reconcile 入口接受未来内部 worker 的完整发现快照；Route/Price 已被 Proxy 与 Billing 数据面消费；真实价格表/币种/商业策略仍由业务提供，缺失保持 fail closed；真实上游和支付仍未接通，下一阶段进入 `bablo-payment`。

## 14. Credentials 验收与验证证据

- `internal/credential` 定义 Provider-owned Credential、source/status、secret descriptor、health、pool membership 和 transient `RuntimeCredential`；业务层不暴露 CPA 类型，只有 `internal/inference/cpa` 导入 CPA SDK。
- Credential secret 使用应用层 AES-256-GCM；AAD 绑定 Credential ID、secret version ID、secret kind 和 key version；数据库仅保存 ciphertext、12-byte nonce、key version 与非敏感 metadata；密钥由 `BABLO_CREDENTIAL_ENCRYPTION_KEY`/版本化 `BABLO_CREDENTIAL_ENCRYPTION_KEYS` 注入，生产缺失时 fail closed。
- secret create/rotate/reencrypt 均在服务层校验 source-kind、大小、重复 kind 和 metadata；历史 secret 通过数据库 trigger append-only 保护；撤销 Credential 不可重新激活；并发 rotate 在 Credential 行锁下分配连续 version，并使用 wall-clock rotation timestamp 满足时间约束。
- `credential_health` 只接受不早于已观测时间的 snapshot；状态写入记录 last success/error/cooldown；pool membership 由数据库 trigger 强制 Credential 与 Pool 属于同一 Provider；管理员 API 不回显 secret，响应 `Cache-Control: no-store`。
- 管理 API 已接线：`GET/POST /api/v1/admin/credentials`、`GET/PATCH /api/v1/admin/credentials/{id}`、`POST .../rotate|reencrypt`、`GET .../health`、`GET/POST /api/v1/admin/credential-pools`、`POST/DELETE .../members`；统一通过 admin RBAC/MFA/CSRF 保护，写入 sanitized audit。
- CPA runtime bridge 使用锁定 `v7.2.149` 的公开 `sdk/cliproxy/auth`，映射为 `runtime_only=true` 的 CPA auth；PostgreSQL 仍是事实源，CPA runtime store 不回写业务 secret；Remove 只清理运行时状态。
- 真实 PostgreSQL 17-alpine 集成测试覆盖 secret ciphertext/非泄漏、AAD/key rotation、secret rotate/reencrypt、health monotonicity、Provider pool 归属、复合游标分页和 12 路并发 rotate；HTTP 端到端覆盖管理员 create/health 与统一路由挂载；CPA 单测覆盖 OAuth/API key runtime mapping 和 unsupported source 清理。
- 验证命令及结果：`BABLO_TEST_DATABASE_URL=... go test -count=1 ./...` 通过（13 packages、4 no-test packages）；`go test -race -count=1 ./internal/credential ./internal/inference/cpa ./internal/config ./internal/httpapi ./cmd/bablo` 通过；`go vet ./...`、`go build -trimpath -o /tmp/bablo-credential-final ./cmd/bablo`、`docker compose --env-file .env.example -f deploy/compose.dev.yaml config --quiet` 通过；前端 `pnpm --dir web lint/typecheck/test/build` 通过。
- 当前限制：真实 OAuth refresh/provider executor E2E、Credential 变更后的运行时热同步、CPA refresh token 回写 PostgreSQL、管理员 Credential UI、Redis/HA keyring reload 尚未实现或验证；启动时从 PostgreSQL reconcile 到 CPA Manager 已由 `cmd/bablo` 接线，但不能替代变更事件同步；这些仍阻塞整体生产 GO。

## 15. Router 验收与验证证据

- `internal/route` 实现 public model canonical/alias exact match、Provider model 与 Credential pool 归属校验、至少一个启用 target、candidate target 解析和 active route version snapshot。
- `route_versions` 只允许旧 active version 一次性写入 `effective_to`；`route_targets` 不允许 UPDATE/DELETE；snapshot hash 为 SHA-256，Route 配置修改只能发布新 version。
- 管理 API 已接线：`GET/POST /api/v1/admin/routes`、`GET/PATCH /api/v1/admin/routes/{id}`、`GET/POST /api/v1/admin/routes/{id}/versions`、`GET /api/v1/admin/routes/preview?model=...`；preview 不执行 Scheduler、Credential 解密或上游请求，数据面仍须先完成 API Key entitlement。
- 真实 PostgreSQL 17-alpine 集成测试覆盖 alias resolve、multi-target snapshot、version publish/历史、cross-provider target rejection、disabled route 和两路并发 publish；HTTP 端到端覆盖 admin create、preview、publish、version list 与统一路由挂载。
- 当前限制：Route 包仍只产生 candidates；prefix/regex、真实推理请求由 Proxy 消费，完整真实上游 E2E 与一 Key 多模型数据面 E2E 仍待后续验证。

## 16. Scheduler 验收与验证证据

- `internal/scheduler` 接收固定的 `route.Resolution`，先过滤 target/member 的 disabled/revoked、Provider/resource commercial policy、协议/能力、cooldown、地区/代理和 quota freshness/reset，再按 priority 与显式策略排序；不接触 Credential secret，也不 import CPA。
- 默认 `round_robin` 使用 PostgreSQL route version + strategy + priority 的稳定 cursor；同时提供显式 `weighted_round_robin`、`fill_first`、`quota_aware` 和有限 affinity。没有隐式随机；affinity 不能绕过硬过滤，失效会写 `affinity_unavailable`。
- Redis 协调器使用 `SET NX PX` owner-token lease、Lua 原子 release/renew/cursor、TTL affinity；内存实现只用于 P0 单实例和测试。Redis 错误不会被当成可用容量，避免并发超卖。
- 验证命令及结果：`go test -count=1 ./...`（无外部测试环境）通过；本次使用本机 PostgreSQL/Redis 测试实例执行 `go test -p 1 -race ./... -count=1`，16 packages 通过；`go vet ./...`、`go build -trimpath -o /tmp/bablo-proxy-verify ./cmd/bablo`、迁移二进制构建和前端 lint/typecheck/build 通过。并行运行带共享 PostgreSQL 的全套集成测试曾因连接/迁移锁竞争触发 Route 测试 30 秒 context deadline，串行 `-p 1` 通过，CI 应显式控制数据库连接容量或测试并发。
- 当前限制：真实 Provider health/429 feedback 和管理员 Decision 查询 API 已由 quota 阶段提供本地观测与查询；真实 Provider/OAuth E2E、stats/告警消费者仍待后续阶段。Proxy、Usage、Billing 与 Scheduler quota 输入已接通。

## 17. Proxy 验收与验证证据

- `internal/proxy` 已实现 `GET /v1/models`、`POST /v1/chat/completions`、`POST /v1/responses`；请求按 request ID、API Key 身份、canonical model entitlement、immutable Route resolution、Scheduler selection/lease、CPA Engine 执行顺序处理。
- `/v1/models` 使用当前 Key 的 fresh authorization 结果跨页筛选 public model；不会返回其他租户模型或 Credential 信息。Chat/Responses 仅把 public model/alias 解析为 canonical model，再把实际 provider slug、upstream model、route version、credential ID 传给 inference adapter。
- JSON 响应拒绝空/非 JSON/非 2xx 上游结果；SSE 覆盖 raw JSON framing、已有 SSE 保留、Chat `[DONE]`、Responses terminal event、首包前 JSON 错误、首包后结构化 SSE 错误、上游断流和客户端取消。首个 payload 后不透明 fallback。
- 请求 Authorization、Cookie、Forwarded、连接控制和客户端 request ID 不转发；响应只 allowlist content type、rate-limit、retry-after 等安全 header，JSON/SSE 均由 Bablo 强制 `Cache-Control: no-store`；客户端取消不写入 Credential failure health，非取消错误会记录安全分类/cooldown 并释放 concurrency lease。
- 请求向 CPA 只转发显式 allowlist（Content-Type、Accept、User-Agent、trace context 和已审计协议头）；Authorization、Cookie、Forwarded、任意 X-* 客户端头和连接控制头全部剥离，并统一注入服务端 request ID。
- `cmd/bablo` 在 CPA 启动前从 Credential service 执行 PostgreSQL runtime reconcile，CPA ready 后才挂载 Proxy；CPA config 含 credential-bearing 字段或 `remote-management.secret-key` 时 fail closed，防止配置文件成为第二事实源或开启嵌入式管理面。
- Proxy fake-engine contract tests 覆盖 Chat/Responses JSON、模型列表、Scheduler request/protocol/capability、credential pin、请求/响应 header allowlist、首包前后错误、断流、取消、租约释放和健康反馈；`go test -race ./internal/proxy -count=1` 通过。
- CPA adapter 现在拒绝非 loopback/远程管理、`remote-management.secret-key` 和配置内上游凭据；管理面配置回归测试已覆盖，避免嵌入式 CPA 管理 API 或配置文件成为旁路入口。
- 本阶段验证：`go test -p 1 -race -count=1 ./...`（17 packages 通过、4 no-test packages）、`go vet ./...`、`go build -trimpath -o /tmp/bablo-usage-final ./cmd/bablo` 全部通过；共享 PostgreSQL 测试按 `-p 1` 串行执行以避免迁移/连接池竞争。
- 尚未宣称真实 Provider/OAuth 协议兼容：CPA 的真实上游、refresh、流式 provider golden、首包后上游行为和支付都需后续环境/凭据与 `bablo-payment` 阶段验证。

## 18. Usage 验收与验证证据

- `internal/usage` 定义 `StartInput`、`FinalizeInput`、不可变 `Event`、`Reconciliation`、`OutboxEvent`；事件记录 request/user/key、requested/resolved model、provider/provider model、route version、credential、price version、started/finished、token breakdown、amount/currency、status/error、latency/TTFT、estimated/provenance；不保存 Prompt/响应正文。
- `BeginRequest` 以 `request_id` 幂等创建/恢复 `request_records`，重复请求的 metadata 不一致会返回冲突；`Finalize` 使用服务端 `usage:v1:<request_id>` settlement key，并同时写 UsageEvent、关闭 request record、写 transactional outbox。
- 数据库 `000010_usage_integrity.sql` 增加 Usage `request_id` 唯一索引、started/finished 时间快照、request-record 关联与 settlement/request/source 边界约束、outbox `claimed_by`；迁移从 v9 回填历史 Usage 时间并恢复 UsageEvent append-only trigger，`000002` 继续阻止 UsageEvent/reconciliation 事实 UPDATE/DELETE，`internal/usage` repository 的 outbox 生命周期更新要求 owner。
- Proxy 在 key/route/scheduler/engine 执行路径按实际 resolved route 和 price snapshot 记录 Usage；成功/非 2xx JSON 与已建立 SSE 都记录真实 HTTP upstream status，流式后续错误另保留 error class。配置 inference engine 时构造器强制同时提供 Usage Recorder、Price Resolver 与 Billing coordinator，避免可执行但不记账的旁路；非流式 JSON、Chat/Responses SSE、首包前失败、首包后错误、上游断流、客户端取消和上游未提供 usage 均生成一次事件。
- Outbox 使用 PostgreSQL `FOR UPDATE SKIP LOCKED` claim、worker owner token、TTL stale recovery、发布/失败重试状态；非 owner ack/retry 被拒绝，失败 attempts 与短错误分类持久化，payload 仅包含非正文事实。
- 验证命令及结果：真实 PostgreSQL 下 `go test ./internal/billing ./internal/usage -count=1` 通过；Proxy contract tests 覆盖 Billing reserve/settle、余额不足不触发上游和 missing usage estimated settlement；完整最终门禁见 Billing 章节。
- 当前限制：Usage/Billing outbox 尚未由独立 worker 接入 stats/告警；真实 CPA usage/provider 行为仍需外部兼容环境验证。Wallet 用户/管理员 HTTP 查询与调账接口按 `bablo-user` / `bablo-admin` 后置，不在 Billing 内核阶段伪造。

## 19. Billing 验收与验证证据

- `internal/billing` 实现 `Quote`、`Reserve`、`Settle`、`Release`、`Credit`、payment refund hold/consume/release、liability open/recover/reverse、pending settlement recovery、`GetWallet`、`RebuildBalance`；entry type 增加 payment_refund_hold、payment_reversal、payment_refund_release 和 payment_liability，管理员调账同事务写 audit；
- 金额不使用 float：价格从 `pricing.Snapshot` decimal string 解析为 12 位缩放整数，按 token/request 维度聚合后一次向上取整到 currency minor unit；cache/reasoning 专属价会去除总量中已包含部分，缺专属价回退基础 input/output rate，不静默免费；
- 非零 reservation 只接受 active/retired 且当前 effective 的 price version，并持久化 request/user/key、resolved model/provider model/route/provider/credential、预估 token 和金额；API-key advisory lock 串行化 daily/monthly budget，wallet row lock 保证 available/reserved 不为负；pending settlement 或 open liability 设置 `financial_hold` 并阻止新消费；
- settle 以 immutable UsageEvent 幂等消费 reservation：少收 release，多收补扣；补扣不足持久化 `pending` settlement/outbox，独立 owner-lease worker 重试同一 Event。外部退款/争议创建 immutable liability，充值按 FIFO 回收；dispute won 追加反向 ledger。missing usage 按 reservation 金额写 `estimated + reconcile_needed`；
- `000011_billing_integrity.sql`、`000016_payment_financial_recovery.sql` 与 `000018_billing_liability_integrity.sql` 覆盖 ledger delta/balance snapshot、reservation/settlement consistency、recovery lease、wallet hold、liability append-only/引用唯一和历史 ledger guard；
- PostgreSQL 集成测试覆盖 credit/reserve/settle/release 幂等、价格版本、budget、128 路余额竞争、pending settlement worker、settlement + liability 组合冻结、外部退款/争议追偿、liability 唯一、refund、ledger rebuild、admin adjustment 和 SQL immutable guard；Proxy 继续记录实际 resolved route/provider/credential/latency 到 UsageEvent；
- 当前限制：Usage/Payment/Billing outbox 尚未接入独立 stats/告警消费者；用户 Wallet 查询、管理员 Wallet 查询/UI、真实业务币种/价格签字和真实 Stripe 商户对账仍未完成。这些不影响账本内核测试，但阻塞对应生产能力。

## 20. Payment 验收与验证证据

- `internal/payment` 建立 Provider-neutral order/event/refund/dispute 状态机、Registry、持久 Provider operation lease、expiration/reconciliation worker、用户订单、管理员退款/关闭/人工充值和 voucher；所有业务层类型保持 Bablo 语义；
- `internal/payment/stripe.go` 精确使用 `stripe-go/v86 v86.2.0`：Checkout/Refund 使用 Bablo order 派生稳定 idempotency key；API 请求固定 Connect account；Webhook 校验原始签名/timestamp/API version/account/live mode，解析 Checkout、Refund 与 Charge Dispute，并可通过 PaymentIntent/Charge metadata 恢复订单引用；
- Provider operation 固定 payload hash、merchant/live mode、owner token、lease/backoff；并发/崩溃重试不会切换商户。expiration/reconciliation 失败或仍 pending 会推进 `maintenance_checked_at`，限量 worker 不会被单个坏订单永久队头阻塞。订单写入前按 user 行锁限制 active order；客户端 success/cancel redirect 从不入账；
- PaymentEvent 只在验签后持久化；同 provider event ID 的 payload/merchant/mode/object IDs 必须一致。订单、Checkout、Refund、PaymentIntent、Charge 等已知引用按各自语义同时核对；冲突引用回调持久 rejected fact 且不入账，无效签名完全不写数据库；
- Bablo 发起退款先将 available 移入 reserved，成功 event 消费、确定失败 event 释放、未知结果保持 pending。Provider 控制台外部退款和 dispute lost 创建 liability 并回收余额，不足时保持 financial hold；dispute won 反转已回收 ledger；
- 人工充值幂等事实绑定 operator/user/currency/amount；voucher 使用 hash + prefix + 版本化 AEAD，未兑换时同幂等请求可重放原码，兑换后清除密文；
- 配置层生产拒绝 fixture、Stripe test key、HTTP/placeholder return URL；任何启用的支付能力都要求 PostgreSQL。实际启动 smoke 已验证：启用 Stripe 且缺 `BABLO_DATABASE_URL` 返回退出码 1；production 缺 `BABLO_REDIS_URL` 同样退出码 1。Webhook body 限制 256 KiB、读取期限 10 秒、进程内并发槽位 32；
- migration 已验证 `0 -> 19`、`19 -> 18 -> 19` 与 v14 历史支付身份阻塞；最终 `BABLO_TEST_DATABASE_URL=... BABLO_TEST_REDIS_URL=... go test -p 1 -race -count=1 ./...` 为 20 个有测试包通过、4 个无测试包，并覆盖维护队列失败轮转、完整 resolved route/provider model/provider/credential/upstream status Usage 事实和 quota freshness/affinity failover；race、vet、binary、前端和 Compose 验证结果见各章节。
- 当前 NO-GO：没有真实 Stripe account/API key/webhook secret/HTTPS domain，未执行官方 test-mode create -> paid webhook -> wallet -> Bablo refund/external refund/dispute -> liability/recovery E2E；没有该证据不得开放 self-service Stripe 支付。

## 21. Quota 验收与验证证据

- `internal/quota` 定义 `QuotaProbe`、`HealthProbe`、被动 `ResponseObserver`、标准化 Window/Observation/Snapshot/ProbeState；只保存 bounded metadata/watermark，默认不保存 Prompt、响应正文或 secret。
- `migrations/000019_quota_observation.sql` 为历史 snapshot 回填 provider/model/observation key，新增 metadata、幂等唯一键、latest/due 索引、immutable snapshot trigger 和可重建 `quota_probe_states`；成功 observation 与 state transition 在同一 PostgreSQL transaction，重复同事实可重放，冲突 payload 拒绝。
- `internal/inference/cpa` 仅调用锁定 CPA v7.2.149 的公开 `ProviderSupportsQuotaObservation` 和 `QuotaState.ObserveResponseHeadersForProvider`；当前真实可核验 signal 只覆盖 Claude/Codex。Proxy 传给 observer 前做 Bablo allowlist、canonicalization、长度/控制字符限制；observer 被动运行，不发额外上游请求。
- Poll worker 从 PostgreSQL 解析 Credential/Provider，使用 Redis `SET NX PX` owner-token lease；无 Redis 只允许单实例/测试内存 fallback。Quota/health probe 总周期受 lease safety margin 约束，失败按 provider Retry-After、指数退避和确定性 jitter 更新 state，不刷新旧 snapshot 的 `observed_at`；401/403 使用长 auth backoff，429/5xx 写标准 error class 并反馈 Credential health。
- Scheduler 在显式 `QuotaPolicy` 下按 requested token 和 selected window 检查 missing/stale/reset；`RequireFresh` 保守拒绝，未知 remaining 不被推导为可用。管理员 `GET /api/v1/admin/credentials/{id}/quota` 返回有界 state/snapshots/supported probes，不回显 secret。
- 测试覆盖：header allowlist/敏感头剥离、metadata/window 边界、稳定 observation key、重复/冲突、probe lease 并发、父 context deadline、退避 cap、health cooldown、真实 PostgreSQL migration/repository 并发幂等、Scheduler missing/stale/exhausted 和 HTTP quota endpoint；先复现并修正 Scheduler 集成 fixture 未使用 resolved provider/model 的回归后，`go test -p 1 -race -count=3 ./internal/scheduler -run '^TestQuotaFreshnessAndAffinityFailover$'` 通过，随后带 PostgreSQL/Redis 的全仓库竞态测试通过。
- 当前限制：没有真实合法 Provider OAuth/API key 和 provider-specific probe E2E；CPA public API 当前没有 Gemini/Grok quota observation，真实 quota-aware scheduling 与外部健康反馈保持 NO-GO。Quota snapshot retention/rollup 和 stats/告警消费者留给 `bablo-stats`/observability 阶段。

## 22. 下一阶段

```text
/bablo-stats
```

Stats 阶段应从 UsageEvent、Wallet Ledger、Scheduler Decision、PaymentEvent/订单事实派生受控维度的查询与小时/日 rollup，并提供 raw-to-rollup 对账、Usage trace 和管理员查询 API；不得引入第二套计费公式。