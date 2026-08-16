# 11 裁决书:跨章冲突唯一事实源

> **文档定位**:本文件是架构委员会对前两轮评审发现的 40 条跨章冲突(C1–C10、S1–S30)作出的**唯一权威裁决**,是后续所有修复 agent 的**唯一事实源**。各章 fixer 必须以本裁决书为准回写;本裁决书与各章已指定的"事实源章"共同构成全书事实体系——当本裁决书与某章已锁定的事实源(如 04 为 Kafka 事实源、01 D0 为产品 code 事实源、01 D8 为欠费参数事实源、10-research 为选型事实源)一致时,直接执行;当本裁决书推翻某条推荐裁决时,以本裁决书的 `overrides` 说明为准。
>
> **版本**:v1.0(2026-08) · **状态**:架构委员会已裁决,进入传播修复阶段
>
> **裁决原则**:① 优先采纳各章已显式指定的"事实源章"结论,避免重定义;② 推翻推荐裁决仅在"采纳会与权威源(10-research 启示/各章已锁定事实源)产生阻塞性矛盾"时进行;③ 命名/标识/分片键/部署阶段等全局规范,以本裁决书 §3/§4/§5 汇总为准,各章统一引用。

---

## 1. 裁决原则与事实源归属

全书事实源按"谁更具体、谁更稳定"原则归属如下,各章 fixer 回写时以此为准:

| 事实域 | 唯一事实源 | 说明 |
|---|---|---|
| 对标启示/选型决策/兼容性坑清单 | **10-research-and-selection-decisions.md** | 全书"对标启示 N""选型坑清单 #N"等编号引用均指向本文件锚点(§3.4/§4.2/§4.4) |
| 产品 code/命名/品牌前缀 | **01-product-catalog.md §1.1 决策 D0** | `sc` 前缀体系(scecs/scoss/scrds…) |
| 一期产品集/批次排期/部署阶段映射/一期 GA 验收/一期规模基线 | **09-roadmap.md**(经本裁决书 §5/§4 修订后) | D-01(原编号 R-01)一期产品集被本裁决书推翻重定(见 C1+S1) |
| 欠费生命周期参数 | **01-product-catalog.md §5.4 决策 D8 参数表** | 宽限 24h/72h、锁定保留 30 天、包年包月保留 15 天、提醒 30/15/7/3/1、释放前 24h |
| Kafka topic 命名/分区/保留/环境隔离 | **04-middleware-infrastructure.md §5.4** | `cloud.{domain}.{aggregate}.{event}` 规范,topic 不含环境,集群隔离 |
| 服务清单/职责/语言栈 | **03-backend-services.md §4.0 服务总表** | `svc-{domain}` 命名;04 §4.3 为命名模式/Group/Data ID 事实源 |
| 分片键/分库分表数 | **04-middleware-infrastructure.md §6.3/§6.4** | account_id 单键;account_db 4×16、trade/resource/metering_db 8×16 |
| OpenAPI 域名与路由形态 | **04-middleware-infrastructure.md §3.2** | 一产品一子域名 + Action/日期型 Version(见本裁决书 S6 override 说明) |
| OpenAPI 签名算法 | **07-security.md §4.1** | CPS1-HMAC-SHA256 |
| SK 存储方式 | **07-security.md §2.2/§2.5** | KMS 信封加密(sk_cipher + sk_key_version) |
| 站点结构/子应用拆分 | **01-product-catalog.md D5/D9** | 六独立子域 + 按产品大类拆 7 品类子应用 |
| 令牌时效/Cookie 属性 | **本裁决书 S13** | 07 牵头,15min/7d/Lax(推翻 07 §2.4 的 12h/Strict) |
| 部署三阶段演进 | **00-overview.md §4.2**(P1/P2/P3) | 09 一期/二期/三期与本表映射(见 §5) |
| 一期规模假设基线表 | **09-roadmap.md §3.4**(本裁决书 §4 要求新增) | 各章容量估算统一引用 |

---

## 2. 逐条裁决表

### 2.1 一期 MVP 产品集与计费节奏

| issueIds | 裁决结论 | 事实源 | 涉及文件与改法 |
|---|---|---|---|
| **C1+S1** | 一期 MVP 可售产品集统一为:**IAM/计费骨架/商品化中台 + SCVPC(网络) + SCECS(云服务器 VM) + SCBS(块存储) + SCOSS(对象存储) + SCRDS(托管 MySQL) + SCMON(监控) + SCEIP(弹性公网 IP)**;**SCECI(弹性容器实例)后置二期**。理由:VPC 是 ECS 前置依赖、ECS 需块存储做系统盘、VM 是用户预期旗舰产品、ECI 依赖 K8s 调度难度高;同时满足 00§1.1 最小可售闭环(计算+存储+网络+托管数据库)与 01§3.2 第一批矩阵。**09 D-01(原编号 R-01)"ECI 替代 VM、VPC 后置"被否决。** | 01§3.2 第一批 + 00§1.1 | 09:改写 D-01/D-02/§3.1/§2.1 甘特/§3.3"产品×3"→"产品×7";09§3.2 决策 D-01 备选方案 A(VM+块存储+VPC)转为正选结论;01§3.2 维持(确认为事实源);02§1.2 MVP 子应用已含 console-rds/ecs/vpc 等,确认无需改;03§11 一期上线顺序补 rc-network/rc-database(现仅 rc-compute/rc-storage);00§1.1 确认维持 |
| **C6+S30** | 计费形态节奏统一:**一期 Day1 = 包年包月 + 按量(两形态);资源包 + 抢占式 后置二期**。免费试用 = 试用代金券,**一期落地**(由 svc-catalog 提供代金券最小实现);**满减/折扣券后置二期**。**01 D6 改为"Day1 支持包年包月+按量,资源包二期"**;**09 §4.1 与 §6.1 自相矛盾修正为:免费试用(代金券)一期内测上线,资源包二期**;03§4.2.1"优惠券后置"改为"代金券一期最小实现,满减/折扣券后置";00§1.2 计费四形态行改为"四形态模型 Day1 预留,抢占式售卖后置(见 01 D6)" | 01 D6/D7 | 01:改 D6/D7 表述;09:改 §4.1 免费试用表述、§6.1 timeline 免费试用条目、§4.3 M-4 资源包排期确认二期;03:改 §4.2.1;00:改 §1.2 照搬档计费四形态行 |

### 2.2 Kafka topic / 计量服务 / 命名 / 分片键

| issueIds | 裁决结论 | 事实源 | 涉及文件与改法 |
|---|---|---|---|
| **C2+S8** | Kafka topic 唯一事实源 = **04§5.3 的 `cloud.{domain}.{aggregate}.{event}` 规范**;topic 名**不含环境标识**,环境隔离一律靠**集群隔离**(否决 05 附录A 的 `prod.`/`staging.` 前缀方案);分区数以 04§5.4 清单为准(usage.raw=64)。各数据流统一命名:计量原始 `cloud.metering.usage.raw`、计量小时聚合 `cloud.metering.billing.event`、账单事件 `cloud.billing.account.event`、订单事件 `cloud.trade.order.event`、资源生命周期 `cloud.resource.lifecycle.event`、通知 `cloud.notify.message`、审计 `cloud.sys.audit.action`、告警 `cloud.sys.alert.event`。03/05/06/07/02 的 topic 表全部改为**引用 04§5.4 清单**而非各自定义 | 04§5.4 | 04:作为事实源,确认清单完整(已含 cloud.* 全量);03§7 topic 表(iam.event.v1/order.event.v1/metering.raw.v1/billing.charge.v1/resource.event.v1/audit.event.v1/notify.task.v1/workflow.task.v1 等)全部改写为引用 04 清单的 cloud.* 名;05§4.1 已引用 04(确认);06§9 topic 表(provision.task/provision.status/metering.raw.oss-bucket/metering.raw.host/agent.command/audit.agent.exec/infra.node.lifecycle/infra.etcd.backup)改写为引用 04 清单对应名(cloud.resource.provision.task/provision.status、cloud.sys.host.metrics、cloud.sys.agent.command、cloud.sys.audit.agent、cloud.sys.node.lifecycle、cloud.sys.etcd.backup);07§6.2 的 `sec.audit.events`/`sec.login-events`/`sec.authz.policy-changed`/`sec.threat.events` 改为 04 清单的 `cloud.sys.audit.action`/`cloud.user.login.event`/`cloud.sys.authz.policy.changed`/`cloud.sys.threat.event`;05 附录A"prod./staging. 前缀"删除并改为"集群隔离(见 04§5.3)";02§11 的 `web.rum.events` 改引用 04 清单 `cloud.sys.rum.event` |
| **C3+S7** | 计量聚合服务语言 = **Go**,统一服务名 **svc-metering**(归 03§4.0 服务总表,语言栈 Go);05 的 `metering-aggregator` 已改名 svc-metering 并标注 Go(05§5.1/§5.4 已确认);06§4.4 时序图计量服务标注为 svc-metering(Go)。理由:高频写入管道+数据接入型符合 03§2.2"吞吐和连接归 Go"口诀与 09 D-04(原编号 R-04)统一 Go 栈决策 | 03§4.0/§4.2.5 | 03:确认 svc-metering=Go(已符合);05:确认 svc-metering(Go)(已符合);06:§4.4 时序图/§4.1 范式图计量服务统一标注 svc-metering(Go),删除任何"计量服务(Java)"表述(若有) |
| **C4+S11** | 全局命名规范:服务名一律 `svc-{domain}`(svc-iam/svc-order/svc-billing/svc-orchestrator/svc-audit/svc-metering/svc-catalog/svc-quota/svc-workflow/svc-monitor/svc-notify/svc-ticket/svc-api-meta/**svc-payment**/svc-kms/svc-doc/svc-idgen/console-bff/site-bff/auth-console-bff),Nacos Group 一律用应用名(Group=服务名,如 Group=svc-order),**废止 `{DOMAIN}_GROUP` 形式**;接入层 BFF 用 `{场景}-bff`,数据面控制器 `rc-*`,告警组件 `alert-engine`/`alert-center`。03§4.0 服务总表为唯一事实源。04/06/07 中新出现的服务(kms-service→svc-kms、resource-center→svc-orchestrator、provision-bridge→rc-* 履约执行层、site-bff/svc-doc/svc-idgen/auth-console-bff)全部改为 svc-* 或规范名并补录进 03§4.0/04§4.3 补录清单。04 APISIX 路由与 `{GROUP}@@{serviceName}` 订阅串全部按 svc-* 重写。**注:支付服务规范名为 `svc-payment`(03§4.0 事实源),全书凡 `svc-pay` 均为简写别名,统一替换为 `svc-payment`** | 03§4.0 + 04§4.3 | 03§4.0:补录 svc-kms/svc-doc/svc-idgen/auth-console-bff/site-bff(语言栈标注),支付服务名锁定 svc-payment;04§4.3:已锁定 Group=应用名(确认);04§3.3 路由表/§3.8 示例已用 svc-* (确认);07§1.3:`iam-service`→`svc-iam`、`kms-service`→`svc-kms`、`audit-service`→`svc-audit`、`auth-console-bff` 保留(规范名);06:resource-center→svc-orchestrator(§4.0 已映射,确认)、provision-bridge→rc* 履约执行层(§4.0 已映射,确认);08§7.2:`ORDER_GROUP`/`BILLING_GROUP` 改为 Group=应用名(svc-order/svc-billing);00§3.2 应用视图 SVC_PAY/PAY/交易账务行的 `svc-pay` 改为 `svc-payment` |
| **C5+S12** | 租户标识字段名统一为 **account_id**,并在 00 附录A 术语表显式声明 `account_id ≡ uid ≡ user_id ≡ tenant_id`(全书凡出现 uid/user_id/tenant_id 指代租户的,改为 account_id 或标注等价)。分片键:账号/交易/资源/计量四库统一以 account_id 分片(**否决 05§3.1 的 region+account_id 组合路由**;资源元数据仍可携带 region_id 字段但不作分片键)。分库分表数以 04§6.3 为准:垂直拆四库(account_db/trade_db/resource_db/metering_db),水平 8 库×16 表起步(倍增扩容);09§3.4 的"MySQL 2 分片"改为对齐 8×16 起步口径(一期预算不足可按 account_db 2×16、trade_db 2×16 最小起步,逻辑库边界与分片键不变)。资源 ID 格式内嵌 2 位分片因子(见 C9) | 04§6.3/§6.4 + 00 附录A | 00附录A:新增 account_id 映射声明 + 全局标识规范(见 §3);04§6.3/§6.4:已锁定(确认);05§3.1:已对齐 account_id 单键、region 不作分片键(确认);03§6:已用 account_id(确认);06:product_instance 表废弃(见 S21),tenant_id 改 account_id;07:uid 改为 account_id(或标注 uid=account_id);09§3.4"MySQL 2 分片×主从"改为"MySQL account_db/trade_db 各 2×16 起步(对齐 04§6.3 8×16 基线,预算不足最小起步)" |

### 2.3 权威源引用与交叉引用修复

| issueIds | 裁决结论 | 事实源 | 涉及文件与改法 |
|---|---|---|---|
| **C7+S27(部分)** | 权威源 = 已存在的 **10-research-and-selection-decisions.md**。全书所有"对标启示 N/选型坑清单 N/选型决策表"引用统一指向 10-research 锚点(§3.4 启示、§3.5 清单、§4.2 选型总表、§4.3 环境隔离、§4.4 坑清单)。00§1.2"详见《01-product-catalog.md》"已改为指向 10-research(确认);各章引用若 10-research 缺对应锚点,由 10-research fixer 补全锚点。另:05§5.5.2/附录C 引用"00 支持体系分级"改指 **01§4.5 支持计划**;08§3.1"05§9 变更关联"改指 **08§9 故障流程**(05§9 是多租户监控隔离,无变更关联);04§6.10"03 附录 DDL 模板"由 03 fixer 在附录补 **DDL 评审模板** | 10-research | 00§1.2:已改(确认);10-research:补全被引用但缺失的锚点(核查各章引用的 §3.x/§4.x 是否存在);05§5.5.2/附录C:"《00-overview.md》支持体系分级"→"《01-product-catalog.md》§4.5 支持计划";08§3.1:"参见《05》第 9 节变更关联"→"参见本章第 9 节故障流程";03附录:新增 DDL 评审模板小节;04§6.10:引用指向 03附录新模板 |

### 2.4 部署演进与可用性/对账口径

| issueIds | 裁决结论 | 事实源 | 涉及文件与改法 |
|---|---|---|---|
| **C8+S15** | 部署演进统一为三阶段(以 **00§4.2 为基线**,00 为部署事实源):**P1 单 IDC 单可用区(0–9 月,即一期全程)、P2 同城双可用区双活(9–18 月,二期)、P3 两地三中心(18 月+,三期)**。一期(0–9 月)对应 P1 单机房单 AZ;**09§2.3/R12"一期双机房"修正为"P1 单机房,P2 起双机房"**;**09§4.3 M-6"同城三机房/三可用区"修正为"同城双 AZ(P2),三 AZ 不在本期规划,异地多活属 P3"**。09 一期/二期/三期 与 00 P1/P2/P3 建立映射表写入 09§2.3。阶段窗口统一:一期=9 个月(T0+6 月 GA 内测、T0+9 月 GA 正式) | 00§4.2 | 00§4.2:确认 P1/P2/P3(事实源);09§2.3:改"单地域双机房(同城)"→"P1 单机房单 AZ(对齐 00§4.2)",新增一期/二期/三期↔P1/P2/P3 映射表;09§4.3 M-6:改"同城三机房/三可用区"→"同城双 AZ(P2)";09 R12:改"一期至少同城双机房"→"一期 P1 单机房,P2 起双机房";04 P2 跨 AZ 设计:移除任何"三可用区"表述(确认 04§4.4 P2 图为双 AZ) |
| **S16** | 可用性数字区分"阶段承诺值"与"目标值":**一期 GA 门禁以 09 A4/00§4.2 的 控制面≥99.9%、对象存储数据面≥99.95% 为准**;**00§1.4 的 99.95%/99.99% 标注为 P2 阶段目标(非一期验收口径)**。消除 00§1.4 与 00§4.2 的同章内部矛盾 | 09 A4 + 00§4.2 | 00§1.4 可用性行:"一期验收口径"列改为"P2 阶段目标(一期验收口径见 09§3.5 A4:控制面≥99.9%/对象存储数据面≥99.95%)"；00§4.2 维持 P1=99.9%/P2=99.95%+99.99%；09§3.5 A4 维持(事实源) |
| **S17** | 对账差异口径统一:**商业验收表述为"无未解释差异"**(09 Gate 用语);**工程阈值 covered_ratio<100% 或差异率>0.1% 仅用于触发补数与产出差异明细**。00§1.4 措辞由"对账差异=0"改为"无未解释差异";05§5.5.2 的 0.5% 阈值改为"差异率>0.1% 触发补数,>0.5% 触发告警"分层;05§12 P1 出口标准"对账差异<0.1%"改为"无未解释差异(covered_ratio=100%)" | 09 Gate 用语 | 09§3.5 A2/§6.2 Gate:维持"对账零差异"语义改为"无未解释差异"(确认);00§1.4:"出账对账差异=0"→"出账无未解释差异";05§5.5.2:0.5% 单阈值改为分层(>0.1% 补数、>0.5% 告警);05§12 P1 出口标准:"对账差异<0.1%"→"无未解释差异(covered_ratio=100%)" |

### 2.5 全局标识规范

| issueIds | 裁决结论 | 事实源 | 涉及文件与改法 |
|---|---|---|---|
| **C9** | 00 附录A 术语表增设"全局标识规范"小节,冻结:① **主域名 starcloud.cn**(各章 cloud.example/xxx.com 统一替换);② **产品 code 前缀 sc**(scecs/scoss/scrds/scvpc/scbs/sceip/scmon/sceci/sccert 等);③ **资源 ID 格式 `{productCode}-{regionId}-{分片因子2位}-{随机8位}`**,如 `scecs-cn-north-1-01-a1b2c3d4`;④ **region 命名 cn-north-1/cn-east-1**(短横线风格,00 的 `cn-north1` 补横线);⑤ **品牌/平台前缀统一 sc**(02 的 `cldp`、07 的 `cps`/`CPSA` 改为 `sc`);⑥ **账号主键 account_id**(见 C5)。注:**OpenAPI 签名头前缀 `x-cps-` 保留为安全域专用头名**(与品牌前缀解耦,不冲突,见 S4) | 00 附录A(新增)+ 01 D0 | 00附录A:新增"全局标识规范"小节(含上述 6 项 + account_id 映射);00§4.1:`cn-north1`→`cn-north-1`;01§1.4:资源 ID 格式 `{productCode}-{regionId}-{12位随机}` 改为含 2 位分片因子格式(对齐 04§6.6);02:`cldp-frontend-platform`→`sc-frontend-platform`、`@cldp/*`→`@sc/*`、`--cldp-*` CSS 变量→`--sc-*`、`cldp:{appCode}:` 前缀→`sc:{appCode}:`;03:示例统一替换(见 S3);04:示例统一替换(见 S3/S6);06:示例 region `cn-north-1`(已符合)、cloud.platform 品牌标签保留(为 K8s 标签,不属品牌前缀);07:`cps`→`sc`、`CPSA`→`SC`、ARN `cps:ecs`→`sc:ecs`、Condition 键 `cps:SourceIp`→`sc:SourceIp`(但签名头 `x-cps-*` 保留) |
| **C10** | 00§3.2 应用视图服务清单改为直接引用 03§4.0 总表口径,服务数量写"**17 个核心服务(详见 03§4.0)**",删除虚构的"OpenAPI BFF",说明 OpenAPI 入口由 APISIX+svc-api-meta 承担 | 03§4.0 | 00§3.2:"约 20 个"→"17 个核心服务(详见《03-backend-services.md》§4.0)";删除"OpenAPI BFF"行;在"接入"分组说明"OpenAPI 入口由 APISIX + svc-api-meta 承担,不单设 OpenAPI BFF 服务" |

### 2.6 欠费生命周期 / 产品 code / 签名 / SK 存储 / OpenAPI 形态

| issueIds | 裁决结论 | 事实源 | 涉及文件与改法 |
|---|---|---|---|
| **S2** | 欠费生命周期参数以 **01 D8 参数表**为唯一事实源:宽限期 24h(大客户 72h)、停服锁定保留 30 天、包年包月到期保留 15 天、续费提醒 30/15/7/3/1 天、释放前 24h 终版通知。03§5.2/§5.4 状态机默认值与注释全部对齐 01 D8;02§7.4 释放交互文案对齐;所有参数声明为 Nacos 可配的产品级配置 | 01§5.4 D8 | 01§5.4:维持(事实源);03§5.2/§5.4:已声明"以 01 D8 为唯一事实源"并配 Nacos 键(确认);02§7.4:释放文案对齐"释放前 24h 终版通知"(确认) |
| **S3** | 产品 code/命名以 **01 D0 的 SC 前缀体系**为唯一规范:产品 code 全小写 sc 前缀,权限 action `scecs:CreateInstance`,资源 ID 前缀 `scecs-`(非 `i-`),错误码 `Quota.Exceeded.ScecsInstance`(非 IcsInstance),ARN `sc:ecs:...`(非 `cps:ecs`)。批量替换 03/04/07/02 中的 ics/ecs/cps:ecs 等示例 | 01 D0 | 03§5.1 资源注册表:`ics`→`scecs`、`i-`→`scecs-`、`Quota.Exceeded.IcsInstance`→`Quota.Exceeded.ScecsInstance`、`quota_ics_instance`→`quota_scecs_instance`、`i-cn1-...`→`scecs-cn-north-1-...`;03§6.2 resource_id 注释 `i-cn1-xxxx`→`scecs-cn-north-1-xxxx`;04§3.2/§3.8:已用 scecs/svc-scecs(确认);07§3.1/§3.2:`cps:ecs`→`sc:ecs`、`ecs:StartInstance`→`scecs:StartInstance`、ARN 示例 `cps:ecs:...`→`sc:ecs:...`、系统策略 `CpsECSFullAccess`→`ScECSFullAccess`;02§7.5:已用 `Scecs.QuotaExceeded.Instance`(确认) |
| **S4** | OpenAPI 签名算法唯一契约 = **07§4.1 的 CPS1-HMAC-SHA256**(SigV4 风格:Authorization 头/CanonicalRequest/分域派生密钥链/x-cps-* 头/body 哈希绑定)。03§9.2 签名节已重写为引用 07 CPS1 契约(确认);04§3.8 forward-auth 插件 request_headers 已改为 `x-cps-*` 头集(确认);SDK 生成与文档同步。注:`x-cps-` 头前缀为签名协议专用,与品牌前缀 `sc` 解耦,保留不改(见 C9) | 07§4.1 | 07§4.1:维持(事实源);03§9.2:已引用 07 CPS1(确认);04§3.8:已用 x-cps-* 头(确认) |
| **S5** | SK 存储统一为 **07 的 KMS 信封加密方案**:access_key 表结构以 07§2.2 为准(`sk_cipher` + `sk_key_version`,SK 可逆解密用于验签,二级缓存)。删除 03§6.1 的 `sk_hash` 单向哈希设计(03§6.1 已改为 sk_cipher,确认)。03§9.2 验签流程"按 AK 取 SK"保持(依赖可逆 SK,现已自洽) | 07§2.2 | 07§2.2:维持(事实源);03§6.1:已用 sk_cipher(确认);03§9.2:验签链路保持(确认) |
| **S6** | **OpenAPI 域名与路由形态裁决(部分推翻推荐)**:① **域名形态锁定"一产品一子域名 `{productCode}.api.starcloud.cn`"**(如 `scecs.api.starcloud.cn`)——采纳推荐;② **版本载体维持 04§3.2 现状锁定"RPC 风格 `Action` + 日期型 `Version` 参数,URI 不承载版本号"**(不使用 `/v1/` 前缀式版本)——**推翻推荐裁决的"URI 路径版本 /v1/"**。**推翻理由**:推荐裁决的 `/v1/` URI 版本与权威源 10-research 启示 5("控制面 OpenAPI 以命令式操作为主 CreateXxx/DescribeXxx/DeleteXxx,RPC 风格参数显式、签名简单、文档模板统一")及 03§9.1 已论证的"RPC 风格(Action+Version),与阿里云生态习惯对齐"产生**阻塞性矛盾**;且 04§3.2(本裁决书认定的事实源)已锁定"Action+日期型 Version,URI 不承载版本号",03§9.1/04§3.8 已按此实现。采纳 `/v1/` 将迫使 03§9.1 放弃 RPC 风格论证、与"对标阿里云"全书策略及 10-research 启示 5 冲突。回写:03§9.1 入口域名由 `api.{domain}` 改为产品子域名 `scecs.api.starcloud.cn`(版本载体维持 Action+日期型 Version);07§4.3 的 `api.<domain>.com/v1/{service}/*` 改为 `{productCode}.api.starcloud.cn` + Action/日期型 Version(去掉 `/v1/{service}` 路径段);04§3.2/§3.3 路由表维持现状(已符合) | 04§3.2(事实源)+ 10-research 启示 5 | 04§3.2/§3.3:维持(事实源);03§9.1:入口 `api.{domain}/?Action=...&Version=...`→`{productCode}.api.starcloud.cn/?Action=...&Version=...`(Version 维持日期型);07§4.3:`api.<domain>.com/v1/{service}/*`→`{productCode}.api.starcloud.cn` + Action/日期型 Version(去 /v1/{service} 段);00/01 无需改 |

### 2.7 前端站点 / 子应用 / 令牌 / 告警 / ES / MySQL HA / 履约链路

| issueIds | 裁决结论 | 事实源 | 涉及文件与改法 |
|---|---|---|---|
| **S9** | 站点结构裁定为**六独立子域**(以 01 D5 为准):www/console/docs/account/billing/ticket;**文档站域名 docs.**(否决 02 的 help.*);**账号中心 account**(02 的 passport→account);billing/ticket 为"独立站+控制台子应用"双形态;SSO 拓扑=account 签发根域 Cookie,各子域共享。02§1.2/§2.2 域名表已补全 account/billing/ticket 子域、docs 取代 help(确认) | 01 D5 | 01§4.1:维持(事实源);02§1.2/§2.2:已对齐(确认);02 passport 域名并入 account(确认) |
| **S10** | 控制台子应用拆分粒度统一为"**按产品大类合并**"(以 01 D9 为准,否决 02 的"一产品一仓库一子应用"):7 个品类子应用(console-compute/storage/network/database/middleware/monitor/security)+ account/billing/ticket 三个双形态站点(上限 10)。02§1.2 应用清单与仓库策略已重写为按大类(每子应用一仓库)(确认);01 D9"已对齐"表述改为事实 | 01 D9 | 01§4.3/D9:维持(事实源);02§1.2:已按大类重写(确认) |
| **S13** | 令牌时效与 Cookie 属性统一(07 牵头):**access_token 15 min、refresh_token 7 天、SameSite=Lax**(02 方案;**推翻 07§2.4 的 12h refresh 与 Strict**——12h 过短影响静默续期,Strict 阻断跨子域 SSO)。由 account 域 **svc-iam** 签发(统一 04§3.4 的"account-service 签发"为 svc-iam)。02§5.1/04§3.4/07§2.4 三处同步:15min/7d/Lax | 本裁决书(07 牵头) | 07§2.4 会话参数表:refresh_token 12h→7 天、SameSite Strict→Lax、session_index TTL 对齐为 7 天;07§2.4 备选/改选条件段同步修订;02§5.1:refresh 12h→7 天、SameSite Strict→Lax;04§3.4:access 15min+refresh 12h→15min+7 天、"account-service 签发"→"svc-iam 签发" |
| **S14** | 对客告警(云监控)实现以 **05 双栈方案**为准:租户 agent 直推 VictoriaMetrics(租户侧不部署 Prometheus),svc-monitor 只负责租户告警规则 CRUD 与查询代理(不再编译 Prometheus AlertManager 配置);对客告警由 alert-engine(Go)+alert-center(Java)走对客通道;大盘不对客用 Grafana。03§4.4.1 已重写(确认);alert-engine/alert-center 已补录进 03§4.0(确认) | 05§8/§9 | 05:维持(事实源);03§4.4.1:已重写(确认);03§4.0:已补录 alert-engine/alert-center(确认) |
| **S18** | ES 用途边界:trace 存储 = SkyWalking OAP 自带存储(**独立 trace-ES 集群,与搜索 ES 物理隔离**)。04§8.1 用途清单保持"ES 只承担三类搜索"(搜索 ES 集群),新增一句"trace 存储由 SkyWalking OAP 使用独立的 trace-ES 集群,与搜索 ES 物理隔离";05§2.1/§7.3/§10.4/§10.6 的"ES(搜索+trace)"改为"搜索 ES(搜索专用)+ trace-ES(OAP 专用,独立集群)",分别给容量(搜索 ES 3master+3data 8C32G/500GB;trace-ES 3 节点 16C/64G/2TB 保留 7–15 天)。04 容量规划补 trace-ES 条目 | 04§8.1(搜索 ES 边界)+ 05§7.3(trace-ES) | 04§8.1:新增 trace-ES 独立集群说明 + 容量条目(11.1 容量表补 trace-ES 行);04§10.1 部署形态表 ES 行补注"搜索 ES + trace-ES 双集群";05§2.1/§7.3/§10.4/§10.6:"ES(搜索+trace)"→"搜索 ES + trace-ES(独立集群)";00§4.3 拓扑 ES 节点补注"搜索 ES + trace-ES" |
| **S19** | 00 总览补入可观测完整栈(已部分完成):00§2.1 基础设施层与§3.3 数据视图已补入 SkyWalking OAP(trace 后端,trace-ES 存储)与 VictoriaMetrics(长期指标)(确认);与 04/05/09 的"OTel→SkyWalking OAP+VM"表述一致 | 00§2.1/§3.3 | 00§2.1/§3.3/§3.4:已含 SkyWalking OAP + VictoriaMetrics(确认);无需进一步修改 |
| **S20** | MySQL HA 统一为 **MGR 单主模式**(04§6.9 为事实源):每逻辑库 1 主 2 从 MGR,跨 AZ 部署(P2)。**00§4.4 P2 拓扑图改标注为"MGR 单主"替换"半同步复制"**;04§6.9 补说明"AZ 间 RTT<2ms 时用 MGR,否则降级半同步"(保留半同步为降级备选,非默认) | 04§6.9 | 04§6.9:维持(事实源)+ 补 RTT<2ms 否则降级半同步说明;00§4.4 P2 拓扑图/§4.4 要点 2:"半同步复制"→"MGR 单主(跨 AZ,AZ 间 RTT<2ms;否则降级半同步)" |
| **S21** | 履约链路统一(06§4.0 已收敛,确认):svc-orchestrator 为资源生命周期唯一所有者(含 resource_instance 台账,状态机唯一写入口);06 的 resource-center 并入 svc-orchestrator(同名);provision-bridge 收敛为 rc-* 履约执行层;下发通道 = gRPC 声明式下发(经 svc-workflow 派发步骤)→ rc-* 写 CR(**否决 06§9 的 `provision.task`/`provision.status` Kafka topic 作为主下发通道**,仅作异步回调与重试通道);资源台账表收敛为 03§6.2 的 resource_instance(**06§4.5 product_instance 表废弃**,状态机事实源=svc-orchestrator 而非 K8s phase;K8s phase 作为观测字段);01§6.2"调用产品 OpenAPI 开通"修正为"经 svc-orchestrator 编排下发"(06§4.0 口径说明已说明) | 03§6.2 + 06§4.0 | 03§6.2:维持(事实源);06§4.0:已收敛(确认);06§4.5:product_instance 表删除/改为引用 03§6.2 resource_instance;06§9 topic 表:provision.task/provision.status 改为引用 04§5.4 的 cloud.resource.provision.task/provision.status(并标注"仅作异步回调与重试通道,非主下发通道");06§6.3 驱动路由图:"ProvisionTask(Kafka)→provision-bridge"改为"gRPC ApplyResource→rc-* 履约执行层"(对齐 06§4.0);01§6.2:"调用产品 OpenAPI 开通资源"→"经 svc-orchestrator 编排下发";00§2.3.1:确认维持 |

### 2.8 规模基线 / ClickHouse / 审计 / 支持计划 / 文档 / 环境 / 余额

| issueIds | 裁决结论 | 事实源 | 涉及文件与改法 |
|---|---|---|---|
| **S22** | 建立"一期规模假设基线表"写入 **09§3.4**(09 为容量事实源),各章统一引用:租户数 1000、活跃用户 1 万、OpenAPI 峰值 QPS 2000(网关峰值=OpenAPI+控制台+BFF 合计 8000)、资源实例数 5 万、微服务数 17(一期,见 C10)、计量吞吐峰值 1250 条/s(均值 250)、APISIX 4 节点 4C8G、K8s 业务节点 6–10 台 32C128G(非 12–20 台 16C64G)。08§1.1"50+"与 05§10.1"60 个/约 20 个"改为"一期 17,二/三期扩至 50+";07§9 审计峰值 2 万/s 标注为"审计写入峰值(非网关 QPS)" | 09§3.4(新增基线表) | 09§3.4:新增"一期规模假设基线表"(含上述指标);09§3.4 K8s 业务集群规模"12~20 worker(16C64G)"→"6~10 worker(32C128G)";00§4.3 P1"6~10 台 32C128G"确认(对齐);00§4.3 APISIX 4 节点 4C8G 确认;03§10"OpenAPI 峰值 2000"确认;05§10.1"微服务 60 个/约 20 个"→"一期 17(见 09§3.4 基线表),二/三期扩至 50+";05§10.1 网关峰值 8000 标注"= OpenAPI 2000 + 控制台 + BFF 合计";05§10.5 计量吞吐"均值 250/峰值 1250"确认;08§1.1"50+ 个服务"→"一期 17,二/三期扩至 50+";07§9"审计峰值 2 万/s"标注"审计写入峰值(非网关 QPS)";07§9 鉴权"5k QPS"标注"鉴权目标(缓存命中后)" |
| **S23** | ClickHouse 节点数统一为 **6 节点(3 分片×2 副本)**,以 05/09 为准。00§4.3 P1 拓扑"ClickHouse ×3"改为"ClickHouse 3 分片×2 副本(6 节点)" | 05§6.5/09§3.4 | 00§4.3 拓扑图 CK 节点:"ClickHouse ×3"→"ClickHouse 3 分片×2 副本(6 节点)";00§4.3 起步容量段同步;05§6.5/09§3.4 维持(事实源) |
| **S24** | 审计留存期统一:**平台合规基线 ≥180 天(等保三级,热存 ClickHouse)+ MinIO 冷备**;售卖产品提供 **365 天/18 个月付费档**。03§4.4.3/05§11/07§6.2 三章同步表述 | 03§4.4.3/05§11/07§6.2 | 03§4.4.3:"TTL 180 天热、转 MinIO 冷备"补"付费档 365 天/18 个月";05§11:"audit.action→CK 长期保留 18 个月"改为"合规基线 180 天热存+MinIO 冷备,付费档 365 天/18 个月";07§6.2:"TTL 180 天(合规)/365 天(付费)"补"18 个月档"并标注"合规基线 180 天" |
| **S25** | 支持计划档数:**模型预留四档(免费/基础/商业/企业),一期先开放两档(基础/商业)**。00§1.2 改为"支持计划:模型四档,先开放基础/商业两档";01§4.5 保留四档对比但标注"一期开放基础/商业" | 00§1.2 + 01§4.5 | 00§1.2 裁剪表"支持计划(先两级:基础/商业)"→"支持计划:模型四档(免费/基础/商业/企业),一期先开放基础/商业两档";01§4.5 工单站"四档计划对比"补标注"一期开放基础/商业两档" |
| **S26** | 最低文档集锁定为**四篇(产品简介+计费说明+快速入门+API 参考)**。01§1.3 与§4.4 统一为四篇;09§3.5 A7 由"三篇"改"四篇";02§8.2"五槽"标注为"完整目标(含最佳实践/FAQ),最低门禁四篇" | 01§1.3/§4.4 | 01§1.3:"快速入门+API 参考+计费说明三篇最低"→"产品简介+计费说明+快速入门+API 参考四篇最低";01§4.4:已为四篇(确认);09§3.5 A7:"快速入门+API 参考+计费说明"→"产品简介+计费说明+快速入门+API 参考四篇";02§8.2:"每产品固定五槽"补标注"完整目标,最低门禁为四篇(产品简介+计费说明+快速入门+API 参考)" |
| **S28** | Nacos 环境 namespace 统一为 **dev/staging/prod 三套**(联调合并入 dev,删除 test/unit/dev-test)。03§2.3.1/04§4.3/08§7.2 三章同步;10-research§4.3 同步 | 10-research§4.3 + 04§4.3 | 03§2.3.1:"dev/test/staging/prod"→"dev/staging/prod(联调合并入 dev)";04§4.3:已含 dev/test/staging/prod,改"dev/test/staging/prod"→"dev/staging/prod(MVP dev 合并原 test)";04§2.2 环境拓扑表同步;08§7.2:"dev/staging/prod"确认(已符合);10-research§4.3:"dev/test/staging/prod"→"dev/staging/prod" |
| **S29** | 余额字段归属:**balance(现金余额)与余额流水从 03§6.1 account 表移除**,归入 billing/trade 域(与 04§6.3 trade_db ledger 对齐,account_id 关联)。03 account 表仅保留身份属性(account_id/名称/状态/RAM 关联等) | 04§6.3 | 03§6.1 account 表:删除 `balance` 列(及"现金余额"注释),改注"balance 与余额流水归 trade_db ledger(见 04§6.3)";04§6.3:确认 trade_db 含 ledger(已符合) |

---

## 3. 全局命名/标识规范汇总

> 本节是 C5/C9/S3/S4 命名裁决的汇总速查,各章 fixer 统一引用。

| 规范项 | 取值 | 示例 | 事实源 |
|---|---|---|---|
| 主域名 | `starcloud.cn` | www.starcloud.cn / console.starcloud.cn / scecs.api.starcloud.cn | 01 D0 + 00 附录A |
| 品牌/平台前缀 | `sc`(替代 cldp/cps/CPSA) | sc-frontend-platform、@sc/ui、--sc-* CSS 变量、sc:ecs ARN | 00 附录A |
| 产品 code 前缀 | `sc` + 品类缩写(全小写) | scecs、scoss、scrds、scvpc、scbs、sceip、scmon、sceci、sccert | 01 D0 |
| 服务名 | `svc-{domain}`(统一 Go) | svc-iam、svc-order、svc-billing、svc-metering、svc-orchestrator、svc-kms | 03§4.0 |
| 接入层 BFF | `{场景}-bff` | console-bff、site-bff、auth-console-bff | 03§4.0 + 04§4.3 |
| 数据面控制器 | `rc-*` | rc-compute、rc-storage、rc-network | 03§4.0 + 06§4.0 |
| 告警组件 | alert-engine(Go)/alert-center(Go) | — | 03§4.0 |
| Nacos Group | = 应用名(=服务名) | Group=svc-order;订阅串 svc-order@@svc-order | 04§4.3 |
| 租户标识字段 | `account_id`(≡ uid ≡ user_id ≡ tenant_id) | 全书物理列/Vitess vindex 分片键/Kafka 分区键一律 account_id | 00 附录A + 04§6.3 |
| 分片键 | account_id 单键(四库统一) | 否决 region+account_id 组合路由 | 04§6.4 |
| 分库分表 | account_db 4×16、trade/resource/metering_db 8×16(实现承载 Vitess,口径不变) | 库×表口径;Reshard 承载水平扩容 | 04§6.3 |
| 资源 ID 格式 | `{productCode}-{regionId}-{分片因子2位}-{随机8位}` | scecs-cn-north-1-01-a1b2c3d4 | 00 附录A + 04§6.6 |
| region 命名 | `cn-north-1`/`cn-east-1`(短横线风格) | cn-north-1-a | 00 附录A |
| 可用区命名 | `{region}-{a/b/...}` | cn-north-1-a | 00§4.1 |
| OpenAPI 域名 | `{productCode}.api.starcloud.cn` | scecs.api.starcloud.cn | 04§3.2 |
| OpenAPI 版本载体 | RPC 风格 `Action` + 日期型 `Version` 参数(URI 不承载版本) | ?Action=RunInstances&Version=2026-08-01 | 04§3.2 + 10-research 启示 5 |
| 权限 action | `{productCode}:{Operation}` | scecs:CreateInstance | 01 D0 |
| ARN | `sc:{service}:{region}:{account_id}:{relative-resource}` | sc:ecs:cn-east-1:100123:instance/i-xxx | 07§3.1(改 cps→sc) |
| 系统策略名 | `Sc{Product}FullAccess`/`Sc{Product}ReadOnlyAccess` | ScECSFullAccess | 07§3.2(改 Cps→Sc) |
| 错误码 | `{Product}.{Module}.{Reason}`(PascalCase) | Quota.Exceeded.ScecsInstance | 03§9.3 |
| OpenAPI 签名头前缀 | `x-cps-`(签名协议专用,与品牌前缀解耦,保留不改) | x-cps-date、x-cps-content-sha256、x-cps-nonce | 07§4.1 |
| AK 前缀 | `SC` | SC****3F(替代 LTAI/CPSA) | 07§2.5(改 CPSA→SC) |
| Kafka topic 命名 | `cloud.{domain}.{aggregate}.{event}` | cloud.metering.usage.raw、cloud.trade.order.event | 04§5.3 |
| Kafka topic 环境隔离 | topic 不含环境,集群隔离 | — | 04§5.3 |

---

## 4. 一期规模假设基线表

> 本表是 S22 裁决的载体,写入 09§3.4,各章容量估算统一引用。

| 指标 | 一期(MVP)取值 | 说明 |
|---|---|---|
| 注册租户数 | 1000 | 含活跃付费约 500 |
| 活跃用户 | 1 万 | 日活 |
| 在管资源实例数 | 5 万 | 计费资源口径 |
| 微服务数 | 17(一期) | 二/三期扩至 50+(见 03§4.0 总表) |
| Pod 数 | 约 200 | 管控面 |
| K8s 业务节点 | 6–10 台 32C128G | 非 12–20 台 16C64G |
| K8s 管理集群 | 3 master + 3 worker(8C16G) | 承载平台控制面 |
| APISIX 节点 | 4 节点 4C8G | 签名插件 CPU 敏感 |
| etcd | 3 节点(独立 PV 50GB) | 三 APISIX 分区共享 |
| MySQL | account_db/trade_db 各 2×16 起步(对齐 04§6.3 8×16 基线) | 每库 1 主 2 从 MGR |
| Redis Cluster | 6 节点(3 主 3 从,16GB) | 会话/缓存/限流 |
| Kafka | 3 broker(8C16G,2×1TB NVMe) | KRaft |
| ClickHouse | 6 节点(3 分片×2 副本,16C/64G/2TB) | 日志+计量+审计 |
| MinIO | 4–8 节点(EC:4) | 对象存储底座+备份桶 |
| OpenAPI 峰值 QPS | 2000 | 鉴权目标 5k QPS(缓存命中后) |
| 网关峰值 QPS | 8000 | = OpenAPI 2000 + 控制台 + BFF 合计 |
| 审计写入峰值 | 2 万/s | 审计写入峰值(非网关 QPS) |
| 计量吞吐 | 均值 250 条/s、峰值 1250 条/s | 组件设计容量 10k msg/s(见 03§4.2.5) |
| Trace 采样 | 网关 10% 采样 + 错误/慢调用 100% 尾采样 | — |

---

## 5. 部署三阶段映射表

> 本表是 C8+S15 裁决的载体,写入 09§2.3,与 00§4.2 P1/P2/P3 一一映射。

| 阶段 | 09 命名 | 00 命名 | 时间窗 | 拓扑 | 可用性承诺 | AZ 形态 |
|---|---|---|---|---|---|---|
| 一期 MVP | 一期(内测→GA) | P1 | T0 ~ T0+9 月(T0+6 月 GA 内测、T0+9 月 GA 正式) | 单 IDC 单可用区 | 控制面≥99.9%、对象存储数据面≥99.95% | 单机房单 AZ |
| 二期 扩展 | 二期 | P2 | T0+9 ~ T0+18 月 | 同城双可用区双活 | 控制面≥99.95%、核心数据面≥99.99%(P2 目标) | 同城双 AZ |
| 三期 规模化 | 三期 | P3 | T0+18 月+ | 两地三中心(同城双活+异地灾备) | 关键数据 RPO≈0、RTO≤30min | 两地三中心(异地多活属 P3) |

**关键修正**:
- 一期(0–9 月)对应 **P1 单机房单 AZ**(原 09§2.3"单地域双机房(同城)"修正为此);
- 二期"同城三机房/三可用区"修正为"同城双 AZ(P2)",**三 AZ 不在本期规划**;
- 异地多活写能力属 P3,P3 不承诺"异地多活写"(见 00§4.5);
- MySQL HA 跨 AZ 用 MGR 单主(AZ 间 RTT<2ms,否则降级半同步,见 S20)。

---

## 6. 裁决与推荐裁决的差异说明

本裁决书**采纳全部 30 条推荐裁决的结论主体**,仅在以下一处**部分推翻**推荐裁决:

| issueId | 推荐裁决 | 本裁决书决定 | 推翻理由 |
|---|---|---|---|
| **S6(版本载体)** | 推荐"版本载体=URI 路径版本 /v1/" | **维持 04§3.2 现状"Action+日期型 Version,URI 不承载版本号"**(仅采纳推荐的"一产品一子域名"域名形态) | 推荐的 /v1/ URI 版本与权威源 **10-research 启示 5**(RPC 风格、Action、参数显式)及 **03§9.1 已论证的"RPC 风格(Action+Version),与阿里云生态习惯对齐"**产生**阻塞性矛盾**;且 04§3.2(本裁决书认定的事实源)已锁定"Action+日期型 Version,URI 不承载版本号",03§9.1/04§3.8 已按此实现。采纳 /v1/ 将迫使 03§9.1 放弃 RPC 风格论证、与"对标阿里云"全书策略及 10-research 启示 5 冲突,属阻塞性矛盾,故推翻版本载体部分 |

其余 29 条推荐裁决均**全量采纳**,无推翻。

> **二期实装追溯(2026-08,非裁决变更)**:本裁决书裁决的 C1–C10/S1–S30 跨章冲突在二期工程(M-4~M-7)中已按裁决结论实装完成——例如 C5+S12 的 `account_id` 单键分片已贯穿四库 DDL、C8+S15 的 P1/P2/P3 三阶段映射在 M-6 双 AZ 已落地、C6+S30 的资源包/抢占式后置在 M-4 已交付、S21 的"CRD 阶段为证据、平台状态机为权威"由 M-7 各 rc-* controller 的 MockDriver 验收门槛承载。实装状态详见《09-roadmap.md》§4.0。本条为追溯登记,不改变裁决结论(裁决是规范事实源,实装是其落地验证)。

---

## 7. 开放问题(需人工介入)

本裁决书 openQuestions 尽量为空。以下 1 项为纯产品商业判断,架构委员会无法独立裁决,留待产品委员会定夺,但不阻塞传播修复(各章 fixer 按"模型预留四档、一期开放两档"先回写):

1. **支持计划一期开放档位与定价**(S25):裁决锁定"模型四档、一期先开放基础/商业两档",但基础/商业两档的具体定价、工单响应 SLA 时限属产品商业判断,由产品委员会在 01§4.5 落地时定稿。

---

> **本裁决书生效后,各章 fixer 按本裁决书 §2 逐条裁决表 + §3 命名规范 + §4 规模基线 + §5 部署映射 回写,回写完成后由复核 agent 按 40 条问题清单逐条清零。**
