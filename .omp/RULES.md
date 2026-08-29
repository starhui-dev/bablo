# Bablo Sticky Rules

- MUST 保持产品/仓库身份为 `Bablo` / `bablo`；不要把 CPA 或旧网关名称变成公共产品名。
- MUST 将 CPA 依赖隔离在 `internal/inference/cpa`；其他业务包 NEVER import CPA 类型。
- MUST 锁定 CPA 精确版本；NEVER 使用 `@latest` 直接进入生产依赖。
- MUST 在实现 CPA 能力前核对锁定 tag 的源码、go.mod、pkg.go.dev 和 examples；NEVER 根据旧文档猜 API。
- MUST 使用 PostgreSQL 作为业务唯一事实来源；Redis 只保存可重建运行时状态。
- MUST 让一个 API Key 能访问多个模型；NEVER 把 Key 强绑定到单一 Group/Provider。
- MUST 使用不可变 UsageEvent + Wallet Ledger 做计费；NEVER 用 CPA 内存统计/usage queue 作为真钱账本唯一依据。
- MUST 按实际 resolved provider/model/route/credential + price version 结算；NEVER 只按请求 alias 计费。
- MUST 对支付回调验签、幂等、防重放；NEVER 信任客户端上报支付成功。
- MUST 让 Scheduler 可解释并写 Decision Log；NEVER 使用不可观察的隐式随机调度。
- MUST 正确保护 API Key、OAuth token、支付密钥和用户密码；NEVER 写入日志或提交仓库。
- MUST 默认不记录 Prompt/响应正文；调试采样必须显式开启、脱敏并有短 TTL。
- MUST 给所有生产变更提供 migration、测试、备份/回滚路径。
- MUST 把受条款约束的 subscription 资源与 official_api 分开；NEVER 默认允许受限制订阅资源公开商业转售。
- SHOULD 优先修复根因而不是叠加旁路兼容层；兼容层必须可删除、可测试、有文档。
- SHOULD 保持依赖和基础设施最少；无量化必要性不要引入 ClickHouse、Kafka、Nacos 等额外中间件。
