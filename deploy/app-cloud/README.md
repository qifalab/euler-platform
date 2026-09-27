# 单机部署应用云

此目录部署独立的欧拉应用平台：`services/app-cloud`、内嵌的 WitShield 引擎和原生 Vue 控制台。八个原项目仅作为功能与代码来源，部署不依赖它们的管理服务，不改变它们的数据或登录。
需要 Docker Engine 与 Docker Compose 插件；主机不需要安装 Go、Node 或 pnpm。
这是 **单台主机、单个后端进程、SQLite 持久化卷** 的部署基线，不支持把后端扩为多副本，也不要把 SQLite 放在 NFS 等网络共享卷上。

八个原生应用、服务账号、应用联动、项目生命周期与 SDK 的当前范围见 [平台说明](../../docs/unified-app-cloud/README.md)。本页描述部署步骤；真实通行证、邮件、AI、SQL/S3 以及生产恢复结果必须在实际环境验收，不能用配置存在或 `/readyz` 成功代替。

## 配置与启动

在仓库根目录执行：

```sh
cd deploy/app-cloud
cp env.example .env
chmod 600 .env
```

编辑 `.env`。必须设置 `EULER_ENCRYPTION_KEY`，用 `openssl rand -base64 32` 生成一次 32 字节随机密钥，并保存在独立的密码管理器中。数据库重启、迁移、恢复都必须使用同一密钥；不要在每次启动时重新生成，也不要把它提交到 Git。

登录使用 **E时代通行证**。EID 是成员资格应用，Trust 提供认证事实，均不是这里的登录身份源。根据通行证已部署接口设置：

| 配置 | 用途 |
| --- | --- |
| `EULER_PUBLIC_URL` | 用户访问的完整来源，例如 `https://cloud.example.com`；不含路径。服务据此校验回调与设置 Cookie，不信任代理传来的用户身份。 |
| `EULER_OIDC_ISSUER` | 通行证实际 issuer，保留其准确路径与末尾斜杠。 |
| `EULER_OIDC_CLIENT_ID` / `EULER_OIDC_CLIENT_SECRET` | 专为 Euler 注册的客户端，不复用 EID 的客户端。 |
| `EULER_OIDC_REDIRECT_URL` | 默认为公开来源加 `/auth/callback`，上游必须登记这个精确地址。 |
| `EULER_IDENTITY_MODE` | 默认 `oidc`；仅在上游明确需要时选 `oauth2_userinfo`，并配置样例列出的固定 OAuth2 端点。 |
| `EULER_PLATFORM_ADMIN_IDENTITIES` | 精确的 `provider + subject` JSON 数组。只允许这些平台运营者授予项目级应用 `review` / `admin` 权限；不按邮箱匹配，不自动给团队 owner 审核权。 |
| `EULER_DATABASE_MYSQL_DSN` / `EULER_DATABASE_POSTGRES_DSN` | 欧拉专用基础数据库的管理连接；产品模块真实创建业务库和独立数据库用户。不可填写原项目的元数据库。 |
| `EULER_DATABASE_*_PUBLIC_HOST/PORT` | 用户连接已创建数据库时使用的外部地址。 |
| `EULER_STORAGE_S3_ENDPOINT` / `ACCESS_KEY` / `SECRET_KEY` | 欧拉专用 S3/MinIO 后端及服务端管理凭据。 |
| `EULER_STORAGE_S3_PUBLIC_ENDPOINT` | 浏览器实际可访问的签名上传/下载端点；通常与容器内地址不同。 |
| `EULER_STORAGE_S3_CORS_MODE` | 默认 `bucket` 使用标准 S3 每桶 CORS。开源 MinIO 配置为 `external`，并在 MinIO 设置 `MINIO_API_CORS_ALLOW_ORIGIN` 为 Euler 来源；此模式 UI 明示 CORS 由部署者统一管理，不伪称支持每桶修改。 |
| `EULER_STORAGE_CORS_ORIGINS` | 逗号分隔浏览器来源，默认采用 Euler Public URL 的来源。在 `external` 模式须与部署的数据面来源配置保持一致。 |
| `EULER_WEAUTH_TRUSTED_PROXIES` | 允许转发真实客户端 IP 的反向代理 CIDR。只填实际受控代理地址；默认忽略客户端伪造的 XFF。 |
| `EULER_OPERATIONS_TOKEN` | 可选的独立只读监控令牌，用于受保护的 `/ops/diagnostics` 和 `/ops/metrics`；未配置时仅允许平台管理员会话访问。 |

身份配置缺失时控制台会说明无法登录，不会建立演示管理员。业务模块配置和完整功能对应关系见 [独立应用架构](../../docs/unified-app-cloud/NATIVE-APPS.md) 及各产品文档。MySQL、PostgreSQL、S3、邮件和 AI 服务可分别配置；未配置的基础服务会明确报告不可用，不返回虚构资源。

身份配置有效但 OIDC discovery 暂不可用时，服务在后台自动重试；已有欧拉会话与登出不依赖此次 discovery 成功。可选数据面故障不改变核心就绪含义，检查时应同时读取受保护诊断。重试周期、指标和各状态的准确含义见 [OPERATIONS.md](../../docs/unified-app-cloud/OPERATIONS.md)。

```sh
docker compose --env-file .env config --quiet
docker compose --env-file .env up -d --build
curl --fail http://localhost:8080/readyz
curl --fail http://localhost:8080/api/v1/session
```

访问 `http://localhost:8080`。本机样例显式启用 `EULER_ALLOW_INSECURE_LOOPBACK=true`；生产应改为 HTTPS 来源，并将该选项设为 `false`。Docker 容器里的 `localhost` 指容器本身，因此容器不能用 `http://localhost:<另一主机进程端口>` 连接主机测试 issuer；集成测试应使用项目测试工具或受控 HTTPS issuer。

后端没有主机暴露端口，Nginx 在同一来源提供静态资源并代理 `/auth/`、`/api/`、`/public/`、`/ops/`、`/healthz`、`/readyz`。Cookie 与 CSRF 均保留同源规则。Nginx 访问日志不记录查询字符串，也不记录登录路径；不要在上层代理开启包含 OAuth 回调查询参数的日志。登录入口限流应放在能识别真实客户端地址的外层代理，避免内层把所有用户视为一个代理 IP 后共用过小限额。

生产 HTTPS 可由主机上现有反向代理终止，然后转发到默认绑定的 `127.0.0.1:8080`。配置真实证书与 HTTPS `EULER_PUBLIC_URL` 后再开放访问。此模板不申请证书、不改 DNS、不执行生产部署。若前置代理运行在另一个容器，应按实际网络拓扑接入，不能将其 `localhost` 当作宿主机。

## 持久化与升级

命名卷 `app-cloud-data` 保存 `/data/app-cloud.db`、业务数据和 `/data/apps/` 下每个项目独立的 WitShield 数据库及相关文件；后端以 UID/GID `10001:10001` 运行，镜像首次创建卷时初始化目录属主。手工导入的目录也必须允许该用户读写。

普通停止或重新构建保留数据库。**`docker compose down --volumes` 会删除数据库与备份卷，不要用于升级。** 升级前按 [完整备份恢复指南](../../docs/unified-app-cloud/RECOVERY.md) 建立并验证协调恢复点，再按 [发布流程](RELEASE.md) 切换镜像。应用版本兼容检查先于模块迁移，全部迁移成功后记录版本；版本回退应恢复相匹配的数据、外部资源和原加密密钥，不能只切回旧镜像。

## 整个平台的备份与恢复

备份入口已升级为 `tools/app-cloud/platform_backup.py`：覆盖整个 `/data` 的主库、各项目 WitShield 库及应用文件，逐文件哈希与主密钥派生 HMAC 认证清单，校验全部 SQLite，拒绝符号链接/路径穿越，恢复只能写入新目录。原 `sqlite_snapshot.py` 继续用于单库维护，不能代替平台备份。

项目页面导出的配置清单不包含业务内容、文件字节或秘密，也不能用作恢复包。完整命令与恢复验收统一维护在 [RECOVERY.md](../../docs/unified-app-cloud/RECOVERY.md)，以下仅展示调度入口。

```sh
# 仓库根目录；先构建 maintenance 镜像。
docker compose -f deploy/app-cloud/compose.yaml --profile maintenance build maintenance
python3 tools/app-cloud/scheduled_backup.py \
  --compose-dir deploy/app-cloud --release <实际部署提交或镜像摘要>
```

调度器停止并确认后端，备份后再次验证，无论成功失败都尝试恢复原来的服务运行状态。存在外部 SQL/S3 资源时，上述命令会要求对应外部数据和配置制品；通过 freeze/capture/thaw hooks 在同一维护窗口生成，不能仅保存本机元数据后宣称完整备份。临时元数据备份须明确选择 `--allow-incomplete-external`。

完整命令、外部制品 schema、Linux 定时任务、新目录恢复、实际恢复演练和安全边界见 [RECOVERY.md](../../docs/unified-app-cloud/RECOVERY.md)。归档本身未整包加密，包含敏感应用数据，应异机加密保存；主密钥在独立密码管理器中保管。恢复历史数据会恢复旧授权和队列记录，需重新应用备份后的撤权并核对已投递通知。

WitShield Agent/Helper 二进制随镜像构建，可从 `/opt/euler/device-tools/` 提取并分发到自己管理的设备，具体部署见 [WitShield 说明](../../docs/unified-app-cloud/WITSHIELD.md)。

## 预发布、监控和回退

`EULER_OPERATIONS_TOKEN` 是与主密钥独立的高熵只读监控令牌，只用于 `/ops/diagnostics` 与 `/ops/metrics`；Nginx 将 `/ops/` 代理到后端并保留服务端认证，不授予业务管理权限。公开的 `/healthz` 与 `/readyz` 只提供核心摘要。不要把运营令牌放在 URL 查询参数或前端配置中。

```sh
python3 tools/app-cloud/deployment_check.py config --env-file deploy/app-cloud/.env
python3 tools/app-cloud/deployment_check.py acceptance --base-url https://<预发布域名>
```

工具只做静态配置和无凭据只读检查，不发送测试通知或替操作者登录。真实通行证、SMTP 入箱、AI、跨项目权限与外部数据恢复保留待验收项；`--strict` 在仍有待验收项时返回 2。正式发布、镜像记录、迁移和新卷回退流程见 [RELEASE.md](RELEASE.md)。

## 验证

`.github/workflows/app-cloud.yml` 检查 Go vet、race 测试、真实 MySQL/PostgreSQL/S3 集成测试、WitShield 设备与 Unix socket 协议测试、静态后端构建、console-base 类型与构建、单库与完整平台备份恢复测试、三个镜像构建及真实 Compose 启动。独立浏览器任务运行 `playwright.app-cloud.config.ts`，使用真实本地后端和协议测试身份源；失败时保留三天截图与 trace。工作流只验证，不发布镜像或部署环境。

浏览器优先通过 Playwright 官方安装器获取；仅当安装失败时，从 [Chrome for Testing 官方元数据](https://github.com/GoogleChromeLabs/chrome-for-testing#json-api-endpoints) 获取 Stable Linux64 `chrome-headless-shell`。工具只接受该官方存储桶的下载地址，记录实际版本、下载来源和本地计算的 SHA-256（记录下载内容，不冒充上游签名验证），不跳过测试。

本地只验证备份工具可运行：

```sh
# 在仓库根目录
python3 -m unittest discover -s tools/app-cloud -p 'test_*.py' -v
```
