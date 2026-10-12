# 欧拉应用云 · Euler Application Cloud

欧拉将 E时代现有开源产品的业务能力整合为独立应用平台。用户通过欧拉登录，在团队和项目中使用八个原生应用，并统一管理成员、服务账号、权限、应用联动和操作记录。

**业务代码、页面、数据与授权由欧拉独立维护。八个原项目继续独立运行，无需改造原项目或接入其生产数据库。** 当前主线提供可部署的应用云；仓库实现与测试通过不等于已经完成生产部署或真实外部服务验收。

## 八个原生应用

| 应用 | 欧拉内的业务能力 | 功能文档 |
| --- | --- | --- |
| EID | 个人资料、身份申请与审核、身份卡、社团报名、面试、录取通知及本人接受 Offer | [EID / Trust](EID-TRUST.md) |
| Trust | 动态认证方案、材料上传、申请与重提、审核改判、历史、受限外部验证 API | [Trust](TRUST.md) |
| WeAuth | 站点与域名、密钥、工作量证明、IP 风控、统计、验证组件与集成预览 | [WeAuth](WEAUTH.md) |
| Database | 实际 MySQL / PostgreSQL 建库、独立凭据、密码轮换、配额、只读限额、资源包及兑换码 | [数据应用](DATA-APPS.md) |
| Storage | 桶与文件、目录、上传下载、受控分片续传、对象版本、恢复、复制/移动、生命周期、配额与资源包 | [数据应用](DATA-APPS.md)、[存储高级能力](STORAGE-ADVANCED.md) |
| Statistics | 页面采集、PV/UV、日期趋势、来源和设备、自定义事件、有序漏斗、CSV 与数据留存 | [统计分析](ANALYTICS.md) |
| Lottery | 活动、公开或资格报名、防刷、参与者、抽奖展示、轮次历史、中奖通知与权益联动 | [活动应用](ACTIVITY-APPS.md)、[应用联动](AUTOMATION.md) |
| WitShield | 设备、扫描与报告、AI 调查、能力和防御策略、修复准备/审批/回滚、审计、通知和计划 | [WitShield](WITSHIELD.md) |

应用在欧拉控制台内办理业务，原生 API 位于 `/api/v1/tenants/{tenantID}/projects/{projectID}/apps/{applicationID}/…`。功能来源、固定上游提交、许可与各模块边界见 [原生应用架构](NATIVE-APPS.md) 及模块中的 `SOURCE.md` / `NOTICE`。

## 平台能力

- **统一身份与范围授权。** 欧拉以独立 OIDC 客户端接入通行证，使用自己的服务端会话与 CSRF 校验。团队、项目角色和应用审核/运营权限分别检查，支持应用授权到期。身份源故障自动重试；已有有效会话与新登录的可用性分开处理。
- **服务账号与开发接入。** 项目服务账号有明确应用、操作范围、到期时间、轮换和撤销。机器 API 采用独立入口和路由白名单；提供当前应用云的 [Python SDK / CLI](../../sdk/app-cloud-python/README.md)。不向机器账号开放个人认证审核、平台运营或 WitShield 人工审批。
- **应用联动。** Trust 资格、EID 成员状态和活动中奖事件可触发明确配置的站内通知或 Database / Storage 资源包。事件和业务变更同事务落库，执行有持久队列、租约、幂等与失败重试；每次执行重查当前授权和应用状态。资源包为每人每条规则一次性奖励，按模板到期，资格撤销可终止对应权益，不自动延长或重新赠送。
- **项目生命周期。** 项目可改名、归档、恢复和导出配置清单。归档停止欧拉业务入口并保留数据；删除只允许已归档且不含应用/服务账号记录的空项目，避免隐式删除外部资源。
- **运行与扩展。** 提供受保护诊断/指标、请求关联 ID、依赖状态、后台维护和审计。可信应用通过代码注册清单声明版本、能力与依赖；启动先检查版本兼容，再迁移并记录。当前没有任意用户上传执行代码的应用市场。

权限与接口细节见 [访问与生命周期](ACCESS-LIFECYCLE.md)，规则和一次性奖励语义见 [应用联动](AUTOMATION.md)。用户的认证资格不会自动转化为项目管理员或产品运营权限。

## 数据与部署边界

欧拉核心采用**单后端进程、本机 SQLite 与持久应用目录**。主数据库保存平台及应用业务元数据，每个项目的 WitShield 引擎拥有独立数据目录与派生密钥；个人材料和敏感配置按模块加密保存。当前不支持多个控制面写入副本。

MySQL / PostgreSQL 模块在部署者提供的欧拉专用基础引擎内创建业务数据库，不提供自动创建集群、伸缩或故障切换。Storage 使用部署者自己的 S3 兼容服务；欧拉机器接口为 REST，不是通用 S3 SigV4 网关。邮件、通知和 AI 也需要实际可用的外部服务配置，未配置时不得以演示资源代替。

项目归档或服务账号撤销不能撤回已建立的数据库连接、已发出的对象签名或公开文件地址。需要完整隔离时，须在外部数据面同步处理。公开健康检查仅表示进程/核心存储状态；全部业务健康还需检查受保护诊断和实际业务流程。

完整恢复点必须包含欧拉整个应用目录、同一维护窗口内的外部 SQL / S3 数据与配置，以及独立保管的原加密密钥。项目清单导出、单个 SQLite 快照和仅复制当前对象均不能代替全平台备份。以 [RECOVERY.md](RECOVERY.md) 为完整备份/恢复操作指南，以 [发布与回退](../../deploy/app-cloud/RELEASE.md) 管理版本切换。

## 开发与验证

控制服务位于 `services/app-cloud`，WitShield 引擎位于 `services/witshield-engine`，控制台位于 `platform/frontend/apps/console-base`。部署及环境变量见 [部署指南](../../deploy/app-cloud/README.md)。

```sh
# Node.js 24、pnpm 11.21.0；先按部署说明配置独立环境变量。
cd platform/frontend
pnpm install --frozen-lockfile
pnpm --filter 'console-base^...' build
pnpm --filter console-base dev

# Go 1.27.1，另一个终端，从仓库根目录执行。
cd services/app-cloud
go run ./cmd/server
```

验收范围包括 Go race 测试、真实本地 HTTP / SQLite 浏览器流程、前端类型及构建、SDK、SQL / S3 集成、设备协议和完整备份恢复测试。具体命令与结果记录见 [VALIDATION.md](VALIDATION.md) 和 [部署指南](../../deploy/app-cloud/README.md)。测试身份源、隔离数据和自动化故障模拟不能替代真实通行证、邮件入箱、AI 效果、容量和生产恢复演练。

## 文档入口

| 主题 | 文档 |
| --- | --- |
| 架构与功能来源 | [NATIVE-APPS.md](NATIVE-APPS.md) |
| 跨应用规则与执行 | [AUTOMATION.md](AUTOMATION.md) |
| 授权、服务账号、项目归档 | [ACCESS-LIFECYCLE.md](ACCESS-LIFECYCLE.md) |
| 访问分析、事件、漏斗、留存 | [ANALYTICS.md](ANALYTICS.md) |
| 分片、版本、生命周期、配额 | [STORAGE-ADVANCED.md](STORAGE-ADVANCED.md) |
| 身份恢复、探针、诊断、指标 | [OPERATIONS.md](OPERATIONS.md) |
| 完整备份与恢复 | [RECOVERY.md](RECOVERY.md) |
| 预发布、发布与回退 | [RELEASE.md](../../deploy/app-cloud/RELEASE.md) |
| 当前 Python SDK / CLI | [sdk/app-cloud-python](../../sdk/app-cloud-python/README.md) |
| 验收记录与阶段目标 | [VALIDATION.md](VALIDATION.md)、[COMPLETION-GOAL.md](COMPLETION-GOAL.md) |

## 历史资料

[PLAN.md](PLAN.md)、[早期 API / 连接器契约](API.md) 和 [连接器说明](../../services/app-cloud/internal/connectors/README.md) 保留早期设计记录。其中外部账号连接、跳转原产品办理和连接器资源摘要的描述，不代表当前原生业务模式。历史 [IaaS Alpha 说明](../archive/iaas-alpha-readme.md)、`docs/architecture/`、`sdk/python` 与 `sdk/terraform` 也不作为当前应用云能力承诺；以本页、原生模块文档和当前实现为准。
