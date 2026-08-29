# Bablo 实施状态

> 最后更新：2026-08-29
> 本次工作：完成 CPA SDK v7.2.145 精确锁定、inference adapter、生命周期与 fake provider 兼容测试；业务表尚未实现

## 1. 仓库审计结果

| 项目 | 观察结果 | 证据/影响 |
|---|---|---|
| 根目录 | `.omp/`、`docs/`、Go/Vue bootstrap 文件 | 保留既有提示词与规划文档，新增实现文件 |
| 后端 | 已建立 `go.mod`、`cmd/bablo/`、`internal/config`、`internal/httpapi`、`internal/inference` 和 CPA adapter | module 为 `github.com/starhui-dev/bablo`，CPA 仅在 adapter 边界 import |
| 前端 | 已建立 Vue 3 + TypeScript + Vite + pnpm shell、路由、API client、登录/Dashboard/404 页面 | `web/package.json`、`web/pnpm-lock.yaml` |
| 数据库 | 未创建业务表；`migrations/.gitkeep` 只保留目录 | 下一阶段执行 `bablo-data` |
| CI/部署 | 已有 `Dockerfile`、`deploy/compose.dev.yaml`；尚无 CI workflow/生产部署 runbook | Compose 仅作为开发基础设施，生产硬化留到后续阶段 |
| 文档 | 架构规划、ADR、README、LICENSE、CPA compatibility 证据均已存在 | 已更新实际 import/symbol/测试结果 |
| Git | 已关联 `origin` 到 `git@github.com:starhui-dev/bablo.git` | CPA 修改尚未提交，不覆盖用户既有工作 |
| CPA 本地使用 | `go.mod` 精确 pin `v7.2.145`；adapter Build/Run/Shutdown、Manager Execute/Stream 和映射测试已落盘 | 真实 Provider/OAuth E2E 仍缺外部凭据 |

## 2. 已落盘的规划

- [x] `docs/product-scope.md`：目标、P0/P1/P2、非目标和上线门禁；
- [x] `docs/architecture.md`：模块边界、请求流水线、单实例/HA、事务与失败语义；
- [x] `docs/data-model.md`：ER、实体、唯一约束、幂等键、账务不变量；
- [x] `docs/api-surface.md`：管理面、推理面、支付面、错误和 streaming 契约；
- [x] `docs/security-model.md`：威胁边界、密钥、认证、SSRF、支付、隐私、部署；
- [x] `docs/upstream-compatibility.md`：CPA tag/commit/Go/package、漂移和 adapter 规则；
- [x] 四份 ADR：CPA boundary、PostgreSQL source-of-truth、Usage/Ledger billing、routing/scheduler；
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

## 4. 上游 CPA 核验摘要

观察日期 2026-08-29：官方稳定 release `v7.2.145`，tag commit `d9cea8904b14fbbebb77ef26e98ef08f6b48a724`；module `github.com/router-for-me/CLIProxyAPI/v7`，要求 `go 1.26.0`。Bablo 已精确 pin 该版本并生成 `go.sum`，本机 Go 1.27.0 完成 compile/test/race/vet/build。adapter 只 import public `sdk/auth`、`sdk/config`、`sdk/cliproxy`、`sdk/cliproxy/auth`、`sdk/cliproxy/executor`、`sdk/translator`。上游 `docs/sdk-usage.md` 的 v6/internal/config/option/stream 漂移仍成立，详见 `docs/upstream-compatibility.md`。

## 5. 完整实施顺序与阶段验收

严格遵循 `.omp/commands/bablo-next.md` 的顺序。每阶段完成代码、测试和必要文档后才勾选；没有实现时不能标记完成。

| 序 | 命令/阶段 | 进入条件 | 最低验收标准 |
|---:|---|---|---|
| 1 | `bablo-plan` | 完成 | 范围、ADR、数据/API/安全、CPA 核验一致 |
| 2 | `bablo-bootstrap` | 完成 | Go/Vue 骨架、配置、日志、优雅退出、health/ready、Makefile/lockfile、无真实 secret |
| 3 | `bablo-cpa` | 完成（真实上游 E2E 后置） | pin/checksum、边界 adapter、fake provider non-stream/stream/cancel/error/shutdown/race |
| 4 | `bablo-data` | 下一阶段 | migration 空库 up/升级/重复启动；核心约束、repository 事务测试 |
| 5 | `bablo-auth` | users/sessions schema | 登录、Session hash/TTL/注销、CSRF、RBAC、管理员 TOTP/recovery 测试 |
| 6 | `bablo-apikey` | auth/policy 表 | Key 只显示一次/hash；revoked/expired/IP/limit；一 Key 多模型 E2E |
| 7 | `bablo-models` | data layer | public/upstream model、capability、visibility、price version 管理和缺价拒绝 |
| 8 | `bablo-credentials` | provider/model policy | AEAD secret/key rotation、状态/health/pool metadata；不泄漏 token |
| 9 | `bablo-router` | models/credentials/policy | exact route 多 target、version snapshot、preview、正确 resolved target |
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
- PostgreSQL、Redis、TLS/域名、镜像 registry 和 secret manager；
- CPA OAuth/Provider Credential、代理/地区和合法资源政策；
- 是否首发 self-service payment、支付 Provider、merchant/app 资质、sandbox/真实凭据；
- 计费币种、最小货币单位、价格表、负余额/退款/赠送/估算 token 业务规则；
- 管理员 MFA 强制范围、用户邀请/注册、数据 retention/合规和首发协议范围；
- 目标用户客户端（是否必须 `/v1/messages`、Gemini 等）。

缺少上述信息不阻塞 `bablo-data` 或 fake provider 兼容测试，但阻塞真实上游/支付 E2E 和最终生产 GO；不得伪造凭据或验证结果。

## 7. 下一阶段

```text
/bablo-data
```

CPA adapter 已完成当前阶段的公开 SDK 边界与 compatibility suite；下一步创建 PostgreSQL migration/repository 基线，但不提前实现 auth、wallet 或 payment 业务逻辑。

## 8. Bootstrap 验收与验证证据

- `go test ./...` 通过；`go vet ./...` 通过；`go build -trimpath -o bin/bablo ./cmd/bablo` 通过；
- `pnpm install` 完成锁文件生成；首次安装的 esbuild 脚本按 pnpm 11 审批后重建成功；
- `pnpm lint`、`pnpm typecheck`、`pnpm test`（2 tests）、`pnpm build` 全部通过；
- 后端实际启动 smoke：`/healthz` 返回 200；`/readyz` 在 DB/Redis/Inference 未初始化时返回 503 和明确 checks；`/metrics` 返回 Prometheus 文本；
- 前端实际浏览器 smoke 验证 Dashboard、`/login` 和未知路径 404，页面标题、导航、空状态和错误页面可见；
- `git diff --check` 通过；文件未进入提交前仍有 bootstrap 未提交变更；
- Bootstrap 阶段未创建业务表或真实 secret；CPA 依赖与 compatibility suite 已在本阶段落盘。

## 9. CPA Adapter 验收与验证证据

- `go.mod` 精确 pin `github.com/router-for-me/CLIProxyAPI/v7 v7.2.145`，`go.sum` 含 module/content checksum；
- `internal/inference` 定义 Bablo Request/ResolvedRoute/ExecutionResult/Stream/Capabilities/UpstreamError，不向业务层暴露 CPA 类型；
- `internal/inference/cpa` 实现 config load、Builder/Service lifecycle、Manager execute/stream、source/response format、request ID、provider/pinned credential、safe error 和 stream headers/cancel 映射；
- fake provider 覆盖 non-stream、stream、401/429/5xx、caller cancel、stream close、request ID、credential pin、capability copy、service build/shutdown；
- `go test -count=1 ./...` 通过；`go test -race -count=1 ./internal/inference/cpa` 通过；`go vet ./...` 通过；`go build -trimpath -o bin/bablo ./cmd/bablo` 通过；
- CPA import 搜索确认源码 import 只存在于 `internal/inference/cpa/**`；真实 Provider/OAuth、Chat/Responses golden、首包后错误/failover 留到 credentials/proxy 阶段，当前不能宣称真实上游兼容。
