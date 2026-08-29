---
description: 实现面向 Bablo 运营与排障的多维统计、聚合、排行与查询 API
---

实现统计系统，目标是让运营者不需要直接查 SQL。

## 必须可筛选维度

时间范围、user、api_key、public model、resolved model、provider、credential、route、endpoint、status/error class、stream/non-stream。

## 指标

请求数、成功/失败、input/output/cache/reasoning token、总 token、费用/收入、TTFT、总延迟、TPS（可可靠计算时）、429/5xx、fallback 次数、scheduler selection、活跃用户、钱包消费。

## 页面/接口

- 全局概览与趋势
- 模型统计
- Provider/credential 统计
- 用户统计与消费排行
- Key 明细
- 错误分析
- Scheduler 决策分析
- Usage request trace
- 充值/收入/消费对账

## 性能

先使用 PostgreSQL 正确索引 + 日/小时 rollup/materialized aggregate；没有真实性能证据不得引入 ClickHouse/Kafka。Raw Usage 保留策略与聚合保留策略可配置。

所有统计值必须能追溯到 Usage/Ledger，不允许 dashboard 自己用另一套计费公式。建立聚合与 raw 对账测试。
