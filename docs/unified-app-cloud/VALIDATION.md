# 独立原生应用平台验收

日期：2026-09-26。范围：[PLAN.md](PLAN.md)、[NATIVE-APPS.md](NATIVE-APPS.md) 的八个原生应用。交付 [PR #3](https://github.com/qifalab/euler-platform/pull/3)。本记录取代此前外部连接器基线的验收；旧提交的绿色 CI 不作为本次实现的证据。

## 本地验证

| 范围 | 执行 | 结果 |
| --- | --- | --- |
| 平台及八个原生应用 | Go 1.27.1 `go vet ./...`、`go test -race -count=1 ./...` | 真实 SQLite、事务、加密、权限及业务测试通过 |
| 控制台 | `console-base typecheck`、`console-base build` | 类型检查与生产构建通过 |
| Vue 操作流程 | `vitest run apps/console-base/src/cloud/apps` | WeAuth、WitShield、Trust、EID 授权选择器 13 项通过 |
| 产品站 | `site build` | Nuxt 生产构建通过 |
| 浏览器 | `playwright test --config playwright.app-cloud.config.ts` | 8 个场景；各场景通过，修正权限表单标签后单独复跑对应场景 |
| 备份工具 | `python3 -m unittest discover -s tools/app-cloud -p 'test_*.py' -v` | 原 SQLite 快照工具 5 项通过；完整平台备份另按部署文档停机归档全部数据目录 |
| 原项目范围 | 八个参照仓库 `git status --porcelain` | 全部无改动 |

浏览器启动真实 Go 服务、全新 SQLite、原生 Vue 页面及独立签名 OIDC 测试身份源。业务接口没有替换成假数据；测试身份源与测试配置不打包进生产服务。场景覆盖：

1. Statistics 站点、真实浏览器采集、PV/UV 报表、站外计数器和应用停用。
2. Lottery 二维码、手机报名、单个/批量参与者、抽奖动画、历史、重试和重置。
3. 抽奖已提交但响应丢失时，切换房间和刷新后仍以原幂等键恢复同一结果，不重复抽奖。
4. Trust 动态方案、文件上传、申请与审核，EID 身份卡、社团资格联动及未配置 SMTP 的真实失败。
5. WeAuth 真实工作量证明 Worker、验证令牌消费、桌面/手机布局、viewer 拒写、成员撤权、旧接口关闭及退出会话。
6. 八个应用工作区、独立审核授权与撤销、项目深链接与无权项目拒绝。
7. 匿名伪造旧身份头不能冒充用户。
8. WitShield 真实 Ed25519 设备注册、一次性令牌、计划 CRUD、AI/通知秘密不回显、viewer 拒写及手机布局。

后端另外验证本人资料/材料隔离、跨项目猜 ID、运营和审核权分离、通知 outbox 事务、真实本地 TLS SMTP、失败/未知投递不推进录取状态、资源包并发兑换、上传大小篡改、密钥撤销、公开入口停用、设备修复审批及回滚协议。

## 必须通过的 CI

[App cloud 工作流](../../.github/workflows/app-cloud.yml) 对 PR 最新提交运行四个任务：

| 任务 | 实际执行内容 |
| --- | --- |
| backend | 平台全部 vet/race/build；独立 MySQL 8.4、PostgreSQL 17、S3/MinIO 真实数据面；完整 WitShield 引擎及 Unix socket 协议 |
| console | 冻结 lockfile 安装、共享包、类型检查、13 项 Vue 测试、控制台和产品站构建 |
| browser | 全部 8 个真实后端浏览器场景 |
| deployment | 构建三个镜像、非 root/只读根文件系统 Compose、Nginx、持久卷启动、健康与同源 API、未配置登录关闭及伪造头拒绝 |

本机没有 Docker、MySQL、PostgreSQL 或 S3 服务，相关真实数据面测试按环境变量明确跳过；本机对 Unix socket 创建有限制，六个原设备协议测试在本机无法运行。这些检查保留在 CI 中实际执行，不以跳过代替通过。最新提交结果见 [PR Checks](https://github.com/qifalab/euler-platform/pull/3/checks)；只有这些任务通过才允许合并。

## 部署输入与验证边界

本轮没有生产登录、生产资源写入、原项目部署或历史数据迁移。上线需配置欧拉独立 OIDC 客户端、HTTPS 地址与回调、持久加密主密钥，以及要启用的数据库、S3、SMTP、机器人、AI 等基础服务。未配置能力明确不可用。

- 原 EID 及其他七个原项目继续独立，原有鉴权保持。
- 欧拉八个应用的人员操作统一走欧拉会话和项目授权；公开采集/报名、机器密钥、短期签名链接与设备协议保留自己的限权规则。
- 数据库已发出的连接凭据及已经签发的 S3 链接受数据面本身的权限/有效期控制；停用应用立即阻断欧拉新请求，不声称能让所有已有数据面连接瞬间失效。
- SMTP 协议确认不等于最终入箱；外部通知处于未知状态时需人工核实，不自动重复发送。
- 当前为单进程 SQLite 控制面。完整备份必须包含主数据库、每项目 WitShield 数据库、材料/应用数据、主密钥和外部数据库/S3数据，见[部署指南](../../deploy/app-cloud/README.md)。
