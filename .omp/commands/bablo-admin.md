---
description: 实现管理员后台：用户、资源、路由、调度、价格、账本、统计、审计
---

实现生产管理员前端，优先可用性和可诊断性，不做炫技 UI。

至少包含：
- Dashboard：趋势、Token、费用、成功率、TTFT/延迟、错误、top users/models/providers。
- Users：状态、角色、MFA、余额、Key、消费、管理员调账入口。
- Credentials：Provider、资源类型、健康、quota window、cooldown、最近错误、启停；绝不展示完整 secret。
- Models/Routes：public model、target、priority/weight、pool、商业策略、dry-run 路由预览。
- Scheduler：策略配置、decision log、fallback、候选排除原因。
- Pricing：版本、未来生效、缺价检查。
- Payments/Ledger：订单、回调事件、入账、退款、对账。
- Usage/Requests：多维筛选与 request trace。
- Audit：管理员操作。
- System：CPA SDK version、DB/Redis、workers、build version，不暴露 secret。

所有危险操作需二次确认；余额/价格/凭证/路由修改展示影响范围并写 audit。前端权限只能改善 UX，后端必须再次鉴权。
