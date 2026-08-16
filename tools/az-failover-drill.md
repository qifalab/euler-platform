# 同城双 AZ 故障切换演练 Runbook (M-6.3, 00§4.4 P2, 09-roadmap §4.3 M-6)

> 验收门禁 B3: 可用区级故障切换 RTO ≤ 5min. 本文件是 B3 的演练载体,每季度执行一次,
> 演练结论归档(08-devops-delivery §故障管理). 与一期 source-only 口径一致: 本 runbook
> 描述性 + 静态,不在本机执行(无集群),但拓扑与步骤与已部署 manifest 一一对应.

---

## 1. 演练目标

证明 P2 同城双 AZ 拓扑满足三条承诺 (00§4.4):

1. **管控面无状态服务双 AZ 对等** — 摘除任一 AZ,业务不中断 (RTO ≤ 5min).
2. **MySQL MGR 单主跨 AZ** — 主 AZ 故障,从节点选举接管 (04§6.9).
3. **Redis/Kafka/ClickHouse 副本跨 AZ** — 客户端容忍单 AZ 副本全失.

以及一条**非目标**的显式约束: MySQL MGR 在 2 AZ 下 (2,1) 分布**只**能在较轻 AZ 侧演练 —
完整"任一 AZ 皆存活"需 3 AZ,属 P3 (见 §5 与 pkg-go/topology.SurvivesAZLossMGR).

## 2. 演练前清单

| # | 检查项 | 通过标准 | 查询 |
|---|---|---|---|
| P1 | 双 AZ 节点就绪 | 两个 `topology.kubernetes.io/zone` 均有 ≥3 业务节点 | `kubectl get nodes -L topology.kubernetes.io/zone` |
| P2 | 中间件副本跨 AZ 分布 | Kafka 3 broker 各 1 AZ;Redis 每 shard master/replica 异 AZ | `kubectl get pods -n mw-mq,mw-db,mw-cache -o wide` |
| P3 | MySQL MGR 集群健康 | group_replication_member_status 全 ONLINE,1 primary | `SELECT * FROM performance_schema.replication_group_members;` |
| P4 | 备份就绪 | 当日 XtraBackup 全备成功 + binlog 归档延迟 < 5min | SRE 备份看板 (04§6.9) |
| P5 | 受影响资源台账可查 | `v_resource_by_az` 对演练 AZ 有预期行数 | `SELECT * FROM v_resource_by_az WHERE az_id='cn-north-1-<drill-az>';` |
| P6 | 告警通道验证 | 对客告警通道可达 (svc-notify 信任通道不降级) | 一次测试通知 |

演练 AZ 选择: **较轻侧** (MySQL MGR 的 (2,1) 中 1 节点侧,见 §5). 通告: 演练前 24h 内部通告,
**不对客公告** (AZ 级切换是 SLA 内行为,不是事故).

## 3. 演练步骤 (RTO 计时从 T0 开始)

### 3.1 T0: 摘除演练 AZ

```
# 标记演练 AZ 节点为不可调度 + 排空 (drain)
DRILL_AZ=cn-north-1-a   # 示例;实际取较轻侧
for n in $(kubectl get nodes -l topology.kubernetes.io/zone=$DRILL_AZ -o name); do
  kubectl cordon $n
  kubectl drain $n --ignore-daemonsets --delete-emptydir-data --force --timeout=120s
done
```

### 3.2 T0+~1min: 验证管控面 (无状态服务)

| 组件 | 期望 | 验证 |
|---|---|---|
| APISIX | 摘除 AZ 副本被剔除,剩余 AZ 继续服务 | `curl -s console.starcloud.cn/healthz` 200 |
| svc-iam/order/billing/... | replicas 跨 AZ,topologySpreadConstraints 重新均衡 | `kubectl get deploy -A -o wide` (DrainAZ 侧 Pod Terminating→Pending 在存活 AZ 重建) |
| console-bff | 请求 503 应为 0 | BFF 错误率看板 |

**判定**: 任一无状态服务在 T0+5min 仍无可用副本 = RTO 失败,进 §4 回滚.

### 3.3 T0+~2min: 验证 MySQL MGR (有状态)

```
# MGR 成员状态: 演练 AZ 的节点应 OFFLINE/UNREACHABLE,存活侧选举新 primary
SELECT member_host, member_state, member_role
FROM performance_schema.replication_group_members;
# 期望: 2 节点 ONLINE (其中 1 PRIMARY),1 节点 UNREACHABLE
```

| 检查 | 通过标准 |
|---|---|
| 新 primary 选举 | 存活侧出现 1 PRIMARY (RTO ≤ 30s) |
| 读写恢复 | `INSERT` 测试行成功 (MGR majority = 2/3,1 节点丢失仍可写) |
| RPO | binlog 连续,无 gap (04§6.9 PITR 保障) |

**关键约束**: 2 AZ 的 MGR (2,1) **只**在 1 节点侧演练可存活 — 若在 2 节点侧演练,丢失后
仅剩 1 节点 < majority(2),写停摆. 故 §2 P3 选较轻侧. 这是 pkg-go/topology
SurvivesAZLossMGR 编码的不变式 (见 §5).

### 3.4 T0+~3min: 验证 Redis Cluster / Kafka

| 组件 | 期望 | 验证 |
|---|---|---|
| Redis | 每 shard 的 master 若在演练 AZ,replica 在存活 AZ 提升为主 | `redis-cli CLUSTER NODES` (每 shard 仍有 1 可用节点) |
| Kafka | 演练 AZ broker 离线,ISR 仍含存活侧副本 (min.insync.replicas=2) | `kafka-topics --describe`;leader 选举完成 |
| 客户端容忍 | 无 "cluster down" 错误 | 业务错误率看板 |

### 3.5 T0+~4min: 验证数据面资源

```
-- 台账: 演练 AZ 的 RUNNING 资源是否正确反映为受影响
SELECT product_code, COUNT(*) FROM v_resource_az_failover_impact
WHERE az_id='cn-north-1-a' GROUP BY product_code;
-- HA 规格 (CrossAZ=true) 的 ZONAL 资源: 由存活 AZ 副本接管,无需人工
-- 非 HA (CrossAZ=false): 进降级清单,演练后按 SLA 恢复
```

### 3.6 T0+5min: RTO 判定

**通过标准**: T0+5min 内,管控面全可写、MySQL MGR 有 primary、Kafka ISR 健康、
对客错误率 < SLA 阈值. 任一未达标 = 演练失败,进 §4.

## 4. 回滚 (演练失败或演练结束)

```
# 解除 drain,恢复演练 AZ 节点调度
for n in $(kubectl get nodes -l topology.kubernetes.io/zone=$DRILL_AZ -o name); do
  kubectl uncordon $n
done
# 等待 Pod 重新均衡 (topologySpreadConstraints 自动回填)
# MySQL MGR: 旧节点重新 ONLINE 为 SECONDARY;若曾在非演练侧提升 primary,
# 视情况决定是否回切 (避免回切抖动,通常保留新 primary)
```

## 5. 拓扑不变式 (为何只在较轻侧演练)

`pkg-go/topology` 编码的 MGR 存活数学 (SurvivesAZLossMGR):

- 3 节点 MGR 跨 2 AZ 均匀分布 (2,1). majority = floor(3/2)+1 = 2.
- 丢 1 节点侧 → 剩 2 节点 = 2 ≥ majority ✓ 存活.
- 丢 2 节点侧 → 剩 1 节点 = 1 < majority ✗ 写停摆.
- 故 2 AZ MGR **不满足"任一 AZ 皆存活"**;只有 3 AZ (每 AZ 1 节点) 才满足,属 P3.

这是 P2 的**显式边界**,不是缺陷: 00§4.4 的 P2 承诺是"同城双 AZ 双活" + "MGR 单主跨 AZ",
而"任一 AZ 皆可丢"的完整 MGR 存活需 3 AZ. 演练选择较轻侧 (1 节点),证明**该侧**故障可恢复;
较重侧 (2 节点) 故障的概率更低 (需该 AZ 整体宕机),且其恢复路径是 §4 uncordon + MGR 重新 ONLINE,
RTO 更长 (业务可读不可写期间靠 Binlog 补偿).

验证数学的工具: `pkg-go/topology` 的 `TestSurvivesAZLossMGR` + `TestCanSatisfy` 断言上述边界.

## 6. 演练后归档

- **结论**: 通过/失败 + RTO 实测值 + 异常项.
- **台账快照**: 演练前后 `v_resource_by_az` 行数对比 (验证无数据丢失/错位).
- **复盘**: 失败项进 SRE 故障看板,下次演练前闭环 (08§故障管理).
- **更新本 runbook**: 若拓扑变化 (如 P3 扩 3 AZ),更新 §2/§3 的 AZ 选择逻辑.

## 7. 交叉引用

- 拓扑事实源: 00-overview.md §4.4 P2; 09-roadmap.md §4.3 M-6.
- MySQL HA: 04-middleware-infrastructure.md §6.9 (MGR 单主,RTT<2ms,否则降级半同步).
- 中间件 manifest: deploy/gitops-manifests/platform/middleware/{kafka-kraft,mysql-mgr,redis-cluster}.yaml.
- 拓扑不变式: pkg-go/topology (SurvivesAZLossMGR / PlacementReport).
- 台账视图: services/svc-orchestrator/sql/V2__resource_db_az_topology.sql (v_resource_by_az, v_resource_az_failover_impact).
- 校验门禁: tools/check-topology-spread.py (CI 中 ensure stateful 工作负载携带跨 AZ 意图).
