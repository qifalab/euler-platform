# Trust 原生模块

功能参照 `miaojilab/trust-center` main `28ae1b3dacacb468047d407e13e35694c61ab8f7`。原站代码及配置未修改；新模块由 Euler 持有业务数据，不调用原站管理 API，不接受原站登录、JWT、OAuth ID 或全局 API Key。

## 功能对应

| 原功能 | Euler 实现 |
|---|---|
| 个人仪表盘、活跃方案、方案字段详情 | 我的认证、认证方案；项目内原生数据 |
| 动态方案创建/编辑/字段排序/必填/选项/启停/删除 | 独立 `admin` 授权；文字、长文本、数字、邮箱、电话、日期、单选、图片、文件；服务端校验 |
| 本人提交、重新提交、状态、资料详情 | 使用 Scope.ActorID，忽略任何前端身份；已通过不可重提；重提保留首次创建时间并更新修改时间 |
| 文件材料 | multipart 上传、加密持久化、授权预览/下载；图片、PDF、纯文本，单文件最多 10 MiB；未提交材料可清理 |
| 待审核队列、分页、用户/方案/状态查询 | 服务端过滤后分页，不沿用原前端只筛前 100 条的行为 |
| 用户查询及统计 | 已进入本项目 Trust 的全部用户，含未申请者；资料加密，搜索前在本项目范围解密 |
| 审核、改判、拒绝原因及历史 | 独立 `review` 授权；版本冲突检查；决定、历史、审计、通知意图同一 SQLite 事务 |
| 外部状态/详情 API 与文档 | 项目、方案、申请人限定的机器密钥；状态查询、已通过资料/材料查询；原生 API 文档及状态调试 |
| API Key 配置 | 原站只有环境配置，新模块增加签发一次显示、范围、最长 90 天有效期、列表与即时撤销 |
| 企微/飞书生命周期通知 | 新提交、重提、通过、拒绝；记录渠道、发送次数和真实结果，支持明确手动重试 |
| 运行配置 | 原站没有配置管理 UI；本模块显示通知是否配置及发送结果，生产密钥仍由部署者管理 |

团队 owner/admin 不自动获得产品 `admin` 或 `review`。方案运营权限不自动获得个人材料读取权限；查询密钥的签发、撤销要求 `admin` 和 `secrets`，详情密钥签发还要求 `review`。原站没有独立方案管理员，此实现以项目级 Trust 运营和审核授权作为隔离边界。

## 数据及路由

构造 `trust.New(runtime)`，实现 `appkit.Module`。表名均使用 `trust_` 前缀，迁移可重入；单连接 SQLite 的 Rows 在后续查询前关闭，事务内仅使用 tx。

人员相对 API 由平台挂载在 `/api/v1/tenants/{tenantID}/projects/{projectID}/apps/trust`：

| 路径 | 方法 | 权限 |
|---|---|---|
| `/bootstrap`, `/schemes`, `/schemes/{id}` | GET | read |
| `/schemes`, `/schemes/{id}` | POST, PUT, DELETE | admin |
| `/submissions`, `/submissions/{id}` | GET | read，必须本人 |
| `/submissions` | POST | write，必须本人；schemeVersion 和 version 必填 |
| `/materials`, `/materials/{id}` | GET | read，必须本人；列表仅未提交材料 |
| `/materials`, `/materials/{id}` | POST, DELETE | write，必须本人；已提交材料不可单独删除 |
| `/review/submissions`, `/review/submissions/{id}` | GET | review |
| `/review/submissions/{id}` | POST | review；status/reason/version |
| `/review/materials/{id}`, `/review/stats`, `/review/users` | GET | review |
| `/keys`, `/keys/{id}` | GET, POST, DELETE | admin；POST/DELETE 额外要求 secrets；details=true 的签发额外要求 review |
| `/notifications`, `/notifications/{id}/retry` | GET, POST | admin |

公开机器 API 为 `/public/trust/verification/status`、`/verification/details`、`/materials/{id}`，仅接受 `Authorization: Bearer trust_…`。密钥摘要持久化，明文只在签发响应出现一次。拒绝查询参数密钥、旧 API Key、OAuth ID 及全局用户查询。每次请求检查密钥有效期/撤销、当前 Trust 是否仍启用、方案仍 active、授权申请人及方案范围。详情与材料还要求当前申请 approved；材料必须仍是当前申请引用的文件。

申请 payload、审核原因、历史原因、姓名/邮箱、文件名称及文件正文通过 Runtime.Encrypt/Decrypt 加密；列表不返回材料正文。审核操作和受保护材料访问写入平台审计，审计摘要不含资料、文件、拒绝原因或密钥。文件响应使用 no-store、nosniff、Content-Disposition 和 sandbox CSP。

申请字段快照与当前方案分开，编辑方案不会改变旧材料的含义；重提以最新方案版本校验。历史保留每次提交/重提/审核的行为、状态、操作者、版本、时间和审核原因。与参照项目一致，不提供历史材料内容版本浏览。

## EID 资格调用合同

`Scheme.ID` 是不透明字符串，前缀 `tsc_`，不可转换为数字。Scheme DTO 包含 `id,name,description,status,fields,version,createdAt,updatedAt`。

```go
ListAvailableSchemes(ctx context.Context, rt *appkit.Runtime, s appkit.Scope) ([]Scheme, error)
CheckQualification(ctx context.Context, rt *appkit.Runtime, s appkit.Scope, schemeID string) (bool, error)
```

允许从 EID 的可信 Scope 调用，不要求 `s.ApplicationID == "trust"`；要求非空 ActorID/TenantID/ProjectID。两者先调用 `Runtime.ApplicationEnabled(ctx, tenant, project, "trust")`，只使用同项目 active 方案。未启用返回空方案/false；未知、停用、跨范围方案及未通过申请返回 false；基础设施失败返回 error。CheckQualification 按 **Scope.ActorID** 查询，不接受前端指定申请人。EID 绑定设置应保存 schemeID 并在提交时重新检查资格。

## 通知配置与恢复

- `EULER_NOTIFY_WECOM_URL`：企业微信群机器人 URL。
- `EULER_NOTIFY_FEISHU_URL`：飞书群机器人 URL。
- URL 来自部署环境，人员界面不读取或修改；要求 HTTPS，仅本机测试允许 loopback HTTP。
- 消息只包含事件、方案名、申请编号和状态，不包含个人表单、文件或拒绝原因。默认文本格式。
- 提交事务写入待发通知，事务成功后发送；不跟随重定向，网络超时 5 秒。
- 仅在渠道返回明确成功码时记录 sent。渠道拒绝为 failed；网络/响应不确定记录 unknown。未配置渠道时不创建伪发送记录。
- 进程中断可能留下 pending/sending。运营人员可从“通知记录”核实并重试；sending 至少一分钟后允许重试。未知结果的重试可能重复发送，界面先提示确认。未自动重放，以免把未知结果伪装为未发送。

不自动发送生产消息；测试只使用本地 HTTP Server。

## 验证

- 真实 SQLite 文件、AES-GCM Runtime 加密、关闭重开后的资料与材料读取。
- 提交→拒绝→重提→通过、乐观版本冲突、审核历史完整性、日志写入失败的事务回滚。
- 另一用户和另一项目不能访问申请/材料；普通项目管理角色不能审核或运营；viewer 不可写。
- EID 调用按 ActorID 隔离并强制 Trust 启用状态。
- 机器密钥限定范围、禁止 OAuth ID/查询密钥、应用停用和密钥撤销即时生效。
- 本机企微模拟服务确认并持久化真实发送结果。
- Vue 组件测试验证独立权限、动态字段编辑和提交 DTO；完整 Trust→EID 浏览器链路由平台整体验收覆盖。

运行 `go test -race ./internal/apps/trust`、`go vet ./internal/apps/trust`；前端 `vitest run apps/console-base/src/cloud/apps/trust/App.test.ts`。
