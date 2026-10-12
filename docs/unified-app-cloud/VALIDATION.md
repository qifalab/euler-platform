# 应用云完整交付验收 · 2026-09-27

本轮从 `b4e0321` 继续完成 [COMPLETION-GOAL.md](COMPLETION-GOAL.md)，只修改 `qifalab/euler-platform`。下方保留 9 月 26 日的基线记录，旧 CI 不充当本轮证据。

## 本轮本地结果

| 范围 | 结果与证据 |
| --- | --- |
| 平台与八应用 | Go 1.27.1 `go vet ./...`、`go test -race -count=1 ./...` 通过；覆盖版本注册、动态身份源恢复、运维权限、服务账号、授权到期、归档、跨项目隔离、持久流程及业务回归 |
| 真实对象服务 | 官方 MinIO 固定提交的本地测试构建；Storage/Trust 全包 race 与 `TestRealS3`、`TestRealS3Advanced`、`TestRealS3PrivateMaterialCompatibilityAndIsolation` 实际通过，未将 SKIP 算通过 |
| 控制台与产品站 | 类型检查、16 项 Vue 交互测试、console-base 与 site 生产构建通过 |
| 真实浏览器 | 13 场景使用真实 Go、SQLite、签名测试 OIDC 与 Vue 页面；完整一次运行通过，见 `platform/frontend/e2e/app-cloud` |
| Python SDK/CLI | 6 项客户端测试通过；Go 集成测试启动真实 WeAuth 模块，由 Python SDK 经 TCP 创建/读取站点，验证范围、轮换、撤销与机器审计 |
| 备份/恢复/发布 | 32 项测试通过，包含多 SQLite/WAL/实际文件恢复、主密钥与清单校验、路径攻击、忙写入、定时失败恢复、Trust 专用材料桶遗漏拒绝及恢复再核对 |

浏览器在原 8 个业务场景上增加：服务账号与归档恢复；空项目/空团队删除；真实双访客事件、日期图表、漏斗与 CSV；审核发放权益、受限报名、开奖与站内通知；统一资源用量页桌面、移动和暗色状态。所有这些业务请求直接进入真实后端，未用路由拦截伪造成功。

本地 MinIO 为受限容器适配了网卡枚举失败时的 loopback 回退，只修改仓库外临时测试源码；S3 协议实现不变，不进入发布镜像。CI 从官方固定提交构建正常 MinIO，详细复现方法见 [STORAGE-ADVANCED.md](STORAGE-ADVANCED.md)。

## 本轮合并门槛

[App cloud 工作流](../../.github/workflows/app-cloud.yml) 必须在本 PR 最新提交通过四个任务：backend（真实 MySQL 8.4、PostgreSQL 17、S3、完整设备引擎）、console、browser（13 场景）、deployment（非 root / 只读容器、停服、三库和文件备份、验证、新目录恢复及实际内容读回）。SDK 与全部 Python 工具测试已纳入门槛。实际运行、审查和合并状态以 GitHub PR Checks 为准，不用先前提交的绿色状态代替。

本地没有 Docker 与独立 SQL 服务，因此这些完整容器/SQL 测试交由 CI 实际执行。本轮不部署生产，也不改原八个仓库。

## 上线边界

已交付可部署、可扩展的单实例应用平台及运维工具。生产独立 OIDC 客户端、HTTPS、主密钥、SQL/S3、邮件/机器人、AI 与设备环境需要部署者提供，再执行 [上线检查](../../deploy/app-cloud/RELEASE.md) 和 [恢复演练](RECOVERY.md)。这些真实环境验收仍属于上线输入，测试 IdP、SMTP 协议服务和 CI 数据面不能替代生产验收。

项目归档与撤权立即阻止新的欧拉业务请求；已建立的外部 SQL 连接及已签发链接仍受数据面控制。跨应用资格不会授予审核/运营权限；自动资源包为每人每规则一次，撤销后终止对应权益而不删除已有资源。通知目前是站内通知。

收费支付、多副本/跨区高可用、底层数据库集群调度与任意第三方代码托管没有被虚构为已完成。它们的实施前提见 [APP-DEVELOPMENT.md](APP-DEVELOPMENT.md)。

---

# 基线历史记录 · 独立原生应用平台验收

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
