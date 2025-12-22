## 转发监控/统计（阶段3）

本目录的监控相关逻辑主要由 `forward_stats`（实时）与 `forward_traffic_history`（历史）两部分组成：

- Agent 周期性上报 `forward_stats`（连接数、iptables 字节计数、去抖后的实时 bps、链路状态、活跃中继节点、节点延迟 map 等）
- Server 接收后写入 `forward_stats`，并：
  - 以入口节点的统计作为规则聚合字段（`forward_rules.total_*`）
  - 将本次采样与上一条采样做差，按分钟桶把增量累加到 `forward_traffic_history`
- Server 每天会执行一次历史维护任务（自动去重且避免并发重入）：
  - 30 天以上的数据聚合到小时级
  - 1 年以上的数据聚合到天级
  - 3 年以上的数据自动清理

前端监控页通过：

- `GET /api/v1/forwards/:id/stats` 获取实时 stats + 历史增量（用于趋势/图表）
- `GET /api/v1/forwards/:id/topology` 获取拓扑与当前活跃节点信息
