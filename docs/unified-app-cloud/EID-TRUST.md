# Euler 原生 EID 与 Trust

本轮业务由 Euler 自己保存和执行，不代理旧 EID/Trust，不复制旧登录或生产配置。旧项目保持独立。人员身份来自 Euler 通行证会话，模块只接受平台注入的 Scope。

## 功能迁入对照

| 原功能 | Euler 页面 / 原生接口 | 权限与数据范围 |
|---|---|---|
| EID 会员资料、头像、简介、联系方式 | 会员资料，`GET/PATCH /profile` | 本人；姓名与邮箱来自通行证，不由请求体指定 |
| EID 身份申请、历史、全部已通过身份卡 | 我的身份，`GET/POST /verifications`、`GET /cards` | 本人；姓名、学号及申请内容加密保存；同一成员只允许一份待审申请 |
| EID 申请筛选、资料编辑、通过/拒绝及改判 | 身份审核，`GET /review/verifications`、`PATCH /review/verifications/{id}`、`POST …/{id}/decision` | 单独 `review`；版本检查；修改已通过申请会同步资格；撤销通过后重新计算资格 |
| EID 社团报名、拒绝后重报、状态与通知进度 | 社团报名，`GET /club`、`POST /club/applications` | 本人、已验证邮箱；不接受客户端自报认证结果 |
| EID 联动 Trust 资格 | 流程设置绑定本项目认证方案；报名与发录取时服务端调用原生 Trust | 校验本人 ActorID、项目、活动方案和 Trust 应用启用状态；无需原站 API/密钥 |
| EID 社团状态统计、筛选、拒绝 | 招募审核，`GET /review/club`、`POST …/{id}/decision` | 单独 `review`；禁止拒绝已确认录取 |
| 笔试、录取、重发 | 招募审核，`POST /review/club/{id}/notifications` | 单独 `review`；实际 SMTP 投递、明确首次/重发状态、幂等请求键、投递结果记录 |
| Offer 接受 | 社团报名，`POST /club/applications/{id}/confirm` | 申请人本人；旧 UUID 无登录入口不迁入新平台 |
| EID 会员业务资料运营、删除 | 会员管理，`GET /admin/members`、`PATCH/DELETE …/{actorID}` | 单独 `admin`；手动变更资格类型/名称还必须有 `review`；仅本项目 EID 数据；不修改或删除 Euler/通行证账号 |
| EID 身份、社团记录删除 | 记录操作，`DELETE /admin/verifications/{id}`、`DELETE /admin/club/{id}` | 单独 `admin`；范围检查并保留审计 |
| EID 原资格对外查询 | 资格查询接口，`GET/POST /keys`、`DELETE /keys/{id}`；`GET /public/eid/verification/status?actorId=` | 密钥绑定安装、指定会员、到期时间；创建/撤销要求 `admin` 与 `secrets`；数据库只保存 token hash；匿名、自报 OAuth ID 不再授权 |
| EID 本人资格查询 | `GET /qualification` | 会话本人；返回资格类型、名称与更新时间，不返回姓名或学号 |
| EID 企业微信/飞书通知 | 流程设置与通知记录，`GET /notifications`、`POST /notifications/{id}/retry` | 业务事务内持久记录待发消息、发送后记录真实结果；消息不含申请姓名、学号、邮箱或凭据 |
| Trust 全部认证方案、动态字段与启停 | 认证方案、原生方案 CRUD | 独立 `admin`；项目作用域；字段类型、顺序、必填、选项与校验规则 |
| Trust 动态申请、分步材料、重提、本人历史 | 我的认证、申请详情、材料预览/下载 | 本人；加密材料；保留提交字段快照；未提交材料可清理 |
| Trust 审核、改判、队列、查询、统计、用户搜索 | 审核工作台、运营查询 | 独立 `review`；审核与历史、平台审计、通知意图同事务 |
| Trust 对外状态与详情 API、文档调试 | 查询密钥、接口文档 | 项目、方案、申请人、有效期和材料读取范围受密钥约束；应用停用/密钥撤销立即拒绝 |
| Trust 生命周期机器人通知 | 通知记录、核实后重试 | 真实 HTTP 投递与持久结果；不伪装成功 |
| 原 Django Admin / OAuth Toolkit / Trust 子应用登录 | Euler 身份、团队项目和应用授权管理 | 统一身份职责取代框架旧账号入口；不复制备用密码、旧 JWT 或全站管理员旁路 |

人员接口均相对于 `/api/v1/tenants/{tenantID}/projects/{projectID}/apps/eid` 或 `/apps/trust`。Trust 的完整 DTO、路由与材料/查询密钥说明见 [TRUST.md](./TRUST.md)。

## 权限与隔离

- 平台审核 Cookie、CSRF、成员关系、项目权限及应用启用状态后注入 Scope；HTTP 用户头、请求体 actor/tenant/project 不能建立身份。
- 普通 `owner/admin` 仅自然取得项目管理权限，不自动取得 `review/admin` 产品授权。运营授权由平台明确授予；审核与运营分别检查。
- EID 的个人申请、身份卡、报名、Offer 和资料按 ActorID 隔离。审核者只能访问获授权项目；不能替申请人接受 Offer。
- Trust 的方案、材料、申请与查询密钥按项目及申请人约束。EID 直接检查原生 Trust 结果，禁用 Trust 或改变审核结果会影响后续报名/发录取检查。
- 个人资料、申请内容、材料与通知内容用 Runtime 加密，并绑定记录用途作为 associated data。秘密不进入列表、日志或审计摘要。
- EID/Trust 都使用共享单连接 SQLite；Rows 关闭后才进行关联查询，事务内只使用 tx。重要状态变更与审计共同提交。

## EID 邮件与通知配置

项目的「流程设置」可以配置招募名称、Trust 前置方案、邮件主题和正文、SMTP 及机器人。凭据写入要求 `secrets`，只提供配置状态，不返回现有秘密；相同 SMTP 目标的密码输入留空保留，勾选清除才移除。修改服务器、用户名或 TLS 方式时必须输入新密码或明确清除，不会把部署默认密码转发到新地址。

可选部署默认值：

| 变量 | 用途 |
|---|---|
| `EULER_EID_SMTP_ADDRESS` | SMTP `host:port`，未配置时通知返回 503，并保持原报名状态 |
| `EULER_EID_SMTP_FROM` | 发件邮箱 |
| `EULER_EID_SMTP_USERNAME` / `EULER_EID_SMTP_PASSWORD` | SMTP 身份；可以由项目加密设置覆盖 |
| `EULER_EID_SMTP_IMPLICIT_TLS=true` | 使用隐式 TLS；否则要求 STARTTLS，最低 TLS 1.2 |
| `EULER_EID_ALLOW_LOCAL_SMTP=true` | 部署者明确允许内网/回环 SMTP；仍验证 TLS 证书，不提供跳过证书开关 |
| `EULER_EID_WECOM_URL` / `EULER_EID_FEISHU_URL` | 对应平台官方 HTTPS 机器人地址；留空不发送 |

邮件正文支持 `{{name}}`、`{{club}}`、`{{url}}`。链接包含 tenantId/projectId，平台验证真实成员权限后选择项目；不依赖收件人上次的项目选择。

邮件操作先持久记录发送意图，再真正投递。只有 SMTP 明确确认接收才推进笔试/录取状态；失败保持原状态，确认丢失标记 `uncertain`。同一幂等键不会重复发送。重发必须是人工新操作，并要求当前处于相应已发送阶段。进程在发送中断开留下的 `sending` 记录也表示结果待核实，不能自动视为未发送。

SMTP 服务器确认接收不等于收件人已阅读或最终入箱；不提供送达/阅读回执的虚假状态。机器人待发记录和业务变更共同提交，提交后使用不受浏览器取消影响的独立上下文发送；进程重启前未发送的消息仍保留为 `queued`。EID 与 Trust 都提供核实后的手工重试，EID 的 `sending` 表示投递结果待核实，不自动重发。HTTP 成功还必须有对应机器人协议的明确成功码，空响应不会记为已发送。没有配置的渠道不伪造通知。

## 验证与来源

- EID Go 测试覆盖实际 SQLite 持久化/重开、加密字段、本人及跨项目隔离、viewer/普通项目管理员权限拒绝、真实 Trust 资格联动、幂等通知、未知/失败投递、Offer 本人确认、并发待审唯一性、资料读写交错与审计事务回滚、SMTP 凭据绑定、资格改动独立审核授权、webhook outbox 原子性/重开/取消后投递/成功码检查，以及资格密钥范围/撤销/停用。
- EID 资格密钥会员选择器按姓名/成员编号搜索、分页加载并保留跨页选择；组件测试覆盖第 101 位会员的选择与移除。
- SMTP 测试启动真实本地 TLS SMTP 服务，验证协议、收件人、主题编码与邮件正文；无外发测试邮件。
- Trust 测试覆盖动态字段、材料、审核、作用域、事务回滚、查询密钥和真实 HTTP 通知；Vue 组件验证真实表单与权限显示。
- `platform/frontend/e2e/app-cloud/identity-apps.spec.ts` 使用真实 Go、SQLite、Vue 和可信测试 OIDC：动态方案 → 上传 → 提交 → 审核 → EID 资格卡/社团联动，并验证另一用户、跨项目及未配置 SMTP 的真实错误；不 mock 业务接口。

行为参照固定公开版本：

- [miaojilab/emoera-eid @ f987e818](https://github.com/miaojilab/emoera-eid/tree/f987e818e83752ceeab93611d76204c79877a855)，Apache-2.0。原路由、模型和通知业务用于功能对应；新 Go/Vue 实现独立编写，没有复制原服务登录、配置或生产数据。
- [miaojilab/trust-center @ 28ae1b3d](https://github.com/miaojilab/trust-center/tree/28ae1b3dacacb468047d407e13e35694c61ab8f7)，来源与许可详见 [TRUST.md](./TRUST.md)。

新平台使用独立数据。没有自动读取旧生产库；如需导入历史会员、方案、材料及申请，应另外设计显式归属、身份映射、加密转换与回滚流程。
