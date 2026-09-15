# StarCloud Platform (Alpha)

自研公有云平台的参考实现：从官网注册到资源释放的完整商业闭环
（注册 → 实名 → 充值/下单 → 开通 → 计量 → 出账 → 欠费治理 → 释放），
以及控制台/官网前端、OpenAPI 生态（签名/SDK/Explorer）、双 AZ 拓扑与
告警/稳定性平台。架构规格见 `docs/architecture/`（00–11 章，约束性事实源）。

**Alpha 状态**：全部 19 个服务可运行、可测试，并已接入真实 MySQL 持久化
（33 个迁移、6 个 schema、86 张表，幂等可重跑）；资源履约仍走
`provision.Driver` 契约的 Mock/KubeVirt-in-process 实现，Kafka 事件与
ClickHouse 下沉是 phase-3 接线点（代码内已留锚点）。

---

## At a glance

| 维度 | 现状 |
|---|---|
| 可售产品 | 15 个：SCVPC / SCECS / SCBS / SCOSS / SCRDS / SCMON / SCEIP / SCECI / SCLB / SCAS / SCBACKUP / SCREDIS / SCKAFKA / SCLOG + RAM 子账号 |
| 计费形态 | 包年包月 / 按量 / 资源包抵扣 / 抢占式浮动价（定价粒度是目录行，不是代码分支——按秒计费即一例） |
| 服务 | 19 个 Go 服务（stdlib HTTP，`cmd/server` 可直接运行），端口 91xx/92xx |
| 领域库 | `pkg-go` 38 个包、70 个测试文件、621 个测试函数（含资金双花、配额超卖、死锁回归） |
| 持久化 | `SC_DB_DSN` 一键切换：真 MySQL（6 schema/86 表）或零依赖内存模式 |
| 合同 | `proto-hub` 17 个 proto 包 + 破坏性变更 CI 门；CPS1 签名一份实现三处消费（SDK/Explorer/网关），7 条金样本向量钉死跨语言一致 |
| 前端 | Vue + Wujie 微前端：官网 + 控制台基座 + 15 个子应用（`platform/frontend`） |

## Quick start

前置：Go ≥ 1.22（以 `pkg-go/go.mod` 为准）、MySQL 8.0+/9.x（本地开发账号需
CREATE/DROP DATABASE 权限）、Node 18+ / pnpm（仅前端）。

### 1. 建库（真实持久化）

```sh
# 迁移工具按 migrations.json 应用全部服务 DDL；重复执行是安全的。
cd tools/sqlmigrate
export SC_DB_DSN='user:password@tcp(127.0.0.1:3306)/'
go run .                # 全量应用；-dry-run 预览；-only svc-order 单服务
```

33 个迁移覆盖 6 个 schema：`account_db`(20 表) / `trade_db`(33) /
`resource_db`(11) / `support_db`(12) / `metering_db`(7) / `openapi_meta`(3)。
规则：**已应用迁移只增不改**——修正一律写新 Vn，幂等可重跑。

### 2. 运行一个服务

```sh
cd services/svc-order
export SC_DB_DSN='user:password@tcp(127.0.0.1:3306)/'   # 不设 = 内存模式（demo/测试）
go run ./cmd/server
```

持久化是 **opt-in**：设了 `SC_DB_DSN`，状态落 `trade_db`，重启不丢；
未设则退回内存实现（`go test` 因此零外部依赖）。一个配置了 DSN 却连不上
的服务会**启动失败**——资金/凭证/审计类服务不允许"静默降级为遗忘"。

### 3. 测试

```sh
cd pkg-go && go test ./...                 # 无 DSN：纯内存，全绿
cd pkg-go && SC_DB_DSN=... go test ./...   # 有 DSN：SQL 适配器在真库上跑
cd services/svc-order && SC_DB_DSN=... go test ./...
```

SQL 适配器的测试跑在 **schema of record** 上（`pkg-go/storage/sqltest`
为每个测试拉起一次性 scratch schema 并应用该服务自己的 DDL 目录），
而不是测试私有建表——列名漂移、DECIMAL 精度、唯一键仲裁、CHECK 约束
这些 fake 无法暴露的问题在 CI 即被抓出。

### 4. 前端

```sh
cd platform/frontend && pnpm install && pnpm -r dev
# console-base :5173；子应用 :517x/518x/519x（vite 代理注入 X-Sc-Account-Id）
```

## Persistence model

- **切换**：每个服务在启动时经 `storage.MustOpenFor(ctx, "<schema>")` 选择
  后端；`EnsureMigrated` 保证"没跑过 sqlmigrate 的 schema"直接启动失败。
- **号段**：领域可见 id（订单号/工单号/规则 id…）由 `storage.Sequence`
  号段分配器发放（批量取段、进程内发号、DB 行 `FOR UPDATE` 抢段）；
  纯代理键则用 AUTO_INCREMENT（各 schema 的 V2+ 迁移已补齐）。
- **审计**：迁移工具在 `schema_migration` 表留痕；幂等键全部来自业务
  （`uk_*`），重试即更新而非重放。

## Repository layout

```
docs/architecture/     约束性架构规格（00–11 章）+实施回写（09 §4.0/§5.0）
proto-hub/             IDL 事实源；VERSIONING.md + breaking-change 基线
pkg-go/                领域库（38 包）：cps1/kms/accesskey/authz/totp/sts、
                       pricing/order/ledger/settlement/reservepack/spot/invoice/
                       resource/workflow/provision/ipam/quota、metering/billing/
                       anomaly、notify/audit/alertcenter、identifier/topology/
                       multiregion/event/errors/slo/chaos/release、storage(SQL 适配层)
services/              svc-* 19 个可运行服务 + rc-* CRD（控制器按契约实现）
sdk/python/            Python SDK（cps1 金样本回归）
sdk/terraform/         Terraform Provider 骨架（source-only）
platform/frontend/     官网 + Wujie 控制台（15 子应用）
platform/frontend/packages/sdk/   前端 SDK
deploy/gitops-manifests/  ArgoCD App-of-Apps；APISIX 网关；跨 AZ 中间件清单
tools/                 sqlmigrate + 仓库校验器（YAML/Lua/routes/proto/topology）
                       + AZ/跨区容灾演练 runbook + 等保三级 checklist
```

## Verification status

| 层 | 验证方式 |
|---|---|
| 领域库 | `go test`：621 个测试（资金/配额/工作流的并发回归；金样本向量） |
| SQL 适配器 | 真库测试：幂等 upsert、乐观锁冲突、DECIMAL 精度、并发死锁（配额首写路径 Error 1213 已根治）、跨重启重水化、双实现 parity |
| DDL | 33 个迁移真实应用 + 幂等重跑验证；迁移工具自身有 `-probe/-dry-run` 与 NULL 探测 |
| 服务 | 19/19 `go vet` + `go test`（有/无 DSN 双模式） |
| 签名链路 | svc-iam verify 线上 e2e（7 项）+ Python SDK 金样本逐字节 |
| 前端 | `pnpm -r typecheck` 全部通过（26 工程） |
| K8s/Helm/Vitess | 清单与 chart 结构校验 + CI 门（YAML/Lua/routes/proto-breaking/topology-spread）——**未实际部署** |
| `-race` | 本 Windows 宿主无 cgo，未跑；并发正确性由双花/超卖/死锁内容测试覆盖 |

## Alpha 已知边界

- **履约驱动**：`provision.Driver` 契约下为 MockDriver / in-process KubeVirt
  （VMDriver 语义已测试：欠费冻结停机不删盘、VMI Ready 起算计费、按秒计量）；
  集群部署换绑 client-go 适配器即可，业务代码零改动。
- **事件通道**：`pkg-go/event` 的 topic 常量与负载已定义，服务间仍以同步
  调用为主；Kafka 接线是 phase-3 首批工作。
- **审计/日志下沉**：审计事件全文与账单明细的 ClickHouse 归档在
  DDL/注释中留有位置，MySQL 只存链检查点与热数据。
- **多区域**：`pkg-go/multiregion` 模型与两地三中心 runbook 就绪，
  实际第二地域未点亮（P3 承诺"不承诺异地多活写"）。

## Docs

- `docs/architecture/00..11` — 平台规格（约束性）
- `docs/architecture/09-roadmap.md` §4.0/§5.0 — 二/三期实装回写
- `proto-hub/VERSIONING.md` — API 版本化与废弃策略
- `tools/az-failover-drill.md`、`tools/cross-region-failover-drill.md`、
  `tools/chaos-drill-runbook.md`、`tools/dengbao-level3-checklist.md`
- `platform/frontend/README.md` — 前端结构与子应用清单

## 不变量（改代码前先读）

以下不变量由测试钉死；改动即改变客户依赖的平台行为。

| 不变量 | 位置 | 为什么 |
|---|---|---|
| 金额定点，永不浮点 | `pricing.Amount` | 百万小时行上的浮点漂移使账单不可对账 |
| `paid → fulfilling` 是唯一开通触发 | `order` | 未支付订单不得创建资源 |
| 退款发生在资源释放之后 | `order.StartRefund` | 否则客户既持有钱又持有运行中的资源 |
| 余额不为负 | `ledger` | 透支是欠费状态，不是无授权授信 |
| 计费起点=首次 RUNNING，永不回拨 | `resource` / `svc-orchestrator` ledger | stop/start 不得重置计费时钟 |
| 释放前必须有可证明的送达警告 | `resource` + `notify` | 不得无留证销毁客户数据 |
| 信任关键通知绕过限流 | `notify` | 限流欠费警告违背其存在意义 |
| 配额计数含在途预留 | `quota` | 忽略它 = 两个订单抢最后一个名额 |
| 补偿逆序执行 | `workflow` | 配额必须在资源删除之后归还 |
| 审计链防篡改，链头存链外 | `audit` + `audit_chain_checkpoint` | 截断靠"存储的头与链不符"发现 |
| 未知 authz 条件 → deny | `authz` | 对不理解的输入 fail closed |
| 抢占回收需已送达的 5 分钟通知 | `provision.Preemptor` | 未警告过的东西不可回收 |
| 一份签名实现，三处消费 | `cps1` | SDK/Explorer/网关共享；Python 必须逐字节复现金样本 |
| proto 大版本内只增不破 | `proto-hub` | 字段号删除/复用是 CI 失败 |
| ZONAL 产品询价必须带 AZ | `svc-catalog` 门 | 在下单前拦下错误 placement，而非履约期 |
| 2 AZ 均布不必然活过 AZ 故障 | `topology.SurvivesAZLoss` | MGR 走多数派数学，(2,1) 只能承受轻侧丢失 |
| 结算分账之和恒等于总额 | `settlement` | 平台份额用减法导出，不允许第二次舍入 |
| 临时凭证必然过期 | `sts` | 零/负 TTL 在构造期拒绝 |
| 试用准入六规则定序，首个拒绝可解释 | `trial.Admit` | 说不出拒绝原因的拒绝无法对客解释 |
| 优惠券扣减定序 RATE→THRESHOLD→VOUCHER | `pricing` | 顺序可变的折扣使同一账单不可复现 |
