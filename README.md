# 欧拉应用云 · Euler Application Cloud

以 E时代现有开源产品为基础，重新设计的独立统一应用平台。一个通行证账号、一个工作空间，在团队与项目中使用身份认证、数据服务、安全和活动工具。

**欧拉独立拥有业务代码、页面、数据和授权。八个原项目继续独立运行，本次开发不改动它们。** 本仓库新的主线是应用平台；历史 IaaS Alpha 实现保留在原目录，不代表已上线的可售公有云基础设施。

## 应用

| 应用 | 欧拉内的业务能力 |
| --- | --- |
| EID | 个人资料、身份申请与审核、身份卡、社团报名、面试/录取通知与本人接受 Offer |
| Trust | 动态认证方案、材料上传、申请与重提、审核改判、查询、历史和受限外部验证 API |
| WeAuth | 站点、域名与密钥、动态/批量工作量证明、IP 风控、统计、真实验证组件及集成预览 |
| Database | MySQL/PostgreSQL 实际建库、连接凭据、密码轮换、用量与只读限额、资源包、兑换码和运营 |
| Storage | 存储桶与文件、目录、上传下载、受控分片续传、历史版本与恢复、复制/移动、生命周期、配额与资源包 |
| Statistics | 访问采集、PV/UV、日期趋势、来源与设备、自定义事件、有序漏斗、CSV、数据留存与站外计数器 |
| Lottery | 活动房间、移动与资格报名、防刷、二维码、参与者、随机抽奖、历史、中奖通知及权益联动 |
| WitShield | 设备、扫描和报告、AI 调查、能力与防御策略、修复准备/审批/回滚、审计、通知和计划 |

完整范围、功能来源与验收约定见 [独立应用架构](docs/unified-app-cloud/NATIVE-APPS.md) 及其中链接的产品矩阵。登录、密码与账号安全统一交给通行证和欧拉会话；不再为每个应用建立旧式人员登录。

## 统一平台

团队和项目承载应用、成员与授权；平台提供应用权限到期、项目服务账号及轮换/撤销、项目归档/恢复、配置清单导出、操作审计、受保护运行诊断和指标。当前应用云的 [Python SDK / CLI](sdk/app-cloud-python/README.md) 使用限定范围的机器 API，历史 IaaS SDK 保持独立。

[应用联动](docs/unified-app-cloud/AUTOMATION.md) 将 Trust 认证、EID 成员状态和活动中奖事件接到站内通知或资源包。事件和业务变更同事务记录，执行时重新检查当前权限、资格与项目状态；支持持久队列、失败重试和幂等。资源包按每人每条规则一次性赠送，不把认证通过当作管理员授权。

能力、配置边界和完整文档索引见 [应用云说明](docs/unified-app-cloud/README.md)。本仓库提供独立平台实现与部署工具，**不表示已替部署者完成真实生产身份源、邮件、AI、容量或灾备验收**。

## 身份与数据边界

- 登录采用欧拉独立的 OIDC Authorization Code + PKCE，验证签名、issuer、audience、nonce 和一次性事务；浏览器使用 HttpOnly 会话 Cookie 与 CSRF 防护。
- 团队、项目和应用是明确的授权范围，每次请求重新验证，过期或撤销授权不依赖重新登录。个人认证资料仅本人及明确授予的审核员可访问。
- 团队 owner/admin 不自动成为认证审核员或产品运营者。部署者以准确的 `provider + subject` 指定平台管理员，再由其分配项目内应用权限。
- 公开验证组件、统计采集、活动报名和设备协议有各自受限入口；WitShield 设备仍需 token、Ed25519 签名及防重放校验。
- 业务元数据由欧拉 SQLite 持久化，敏感材料和配置加密；每个项目的 WitShield 引擎使用独立数据库与派生密钥。数据库密码和存储签名仅通过明确授权的接口提供。
- 数据库与对象存储连接欧拉自己的 MySQL/PostgreSQL/S3 基础设施。原项目的管理 API、共享管理员令牌及原生产数据库不参与新平台运行。

## 开发与部署

后端位于 `services/app-cloud`，完整 WitShield 引擎位于 `services/witshield-engine`，控制台位于 `platform/frontend/apps/console-base`。

按 [部署指南](deploy/app-cloud/README.md) 配置独立通行证客户端、持久加密密钥及需要的基础服务。缺少登录、数据库、S3、邮件或 AI 配置时会明确报告不可用，不生成演示资源。

```sh
# Node.js 24、pnpm 11.21.0
cd platform/frontend
pnpm install --frozen-lockfile
pnpm --filter 'console-base^...' build
pnpm --filter console-base dev

# Go 1.27.1，另一个终端，先配置部署文档中的环境变量
cd services/app-cloud
go run ./cmd/server
```

本地控制台以同源方式代理 `/auth/`、`/api/` 与 `/public/`。生产使用非 root Docker/Compose 部署；当前为单后端进程及本地 SQLite 持久卷，不支持多个写入副本。

升级与灾备使用 [全平台备份恢复流程](docs/unified-app-cloud/RECOVERY.md)，覆盖主库、WitShield 数据及其他应用文件，并要求外部 SQL / S3 数据与配置处于同一协调恢复点。主加密密钥独立保管；单库快照和项目清单导出不能替代此流程。镜像与数据版本切换见 [发布及回退](deploy/app-cloud/RELEASE.md)。

## 验证

```sh
cd services/app-cloud
go vet ./...
go test -race ./...
cd ../witshield-engine
go test -race ./...

cd ../../platform/frontend
pnpm --filter 'console-base^...' build
pnpm --filter console-base typecheck
pnpm --filter console-base build
pnpm exec playwright test --config playwright.app-cloud.config.ts
```

浏览器测试使用独立测试 OIDC 身份源、真实 Go 服务与 SQLite，不模拟欧拉业务接口。CI 另运行真实 MySQL/PostgreSQL/S3 数据面、设备协议、镜像构建与 Compose 启动测试。生产身份源、邮件或 AI 服务仍须使用部署者自己的配置完成环境验收。

## 文档

- [平台总览与完整文档索引](docs/unified-app-cloud/README.md)
- [当前架构、路由与完整功能标准](docs/unified-app-cloud/NATIVE-APPS.md)
- [应用联动、资格和一次性权益](docs/unified-app-cloud/AUTOMATION.md)
- [授权、服务账号与项目生命周期](docs/unified-app-cloud/ACCESS-LIFECYCLE.md)
- [EID 与 Trust](docs/unified-app-cloud/EID-TRUST.md)
- [数据库与对象存储](docs/unified-app-cloud/DATA-APPS.md)
- [存储分片、版本和生命周期](docs/unified-app-cloud/STORAGE-ADVANCED.md)
- [WeAuth](docs/unified-app-cloud/WEAUTH.md)
- [统计与抽奖](docs/unified-app-cloud/ACTIVITY-APPS.md)
- [日期分析、事件、漏斗与留存](docs/unified-app-cloud/ANALYTICS.md)
- [WitShield](docs/unified-app-cloud/WITSHIELD.md)
- [运行诊断、故障恢复与指标](docs/unified-app-cloud/OPERATIONS.md)
- [当前 Python SDK / CLI](sdk/app-cloud-python/README.md)
- [验收记录](docs/unified-app-cloud/VALIDATION.md)
- [配置与部署](deploy/app-cloud/README.md)
- [全平台备份与恢复](docs/unified-app-cloud/RECOVERY.md)
- [预发布、发布与回退](deploy/app-cloud/RELEASE.md)

复用代码的来源、固定提交与许可记录在各模块的 `SOURCE.md` / `NOTICE` 及随附许可证中。原仓库与历史设计文档保留各自职责，冲突处以当前原生应用实现和功能矩阵为准。
