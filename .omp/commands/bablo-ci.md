---
description: 建立 GitHub Actions CI/CD、依赖锁定、镜像、安全扫描与发布流程
---

建立生产 CI/CD，默认以 GitHub 为代码托管/发布环境；若仓库已有其他 CI，保持单一主流程并说明。

## PR CI

至少：
- Go fmt/vet/static analysis/test/race（race 可拆 job）
- migration tests
- frontend lint/typecheck/test/build
- CPA adapter compatibility test
- secret scan / dependency vulnerability scan
- Docker build

## Release

- tag/version 规则清晰；生成 SBOM/校验摘要（可用当前维护工具）。
- Docker image 标记 immutable version + git sha；不要只发布 `latest`。
- CPA SDK version 写入 build metadata。
- 数据库 migration 在部署阶段显式执行。
- Production deploy 需要 protected environment/manual approval（如果当前仓库权限允许）。

## Dependency bot

可配置 Renovate/Dependabot，但 CPA SDK 更新只能开 PR，不能自动合并到生产。升级必须触发专门 compatibility suite。

产出/更新 release runbook。
