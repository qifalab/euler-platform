# 混沌工程演练 Runbook (M-9.2, 08§9.5, 09-roadmap §5.2 M-9)

> 验收门禁 C2(09§5.3): 混沌演练每季度 ≥1 次; 全年 P0 ≤ 2 次. 本文件是混沌演练的
> 季度载体, 与 `pkg-go/chaos` 的纪律模型一一对应: 那 6 个必练科目 + prod 10 分钟
> 终止手段 + 限定爆炸半径 + 偏差 >50% 立项整改, 全部在代码里有断言, 本 runbook 是
> 把"何时打、怎么打、怎么判、怎么归档"落成可执行、可审计的流程.
>
> source-only 口径: 描述性 + 静态, 不在本机执行(无集群); 注入动作(杀 Pod/摘节点等
> K8s-native 手段)是执行者的职责, 本文件不重述注入工具. 分层参照: 本 runbook 是
> **故障注入纪律**(打什么), `az-failover-drill.md`/`cross-region-failover-drill.md`
> 是**故障场景验证**(AZ 级/地域级存活), 三者互补不替代.

---

## 1. 演练目标

把 08§9.5 的"混沌工程常态化"变成可重复的季度节奏:

1. **6 个必练科目全覆盖** — 每个科目在季度内至少演练一次(§2 科目表).
2. **staging 先行** — 任何科目先在 staging 排练, 再上 prod(prod 是"限定爆炸半径的
   真实彩排", 不是第一次碰).
3. **prod 可终止** — prod 演练必须有 10 分钟内的终止手段(`pkg-go/chaos.ProdAbortDeadline`),
   且声明爆炸半径(哪几个 Pod/哪个 broker/哪个 AZ).
4. **偏差闭环** — 实际恢复时间 > 期望 1.5 倍(`chaos.DrillResult.NeedsRemediation`,
   08§9.5 "偏差 >50% 立项整改")必须立项, 不静默归档.

## 2. 六必练科目 (08§9.5, `pkg-go/chaos.DrillKind`)

| # | 科目 | DrillKind 值 | 注入要点 | 期望恢复 |
|---|---|---|---|---|
| 1 | Nacos 脑裂 | `nacos-split-brain` | 摘除 Nacos 半数节点或制造网络分区 | 服务发现降级可用, 注册/订阅不中断 |
| 2 | Redis 主从切换 | `redis-failover` | 摘主节点 | 哨兵选主, 读写恢复; shard pair 跨 AZ 存活(见 M-6.2a) |
| 3 | MySQL 主库切换 | `mysql-primary-failover` | 摘主库 | MGR 选新主, 分库分表连接池恢复 |
| 4 | APISIX/etcd 自愈 | `apisix-etcd-self-heal` | 摘 etcd 从节点 / 重启 APISIX | 网关路由不丢, 证书/限流配置自愈 |
| 5 | Kafka broker 宕机 | `kafka-broker-loss` | 摘 1 个 broker | ISR 缩容, 计量数据不丢(消费方幂等重放) |
| 6 | 单 AZ 整体不可用 | `single-az-loss` | 摘一个 AZ 的全部节点 | 较轻侧 AZ 失效后存活(2 AZ MGR 边界见 M-6.3) |

> 科目清单是**封闭集合**: `chaos.DrillKind.Valid()` 拒绝未知名, 防止"自由发挥"
> 演变成不受约束的爆炸面. 新增科目必须先改 `pkg-go/chaos` 的常量表, 再进本 runbook.

## 3. 演练节奏与前置

- **频率**: 平台级季度 1 次(6 科目轮转, 每季度至少覆盖 6 科目各 1 次); 域内月度
  自演练(单科目).
- **时序**: staging 先跑同科目 → 结论通过 → 同科目 prod 彩排(prod 只做**已验证**的
  注入).
- **通告**: prod 演练前 24h 内部通告, **不对客公告**(SLA 内行为, 不是事故).

## 4. 演练前检查清单

| # | 检查项 | 通过标准 |
|---|---|---|
| P1 | 演练计划通过校验 | `chaos.DrillPlan.Validate()` 不报错(prod 必须有终止手段 + 爆炸半径) |
| P2 | 爆炸半径已声明 | prod 计划 `BlastRadius` 非空, 且写成可执行的白名单(如 "kafka-broker-1") |
| P3 | 终止手段就绪 | 10 分钟内可回滚注入(如恢复被摘节点/重连分区)的脚本或 playbook 已备好 |
| P4 | 监控就绪 | 演练科目对应的 SLO 燃烧率告警(见 `pkg-go/slo`)在线, 能在演练期间观测 |
| P5 | 归档位置就绪 | 结论回写本 runbook §6 + SRE 故障看板 |

## 5. 演练步骤 (每科目通用模板)

```
# 1. 基线: 记录该科目恢复的期望值(历史中位数), 作为 Expected
# 2. 注入: 按 §2 注入要点打故障(先 staging)
# 3. 计时: T0 = 注入完成; 记录系统自愈/人工处置到恢复的时间 = Actual
# 4. 判定: Actual vs Expected (见 §6 判定规则)
# 5. 恢复: 确认爆炸半径内的组件全部回到健康态, 无残留告警
```

**prod 专属**: 任何一步失控, 立即执行 10 分钟终止手段(`AbortDeadline` 上限), 判定
演练失败并进 §6 立项.

## 6. 判定与归档

判定规则与 `pkg-go/chaos` 对齐:

| 结果 | 判定条件 | 动作 |
|---|---|---|
| 通过 | `Actual ≤ Expected` (恢复不慢于期望) | 归档通过, 记录 Actual |
| 观察 | `Expected < Actual ≤ 1.5×Expected` | 归档通过但标注, 连续 2 次进整改观察 |
| 立项整改 | `Actual > 1.5×Expected` (`NeedsRemediation()` = true) | 立项整改, 下次演练前闭环(08§9.5) |

归档内容: 科目 + Stage + 爆炸半径 + Expected/Actual + 是否立项 + 整改项 ID.

## 7. 交叉引用

- 纪律模型: pkg-go/chaos (DrillKind/MandatoryDrills/DrillPlan.Validate/DrillResult.NeedsRemediation/ProdAbortDeadline).
- 场景演练: tools/az-failover-drill.md (M-6.3, AZ 级), tools/cross-region-failover-drill.md (M-11.1, 地域级).
- SLO/燃烧率: pkg-go/slo (08§10); 变更门禁: pkg-go/release (08§6).
- 事实源: 08-devops-delivery.md §9.5 (混沌工程), 09-roadmap.md §5.2 M-9 / §5.3 C2.
