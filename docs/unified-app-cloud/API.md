> 旧外部连接器 API 的历史契约。当前独立平台使用 [NATIVE-APPS.md](NATIVE-APPS.md) 的原生模块契约；生产启动会拒绝旧 `/connection`、`/summary`、`/resources` 入口。身份、团队、项目、安装与审计 API 继续有效。

# 应用云 HTTP 与模块契约

所有新增路径由 `services/app-cloud` 提供，前端同源访问。JSON采用 camelCase。有响应体时，成功对象直接返回；列表为 `{ "items": [...] }`；业务错误为 `{ "error": { "code": "...", "message": "..." } }`。时间使用RFC3339，ID是不透明字符串。业务写接口的JSON请求体最大64 KiB，未知字段、多段JSON和无效字段返回400。

## 身份

- `GET /auth/login?returnTo=/`：跳转已配置身份源。returnTo仅接受本站相对路径。
- `GET /auth/callback`：校验授权结果并建立独立Euler会话。
- `GET /api/v1/session`：`{ authenticated, user?: {id, displayName, provider, subject}, csrfToken?: string, loginAvailable: boolean, loginError?: string }`。
- `POST /auth/logout`：销毁会话，返回204。
- 所有写请求带 `X-CSRF-Token`，Cookie凭据同源；未登录401、无权限403或不可枚举资源404。绝不信任浏览器提交的用户ID/角色或X-Euler-Account-Id。

identity模块的 `SessionStore` 由 `platform.Store` 实现，具体接口见 `services/app-cloud/internal/identity/types.go`。它包含主体联结、会话读写与一次性登录事务的原子消费。登录事务中的 PKCE verifier 在库中加密保存。

会话令牌只在Cookie原文传输，数据库保存摘要。identity提供 `ConfigFromEnv()`、`New(ctx, Config, SessionStore)`、`Register(mux)`、`Authenticate(request)` 和 `ValidateCSRF(request, session)`；platform在每次业务请求中执行认证与写请求CSRF。构造错误不得降级为匿名管理员；会话接口说明登录配置缺失或身份源不可用。

## 团队与项目

- `GET/POST /api/v1/tenants`：列出我的团队/创建 `{name}`；创建者owner。Tenant `{id,name,role,createdAt}`。
- `GET /api/v1/tenants/{tenantID}/members`：`{items:[{userId,displayName,role,joinedAt}]}`。
- `PATCH/DELETE /api/v1/tenants/{tenantID}/members/{userID}`：变更 `{role}`/移除成员，成功返回204。需要团队owner/admin；admin不能修改owner或授予owner。最后一名owner不能被移除或降级，否则409。
- `POST /api/v1/tenants/{tenantID}/invitations`：`{role}` → `{id,role,token,expiresAt}`，成功返回201。owner/admin创建，role只能为admin/member/viewer；有效期7天，token仅首次展示，哈希持久化；不自动发消息。
- `GET /api/v1/tenants/{tenantID}/invitations`：有效邀请，不返回token。
- `DELETE /api/v1/tenants/{tenantID}/invitations/{id}`：owner/admin撤销尚未使用、未撤销的邀请，成功204。
- `POST /api/v1/invitations/accept`：`{token}` → Tenant，成功200；邀请为持有者加入的限时单次链接，UI明确告知勿公开。已是成员时仍消费邀请，但不会改变现有角色；过期、已消费或已撤销返回404。
- `GET/POST /api/v1/tenants/{tenantID}/projects`：列出可访问项目/创建 `{name}`。Project `{id,tenantId,name,role,createdAt}`，role为当前用户的有效项目角色；创建仅限团队owner/admin，成功201。
- `GET/POST /api/v1/tenants/{tenantID}/projects/{projectID}/members`：列出显式项目成员绑定/赋予 `{userId,role}` (admin/member/viewer)。GET返回Member列表，不额外列出从团队继承权限的owner/admin；POST需要项目admin，成功204。只能赋予有效团队成员，团队viewer只能被赋予项目viewer。
- `DELETE /api/v1/tenants/{tenantID}/projects/{projectID}/members/{userID}`：项目admin移除显式项目授权，成功204；不会撤销团队owner/admin继承的权限。
- `GET /api/v1/tenants/{tenantID}/audit?projectId=...&limit=50&before=...`：返回 `{items:[{id,actorId,action,targetId,projectId,createdAt,summary}]}`。团队owner/admin可读全团队或指定项目；其他用户必须指定projectId且具有该项目的有效admin权限。

审计按 `createdAt DESC, id DESC` 排序。`limit`省略、非整数或小于等于0时使用50，大于100时限制为100。首次不传`before`；下一页传上一页最后一条事件的id，返回该事件之前的记录，不重复游标记录。响应没有`nextCursor`字段。游标必须属于当前团队，传projectId时也必须属于该项目，否则404。

团队角色为owner/admin/member/viewer；项目角色为admin/member/viewer。团队owner/admin自动具有全部项目的admin权限；团队member/viewer须先获得项目绑定才能访问对应项目。团队viewer是只读上限，即使此前存在项目admin/member绑定，降为团队viewer后也不能写入。成员被移出团队时，其项目绑定一并删除，重新加入不会恢复旧绑定。

项目admin可管理项目成员、应用启用状态和连接；项目admin/member可执行目录允许的资源创建、删除；项目viewer只能读取。有效项目角色在每次请求时重新校验。

## 目录与应用

- `GET /api/v1/catalog`：无需登录，返回 `{items:[Application]}`。Application `{id,name,category,description,color,homepage,repository,capabilities:string[],connectionMode,limitations:string[]}`。固定ID `eid,trust,weauth,database,storage,statistics,lottery,witshield`。其余团队、项目及应用业务接口均需登录。
- `GET/POST /api/v1/tenants/{tenantID}/projects/{projectID}/installations`：列表/启用 `{applicationId}`。Installation `{id,tenantId,projectId,applicationId,status,createdAt,connection?:{baseUrl,configured,updatedAt}}`，项目每产品一条，状态 `enabled/disabled`。POST需要项目admin，成功201；已有记录时409，重新启用使用PATCH。
- `PATCH /api/v1/tenants/{tenantID}/projects/{projectID}/installations/{id}`：`{status}`，仅项目admin，成功200并返回更新后的Installation。
- `PUT /api/v1/tenants/{tenantID}/projects/{projectID}/installations/{id}/connection`：`{baseUrl,credential?:string,externalAccountId?:string}`，仅项目admin，成功200并返回Installation；凭据和外部账号标识不出现在响应。地址需处于部署者允许来源；仅地址不变时，空credential保留已有凭据。换来源不能隐式携带旧凭据，已有本地资源关联时替换来源或外部账号返回409，须先断开连接。兼容接收externalAccountId，但真实归属来自上游身份接口，不能由用户声明。外部账号同一产品只绑定一个项目，防止跨项目共享资源。Storage凭据字符串为JSON `{kind:"access_key_pair",accessKey,secretKey}`，WeAuth/Database可传独立用户Bearer令牌。
- `DELETE .../installations/{id}/connection`：项目admin清除连接、凭据和本地资源关联，成功204；不删除远端业务资源。
- `GET .../installations/{id}/summary`：当前用户/项目摘要 `{state:"ready"|"unconfigured"|"unavailable"|"unsupported",message,metrics?:[{label,value}],verification?:{status,identityType?},consoleUrl?,checkedAt}`。上游没有的数据不虚构。
- `GET .../installations/{id}/resources`：适配器可用时返回 `{items:[{id,name,type,status?,url?}]}`；仅列出本项目绑定资源或独立外部账号资源。
- `POST .../installations/{id}/resources`：第一版支持WeAuth站点，`{name,domains:string[]}`，成功201并返回真实创建的Resource，记录外部ID。
- `DELETE .../installations/{id}/resources/{resourceID}`：仅支持目录声明的能力，检查项目资源绑定后调用上游；未绑定资源返回404，成功204。
- 不支持的操作返回 `unsupported` 错误及501，不生成模拟实例。

`connection.configured`仅表示已有连接配置，不保证上游凭据仍有效；有效性由摘要或资源请求重新检查。禁用应用的summary仍返回200、state为unavailable；资源列表及写操作返回409。EID/Trust摘要使用部署者的固定来源与当前登录主体，无须先保存项目连接；不接受项目凭据或浏览器指定的主体。

资源写操作在调用上游前持久化授权与操作编号，并返回 `X-Euler-Operation-Id`。审计事件为 `resource.create_requested` / `resource.delete_requested`，确认完成后为 `resource.create_completed` / `resource.delete_completed`。上游成功后的本地资源关联与审计使用独立短时事务完成；调用过程中撤权不丢弃已发生的结果，后续请求仍立即重新校验权限。上游调用返回错误时保守记录 `resource.operation_uncertain`，需要先检查原产品再决定重试，不自动重复创建；不支持的操作在调用上游及创建操作记录前直接返回501。

## 连接器接口（internal/connectors）

本包不依赖platform/identity，用请求参数携带已授权上下文。平台负责权限、解密和持久化资源ID。连接器负责固定API路径、响应校验、超时、可控出站与错误分类。具体Go声明由connector owner定义并与platform owner直接对齐。

`Connection{BaseURL, Credential, ExternalAccountID}`、`Subject{Provider, ID}`、`Application`、`Summary`、`Resource`、`CreateResource{Name, Domains}`。

构造 `New(Config) (*Manager,error)`。`Catalog() []Application`；`ValidateConnection(applicationID,Connection)`和`ValidateConnectionContext(ctx,applicationID,Connection)`返回规范化且已核验账号归属的`(Connection,error)`；`Summary(ctx,applicationID,Connection,Subject) Summary`将不可用原因放在state/message中；`Resources`、`Create`、`Delete`分别返回`([]Resource,error)`、`(Resource,error)`、`error`。HTTP连接保存使用带context的校验方法。能力不足明确返回错误。EID/Trust凭部署配置查询当前已验证主体；前端不能任意指定oauthId。Trust方案ID来自服务端配置。

## 开发、测试和生产边界

测试使用独立可验证的OIDC测试issuer和符合各项目代码的HTTP契约服务。测试替身只属于tests/tools，不在生产接口中添加选择任意用户的登录入口。生产缺失issuer/client配置时登录明确不可用。集成测试必须在真实持久化库和真实HTTP服务上执行。
