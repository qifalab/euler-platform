# 双地域容灾切换演练 Runbook (M-11.1, 00§4.5 P3, 09-roadmap §5.2 M-11)

> 验收门禁 C1(09§5.3): 核心链路地域级故障切换演练通过; 全局服务(IAM/计费)双活或
> 主备自动切换. 验收 M-11: RPO ≤ 5min / RTO ≤ 30min 核心链路. 本文件是 C1 的演练
> 载体,每季度执行一次,结论归档(08§故障管理). 与 M-6.3 的 az-failover-drill.md
> 分层: 那是"同城双 AZ 摘一侧"(P2), 这是"主地域整体失效, 异地冷备接管"(P3).
>
> source-only 口径: 描述性 + 静态,不在本机执行(无集群); 拓扑与步骤与已部署
> manifest / pkg-go/multiregion 一一对应.

---

## 1. 演练目标

证明 P3 两地三中心拓扑满足 (00§4.5):

1. **异地冷备可接管** — 主地域(cn-north-1)整体不可用时, 异地(cn-east-1)管控面
   冷转热 + DNS 切换, RTO ≤ 30min.
2. **核心账务 RPO ≈ 0** — 账务 binlog 准实时同步, 核心链路 RPO ≤ 5min
   (09§5.2 M-11 口径; 账务级 RPO≈0 见 00§4.5).
3. **全局服务主备切换** — IAM/计费主备自动切换 (C1), 账号体系是全局域.
4. **Kafka 异地重建** — 不做跨城镜像, 异地按 cloud.* 规范重建 topic (00§4.5).

以及一条**非目标**的显式边界: P3 **不承诺异地多活写** (00§4.5). 异地是只读/灾备,
演练只验证"主挂→备接管", 不验证"主备同时写" —— 那是单元化架构, 另行立项.

## 2. 演练前清单

| # | 检查项 | 通过标准 | 查询 |
|---|---|---|---|
| P1 | 异地中间件就绪 | cn-east-1 的 MySQL binlog 副本 / MinIO Replication 目标在线 | `kubectl get pods -n mw-* --context cn-east-1` |
| P2 | 复制水位健康 | ledger-binlog lag ≤ 5s; object-storage lag ≤ 1h | `SELECT * FROM region_replication_status WHERE region='cn-east-1'` (V3) |
| P3 | 冷备管控面在位 | 全局+地域服务 replicas=0 (冷备), 可一键扩容 | `kubectl get deploy -A --context cn-east-1` |
| P4 | 地域台账可查 | `v_resource_by_region` 对 cn-north-1 有预期行数 | `SELECT * FROM v_resource_by_region WHERE region='cn-north-1'` |
| P5 | DNS 切换路径就绪 | 权威 DNS 已配置 cn-east-1 的接管记录(预置不启用) | DNS 服务商控制台 |
| P6 | 告警通道验证 | 对客告警通道可达(svc-notify 信任通道不降级) | 一次测试通知 |

演练通告: 演练前 24h 内部通告, **不对客公告**(地域级切换是 SLA 内行为, 不是事故).

## 3. 演练步骤 (RTO 计时从 T0 = 主地域判定不可用开始)

### 3.1 T0: 宣告主地域不可用

```
# 演练口径: 主地域整体失效(机房级供电/网络中断). 不实际摘除主地域,
# 而是"冻结主地域写入 + 阻断其出口", 模拟失效. 实操:
#   - 主地域 APISIX 上游摘除 / 主地域数据库置 read-only
#   - 宣布 T0, 启动 cn-east-1 接管
```

### 3.2 T0+~5min: 异地管控面冷转热 (00§4.5 "管控面冷转热")

```
# 冷备服务 replicas 0 → N (svc-iam/svc-billing 全局主备切换 C1; 地域服务同步拉起)
kubectl --context cn-east-1 scale deploy svc-iam svc-billing --replicas=3
kubectl --context cn-east-1 scale deploy svc-order svc-payment svc-catalog --replicas=3
# 期望: /healthz 就绪, Nacos 注册完成
```

**判定**: 任一全局服务(IAM/计费)在 T0+15min 仍不可写 = RTO 失败, 进 §4 回滚.

### 3.3 T0+~10min: 验证 RPO (核心账务不丢)

```
-- 账务 binlog 复制水位: lag 应 ≤ 5s (RPO ≤ 5min, 09§5.2 M-11)
SELECT channel, last_replicated_at, lag_ms
FROM region_replication_status WHERE region='cn-east-1';
-- 期望: ledger-binlog lag_ms ≤ 5000
```

| 检查 | 通过标准 |
|---|---|
| ledger-binlog RPO | lag ≤ 5min (账务级目标 RPO≈0) |
| object-storage RPO | lag ≤ 1h (异步通道) |
| 账务对账 | 切换窗口内已提交订单在主备两侧账本一致(无未解释差异) |

### 3.4 T0+~15min: DNS 切换 (00§4.5 "DNS 切换")

```
# 把公网 API/控制台解析切到 cn-east-1 入口
# 期望: TTL 内生效; OpenAPI + 控制台可登录、可询价、可下单
curl -s https://console.euler.emoera.com/healthz   # 200
curl -s https://api.euler.emoera.com/healthz       # 200
```

### 3.5 T0+~20min: 验证 Kafka 异地重建 (00§4.5)

```
# 不做跨城镜像: cn-east-1 的 Kafka topic 按 cloud.* 规范重建(重建 = 新集群从
# 空 topic 起步, 消费方幂等重放, 不是复制主地域的 offset)
kafka-topics --bootstrap-server cn-east-1-kafka:9092 --list
# 期望: cloud.trade.order.event / cloud.metering.usage.raw 等核心 topic 已存在
```

### 3.6 T0+30min: RTO 判定

**通过标准**: T0+30min 内, 登录/下单/出账链路在 cn-east-1 全可写, 核心账务
RPO ≤ 5min, 全局服务主备切换完成. 任一未达标 = 演练失败, 进 §4.

## 4. 回滚 (演练失败或演练结束)

```
# 反向 DNS 切换回 cn-north-1, 主地域恢复写入
# 异地冷备缩回 replicas=0 (冷备状态, 成本水位)
kubectl --context cn-east-1 scale deploy svc-iam svc-billing svc-order svc-payment svc-catalog --replicas=0
# 主地域 binlog 恢复: 若演练期间主地域曾冻结写入, 解除 read-only 并补偿
```

## 5. 拓扑不变式 (为何是"主备"而非"多活")

`pkg-go/multiregion` 编码的 P3 边界 (00§4.5):

- **异地不承担写入** (`RegionRole.Writable`): STANDBY 只读/灾备. 主备切换是
  "主挂→备接管", 不是"主备同时写". 双写意味着跨地域分布式事务与一致性和解,
  即单元化架构 —— 00§4.5 明确"另行立项".
- **Kafka 是重建不是复制** (`ClassifyState` → REBUILT): 跨城 Kafka 镜像会把
  topic 契约悄悄分叉(offset/分区/保留期漂移); 异地重建 + 消费方幂等是 P3 的
  显式选择.
- **RPO 分通道** (`ReplicationChannel.MeetsRPO`): 账务 binlog 准实时(≈0),
  对象存储异步(小时级). 不同数据不同 RPO, 不搞"一刀切强一致".
- **RTO 单值** (`multiregion.MaxRTO` = 30min): 冷转热 + DNS 切换的预算.

验证数学的工具: `pkg-go/multiregion` 的 `TestValidateRejectsStandbyTooClose` +
`TestMeetsRTO` 断言上述边界.

## 6. 演练后归档

- **结论**: 通过/失败 + RTO 实测 + 各通道 RPO 实测 + 异常项.
- **台账快照**: 演练前后 `v_resource_by_region` 行数对比(验证无数据丢失/错位).
- **复制水位快照**: `region_replication_status` 演练前后 lag 对比.
- **复盘**: 失败项进 SRE 故障看板, 下次演练前闭环(08§故障管理).
- **更新本 runbook**: 若拓扑变化, 更新 §2/§3 的接管步骤.

## 7. 交叉引用

- 拓扑事实源: 00-overview.md §4.5 P3; 09-roadmap.md §5.2 M-11 / §5.3 C1.
- 多地域模型: pkg-go/multiregion (Region/Classify/ClassifyState/MeetsRPO/MeetsRTO).
- 复制水位台账: services/svc-orchestrator/sql/V3__resource_db_region_topology.sql.
- 异地对象存储复制: deploy/gitops-manifests/platform/middleware/minio-replication.yaml.
- 异地冷备 override: deploy/gitops-manifests/envs/cn-east-1/prod/values-overrides/.
- 同城 AZ 级演练(分层参照): tools/az-failover-drill.md (M-6.3).
