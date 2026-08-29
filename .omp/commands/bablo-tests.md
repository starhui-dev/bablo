---
description: 补齐生产测试金字塔：单测、集成、契约、E2E、race、fuzz 与故障注入
---

审计现有测试覆盖并补齐关键路径。

## 必测不变量

- 一个 Key 可连续请求多个模型并进入正确 route/pool。
- revoked/expired/无权限 Key 永远无法请求。
- Scheduler 永不选择硬过滤掉的 credential。
- 并发预算不会重复扣/免费/非法透支。
- 同一 Usage 只能 settle 一次。
- 同一支付 event 只能到账一次。
- price version 切换不重写历史。
- stream cancel/断流能正确释放 lease 与处理结算。
- CPA/Redis/Postgres 临时失败不造成 silent corruption。

## 套件

- Go unit + integration（真实 PostgreSQL/Redis 容器）
- `go test -race`
- fuzz/property tests：Key parser、route match、scheduler invariants、webhook parser
- CPA adapter contract tests
- HTTP/SSE golden tests
- Frontend unit + E2E
- migration from empty + previous schema

修掉测试暴露的问题。禁止为了绿测试降低安全或删业务逻辑。记录实际执行结果。
