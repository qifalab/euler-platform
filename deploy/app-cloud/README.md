# 单机部署应用云

此目录部署独立的欧拉应用平台：`services/app-cloud`、内嵌的 WitShield 引擎和原生 Vue 控制台。八个原项目仅作为功能与代码来源，部署不依赖它们的管理服务，不改变它们的数据或登录。
需要 Docker Engine 与 Docker Compose 插件；主机不需要安装 Go、Node 或 pnpm。
这是 **单台主机、单个后端进程、SQLite 持久化卷** 的部署基线，不支持把后端扩为多副本，也不要把 SQLite 放在 NFS 等网络共享卷上。

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

身份配置缺失时控制台会说明无法登录，不会建立演示管理员。业务模块配置和完整功能对应关系见 [独立应用架构](../../docs/unified-app-cloud/NATIVE-APPS.md) 及各产品文档。MySQL、PostgreSQL、S3、邮件和 AI 服务可分别配置；未配置的基础服务会明确报告不可用，不返回虚构资源。

```sh
docker compose --env-file .env config --quiet
docker compose --env-file .env up -d --build
curl --fail http://localhost:8080/readyz
curl --fail http://localhost:8080/api/v1/session
```

访问 `http://localhost:8080`。本机样例显式启用 `EULER_ALLOW_INSECURE_LOOPBACK=true`；生产应改为 HTTPS 来源，并将该选项设为 `false`。Docker 容器里的 `localhost` 指容器本身，因此容器不能用 `http://localhost:<另一主机进程端口>` 连接主机测试 issuer；集成测试应使用项目测试工具或受控 HTTPS issuer。

后端没有主机暴露端口，Nginx 在同一来源提供静态资源并代理 `/auth/`、`/api/`、`/public/`、`/healthz`、`/readyz`。Cookie 与 CSRF 均保留同源规则。Nginx 访问日志不记录查询字符串，也不记录登录路径；不要在上层代理开启包含 OAuth 回调查询参数的日志。登录入口限流应放在能识别真实客户端地址的外层代理，避免内层把所有用户视为一个代理 IP 后共用过小限额。

生产 HTTPS 可由主机上现有反向代理终止，然后转发到默认绑定的 `127.0.0.1:8080`。配置真实证书与 HTTPS `EULER_PUBLIC_URL` 后再开放访问。此模板不申请证书、不改 DNS、不执行生产部署。若前置代理运行在另一个容器，应按实际网络拓扑接入，不能将其 `localhost` 当作宿主机。

## 持久化与升级

命名卷 `app-cloud-data` 保存 `/data/app-cloud.db`、业务数据和 `/data/apps/` 下每个项目独立的 WitShield 数据库及相关文件；后端以 UID/GID `10001:10001` 运行，镜像首次创建卷时初始化目录属主。手工导入的目录也必须允许该用户读写。

普通停止或重新构建保留数据库。**`docker compose down --volumes` 会删除数据库与备份卷，不要用于升级。** 升级前先做以下备份，再执行 `docker compose up -d --build`。数据库迁移随服务启动运行；版本回退应同时恢复对应版本的备份与原加密密钥。

## 整个平台的备份与恢复

完整备份必须覆盖整个 `/data`，包括各项目的 WitShield 数据库。原 `sqlite_snapshot.py` 仍可校验单个数据库，但单独备份 `app-cloud.db` 不再代表整个应用平台。

以下归档在停止后端的维护窗口执行，保留所有 SQLite 文件和可能存在的 WAL。先构建维护镜像，再停后端：

```sh
docker compose --profile maintenance build maintenance
app_cloud_stamp=$(date -u +%Y%m%dT%H%M%SZ)
docker compose stop app-cloud
docker compose run --rm --no-deps --entrypoint sh maintenance \
  -c 'umask 077; exec tar -czf "$1" -C /data .' sh "/backups/euler-${app_cloud_stamp}.tar.gz"
docker compose start app-cloud
```

命令成功后，导出归档到已有的受保护目录，并单独保管 `EULER_ENCRYPTION_KEY`：

```sh
umask 077
docker compose run --rm --no-deps -T --entrypoint cat maintenance \
  /backups/euler-YYYYMMDDTHHMMSSZ.tar.gz > /secure/backup/euler-data.tar.gz
```

恢复时使用一个**新的空数据卷**，保留现有卷用于回退，避免混入较新版本的 WAL 或项目文件。归档仅接受自己创建且已校验来源的备份。停止后端后，创建恢复卷并通过临时维护容器解包，再在 Compose override 中将 `app-cloud-data` 指向该卷；使用备份对应版本和同一加密密钥启动，并检查 `/readyz`、项目、申请和设备记录。确认恢复成功后再处理旧卷。恢复历史快照会恢复当时的成员授权与会话状态，需核对近期撤权。

外部业务数据库、S3 对象及其服务凭据需要用相应基础设施的备份流程另行保护；`/data` 归档只包含欧拉控制与应用元数据，不包含这些远程数据。WitShield Agent/Helper 二进制随镜像构建，可从 `/opt/euler/device-tools/` 提取并分发到自己管理的设备，具体部署见 [WitShield 说明](../../docs/unified-app-cloud/WITSHIELD.md)。

## 验证

`.github/workflows/app-cloud.yml` 检查 Go vet、race 测试、真实 MySQL/PostgreSQL/S3 集成测试、WitShield 设备与 Unix socket 协议测试、静态后端构建、console-base 类型与构建、SQLite 备份恢复测试、三个镜像构建及真实 Compose 启动。独立浏览器任务运行 `playwright.app-cloud.config.ts`，使用真实本地后端和协议测试身份源；失败时保留三天截图与 trace。工作流只验证，不发布镜像或部署环境。

浏览器优先通过 Playwright 官方安装器获取；仅当安装失败时，从 [Chrome for Testing 官方元数据](https://github.com/GoogleChromeLabs/chrome-for-testing#json-api-endpoints) 获取 Stable Linux64 `chrome-headless-shell`。工具只接受该官方存储桶的下载地址，记录实际版本、下载来源和本地计算的 SHA-256（记录下载内容，不冒充上游签名验证），不跳过测试。

本地只验证备份工具可运行：

```sh
# 在仓库根目录
python3 -m unittest discover -s tools/app-cloud -p 'test_*.py' -v
```
