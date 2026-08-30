# Bablo 实施状态

> 最后更新：2026-08-30
> 本次工作：完成 `bablo-router` P0 exact Route、多 target version snapshot、active version 原子切换、preview/resolution、管理员 Route API，并完成真实 PostgreSQL/HTTP/并发验证。

## 1. 仓库审计结果

| 项目 | 观察结果 | 证据/影响 |
|---|---|---|
| 根目录 | `.omp/`、`docs/`、Go/Vue bootstrap 文件 | 保留既有提示词与规划文档，新增实现文件 |
| 后端 | 已建立 Go bootstrap、CPA adapter、data layer、`internal/auth`、`internal/apikey`、`internal/model`、`internal/provider`、`internal/pricing`、`internal/credential`、`internal/route` 与共享 `internal/audit`；`cmd/bablo` 已接线用户模型目录、管理员 catalog、Credential 和 Route API | Web Session 只保护管理面；admin catalog 强制 RBAC/MFA/CSRF；CPA 仍只在 adapter 边界 import |
| 前端 | Vue 登录页已接通 Session/MFA 登录、CSRF header、路由守卫和退出；模型/Key/Credential 管理 UI 留到后续前端阶段 | 当前目录 HTTP API 已可供后续 UI 使用，Dashboard/404 仍为业务壳 |
| 数据库 | 已落地 `000001`–`000008` migrations；`000008_route_integrity.sql` 增加 Route match/hash/metadata 约束、version 关闭一次性保护和 target immutable 保护 | `cmd/bablo-migrate` 显式执行 up/down；应用启动不自动改 schema；migration 测试已升级至 v8 |
| 文档 | 架构规划、ADR、README、LICENSE、CPA compatibility 证据均已存在 | API/data/security/architecture/status 已同步模型目录与 Credential 实际契约 |
| Git | 当前工作目录未检测到 `.git` 元数据，不能独立报告分支/未提交 diff | 未执行破坏性覆盖；保留既有文件并增量落盘 |
| CPA 本地使用 | `go.mod` 精确 pin `v7.2.145`；adapter Build/Run/Shutdown、Manager Execute/Stream 和 Credential runtime 注册已接线 | 真实 Provider/OAuth E2E 仍缺外部凭据 |

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
2. 只有 `internal/inference/cpa` import CPA；已精确 pin `github.com/router-for-me/CLIProxyAPI/v7 v7.2.145`；
3. CPA module major 为 v7，tag `go 1.26.0`；main 不作为生产依赖；
4. API Key -> policy/entitlement -> model route，一个 Key 可访问多个模型；
5. PostgreSQL 是业务唯一事实源，Redis 只存可重建运行时状态；
6. UsageEvent + Wallet Ledger 是唯一计费事实；CPA usage queue 不入账；
7. route snapshot 和 price version 按请求固定；scheduler 先硬过滤、再确定性选择、每次写 Decision Log；
8. 原始 Prompt/响应默认不持久化；subscription 与 official/enterprise/third_party 资源政策分离且默认不商业开放；
9. P0 单实例、邀请制/预充值或管理员授信；支付 Provider 未完成官方 sandbox/真实 E2E 前为 NO-GO。
10. Web Session 与推理 API Key 完全分离；生产管理员操作必须 MFA，Session/CSRF 只存 hash，TOTP secret 使用版本化 AES-256-GCM ciphertext。
11. API Key 固定为 `bablo_sk_` + 32-byte CSPRNG base64url；数据库只保存完整 Key SHA-256、展示 prefix 和 metadata；P0 rotate 原子替换且旧 secret 立即失效，不提供双 Key 并行窗口。

## 4. 上游 CPA 核验摘要

观察日期 2026-08-29：官方稳定 release `v7.2.145`，tag commit `d9cea8904b14fbbebb77ef26e98ef08f6b48a724`；module `github.com/router-for-me/CLIProxyAPI/v7`，要求 `go 1.26.0`。Bablo 已精确 pin 该版本并生成 `go.sum`，本机 Go 1.27.0 完成 compile/test/race/vet/build。adapter 只 import public `sdk/auth`、`sdk/config`、`sdk/cliproxy`、`sdk/cliproxy/auth`、`sdk/cliproxy/executor`、`sdk/translator`。上游 `docs/sdk-usage.md` 的 v6/internal/config/option/stream 漂移仍成立，详见 `docs/upstream-compatibility.md`。

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
| 10 | `bablo-scheduler` | router + Redis lease interface | 硬过滤、确定性 priority/RR、TTL lease、Decision Log、并发测试 |
| 11 | `bablo-proxy` | CPA adapter + scheduler | `/v1/models`、Chat、Responses；JSON/SSE、cancel、首包前后错误、request ID |
| 12 | `bablo-usage` | proxy execution facts | immutable UsageEvent、settlement key、stream cancel/no-usage/reconcile/outbox 测试 |
| 13 | `bablo-billing` | usage/pricing/wallet schema | reservation/charge/release、price snapshot、并发 100+、重复 settle、无非法透支 |
| 14 | `bablo-payment` | payment business decision + provider docs | order state machine、验签 fixture、金额/币种/防重放/幂等；无真实凭据则 NO-GO |
| 15 | `bablo-quota` | provider-specific legal probe | snapshot observed_at/stale/confidence、backoff、单 credential 防并发；不猜接口 |
| 16 | `bablo-stats` | usage/ledger | 维度筛选、趋势/排行/trace；rollup 与 raw/ledger 对账 |
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
- PostgreSQL、Redis、TLS/域名、镜像 registry 和 secret manager；API Key 配置 Redis 时会 fail closed，未配置时只适用于 P0 单实例；
- 生产反向代理的可信来源/IP 传递规范尚未确定；当前 IP allowlist 安全地只读取直连 `RemoteAddr`，在完成 trusted-proxy 设计前不得把客户端 `X-Forwarded-For` 当真实源；
- CPA OAuth/Provider Credential、代理/地区和合法资源政策；
- 是否首发 self-service payment、支付 Provider、merchant/app 资质、sandbox/真实凭据；
- 计费币种、最小货币单位、价格表、负余额/退款/赠送/估算 token 业务规则；API Key daily/monthly budget 已保存，但消费门禁必须等待 Usage/Billing 事实源；
- 管理员 MFA 已固定为生产强制；仍需决定普通用户是否强制 MFA、邀请/自注册策略、邮件或外部 IdP、数据 retention/合规和首发协议范围；
- 目标用户客户端（是否必须 `/v1/messages`、Gemini 等）。

缺少上述信息不影响已完成的 `bablo-apikey` 本地实现与测试，但阻塞真实上游/支付 E2E、代理后端客户端 IP allowlist、预算消费门禁、邮件自助恢复和最终生产 GO；不得伪造凭据或验证结果。

## 7. 下一阶段

```text
/bablo-scheduler
```

`bablo-router` 已完成 P0 exact route、Provider/Pool candidate、immutable version snapshot、preview 和 active version 原子切换；下一步实现硬过滤、确定性选择、Redis lease 和 Decision Log。

## 8. Bootstrap 验收与验证证据

- `go test ./...` 通过；`go vet ./...` 通过；`go build -trimpath -o bin/bablo ./cmd/bablo` 通过；
- `pnpm install` 完成锁文件生成；首次安装的 esbuild 脚本按 pnpm 11 审批后重建成功；
- `pnpm lint`、`pnpm typecheck`、`pnpm test`（2 tests）、`pnpm build` 全部通过；
- 后端实际启动 smoke：`/healthz` 返回 200；`/readyz` 在 DB/Redis/Inference 未初始化时返回 503 和明确 checks；`/metrics` 返回 Prometheus 文本；
- 前端实际浏览器 smoke 验证 Dashboard、`/login` 和未知路径 404，页面标题、导航、空状态和错误页面可见；
- `git diff --check` 通过；当前数据阶段文件尚未提交，保留既有 CPA/Bootstrap 工作不覆盖。
- Bootstrap 阶段未创建业务表或真实 secret；CPA 依赖与 compatibility suite 已在本阶段落盘。

## 9. CPA Adapter 验收与验证证据

- `go.mod` 精确 pin `github.com/router-for-me/CLIProxyAPI/v7 v7.2.145`，`go.sum` 含 module/content checksum；
- `internal/inference` 定义 Bablo Request/ResolvedRoute/ExecutionResult/Stream/Capabilities/UpstreamError，不向业务层暴露 CPA 类型；
- `internal/inference/cpa` 实现 config load、Builder/Service lifecycle、Manager execute/stream、source/response format、request ID、provider/pinned credential、safe error 和 stream headers/cancel 映射；
- fake provider 覆盖 non-stream、stream、401/429/5xx、caller cancel、stream close、request ID、credential pin、capability copy、service build/shutdown；
- `go test -count=1 ./...` 通过；`go test -race -count=1 ./internal/inference/cpa` 通过；`go vet ./...` 通过；`go build -trimpath -o bin/bablo ./cmd/bablo` 通过；
- CPA import 搜索确认源码 import 只存在于 `internal/inference/cpa/**`；真实 Provider/OAuth、Chat/Responses golden、首包后错误/failover 留到 credentials/proxy 阶段，当前不能宣称真实上游兼容。

## 10. Data Layer 验收与验证证据

- `go.mod` 精确 pin `github.com/jackc/pgx/v5 v5.10.0` 与 `github.com/pressly/goose/v3 v3.27.3`；Goose v3.27.3 要求 Go 1.25.7，当前项目/CPA Go 基线为 1.26.0，实际环境 Go 1.27.0。
- `migrations/000001_initial_schema.sql` 覆盖 users/roles/sessions/MFA/API keys/policy/models/providers/credentials/pools/routes/quota/prices/requests/usage/wallet/payment/audit/outbox/stats；所有主键由应用 UUIDv7 提供。
- `migrations/000002_fact_table_guards.sql` 建立事实表 append-only trigger 和 provider/pool/route target 归属校验；PostgreSQL 错误码断言已纳入集成测试。
- `migrations/000003_wallet_payment_integrity.sql`、`000004_auth_security.sql`、`000005_api_key_security.sql`、`000006_model_catalog_integrity.sql`、`000007_credential_security.sql` 与 `000008_route_integrity.sql` 依次补充账务/支付、Web Session/MFA、API Key、模型目录/价格、Credential 和 Route 完整性；已应用迁移保持不可变。
- `internal/data.Open` 解析 pgxpool、固定会话 timezone=UTC/application_name=bablo 并执行真实 Ping；`Store.WithTx` 提供提交/回滚边界。
- `cmd/bablo-migrate` 与 Makefile `migrate`/`migrate-down` 可显式运行 schema 变更；应用启动不自动迁移。
- `go test -count=1 ./internal/data` 在真实 PostgreSQL 测试库验证空 schema up-by-one、连续升级至 v8、重复启动、核心唯一约束、append-only、Provider/pool/route target 与模型/价格/Credential/Route 约束。
- PostgreSQL 17-alpine 集成测试已验证完整 `0 -> 8`；Bablo `/readyz` 仍因 inference `not_initialized` 保持 503，未伪造整体 ready。

## 11. Auth 验收与验证证据

- `internal/auth` 已实现 Argon2id PHC hash/verify/login rehash、32-byte CSPRNG Session/CSRF token hash、Session rotation/fixation 防护、单个/全部注销和密码变更/重置撤销；
- `migrations/000004_auth_security.sql` 增加 `password_changed_at`、`csrf_token_hash`、`mfa_verified_at`、`last_totp_counter`、factor 唯一约束和恢复码可用索引；旧无 CSRF Session 在 upgrade 时主动撤销；
- 所有 mutation 同时校验 `Origin`、CSRF Cookie、`X-CSRF-Token` 和 Session-bound hash；生产配置要求 32-byte base64 MFA key、明确 Web origin、Secure Cookie，并禁止关闭管理员 MFA；
- TOTP secret 使用 AES-256-GCM + factor/user/key-version AAD；绑定二次确认、TOTP counter 防重放、10 个 80-bit hash 恢复码和单次消费在 PostgreSQL 行锁事务内；
- `bablo auth create-admin` 与 `bablo auth reset-password` 已实际运行；密码从终端/stdin 读取，reset 实际撤销全部 Session，浏览器验证旧密码被拒绝、新密码可登录；
- 真实 PostgreSQL 17-alpine：`BABLO_TEST_DATABASE_URL=... go test ./internal/data ./internal/auth -count=1` 通过，覆盖迁移 v4、Session fixation、CSRF、password change/logout、admin MFA、recovery replay 和 RBAC；
- 完整验证：真实 PostgreSQL 下 `go test -count=1 ./...`、`go test -race -count=1 ./internal/auth`、`go vet ./...`、`go build -trimpath ./cmd/bablo` 全部通过；前端 `pnpm lint`、3 tests、typecheck/Vite build 全部通过；Compose 带示例变量 `config --quiet` 通过；
- 浏览器实际访问 Vue 登录页，经 Vite proxy 完成登录、路由守卫进入 Dashboard、HttpOnly Session/Path `/api/v1`、CSRF/Path `/` 和退出清理验证；开发环境 Cookie 非 Secure，生产强制 Secure 由配置测试覆盖；
- 当前限制：没有邮件/IdP 自助恢复；TOTP 只读取一个活动 key version，多版本解密/re-encrypt 是生产 key rotation blocker；登录/MFA limiter 为 P0 单实例内存实现，HA 前必须接入 Redis 协调；管理员 MFA enrollment 管理 UI 未实现，但 API 与服务端强制已完成。

## 12. API Key 验收与验证证据

- `internal/apikey` 实现 `bablo_sk_` + 32-byte CSPRNG base64url、严格格式校验、完整 Key SHA-256、展示 prefix；Repository 只接收 hash/prefix，raw secret 只在 create/rotate 已提交响应返回一次；
- 每个用户 Key 在单事务内建立 metadata 标识的 default-deny managed policy；PATCH 原子替换该 policy 的多模型 allow entitlement，所有模型必须 enabled/public/non-deleted；授权执行 key+owner+secret version 有效性、显式 deny、allow、default action 的确定顺序，rotate 后陈旧 Principal 不能继续授权；
- 自助接口 `GET/POST /api/v1/me/api-keys`、`PATCH /api/v1/me/api-keys/{id}`、`POST .../rotate|revoke` 已接线；Web Session `Protect` 强制 full session，unsafe method 复用 Origin+CSRF；所有 owner 查询 fail closed，响应 `Cache-Control: no-store`；
- 数据面 `IdentityMiddleware` 严格解析单一 Bearer、只读取 `RemoteAddr`、context 只含内部 Principal；后续推理 handler 必须在解析 model/token 后调用 `Service.Authorize`；
- RPM/TPM 使用 UTC 固定分钟窗口；Redis v9 Lua 原子 `HINCRBY` + `PEXPIRE`，配置后故障 fail closed；无 Redis 时使用有容量上限的单实例内存实现。daily/monthly budget 仅保存阈值并进入 Principal，真实消费门禁明确后置到 Usage/Billing；
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
- 当前限制：CPA model registry 尚未接入自动 poller，reconcile 入口接受未来内部 worker 的完整发现快照；Route 已实现但尚未被 Scheduler 消费；真实价格表/币种/商业策略仍由业务提供，缺失保持 fail closed；下一阶段应进入 `bablo-scheduler`。

## 14. Credentials 验收与验证证据

- `internal/credential` 定义 Provider-owned Credential、source/status、secret descriptor、health、pool membership 和 transient `RuntimeCredential`；业务层不暴露 CPA 类型，只有 `internal/inference/cpa` 导入 CPA SDK。
- Credential secret 使用应用层 AES-256-GCM；AAD 绑定 Credential ID、secret version ID、secret kind 和 key version；数据库仅保存 ciphertext、12-byte nonce、key version 与非敏感 metadata；密钥由 `BABLO_CREDENTIAL_ENCRYPTION_KEY`/版本化 `BABLO_CREDENTIAL_ENCRYPTION_KEYS` 注入，生产缺失时 fail closed。
- secret create/rotate/reencrypt 均在服务层校验 source-kind、大小、重复 kind 和 metadata；历史 secret 通过数据库 trigger append-only 保护；撤销 Credential 不可重新激活；并发 rotate 在 Credential 行锁下分配连续 version，并使用 wall-clock rotation timestamp 满足时间约束。
- `credential_health` 只接受不早于已观测时间的 snapshot；状态写入记录 last success/error/cooldown；pool membership 由数据库 trigger 强制 Credential 与 Pool 属于同一 Provider；管理员 API 不回显 secret，响应 `Cache-Control: no-store`。
- 管理 API 已接线：`GET/POST /api/v1/admin/credentials`、`GET/PATCH /api/v1/admin/credentials/{id}`、`POST .../rotate|reencrypt`、`GET .../health`、`GET/POST /api/v1/admin/credential-pools`、`POST/DELETE .../members`；统一通过 admin RBAC/MFA/CSRF 保护，写入 sanitized audit。
- CPA runtime bridge 使用锁定 `v7.2.145` 的公开 `sdk/cliproxy/auth`，映射为 `runtime_only=true` 的 CPA auth；PostgreSQL 仍是事实源，CPA runtime store 不回写业务 secret；Remove 只清理运行时状态。
- 真实 PostgreSQL 17-alpine 集成测试覆盖 secret ciphertext/非泄漏、AAD/key rotation、secret rotate/reencrypt、health monotonicity、Provider pool 归属、复合游标分页和 12 路并发 rotate；HTTP 端到端覆盖管理员 create/health 与统一路由挂载；CPA 单测覆盖 OAuth/API key runtime mapping 和 unsupported source 清理。
- 验证命令及结果：`BABLO_TEST_DATABASE_URL=... go test -count=1 ./...` 通过（13 packages、4 no-test packages）；`go test -race -count=1 ./internal/credential ./internal/inference/cpa ./internal/config ./internal/httpapi ./cmd/bablo` 通过；`go vet ./...`、`go build -trimpath -o /tmp/bablo-credential-final ./cmd/bablo`、`docker compose --env-file .env.example -f deploy/compose.dev.yaml config --quiet` 通过；前端 `pnpm --dir web lint/typecheck/test/build` 通过。
- 当前限制：真实 OAuth refresh/provider executor E2E、credential 启动时从 PostgreSQL reconcile 到 CPA Manager 的主程序 wiring、scheduler 消费 pool、管理员 Credential UI、Redis/HA keyring reload 尚未实现或验证；这些不阻塞本地 Credential store，但阻塞整体生产 GO。

## 15. Router 验收与验证证据

- `internal/route` 实现 public model canonical/alias exact match、Provider model 与 Credential pool 归属校验、至少一个启用 target、candidate target 解析和 active route version snapshot。
- `route_versions` 只允许旧 active version 一次性写入 `effective_to`；`route_targets` 不允许 UPDATE/DELETE；snapshot hash 为 SHA-256，Route 配置修改只能发布新 version。
- 管理 API 已接线：`GET/POST /api/v1/admin/routes`、`GET/PATCH /api/v1/admin/routes/{id}`、`GET/POST /api/v1/admin/routes/{id}/versions`、`GET /api/v1/admin/routes/preview?model=...`；preview 不执行 Scheduler、Credential 解密或上游请求，数据面仍须先完成 API Key entitlement。
- 真实 PostgreSQL 17-alpine 集成测试覆盖 alias resolve、multi-target snapshot、version publish/历史、cross-provider target rejection、disabled route 和两路并发 publish；HTTP 端到端覆盖 admin create、preview、publish、version list 与统一路由挂载。
- 当前限制：Route 只产生 candidates，不选择 Credential；prefix/regex、Scheduler、quota、lease、真实推理请求和一 Key 多模型完整数据面 E2E 留到后续阶段。