# 对象存储与认证材料

欧拉管理项目、权限、配额和审计，实际字节保存在部署者配置的 S3 兼容服务。所有以下能力属于欧拉仓库，原有独立产品保持独立。

## 已实现的使用流程

在“对象存储 → 浏览文件”中：

- **普通上传**：未启用版本管理的桶可以用限长 POST 策略直传 S3，然后由欧拉核对大小并发布。重名文件需要明确确认，携带原文件 ETag；上传期间目标变化会拒绝覆盖。
- **分片上传与续传**：选择文件后，以 8 MiB 一片经过欧拉鉴权入口上传至真实 S3 multipart 会话。支持暂停、刷新后重新选原文件续传、中止。续传逐片比对 SHA-256；同样大小但内容不同的文件不会拼接进旧会话。空文件使用普通上传。
- **历史版本与回收**：按路径前缀查看版本和删除标记；恢复旧版本为当前对象；管理者可永久删除指定版本。普通删除在版本桶内产生删除标记，历史内容仍保留并计入容量。
- **版本与生命周期**：启用或暂停 S3 版本管理；设置当前对象过期天数、旧版本永久删除天数，可限定路径前缀。欧拉只修改自身 `euler-files-` 前缀规则，保留部署者的其他规则。
- **复制 / 移动**：桶内指定源、目标路径；目标必须不存在。移动先复制成功再删除源文件。如果某一步结果不确定，界面返回待核验状态并保留日志，不自动重做可能破坏数据的操作。

所有高级写操作均有实际 S3 调用；不支持某项能力的后端会返回失败，不记录虚假的成功状态。

## 鉴权、容量与恢复语义

- 每次分片、查询、合并、中止都重新经过欧拉项目权限检查，并匹配租户、项目、桶和创建者。分片上传会话不返回 S3 凭据或上传许可。
- 一次会话预留完整文件容量，和项目其他未完成上传一起计算。默认最大文件 5 GiB，由 `EULER_STORAGE_MAX_UPLOAD_BYTES` 控制；最多 10,000 片。上传保留 24 小时，普通直传许可保留 15 分钟。
- 单片在欧拉内最多缓冲 8 MiB，收到完整数据并校验准确大小、SHA-256 后才写 S3。重复上传同一内容返回已保存结果，不同内容必须中止会话后重开。
- 合并使用数据库保存的完整、有序 S3 ETag 集合。成功合并的 staging 对象允许恢复一次响应丢失的请求。发布写入上传标识元数据，重复完成不会创建额外对象版本。
- 历史版本字节也计入项目配额。暂停版本管理不释放旧版本容量，删除标记本身不计文件容量。恢复和复制必须有足够的额外容量；移动也保守预留复制阶段的容量。
- 欧拉每五分钟清理过期上传：中止 multipart，永久移除 staging 的全部版本，再释放未完成预留。完成状态保留，以支持幂等查询。进程中断留下的未登记 S3 multipart 可按 staging 对象前缀发现并清理。
- **版本桶禁止可重放的 presigned POST**，必须使用受控分片入口。启用或暂停版本策略前，需等待该桶已签发的普通许可全部到期，包含已经确认完成的许可。这样避免旧签名重复写 staging 产生不计入预留的历史版本。部署者不应在欧拉之外同时修改其受管桶的版本策略。
- 生命周期由 S3 自身后台执行。保存规则成功不代表对象已立即删除；实际保留期限和执行延迟依赖后端。刷新“同步统计”会重新读取真实用量。

当前部署是单个 app-cloud 写实例；项目互斥保护配额与发布流程。禁止让多写实例共享同一个 SQLite 数据库或绕开欧拉对受管桶直接写入。底层 S3 运维账号能更改数据，本来就属于部署者信任边界。

## 控制台 API

前缀：`/api/v1/tenants/{tenant}/projects/{project}/apps/storage`。使用欧拉会话或平台规定的服务账号授权；浏览器修改请求需要 CSRF。每个 `{id}` 都是欧拉桶 ID，而非任意 S3 桶名。

| 方法和相对路径 | 权限 | 用途 |
| --- | --- | --- |
| `GET /buckets/{id}/multipart` | write | 本人未完成会话 |
| `POST /buckets/{id}/multipart` | write | `{key,size,contentType,expectedETag?}` 创建 |
| `GET /buckets/{id}/multipart/{upload}` | write | 查看已存分片、大小与校验值 |
| `PUT /buckets/{id}/multipart/{upload}/parts/{part}` | write | multipart/form-data 的 `file`、`sha256` |
| `POST /buckets/{id}/multipart/{upload}/complete` | write | 核验、合并、发布 |
| `DELETE /buckets/{id}/multipart/{upload}` | write | 中止并删除临时数据 |
| `GET /buckets/{id}/versioning` | read | 读取实际版本策略 |
| `PUT /buckets/{id}/versioning` | manage | `{status:"Enabled"或"Suspended"}` |
| `GET /buckets/{id}/versions?prefix=&cursor=` | read | 版本与删除标记分页，每页 100 条 |
| `POST /buckets/{id}/versions/restore` | write | `{key,versionId,expectedETag?}` |
| `DELETE /buckets/{id}/versions?key=&versionId=` | manage | 永久删除一个版本 |
| `GET /buckets/{id}/lifecycle` | manage | 实际生命周期规则 |
| `PUT /buckets/{id}/lifecycle` | manage | `{items:[{id,prefix,enabled,expirationDays,noncurrentDays}]}` |
| `POST /buckets/{id}/objects/transfer` | write | `{source,target,move}`，不覆盖目标 |

生命周期天数为 1–36500；0 表示不执行这一项，至少启用一项；最多 20 条规则。版本列表分页游标依赖所指版本仍存在；同时删除版本后应从第一页刷新。

## S3 SDK 与欧拉 REST 的区别

欧拉 `/public/storage/s3/...` 是历史兼容命名的 **欧拉 REST API**，不是 AWS S3 协议终点。不能把欧拉项目 AK/SK 填进 AWS CLI、boto3 或标准 S3 SDK 并宣称兼容 SigV4。欧拉不会把任意签名请求转发成运维权限。

- 标准 S3 SDK 可连接**底层 S3 服务**，但需要由部署者单独签发并约束的 S3 凭据；这条链路不自动继承欧拉会话、项目撤权或欧拉配额，不能向普通项目用户分发底层运维凭据。
- 日常用户和应用应通过欧拉 REST、欧拉 SDK 或控制台访问受管资源。
- 普通直传是欧拉发放严格目标、大小和有效期约束的 S3 POST policy；它不等于通用 S3 账号。
- 数据面匿名桶策略保持关闭。公开文件通过欧拉入口检查当前安装状态与桶访问策略，再返回内容。

## Trust 的可选私有材料后端

默认保留原来的 SQLite 加密材料存储。配置 `EULER_TRUST_MATERIAL_S3_BUCKET` 后，**新上传**的材料密文写入独立私有 S3 桶，SQLite 保存加密引用；原有 SQLite 密文继续正常读取，无需停机改表或先迁移全部材料。

| 配置 | 说明 |
| --- | --- |
| `EULER_TRUST_MATERIAL_S3_BUCKET` | 必须是部署者预建的专用私有桶；留空则新材料继续放 SQLite |
| `EULER_TRUST_MATERIAL_S3_ENDPOINT` | 可选；默认使用 `EULER_STORAGE_S3_ENDPOINT` |
| `EULER_TRUST_MATERIAL_S3_ACCESS_KEY` | 可选；默认使用 Storage 后端 AK，推荐单独最小权限账号 |
| `EULER_TRUST_MATERIAL_S3_SECRET_KEY` | 可选；默认使用 Storage 后端 SK |
| `EULER_TRUST_MATERIAL_S3_SECURE` | 可选；默认遵循 Storage，生产应使用 HTTPS |
| `EULER_TRUST_MATERIAL_S3_REGION` | 可选；默认遵循 Storage 区域 |

如果材料端点与 Storage 端点不同，必须显式配置材料 AK/SK，避免将原服务凭据发给另一个端点。独立凭据必须成对填写。

这只共享底层对象存储基础设施，不共享项目文件访问权限：

- 私有材料桶不注册进 `storage_buckets`，也不接受已经注册为项目桶的名称。项目 Storage 管理者不能通过文件列表、版本、复制或公开链接访问认证材料。
- 对象内容先使用 Trust 原有材料专用 AAD 加密，再写 S3。引用本身再次加密；下载时先验证申请人/审核者或限定凭据权限，再读取密文，核验长度和 SHA-256，最后解密。
- 不发放材料预签名 URL，不给浏览器任何 S3 凭据。独立私有桶不允许配置桶策略，配置错误或 S3 不可达时拒绝写入，不偷偷回退或写明文。
- 删除未提交材料后立即尝试清除所有 S3 版本。失败记录保留。方案删除或中断事务留下的密文，由 app-cloud 启动后及每小时维护清理；为避免误删正在提交的材料，孤儿记录至少保留一小时后再清理，每轮最多 100 条。
- 关闭新上传的 S3 开关不会损坏已有材料；读取已有 S3 引用仍需要可用的 endpoint/凭据。将 S3 材料全部转回 SQLite 或跨 S3 迁移，需要另外执行有完整校验的迁移任务，不能通过清空配置完成。
- 灾备必须同时保存 app-cloud 数据库、加密主密钥和私有材料桶数据。只有数据库备份不足以恢复 S3 材料。备份工具会从 `trust_material_objects` 的全部记录枚举专用桶，包含等待清理的孤儿；缺少对应 S3 制品时拒绝声称完整备份，恢复核验也会从数据库重新计算清单。

## 验证

`go test -race ./internal/apps/storage ./internal/apps/trust` 覆盖权限边界、跨项目和跨用户隔离、容量预留并发、分片校验与续传、中止及到期清理、目标 ETag 冲突、复制/移动拒绝覆盖。

设置以下变量后，`TestRealS3`、`TestRealS3Advanced` 和 `TestRealS3PrivateMaterialCompatibilityAndIsolation` 会实际连接隔离 S3：

```
EULER_TEST_S3_ENDPOINT=127.0.0.1:9000
EULER_TEST_S3_ACCESS_KEY=<isolated-test-access-key>
EULER_TEST_S3_SECRET_KEY=<isolated-test-secret>
EULER_TEST_S3_SECURE=false
EULER_TEST_S3_CORS_MODE=external
```

测试创建随机临时桶并在结束时删除全部版本。高级验收真实上传两片、重新读取会话、重复完成、读取完整字节、生成历史版本和删除标记、恢复、永久删除、统计所有历史字节、往返生命周期规则及执行拒绝覆盖复制。材料验收验证旧 SQLite 兼容、S3 内容不含明文、匿名读取被拒绝、Storage 管理者无权下载、删除和维护清理。

未设置实际 S3 时这些测试明确 **SKIP**，不能称为外部集成通过。本地受限容器使用官方 MinIO 源码的临时测试构建，仅将受禁止的网卡枚举失败回退到 loopback 地址；S3 存储与协议代码未修改。CI 使用正常 MinIO 服务进行验收。

CI 的可复现构建、启动及清理脚本位于 [app-cloud workflow](../../.github/workflows/app-cloud.yml)，固定官方源码版本 `RELEASE.2025-09-07T16-13-09Z` / `07c3a429bfed433e49018cb0f78a52145d4bedeb`，校验提交后构建，不依赖匿名镜像拉取是否可用。上述本地 loopback 适配只应用在仓库外的测试源码；它不属于发布镜像或欧拉源代码。
