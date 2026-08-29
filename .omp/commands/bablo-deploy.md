---
description: 实现可上线 Docker/Compose 部署、迁移、备份恢复、滚动与回滚 Runbook
---

完成生产部署资产。

## 镜像

- 多阶段构建，锁定依赖，可复现。
- 最终容器非 root，尽量小；包含 CA/时区等必要运行文件。
- build version/commit/CPA SDK version 可查询。

## 部署

提供生产 `compose` 示例或当前环境适配方案：gateway、PostgreSQL、Redis；数据库/Redis 不公开。反向代理/TLS 与域名采用环境配置，不硬编码真实域名。

- migrations 作为显式 release step；失败不得启动新版本。
- healthz/readyz 配合滚动。
- secrets 通过 env file/secret manager，不 bake 进镜像。
- volume、日志、时区、资源限制、ulimit 按压测结果配置。

## 数据保护

- PostgreSQL 自动备份方案、保留策略、加密、异地/独立存储说明。
- Redis 按“可重建状态”设计，不依赖其作为财务唯一数据。
- 必须执行一次实际 restore drill 到临时数据库并记录结果。

## 回滚

应用版本回滚 + migration 兼容策略 + CPA SDK rollback。禁止把危险 down migration 当常规回滚。

产出 `docs/deployment.md`、`docs/backup-restore.md`、`docs/rollback.md`。
