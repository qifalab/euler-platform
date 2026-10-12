# 数据库与对象存储原生应用

本实现仅位于 Euler 仓库。原产品作为 Apache-2.0 功能参照，没有修改或调用原产品的管理 API、JWT 或账号库。

- 数据库参照 `ctipscn/ecloud-database@c670a395b27a97618d3d165648c4ecfa77d7a36e`。
- 对象存储参照 `ctipscn/ecloud-storage@34d138b5ef4351bdb016996fee8db7afe63b2150`。
- 原产品登录、个人资料与备用密码由 Euler 和 E时代通行证统一管理。
- 业务元数据使用平台 SQLite；数据库直接操作部署者配置的 MySQL/PostgreSQL，文件直接存入配置的 S3/MinIO。
- 数据库、桶、配额、机器密钥和资源包归项目。人员 `actorId` 用于授权和审计，不能由客户端选择资源所属项目。

## 完整功能对应

人员 API 下表均相对于 `/api/v1/tenants/{tenant}/projects/{project}/apps/{database|storage}`。所有资源 ID 都再次校验当前租户与项目。`admin` 是独立产品运营授权；项目管理员不能自动获得。

| 原数据库能力 | Euler 页面/接口 | 权限与实施说明 |
| --- | --- | --- |
| 总览、MySQL/PG 数量与容量 | 数据库页指标；`GET /`、`GET /quota` | `read`；显示真实采集量、配置状态和有效配额 |
| 库列表、详情、引擎筛选 | 数据库列表；`GET /databases`、`GET /databases/{id}` | `read`；列表不含密码 |
| 创建 MySQL/PG | 创建数据库表单；`POST /databases` | `write`；预留配额，真实 DDL，每库独立用户 |
| 删除库与用户 | 删除确认；`DELETE /databases/{id}` | `manage`；真实 DROP，失败保留资源和待核验记录 |
| 查看/复制连接信息 | 连接信息弹窗；`GET /databases/{id}/credentials` | `secrets`；AES-GCM 持久化边界由 Runtime 提供，独立读取并审计，`no-store` |
| 重置数据库密码 | 密码轮换确认；`POST /databases/{id}/password` | `secrets`；真实 ALTER，断开该用户既有会话 |
| phpMyAdmin、Adminer 入口 | 连接信息中的工具链接 | 部署者配置 URL；没有把原站没有的 SQL 编辑器列作已有功能 |
| 基础配额与资源包叠加 | 资源包页；`GET /packages` | 容量与数量按未过期资源包快照累加 |
| 兑换码兑换 | 兑换表单；`POST /packages/redeem` | `manage`；同团队项目可兑换该团队发行的码，每项目每码一次，使用上限原子校验 |
| 配额定时检查与只读恢复 | 后台任务；`POST /admin/check-quota` | 容量或数量超额时收回写权限；扩容/到期后的调整再次按真实引擎执行 |
| PG 权限修复 | 运行维护；`POST /admin/fix-pg-permissions` | `admin`；按当前配额修复当前项目，不变成跨项目全库入口 |
| 资源包模板创建/禁用/启用 | 资源包运营；`GET/POST /admin/templates`、`PUT/DELETE /admin/templates/{id}`、`POST /admin/templates/{id}/enable` | `admin`；额外提供模板编辑，历史已赠送/兑换包不被修改 |
| 兑换码批量生成、查看、复制、禁用 | 兑换码表单/表格；`GET/POST /admin/codes`、`DELETE /admin/codes/{id}` | `admin`；支持模板筛选、有效性筛选、分页、截止时间、项目使用次数 |
| 运营查看账号的资源/配额/包 | 当前项目指标、库列表、资源包页；`GET /admin/accounts` | 原个人资源账号改为项目账户，不复制原站人员密码后台 |
| 维护记录 | 运行维护；`GET /admin/operations` | `admin`；保留 pending/completed/uncertain 状态，最近 100 条 |

| 原对象存储能力 | Euler 页面/接口 | 权限与实施说明 |
| --- | --- | --- |
| 存储总览、桶/文件数、容量 | 概览指标；`GET /`、`GET /quota`、`GET /buckets` | `read`；统一使用基础配额加有效资源包，另显示上传预留 |
| 桶创建、详情、显示名/说明、删除 | 存储桶表单；`POST /buckets`、`GET/PUT/DELETE /buckets/{id}` | 创建 `write`，管理 `manage`；生成唯一真实 S3 桶名 |
| 强制删除非空桶 | 删除确认；`DELETE /buckets/{id}?force=true` | `manage`；删除全部真实对象后再删桶，结果不确定时保留记录 |
| private/public-read/public-read-write | 桶设置的访问策略 | `manage`；逻辑公开权限由 Euler 入口执行，物理 S3 桶始终私有 |
| 桶统计同步 | 同步统计；`POST /buckets/{id}/sync` | `write`；扫描实际 S3 对象，更新容量/数量 |
| 桶 CORS | CORS 来源编辑器；`POST /buckets/{id}/cors` | `manage`；bucket 模式执行真实 S3 CORS；external 模式明确显示部署者集中管理并拒绝按桶修改 |
| 文件列表、路径、目录、详情 | 文件浏览器；`GET /buckets/{id}/objects`、`GET /objects/info?key=` | `read`；`prefix/recursive/after/limit`，最多 1,000 项/页 |
| 创建目录 | 新建目录表单；`POST /objects/folder` | `write`；真实零字节目录标记 |
| 多文件上传、进度、直传与确认 | 上传按钮及进度；`POST /objects/presigned-upload`、`POST /objects/confirm-upload` | `write`；预留大小、限定大小 POST policy、随机临时对象、确认后发布 |
| 下载、签名链接分享 | 文件行操作；`POST /objects/presigned-download` | `read`；默认一小时，可请求 60–604,800 秒 |
| 文件删除、批量删除 | 单项确认、复选框批量；`DELETE /objects?key=`、`POST /objects/delete-batch` | `write`；批量返回成功和未完成对象，不能把部分失败当全部成功 |
| 公开链接、公开文件信息 | 复制公开链接；`GET /objects/public-url?key=`；公开接口见下文 | 私有桶不签发公开 URL |
| API 密钥列表、创建、一次性 SK、启停、删除 | API 密钥页；`GET/POST /keys`、`PUT/DELETE /keys/{id}` | 创建 `secrets`，管理 `manage`；项目级 read/write/delete 权限、有效期、最多 10 个 |
| 程序化对象接口 | `/public/storage/s3/...` | 只认 Euler 项目密钥，实时检查密钥状态和应用启用，不能用于人员管理 API |
| 活动/用量日志 | 使用日志；`GET /stats/logs?limit=&offset=` | `read`；按项目分页，保留真实结果状态 |
| 会员包、过期状态、兑换 | 配额与资源包页；`GET /packages`、`POST /packages/redeem` | 项目容量与桶数量累加；与 Web/机器接口统一计量 |
| 模板、码批量生成/筛选/禁用 | 资源包运营；`/admin/templates`、`/admin/codes` 同数据库 | 独立 `admin`，码支持同团队不同项目各兑换一次 |
| 管理员直接赠送包 | 模板行“赠送到本项目”；`POST /admin/grants` | 独立 `admin`；显式赠送当前项目，记录快照及审计 |

目录和完整业务实现在 `services/app-cloud/internal/apps/database/`、`.../storage/`。原生 Vue 页面在 `platform/frontend/apps/console-base/src/cloud/apps/database/`、`.../storage/`；两者复用资源包组件 `database/PackagesPanel.vue`。

## 数据库权限和操作结果

创建先持久化资源、配额预留和操作意图，再调用实际引擎。操作完成的记录使用独立短时 context，浏览器取消或事后人员撤权不会抹去已经发生的引擎变更。引擎无法确认结果时保存 `error/uncertain`，不报告创建成功，也不自动释放可能已经使用的配额。

MySQL 配额限制使用真实 GRANT/REVOKE，并断开该项目数据库用户的既有会话，避免连接缓存保留写权限。PostgreSQL 数据库由每库独立的 `NOLOGIN` owner 持有；只读时把该项目用户的对象交给这个无登录 owner，撤销写入、DDL 和函数/过程执行权限，只保留读取。不能只设置可由客户端取消的 `default_transaction_read_only`。恢复时逐个转回表、序列、视图、函数/过程和自定义类型的所有权，数据库与 schema 仍不转交给登录用户。密码轮换也断开该项目用户的会话。

后台配额任务只为仍启用的安装执行引擎权限修改；停用应用不等于撤销已经提供给客户端的数据库连接能力。模块 `Close()` 会取消后台任务并关闭管理员连接池。

## 存储上传、公开访问与机器接口

签名上传请求体为 `{key,size,contentType}`。响应是 `{uploadId,signature:{url,method:"POST",fields,expiresAt}}`。浏览器将 `fields` 和 `file` 作为 FormData 直传 S3，再提交 `{uploadId}` 确认。签名最多 15 分钟，POST policy 限定声明大小；SDK 对零大小文件不能表示 `(0,0)`，因此空文件签名限制最多 1 字节，发布确认仍严格要求 0 字节。确认验证项目、桶、最初操作人、实际对象大小和当前有效配额，并使用 ETag 条件复制，防止临时文件在检查后被替换。

实际用户对象保存于桶内 `files/`，临时对象保存于 `staging/`。临时对象不会进入文件浏览器、公开链接或用量文件数。每项目最多 100 个未完成上传，防止零字节预留无限创建。已过期临时对象由独立垃圾回收删除，停用应用仍继续清理过期临时对象；回收不删除已发布文件。

公开 URL 已由原站 MinIO 直链改为以下 Euler 入口，保留公开分享和公开写入能力：

- `GET /public/storage/files/{bucketId}/{key}`：校验当前公开读策略和应用启用后，跳转至最多五分钟的 S3 签名下载。
- `GET /public/storage/info/{bucketId}/{key}`：公开文件元数据。
- `PUT /public/storage/files/{bucketId}/{key}`：仅逻辑 `public-read-write` 桶开放；支持原始文件体或 multipart `file`，仍执行项目配额、大小上限和操作记录。

物理桶始终私有，避免匿名 S3 GET/PUT 绕过 Euler 的安装状态和访问策略。已经签发的短时链接在其有效期内继续有效；停用应用会阻止签发新链接和新的公开/机器写入。

机器接口使用 `X-Access-Key`、`X-Secret-Key`，也支持 `Authorization: Bearer <accessKey>.<secretKey>`。这是项目 REST 数据接口，并非完整 AWS SigV4 S3 网关：

- `GET /public/storage/s3/buckets`
- `GET /public/storage/s3/user/quota`
- `GET /public/storage/s3/buckets/{bucketIdOrName}/objects`
- `PUT/GET/HEAD/DELETE /public/storage/s3/buckets/{bucketIdOrName}/objects/{key}`

API 密钥只能使用创建时选择的 `read/write/delete` 数据权限。密钥只存储服务端 HMAC 摘要，SK 只在创建时返回一次。文件代理先写入有大小限制的临时文件，避免把整个上传载入内存；最终与 Web 直传使用相同的项目计量和发布逻辑。

原站仅声明但没有可用路由的分片上传、对象复制/移动、版本管理、生命周期策略不列为既有功能；新平台也不冒充实现这些功能。

## 基础设施配置

未配置引擎/S3 时，应用明确显示未配置并拒绝资源操作，不创建虚拟成功记录。模块初始化错误会阻止服务启动。

| 环境变量 | 说明 |
| --- | --- |
| `EULER_DATABASE_MYSQL_DSN` | go-sql-driver/mysql 管理员 DSN，例如部署者自己的 `user:password@tcp(host:3306)/?parseTime=true`；在部署秘密中配置 |
| `EULER_DATABASE_POSTGRES_DSN` | pgx 管理员 DSN；需要创建数据库/角色、管理权限、采集大小与结束项目会话的权限 |
| `EULER_DATABASE_MYSQL_PUBLIC_HOST` / `_PUBLIC_PORT` | 返回给项目用户的 MySQL 连接地址；默认取 DSN，端口默认 3306 |
| `EULER_DATABASE_POSTGRES_PUBLIC_HOST` / `_PUBLIC_PORT` | 返回给项目用户的 PG 连接地址；默认取 DSN |
| `EULER_DATABASE_PHPMYADMIN_URL` | 可选 MySQL 管理工具 URL，不携带密码 |
| `EULER_DATABASE_ADMINER_URL` | 可选 MySQL/PG 管理工具 URL，不携带密码 |
| `EULER_DATABASE_DEFAULT_MYSQL_MB` / `_MYSQL_COUNT` | 项目基础 MySQL 配额，默认 100 MiB / 1 库 |
| `EULER_DATABASE_DEFAULT_POSTGRES_MB` / `_POSTGRES_COUNT` | 项目基础 PG 配额，默认 100 MiB / 1 库 |
| `EULER_DATABASE_QUOTA_INTERVAL_MINUTES` | 配额检查周期，默认 5 分钟，最小 1 分钟 |
| `EULER_DATABASE_DISABLE_SCHEDULER` | 测试使用的显式 `true` 可停用周期任务；生产应保留周期执行 |
| `EULER_STORAGE_S3_ENDPOINT` | S3/MinIO API 主机和端口，不带 scheme |
| `EULER_STORAGE_S3_ACCESS_KEY` / `_SECRET_KEY` | 数据面管理凭据，仅服务端部署秘密 |
| `EULER_STORAGE_S3_REGION` | 可选 S3 region |
| `EULER_STORAGE_S3_SECURE` | 默认 TLS；仅明确设为 `false` 时使用 HTTP |
| `EULER_STORAGE_S3_PUBLIC_ENDPOINT` | 浏览器可访问的 S3 主机和端口；可与服务端地址不同，采用同一 TLS 设置 |
| `EULER_STORAGE_S3_CORS_MODE` | `bucket`（默认）调用标准 S3 每桶 CORS；`external` 用于开源 MinIO/外部网关统一配置，不伪称支持每桶 CORS |
| `EULER_STORAGE_CORS_ORIGINS` | 逗号分隔来源；默认平台 PublicURL 的 origin |
| `EULER_STORAGE_DEFAULT_BYTES` / `_DEFAULT_BUCKETS` | 项目基础存储与桶配额，默认 1 GiB / 2 桶 |
| `EULER_STORAGE_MAX_UPLOAD_BYTES` | 单次签名上传上限，默认 5 GiB |
| `EULER_STORAGE_MAX_PROXY_BYTES` | 公开/机器代理上传上限，默认 100 MiB |
| `EULER_STORAGE_DISABLE_SCHEDULER` | 测试使用的显式 `true` 停用临时对象回收；生产应保留 |

开源 MinIO 的 [官方限制文档](https://github.com/minio/minio/blob/master/docs/minio-limits.md) 将 BucketCORS 列为不支持；其 [配置文档](https://github.com/minio/minio/blob/master/docs/config/README.md) 提供集群级 `MINIO_API_CORS_ALLOW_ORIGIN`。因此使用开源 MinIO 时应明确设 `EULER_STORAGE_S3_CORS_MODE=external`，并由部署者设置 MinIO 的允许来源。该模式不会因不受支持的 API 阻塞建桶，也不会把仅保存来源字符串当作已执行每桶 CORS。S3/支持此能力的数据面保持 `bucket` 模式，API 失败时明确返回错误。

SQLite、平台加密主密钥、真实数据库和 S3 数据必须一起纳入部署备份方案。平台秘密丢失将使加密数据库凭据无法解密；不能通过生成新主密钥伪装修复。

## 验证

常规测试使用真实单连接 SQLite、真实 AES-GCM 和可控制结果的数据面替身，覆盖持久化、事务回滚、配额并发预留、兑换上限、过期资源包、项目隔离、viewer 拒写、普通项目管理员拒绝运营权限、凭据不进入列表、密钥撤销/停用安装、上传大小篡改，以及浏览器取消后保存真实外部结果。

真实服务验收以独立测试基础设施运行，不能把替身测试写成真实服务已通过：

- `TestRealEngines`：设置 `EULER_TEST_MYSQL_DSN` 和 `EULER_TEST_POSTGRES_DSN`，实测创建、写入、大小、跨库隔离、现有/新会话只读拒写、PG 过程/函数权限、恢复 DDL、密码轮换、删除及操作审计。
- `TestRealS3`：设置 `EULER_TEST_S3_ENDPOINT`、`EULER_TEST_S3_ACCESS_KEY`、`EULER_TEST_S3_SECRET_KEY`、`EULER_TEST_S3_SECURE`、`EULER_TEST_S3_CORS_MODE`，实测桶/策略、配置的 CORS 模式、带大小条件的签名 POST、确认发布、列表与读回、签名下载、拒绝匿名 S3 绕过、同步统计和删除。

没有上述测试环境变量时，真实服务测试明确 skip；CI 配置提供隔离 MySQL 8.4、PostgreSQL 17 和 S3/MinIO 数据面。

S3 CI fixture 从 [官方 MinIO release](https://github.com/minio/minio/releases/tag/RELEASE.2025-09-07T16-13-09Z) 的固定 tag `RELEASE.2025-09-07T16-13-09Z` 获取源码，并强制校验其 commit 为 `07c3a429bfed433e49018cb0f78a52145d4bedeb` 后构建。该方式替代返回 unauthorized 的 release registry 镜像拉取，不增加 registry 凭据。服务仅绑定 runner 的 `127.0.0.1:19000`，经过健康等待才运行测试；构建、启动或后续测试失败时会输出 fixture 日志，作业结束时停止进程。这仅用于测试，不改变生产 S3 部署配置。
