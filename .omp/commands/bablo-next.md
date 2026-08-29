---
description: 读取实施状态并执行下一阶段，不重复已完成工作
---

读取 `docs/implementation-status.md`、ADR 和当前代码，确定下一项未完成且没有上游依赖阻塞的阶段，然后**实际执行这一阶段**。

推荐顺序：
plan -> bootstrap -> cpa -> data -> auth -> apikey -> models -> credentials -> router -> scheduler -> proxy -> usage -> billing -> payment -> quota -> stats -> admin -> user -> observability -> security -> tests -> loadtest -> deploy -> ci -> audit -> ship。

规则：
- 若 status 不存在，先执行等价于 `bablo-plan` 的工作并创建它。
- 已完成阶段不要机械重做；只在发现回归/缺口时修补。
- 若下一阶段被外部凭据阻塞，完成所有不依赖真实凭据的实现与测试，然后选择下一个可做阶段，并把真实 E2E 标为 blocker。
- 每次只聚焦一个主要阶段，避免单次改动失控。
- 完成后更新 status 并说明下一阶段。

附加要求：$ARGUMENTS
