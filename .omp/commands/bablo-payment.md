---
description: 实现充值订单与生产支付适配器；支持参数指定 provider
---

实现自助充值。`$ARGUMENTS` 可指定支付 Provider，例如 `alipay`、`wechatpay`，可多个；未指定时先实现 Provider 抽象、兑换码/管理员充值，并选择项目计划上线的至少一个正式 Provider，选择前必须核对当前官方文档。

## Payment Domain

订单状态机至少：created -> pending -> paid/failed/expired/refunded/closed。订单号全局唯一；金额、币种、用户、provider、provider_trade_no 明确。

## Webhook

- 必须按 Provider 官方当前规范验签。
- 验证 merchant/app id、订单号、金额、币种、状态。
- 防重放；provider event/trade id 唯一。
- webhook 事务中只做必要状态和 outbox，耗时操作异步。
- 收到 paid 事件后以同一 idempotency key 写 wallet recharge ledger；重复通知不得重复到账。
- 客户端“支付成功”页面绝不作为入账依据。

## 安全与真实验证

不得伪造商户凭据。没有真实/沙箱凭据时，完成代码、fixture signature test、sandbox runbook，并把“真实商户端到端验证”列为 release blocker。

提供订单查询、过期关闭、退款/冲正接口与审计。更新支付文档和上线清单。
