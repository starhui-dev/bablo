# CPA Upstream Compatibility

> 核验日期：2026-08-29
> 结论：已锁定并编译验证 `v7.2.145`；Bablo adapter 已落盘，真实上游凭据与协议 E2E 仍未验证。

## 1. 版本事实与来源

| 项目 | 已核验事实 | 来源 |
|---|---|---|
| 官方仓库 | `router-for-me/CLIProxyAPI` | <https://github.com/router-for-me/CLIProxyAPI> |
| 最新稳定 release | `v7.2.145`，非 prerelease，发布于 2026-08-28 09:30:32Z | <https://api.github.com/repos/router-for-me/CLIProxyAPI/releases/latest> |
| 稳定 tag commit | `d9cea8904b14fbbebb77ef26e98ef08f6b48a724` | <https://api.github.com/repos/router-for-me/CLIProxyAPI/git/ref/tags/v7.2.145>；<https://api.github.com/repos/router-for-me/CLIProxyAPI/commits/d9cea8904b14fbbebb77ef26e98ef08f6b48a724> |
| main commit（观察时） | `f0de1d008fe8881dcb7431cf97b147295874c2b2`，2026-08-28 17:20:46Z | <https://api.github.com/repos/router-for-me/CLIProxyAPI/branches/main> |
| main 与 tag | main ahead 2；观察到的差异仅 README/README_CN/README_JA 相关项目文本，无 SDK/go.mod/docs 变化 | <https://api.github.com/repos/router-for-me/CLIProxyAPI/compare/v7.2.145...main> |
| module | `github.com/router-for-me/CLIProxyAPI/v7` | <https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/go.mod> |
| module major | v7 | 同上 |
| Go 版本 | `go 1.26.0` | 同上 |

`main` 不是稳定 pin。Bablo 首次接入必须精确 pin `v7.2.145`（含校验和），先验证构建环境能使用 Go 1.26.0 或遵守该 `go.mod` 的 toolchain；当前 Bablo 没有 `go.mod`，这个兼容性仍是 blocker。

## 2. 公开 `sdk/*` 包

在 `v7.2.145` tag 的 SDK tree 中发现 23 个含非测试 Go 文件的公开包目录：

```text
sdk/access
sdk/api
sdk/api/handlers
sdk/api/handlers/claude
sdk/api/handlers/gemini
sdk/api/handlers/openai
sdk/auth
sdk/cliproxy
sdk/cliproxy/auth
sdk/cliproxy/executionregistry
sdk/cliproxy/executor
sdk/cliproxy/pipeline
sdk/cliproxy/session
sdk/cliproxy/usage
sdk/config
sdk/logging
sdk/pluginabi
sdk/pluginapi
sdk/pluginhost
sdk/pluginstore
sdk/proxyutil
sdk/translator
sdk/translator/builtin
```

目录清单来源：<https://api.github.com/repos/router-for-me/CLIProxyAPI/contents/sdk?ref=v7.2.145>；版本化 pkg.go.dev module 入口：<https://pkg.go.dev/github.com/router-for-me/CLIProxyAPI/v7@v7.2.145>。pkg.go.dev 对刚发布版本可能显示“not in the latest version of its module”，且 proxy 的版本索引在核验时不可用；因此以 tag 源码和 `go.mod` 为首要事实，pkg.go.dev 作为公开文档交叉核对。

## 3. 已核验的公开候选接口

以下只是 adapter 可使用的候选表面；Bablo 业务类型不直接采用这些类型。

### `sdk/cliproxy`

源码：<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/cliproxy/builder.go>、<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/cliproxy/service_lifecycle.go>、<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/cliproxy/types.go>

- `NewBuilder`、`Builder.Build`；
- `WithConfig(*sdk/config.Config)`、`WithConfigPath`、`WithTokenClientProvider`、`WithAPIKeyClientProvider`、`WithWatcherFactory`、`WithHooks`、`WithAuthManager`、`WithRequestAccessManager`、`WithCoreAuthManager`、`WithCooldownStateStore`、`WithPluginHost`、`WithServerOptions(...api.ServerOption)`、`WithLocalManagementPassword`、`WithPostAuthHook`；
- `Service.Run(ctx)`、`Shutdown(ctx)`、`RegisterUsagePlugin`；
- model registry 接口/ hook；`NewFileTokenClientProvider`/`NewAPIKeyClientProvider` 等 provider/result surface。

### `sdk/api`

`ServerOption`、`WithMiddleware`、`WithEngineConfigurator`、`WithRouterConfigurator`、`WithLocalManagementPassword`、`WithKeepAliveEndpoint`、`WithRequestLoggerFactory`。来源：<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/api/options.go>。

### `sdk/cliproxy/auth` 与 executor

来源：<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/cliproxy/auth/conductor.go>、<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/cliproxy/executor/types.go>、<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/examples/custom-provider/main.go>。

- core `auth.NewManager(Store, Selector, Hook)`；`Store` 为 `List/Save/Delete`；
- `RoundRobinSelector`、`WeightedRoundRobinSelector`、`FillFirstSelector` 等；
- Manager 的 auth lifecycle、executor registration、`Execute`/`ExecuteCount`/`ExecuteStream`、refresh/cooldown/retry 相关方法；
- 自定义 `ProviderExecutor` 必须实现六个方法：`Identifier`、`Execute`、`ExecuteStream`、`Refresh`、`CountTokens`、`HttpRequest`；可选 `RequestPreparer`、`RequestAuthPreparer`、`ExecutionSessionCloser`；
- `ExecuteStream` 返回 `*executor.StreamResult`，其 `Chunks` channel 必须由 provider 正确关闭；首个非空 payload 前可处理 retry/failover，首个 payload 后不能透明切换；取消时需停止下游并处理 producer channel；
- `executor.Options` 包含 `Stream` 等请求元数据；依赖流式行为的调用者必须明确设置 `Stream=true`。

### `sdk/auth` / config / usage / translator

- `sdk/auth.NewFileTokenStore`、`SetBaseDir`、Store 的 `List/Save/Delete`；`sdk/config` 提供 `Config` alias 与配置读写 helper；
- `sdk/cliproxy/usage` 提供 `Manager`、`Plugin`、`Publish` 等异步观测队列；它不是持久 UsageEvent 或 Wallet Ledger；无 plugin 时记录可被丢弃；
- `sdk/translator` 提供 Format、Registry、TranslateRequest/Stream/NonStream/TokenCount 等协议转换表面。

来源：<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/auth/filestore.go>、<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/config/config.go>、<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/cliproxy/usage/manager.go>、<https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/sdk/translator/types.go>。

## 4. `docs/sdk-usage.md` 漂移审计

本地 Bablo 工作区不存在 `docs/sdk-usage.md`。上游锁定 tag 的文件为 <https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/v7.2.145/docs/sdk-usage.md>；实际源码以 tag tree 为准：

| 上游文档说法 | tag 源码事实 | 影响 |
|---|---|---|
| 示例仍使用 `/v6`，并把 `/v6/internal/config` 当外部导入 | module 已是 `/v7`；外部应优先使用 `/v7/sdk/config`；`internal/*` 不应作为 Bablo 集成边界 | v6 是 major 编译断裂；禁止照抄 |
| 把 `sdk/cliproxy` 描述成 module | 单一 module 是 `github.com/router-for-me/CLIProxyAPI/v7`，`sdk/cliproxy` 是 package | 安装/依赖说明错误 |
| `cliproxy.WithMiddleware`、`WithEngineConfigurator` 等 | 这些 option 在 `sdk/api`；`cliproxy` 提供的是 Builder.WithServerOptions | 示例不编译 |
| `cliproxy.Config` provider 参数 | `TokenClientProvider.Load` 实际接收 `*sdk/config.Config`；不存在该 `cliproxy.Config` 类型 | 自定义 provider 无法满足接口 |
| `coreauth.NewFileStore` | 未发现该 core auth API；公开 file store 是 `sdk/auth.NewFileTokenStore` + `SetBaseDir`，或自定义 core `Store` | credential 适配不能按文档实现 |
| `ExecuteStream` 返回可直接 range 的 chunks | 实际返回 `*executor.StreamResult`；调用者读其 `Chunks`，并按 executor 语义关闭/处理 channel | streaming 示例非编译级漂移 |
| streaming 示例未设置 `opts.Stream=true` | Manager 透传 Options，不自动设置该字段 | 可能走非流式行为 |
| Build 后自定义 core manager 的 transport 设置保持不变 | `Builder.Build` 会无条件设置 default RoundTripperProvider，可能覆盖 Build 前的自定义设置 | transport 定制需在 adapter 内验证生命周期 |
| 仅说明 remote-management secret key 开启管理 API | 实际还可由环境变量 `MANAGEMENT_PASSWORD` 或 local management password 触发 | 管理面暴露审计不能只看一个配置字段 |
| 自定义 token provider 返回成功数量即注入 credential | provider result 是计数，默认 file token client provider 可为 no-op；真正 credential store/executor wiring 仍需建立 | 不可把 loader 计数当主数据源或注入完成证据 |

上游 SDK 自身公开 wrapper 存在 internal 类型 alias/leak（config、registry、Auth.Storage、ServerOption 等），这是 CPA 的实现事实，不是 Bablo 可以 import CPA `internal/*` 的许可。`sdk/pluginhost` 与 `sdk/cliproxy/pipeline` 也不能未经源码验证当作 Bablo 首发依赖：前者与 Builder 的 public 类型存在不直接可接关系，后者在 tag 中主要是 context/hook surface。

## 5. Bablo 适配规则

1. 生产依赖精确 pin `github.com/router-for-me/CLIProxyAPI/v7 v7.2.145`；不使用 `@latest`、main 或未审计 tag；`go.mod`/`go.sum` 必须一起审查。
2. 只有 `internal/inference/cpa/**` 可 import CPA 包；业务、handler、repository、billing、scheduler 只依赖 `internal/inference` 自有接口和 Bablo domain types。
3. CPA adapter 使用公开 `Builder.Build`、`Service.Run`、`Service.Shutdown` 和 core auth Manager；CPA v7.2.145 没有公开 readiness API，Bablo 的 readyz 还必须由宿主服务基于 adapter 状态/依赖探针实现。
4. adapter 负责能力快照、协议格式/请求头/路由凭据映射、stream/cancel、safe error classification、request ID metadata/header 传播；credential 主数据仍由 Bablo PostgreSQL 管理。
5. CPA usage manager/queue 只作为观测或 reconcile signal，不直接入账；如 CPA 需要 runtime artifact，由 Bablo 状态生成，不把 CPA 文件反向当事实源。
6. 禁止 import CPA `internal/*`。如果公开 API 无法满足 credential/runtime 需求，先写 ADR 并选择公开 SDK、受控 loopback 或暂时将能力标为 NO-GO，而不是越界。

## 6. 首次集成验证清单

- [x] Go 1.26.0 module requirement 在 Go 1.27.0 本机通过依赖下载、编译和测试；
- [x] adapter 编译契约锁定六个 `ProviderExecutor` 方法，且只在 `internal/inference/cpa` 使用 CPA import；
- [x] fake provider 覆盖 non-stream/stream、stream headers/close、429/401/5xx、cancel、request ID、pinned credential、service build/shutdown；
- [x] translator public format mapping 覆盖 OpenAI Responses、Claude；
- [x] empty stream 已在 adapter 层拒绝；首包前后错误、partial output 后不 failover 仍需真实 proxy/上游协议阶段补齐；
- [ ] credential refresh/cooldown/fallback 与 Bablo scheduler decision；CPA Manager 基础能力已核验，领域调度尚未实现；
- [ ] 一个 Key 访问多个模型的端到端测试；依赖 apikey/models/router/proxy 阶段；
- [ ] CPA tag 升级跑 compatibility + regression + race；当前仅完成锁定版本回归与 race；
- [x] 实际 import 列表、符号、编译命令和结果已记录如下。

## 7. 本次 adapter 实现证据

实际代码位于 `internal/inference/cpa/adapter.go`、`stream.go`、`doc.go`；Bablo 稳定契约位于 `internal/inference/inference.go` 与 `errors.go`。adapter 使用的 CPA public symbols：

```text
sdk/auth.GetTokenStore
sdk/config.LoadConfig
sdk/cliproxy.NewBuilder
sdk/cliproxy.Builder.WithConfig
sdk/cliproxy.Builder.WithConfigPath
sdk/cliproxy.Builder.WithCoreAuthManager
sdk/cliproxy.Builder.Build
sdk/cliproxy.Service.Run
sdk/cliproxy.Service.Shutdown
sdk/cliproxy/auth.NewManager
sdk/cliproxy/auth.Manager.Register
sdk/cliproxy/auth.Manager.RegisterExecutor
sdk/cliproxy/auth.Manager.Execute
sdk/cliproxy/auth.Manager.ExecuteStream
sdk/cliproxy/auth.Manager.StopAutoRefresh
sdk/cliproxy/executor.Request / Options / Response / StreamResult / StreamChunk
sdk/cliproxy/executor.RequestedModelMetadataKey / PinnedAuthMetadataKey
sdk/translator.Format* / FromString
```

验证命令及结果：`go test -count=1 ./...`、`go test -race -count=1 ./internal/inference/cpa`、`go vet ./...`、`go build -trimpath -o bin/bablo ./cmd/bablo` 均通过。fake provider 使用空 CPA model registry 做生命周期/错误/stream contract 测试；真实模型目录、协议翻译、首包后的 failover 仍必须在 `bablo-proxy` compatibility suite 验证，不能把本阶段测试当作真实上游 E2E。
