---
description: 初始化或整理 Bablo 的 Go + PostgreSQL + Redis + Vue/pnpm 生产项目骨架
---

把当前仓库初始化/整理为 **Bablo** 的可持续生产工程。项目名、仓库语义和默认二进制名固定使用 `Bablo` / `bablo`。附加要求：$ARGUMENTS

## 开始前

1. 审计当前仓库、Git remote、已有 go.mod/package.json、未提交修改。
2. 若仓库已有正确代码，增量整理，禁止为了套模板重写有效实现。
3. Go module 优先从 Git remote 推导；无法可靠推导时不要猜组织名，记录待确认项。

## 后端

- 使用与锁定 CPA SDK 兼容的 Go 版本。
- 默认入口 `cmd/bablo/`。
- 建立 `internal/`、`migrations/`、`configs/`、`docs/` 等结构。
- 创建配置加载、结构化日志、优雅关闭、request id、`/healthz`、`/readyz`。
- PostgreSQL 使用 pgx v5 或当前已选等价维护良好的驱动；Redis 使用维护活跃的 Go 客户端。
- migration 工具选择维护活跃、SQL-first 的方案并锁版本。
- 先建立 Bablo 自己的 `internal/inference` interface；CPA 空适配边界放在 `internal/inference/cpa`。
- 其他业务包禁止直接依赖 CPA。

## 前端

- Vue 3 + TypeScript + Vite + pnpm。
- 产品显示名使用 `Bablo`，需要副标题时使用 `AI Gateway`。
- 建立 admin/user 共用基础布局、路由、API client、错误处理、类型生成/共享策略。
- 不堆演示页；先完成登录壳、空 Dashboard、404/错误页。

## 本地基础设施

创建生产意识的 `compose.yaml` 或 `deploy/compose.dev.yaml`；PostgreSQL/Redis 默认只在容器网络可达。提供 `.env.example`，不能包含真实 Secret。

默认服务/镜像语义使用 `bablo`，但镜像 registry/组织名必须从实际仓库或部署环境获取，不得猜测。

## 工程质量

至少建立：

- Makefile/Taskfile：fmt、lint、test、test-race、migrate、dev、build；
- Go 单测骨架；
- 前端 lint/typecheck/test/build；
- `.gitignore`、EditorConfig；
- 可重复构建。

执行当前环境能运行的格式化、编译和测试。更新 `docs/implementation-status.md`，记录真实命令和结果。
