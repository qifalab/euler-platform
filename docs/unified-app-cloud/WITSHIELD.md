# Euler 原生 WitShield

本模块在 Euler 内运行完整的 WitShield 控制器、Agent 和受限 Helper 引擎。原项目 `witkitlab/witshield` 固定参照 `17dd1f5f1a743fa8428668f15f2398df11d83e9d`，原仓库保持只读。派生源码位于 `services/witshield-engine`，Apache-2.0 的 LICENSE 与 NOTICE 随源码保留；Vue 是 Euler 内的独立业务页面。

## 功能对应

原控制器共 49 条管理/会话 API，其中 5 条旧身份 API（status、bootstrap、login、logout、me）由 Euler 的 OIDC、Cookie 会话、CSRF 与项目权限取代。其余 **44 条业务 API**、**8 条设备 API** 保留原业务逻辑与严格输入校验。平台 `/instance` 额外提供当前安装信息与设备接入 URL。

| 原功能 | Euler 页面 | 保留的业务能力 |
|---|---|---|
| 安全概览 | 安全概览 | 项目设备、严重风险、待审批与未知状态、真实引擎/数据库/worker 健康、传感器覆盖 |
| 风险发现 | 风险发现 | 按设备/严重度检索、证据、发现状态、修复建议；不将未完成检查当作安全 |
| 安全报告 | 安全报告 | 历史扫描与单份完整报告、分数、覆盖率、扫描错误及发现详情 |
| 设备 | 设备与计划 | 一次性 enrollment token 的创建/列表/撤销、设备详情/撤销/扫描、扫描计划 CRUD、只读 observer 模式 |
| AI 安全工程师 | AI 安全工程师 / 设备与计划 | 实时事件、调查、证据链、响应计划 prepare、五类精确授权、SSH 暴力破解策略及模拟、紧急停止、AI 咨询 |
| 审批与审计 | 操作审计 | action 草稿→nonce 审批→设备签收→执行回执；回滚、SSH 二次确认、结果未知、原始安全观察分页 |
| 设置 | 设置 | OpenAI Responses/Chat、Anthropic Messages、密钥与自定义请求头、连接测试、调查预算/隐私、带签名 Webhook/SMTP、通知测试 |

五类 playbook 为安全软件包升级、SSH 密码登录加固、临时 IP 封禁、文件权限修复和临时进程暂停。Agent 的离线队列、SQLite 控制器存储、设备 Ed25519 身份、nonce 防重放、每设备并发/请求预算、传感器证据验证、SSH 自动回滚、进程 TTL 恢复、APT 事务校验、Helper 本地 socket/peer credential/令牌校验均来自完整引擎，未使用模拟执行替代。

## 人员、项目与机器边界

管理路由的外部前缀是 `/api/v1/tenants/{tenant}/projects/{project}/apps/witshield`。平台先验证人员会话、CSRF、成员关系、应用启用，再注入进程内 `appkit.Scope`。本模块只接受该 typed context，任何请求头、请求体 actor/project/admin 字段都不能创建身份。

| 权限 | 操作 |
|---|---|
| read | 所有 GET 业务读取，包含已掩码的配置、计划、审批与审计 |
| write | 触发扫描、计划 CRUD、AI 咨询、调查事件、更新事件状态 |
| manage | 接入令牌、撤销设备、AI/通知/调查策略配置与测试、设备策略/紧急停止、动作准备/审批/回滚/SSH 确认 |

团队管理员不会被转换成旧的全局管理员。`manage` 只适用于已验证的当前项目；每次处理还会核对 installation ID、tenant、project、application 和 enabled 状态。引擎的审批 actor 使用 Euler ActorID，旧 admins/sessions 表保持空且不提供任何旧身份入口。每个资源查找发生在当前安装自己的数据库，跨项目猜测设备、action、report、token 或计划 ID 无法命中其他项目。

公共设备入口是：

```text
/public/witshield/{tenant}/{project}/{installation}/agent/v1/...
```

`GET /instance` 中 `publicAgentURL` 是 `https://控制台域名/public/witshield/{tenant}/{project}/{installation}`。Agent 自行追加 `/agent/v1/...`，**不要再给该 URL 追加管理前缀**。上层 StripPrefix 保留原始 RequestURI；设备证明始终覆盖完整公共路径及 query，不能只签名引擎内部的 `/agent/v1/...`。原始 URI 由同进程参数传入私有 context，不读取可伪造的 original-URI/header。浏览器人员 Cookie 或 Bearer session 不能成为机器凭据。

8 条设备路径是 enrollment challenge、enroll、heartbeat、sync、commands/{id}/start、commands/{id}/result、reports、events。enrollment 必须拥有一次性令牌并证明 Ed25519 私钥；已注册设备必须同时持有设备 token 与签名密钥，并通过时间窗口及持久 nonce 防重放检查。

## 审批与停用

动作创建与响应计划 prepare 仅生成草稿、可审阅的 preview 和 10 分钟一次性 nonce。审批在引擎 SQLite 事务内同时验证状态/nonce/时效并入队，不允许绕过 prepare 直接执行。AI 模型不能调用任意命令，模型提出的计划仍需人工审批；独立明确授权的低风险确定性响应保留原规则边界。

SSH 加固的批准不代表确认成功。必须建立新的 SSH 会话，提交确认，再等设备确认回执；本地 Helper 的持久化回滚计时器独立运行。临时进程暂停同样由 Helper 独立恢复。网络丢失后的 `indeterminate` 被保留，不能把未知执行结果显示为成功或自动重试。

停用应用后，新的管理/设备请求、Agent 长轮询继续领命令、授权开始动作、定时扫描和后台调查/通知会被拒绝或暂停。正在进行的网络工作每 250 ms 检查安装状态并取消 context；已交付并开始执行的主机动作无法被远端同步撤回，Helper 的安全回滚/恢复继续有效。状态到期、审计留存与压缩维护仍运行，不派发新的设备命令。重新启用后 worker 恢复，旧动作仍受原命令 TTL 与状态机约束。

AI、Webhook 和 SMTP 凭据不能跟随目的地被静默转发。Webhook 完整 URL 改变，或 SMTP host/port/username 改变时，必须显式重新输入或清除相应秘密；SMTP 端口也决定隐式 TLS/STARTTLS 传输边界。修改开关、收件人等不改变凭据目的地的配置可保留原秘密。停用导致的 context 取消会关闭阻塞的 SMTP 连接。

平台对变更先写固定的操作意图审计，再调用引擎，最后记录 HTTP 结果。引擎内的关键审批/命令与业务审计仍在同一事务内完成。平台和每安装 SQLite 不共享事务；若完成审计失败，接口返回结果待核查的错误，禁止假装操作未发生后盲目重试。审计摘要不包含请求体、密钥、nonce 或凭据。

## 数据与生命周期

每安装数据库位于 `EULER_APP_DATA_DIR/witshield/<sha256(instance-purpose)>/witshield.db`，目录 0700、数据库 0600。purpose 为 `witshield/v1/{tenant}/{project}/{installation}`；AI/API/通知秘密的加密 key 从 Euler 根 key 按这个 purpose 单独派生，不共用其他项目的 vault key。

启动时 `Migrate` 恢复所有 enabled installation 的引擎和后台 worker。新安装首次业务请求时创建自己的引擎。主进程退出会调用 `Close`，取消并等待 worker，最后关闭数据库。备份必须同时保存平台数据库、整个持久数据目录和 Euler 根加密 key；只复制 engine DB 或更换根 key 都无法恢复已保存秘密。在线复制 SQLite 必须使用一致性备份或停服后连同 WAL/SHM 正确处理，不能把随机文件拷贝当作完整备份。

## 获取与安装 Euler 设备程序

Euler app-cloud 镜像在 `/opt/euler/device-tools/` 提供同版本 `witshield-agent`、`witshield-helper`。从自己构建并固定摘要的 Euler 镜像取出，选择与设备相同的架构；不使用原站 installer 或原 GitHub release。

```sh
mkdir -p ./euler-device-tools
docker create --name euler-device-tools-export YOUR_EULER_IMAGE_AT_DIGEST
docker cp euler-device-tools-export:/opt/euler/device-tools/. ./euler-device-tools/
docker rm euler-device-tools-export
sha256sum ./euler-device-tools/witshield-agent ./euler-device-tools/witshield-helper
```

也可在 `services/witshield-engine` 目录用 Go 1.27.1 构建：

```sh
CGO_ENABLED=0 go build -trimpath -o ./dist/witshield-agent ./cmd/witshield-agent
CGO_ENABLED=0 go build -trimpath -o ./dist/witshield-helper ./cmd/witshield-helper
```

原生受限 playbook 以 Debian/Ubuntu、systemd、OpenSSH、APT、nftables 为支持面。将二进制传到待接入主机，部署者审核后，以 root 安装；Helper 使用自己的本地最小权限协议，Agent 以专用非 root 用户运行。参考 systemd 单元随派生引擎存放于 `services/witshield-engine/packaging/systemd/`，已移除独立旧 Controller 依赖并更新 Euler 文档地址。

首次部署的账号、目录、单元应由主机配置管理创建，参数对应如下：

- `witshield-agent` 用户的主组为 `witshield-agent`，补充组为 `witshield-helper`；按需要加入 `adm`、`systemd-journal`，否则 UI 应显示相应证据缺失。
- Agent 可执行文件 `/usr/local/bin/witshield-agent` 为 root:root 0755；Helper `/usr/libexec/witshield/witshield-helper` 为 root:root 0755。
- `/var/lib/witshield-agent` 为 witshield-agent:witshield-agent 0700；`/var/lib/witshield-helper` 为 root:root 0700。
- `/etc/witshield` 为 root:witshield-helper 0750。Helper 创建 token、root 私有状态 key、受限 socket 及持久回滚/恢复 journal；不要把 Helper 暴露成网络服务。
- `/etc/witshield/agent.env` 设置下列配置，权限 root:root 0600；systemd 读取后把非秘密配置传入专用进程。

```dotenv
WITSHIELD_CONTROLLER_URL=https://console.example/public/witshield/TENANT/PROJECT/INSTALLATION
WITSHIELD_DEVICE_NAME=production-node-1
WITSHIELD_DATA_DIR=/var/lib/witshield-agent
WITSHIELD_SCAN_INTERVAL=24h
WITSHIELD_ENROLLMENT_TOKEN_FILE=/var/lib/witshield-agent/enrollment.token
```

在 Euler “设备与计划”创建新的单次令牌，写入上述 enrollment.token 文件（witshield-agent:witshield-agent 0600），不要写进命令行、历史、镜像或 Git。单元中的 `--consume-enrollment-token` 在成功注册后删除该文件。将对应 systemd 单元安装到 `/etc/systemd/system/` 后，执行 `systemctl daemon-reload` 和 `systemctl enable --now witshield-helper witshield-agent`。后续重启使用 Agent 持久身份，不重复生成设备。服务名及本地路径沿用受限 Helper 协议，但控制器目标只能是该 Euler 安装的完整 HTTPS URL。

只需要观测时，可运行 Agent 的 `--observer-only` 模式而不部署 Helper；该能力被持久记录在 enrollment 设备身份中，服务端拒绝对此设备启用自动处置或创建修改动作。容器观测必须把主机数据以只读方式挂载并显式配置 `--host-root`，容器中的在线状态不等于拥有主机完整覆盖。

## 验证

新增 `services/app-cloud/internal/apps/witshield/module_test.go` 验证 typed scope、伪造身份头、viewer/write/manage 边界、token/安装/应用隔离、公共入口、无密钥审计与停用。`services/witshield-engine/integration/engine_test.go` 使用真实 Agent 客户端、Ed25519 注册和持久 SQLite，验证带实例前缀签名、错误前缀、nonce 重放、设备跨项目查询、动作 nonce 二阶段审批、停用与重启恢复。新增 scheduler/HTTP 测试验证停用后不入队和取消外部工作。原引擎全套测试随源码保留。`platform/frontend/e2e/app-cloud/witshield.spec.ts` 使用真实 OIDC、项目与公开 Ed25519 enrollment，覆盖工作台、注册码生成/撤销、扫描计划 CRUD、配置持久化及秘密不回显、viewer 页面与服务端拒绝，并检查手机布局。浏览器测试不拦截本平台 API，也不向真实通知收件人发消息。

```sh
cd services/app-cloud
go test -race -count=1 ./internal/apps/witshield
cd ../witshield-engine
go test -race -count=1 ./...
```

2026-09-26 开发环境中新增集成与权限测试、原 httpapi/store/AI/identity/action/scanner/notification 等测试已通过。原套件中 6 个 Unix socket 测试被执行环境拒绝 `socket: operation not permitted`，没有删除或跳过；完整 Linux CI 包含这些测试并构建设备二进制。真实浏览器工作流首次运行通过（1 passed，19.8 秒）；其余最终合并验收以完整 Linux CI 为准。
