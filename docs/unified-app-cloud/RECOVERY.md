# 备份、恢复与发布验收

欧拉当前采用单个后端进程和本机 SQLite。备份以停止全部写入后的共同恢复点为准；不把多个数据库分别在线快照称为跨应用一致性。原八个项目仍独立，不在欧拉备份工具的操作范围内。

## 备份覆盖及完成状态

`tools/app-cloud/platform_backup.py` 是完整应用数据目录的备份入口。它保存主 SQLite、每个项目的 WitShield SQLite、其他应用文件，以及显式提供的外部数据制品。每个 SQLite 使用 backup API 收入已提交 WAL，再做 `integrity_check` 与 `foreign_key_check`；恢复文件不携带旧 WAL/SHM。

| 状态 | 实际保证 | 仍需验收 |
| --- | --- | --- |
| 本地文件校验通过 | 清单与 ZIP 文件逐一对应，大小、SHA-256、SQLite 内容检查通过 | 业务页面和权限行为 |
| 清单认证通过 | 由主密钥派生的 HMAC 认证清单；未持有原密钥者不能重新签署篡改后的文件哈希 | 主密钥独立保管、备份存储访问控制 |
| `artifacts-included` | 元数据库登记的每一个 SQL 数据库/S3 桶都有对应数据和配置制品，全部哈希通过 | SQL、对象及权限配置在隔离基础设施中的实际恢复 |
| `incomplete` | 操作者显式选择了 `--allow-incomplete-external`，存在未备份的外部资源 | 不能作为完整业务恢复点 |
| `productionRestoreAccepted: false` | 工具没有宣称完成真实生产验收 | 真实身份登录、业务、邮件、AI、数据面及恢复演练 |

归档包含 SHA-256 主密钥指纹，不包含主密钥明文。归档本身没有整包加密，可能包含个人资料、SQL 转储及外部服务配置，应存入加密、限权、独立于服务器的备份存储。主密钥保存在独立密码管理器中，恢复时使用相同的 `EULER_ENCRYPTION_KEY`。指纹只检查所提供密钥一致，不能凭空判断最初备份时提供的密钥是否就是运行平台的密钥。

所有持久应用目录必须放在 `--data-dir` 下。Compose 已将主库和 `EULER_APP_DATA_DIR` 固定在 `/data`。自定义裸机部署若将应用文件分散在其他位置，必须先统一持久目录或为每个额外位置建立同一停机窗口内的备份，不能漏备。

## 停机与一致性

备份必须确认 app-cloud、WitShield 后台任务及其他直接写本机应用目录的进程停止。工具还会：

- 尝试拒绝同一可见 Linux PID 空间内仍打开数据文件的其他进程。
- 同时取得全部 SQLite 的写入保留锁；存在写事务则拒绝备份。
- 检查复制前后目录、文件大小、inode 和修改时间，发现并发变更就丢弃临时制品。
- 拒绝符号链接、管道及特殊文件，不覆盖已有归档。

这些检查无法看见其他容器的全部进程，也无法冻结远端数据库/S3 写入。因此 `--service-stopped` 是必须确认的操作前提，不是远程停机证明。停用欧拉控制台不等于停止第三方直连数据库客户端、签名上传和设备流量。跨数据面恢复点需要先冻结这些写入口，再导出 SQL/S3，最后备份欧拉本机数据。

## 本机 CLI

在受保护的运维会话中通过环境变量注入原主密钥，不把密钥写进命令行参数或 Shell 历史。以下均从仓库根目录执行：

```sh
# 先停止所有写入。必须是已存在的安全备份目录，输出文件必须全新。
python3 tools/app-cloud/platform_backup.py backup \
  --data-dir /srv/euler/data \
  --output /secure/euler/20260927-before-upgrade.zip \
  --service-stopped --release <实际部署提交或镜像摘要> \
  --external-inventory /secure/euler/external/manifest.json

python3 tools/app-cloud/platform_backup.py verify /secure/euler/20260927-before-upgrade.zip

# destination 必须不存在。恢复产生 destination/data 和 destination/external。
python3 tools/app-cloud/platform_backup.py restore \
  /secure/euler/20260927-before-upgrade.zip /srv/euler/recovered-20260927 \
  --service-stopped
```

如果元数据库完全没有外部 SQL 数据库或 S3 桶，可省略 `--external-inventory`。存在资源但尚无外部制品时默认失败；临时只保存控制与应用元数据必须明确传 `--allow-incomplete-external`，之后 verify/restore 同样需要这个选项。

ZIP 解包不使用 `extractall`。工具拒绝绝对路径、`..`、反斜线/盘符、重名条目、链接、特殊文件、未声明文件及不存在的清单项；先在私有暂存目录验证全部数据，再创建新目标目录。默认解压上限 64 GiB、100000 个文件；较大备份使用明确的 `--max-bytes` 调整容量，并保证工作盘能容纳未压缩内容。文件按 0600、目录按 0700 恢复，原有宽松权限不传播。

## 外部 SQL/S3 接入

生成实际资源清单不读取或输出数据库密码：

```sh
python3 tools/app-cloud/platform_backup.py inventory /srv/euler/data/app-cloud.db \
  > /secure/euler/external/inventory-template.json
```

清单来源是 `database_instances`、`storage_buckets` 与 `trust_material_objects` 的真实记录，包括异常状态资源，不根据 UI 是否显示正常就省略。Trust 的材料桶独立于项目 Storage；工具按 ledger 中的 bucket 聚合，包括等待清理的孤儿和旧材料桶，不能只备当前环境变量中的桶，也不能只 JOIN 尚未删除的材料。由基础设施备份程序将清单补成下面格式，制品路径相对于清单所在目录，不能包含 `..` 或符号链接：

```json
{
  "schemaVersion": 1,
  "resources": [
    {
      "kind": "postgres",
      "id": "控制面中实际资源ID",
      "name": "实际数据库名",
      "capturedAt": "2026-09-27T10:00:00Z",
      "restoreProcedure": "先在隔离 PostgreSQL 恢复角色和权限，再恢复数据库并执行读写验证",
      "artifacts": [
        {"purpose": "data", "path": "db.dump", "sha256": "实际文件SHA-256"},
        {"purpose": "configuration", "path": "roles-and-grants.sql", "sha256": "实际文件SHA-256"},
        {"purpose": "restore-evidence", "path": "isolated-restore-result.json", "sha256": "可选真实演练报告SHA-256"}
      ]
    }
  ]
}
```

每个资源至少有 `data` 和 `configuration` 两类制品。可包含同一基础设施快照内的多个资源，但必须逐个对应实际 ID；工具不接受清单中多出未知资源，避免混入独立原项目。制品的内容由基础设施备份系统产生；哈希验证不判断一个 SQL 文件是否包含完整数据，也不替代恢复测试。

Trust 私有材料桶的条目仍使用 `kind: "s3"`，ID 是 `trust-materials:` 加桶名 UTF-8 的 SHA-256，另带 `scope: "trust-private-materials"` 和 ledger 的 `objectCount`。直接采用 inventory 命令产生的 ID，不手工猜写。清单不暴露材料 ID、对象 key 或材料明文；verify 从已恢复 SQLite 重新计算桶集合和引用数，即使清单签名有效，遗漏私有材料桶仍会失败。只用内联 SQLite 材料的旧部署不需要额外 S3 制品。

材料桶由 `EULER_TRUST_MATERIAL_S3_BUCKET` 启用，可使用专用 ENDPOINT/ACCESS_KEY/SECRET_KEY/SECURE/REGION；空连接字段沿用 STORAGE 对应配置。更换独立服务地址时应同时配置匹配凭据。maintenance 只需从 SQLite ledger 读取桶标识，不接收材料 S3 凭据，也不解密材料；外部 capture hook 才通过受保护配置访问实际数据面。恢复必须保留私有策略、对象密文与原主密钥，再由 Trust 的本人/审核接口校验内容和权限，不能将这些桶导入项目可管理的 Storage 列表。

| 数据面 | `data` 至少包含 | `configuration` 至少包含 | 实际恢复检查 |
| --- | --- | --- | --- |
| MySQL | 数据、表结构、视图、触发器、存储程序及事件；使用引擎适配的逻辑/物理备份 | 独立用户、认证/授权、只读策略、数据库字符集与版本 | 在隔离 MySQL 中恢复，用项目用户读取约定记录，验证允许的写入与禁止的操作 |
| PostgreSQL | 数据、schema、序列、函数、扩展要求和大对象（如使用） | 角色、所有权、ACL、默认权限、只读策略与版本 | 恢复角色再恢复库，检查序列、表数据及项目用户的权限边界 |
| S3 | 对象内容；启用版本时包括历史版本与删除标记，不能只复制当前键 | bucket policy、CORS、生命周期、版本设置、保留/Object Lock 要求及服务端加密恢复前提 | 在隔离端点读取清单样本/版本并比对 SHA-256，检查对象数/总字节、ACL、签名读写和跨桶拒绝 |

不要把普通 `sync`/`mirror` 当作含版本、删除标记和配置的完整 S3 备份。物理快照也需按后端服务的停机/一致性要求制作。基础设施凭据应通过私有配置文件或 Secret 注入备份程序，不加入命令行或日志；恢复凭据/加密服务若位于外部密码管理器，记录其恢复引用和访问方法。

## 可调度 Compose 备份

先构建 maintenance 镜像。维护容器以 UID/GID 10001 运行，`/data` 和 `/backups` 与平台命名卷一致：

```sh
cd deploy/app-cloud
docker compose --profile maintenance build maintenance
cd ../..
python3 tools/app-cloud/scheduled_backup.py \
  --compose-dir deploy/app-cloud --release <实际部署提交或镜像摘要>
```

调度器使用文件锁防止重入，记录后端原先是否运行，停止并再次确认 app-cloud，执行 backup + verify，最后恢复原先运行状态。失败也尝试恢复服务，返回非零状态；不会自动删除旧备份。没有外部资源时上面的命令即可使用。

有外部资源时提供三个由管理员维护的**绝对路径可执行文件**：

```sh
python3 tools/app-cloud/scheduled_backup.py \
  --compose-dir /opt/euler-platform/deploy/app-cloud \
  --release <实际部署提交或镜像摘要> \
  --external-dir /secure/euler/capture-20260927 \
  --freeze-hook /etc/euler/backup/freeze \
  --capture-hook /etc/euler/backup/capture \
  --thaw-hook /etc/euler/backup/thaw
```

三者都收到 `external-dir` 作为第一个参数。目录必须是新路径。freeze 停止直连写入，capture 导出外部制品并写 `manifest.json`，thaw 恢复写入口；即使 freeze/capture 中途失败也会尝试 thaw。hook 不是由业务用户上传的脚本，不从网络清单执行代码。捕获目录及其文件必须可由容器 UID 10001 读取，可由有权限的 capture hook 对这个专用新目录设置属主；不要放宽成全局可读。hook 输出不进入一般日志，故障细节应在其受保护运维日志中保留。

可参考 `deploy/app-cloud/systemd/euler-backup.service` 和 `.timer`，按主机路径、维护窗口和外部 hook 修改后安装。样例不自动启用；生产必须告警非零退出、核验最新成功时间，并在备份后复制到异机加密存储。不要将主机数据卷和备份卷放在同一磁盘后就认为已经有灾备。

## 隔离恢复演练

```sh
python3 tools/app-cloud/recovery_drill.py \
  /secure/euler/20260927-before-upgrade.zip \
  /srv/euler/drill-20260927 \
  --output /secure/euler/drill-20260927.json
```

工具验证包、恢复新目录，并实际打开每个 SQLite，列出全部数据表的行数。不会启动恢复出来的服务，避免旧 outbox 向真实收件人再次投递。报告始终保留外部数据面实际恢复待验收状态。

继续执行完整演练时：

1. 在专用隔离 SQL/S3 恢复数据和配置，确认没有指向原生产数据库或原八个项目。
2. 复制恢复目录的 `data/` 到一个**新**数据卷，保持 10001 属主；配置对应版本镜像和同一主密钥。保留旧卷，不使用 `down --volumes`。
3. 用 Compose override 将后端数据卷切到新卷；预发布域名与专用 OIDC 客户端必须匹配。恢复环境限制外发，SMTP/机器人使用受控测试收件人，设备修复不接真实生产 Agent。
4. 运行只读 HTTP 检查，然后用真实身份核验项目、Trust 申请/材料、EID 资格、配额、文件内容/版本、抽奖历史及各项目设备记录；验证不同项目互相不可见。
5. 验证撤销凭据仍被拒绝，核对备份之后的撤权变更。历史恢复点会恢复旧会话、密钥与授权，切换前需要重新应用后续撤权。
6. 保存恢复开始/完成时间、备份时间、实际恢复版本、样本记录和文件校验结果，形成 RTO/RPO 的测量值；没有实际测量不填写虚构目标达成。

需要系统级回滚时，把 Compose 挂载切回未修改的原卷及原镜像；不要把新版本已迁移数据库直接交给旧二进制。外部 SQL/S3 也必须恢复对应时间点，存在新业务写入时先冻结并保留现场数据，评估需要补偿的差异。

## 预发布与正式发布

```sh
python3 tools/app-cloud/deployment_check.py config \
  --env-file deploy/app-cloud/.env --output /secure/euler/config-check.json
python3 tools/app-cloud/deployment_check.py acceptance \
  --base-url https://<预发布域名> --output /secure/euler/http-check.json
```

配置检查不执行 Shell 展开，不打印配置值。它检查持久主密钥、HTTPS 来源、回调、身份客户端、精确运营身份、代理信任范围、基础服务配置及秘密文件权限。HTTP 验收只发无凭据 GET，检查健康、就绪、匿名会话及受保护项目入口；不会登录、创建资源或发送通知。`--strict` 在仍有待验收项时返回 2，配置或接口失败返回 1。

真实通行证新版客户端、域名/证书、SMTP 最终入箱、AI 调查效果和外部恢复均需实际部署输入，工具明确标记 pending。只有配置正确或健康探针成功不能转成“生产已完成”。正式发布与回退步骤见 [发布说明](../../deploy/app-cloud/RELEASE.md)。

## 自动化测试

```sh
python3 -m unittest discover -s tools/app-cloud -p 'test_*.py' -v
```

测试覆盖三个独立 SQLite 的真实内容恢复、已提交 WAL、外键损坏、文件损坏/缺失/多出、错误主密钥、重新计算哈希后的清单伪造、路径穿越、符号链接、活跃写事务、其他进程打开文件、捕获期间目录变化、外部制品缺失/篡改、私有 Trust 材料桶及孤儿发现、签名清单遗漏材料桶拒绝、旧版内联材料兼容、恢复演练读取，以及调度失败仍恢复原服务状态。外部制品测试使用明确的本地制品夹具，不冒充真实 SQL/S3 恢复。
