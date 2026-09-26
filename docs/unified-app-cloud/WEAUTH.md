# Euler 原生 WeAuth

WeAuth 的业务在 Euler 内独立运行并持有数据，不连接原 WeAuth 账户、不导入原站令牌、不修改或部署原仓库。人员通过 Euler / 新版 E时代通行证登录；项目身份由宿主注入 `appkit.Scope`。原个人资料与备用密码入口由平台身份管理取代。

## 功能对应

| 原 WeAuth 功能 | Euler 实现 |
| --- | --- |
| 站点列表、新建、详情、修改、删除 | 原生站点侧栏与表单，`/sites` 与 `/sites/{id}` |
| 动态难度与批量模式 | 基础/最大难度、批量单次难度、最少/最多批次；真实 SHA-256 前导零规则 |
| 挑战与令牌有效期、站点开关 | 配置表单与服务端范围验证；停用后公开业务请求拒绝 |
| 域名白名单 | 列表、添加、移除；支持 `*.example.com`，不隐式包含主域名 |
| 服务端密钥查看与轮换 | 独立 `secrets` 权限、明确查看/确认、加密持久化、审计；列表不返回密钥 |
| IP 风控策略 | 时间窗口、阈值、失败权重、动态难度/批次数增量及开关 |
| IP 记录与清除 | 项目管理权限查看 IP、请求/失败计数、最近访问与单 IP 清除 |
| 总量和最近 24 小时统计 | 挑战、成功证明、验证尝试、成功率；批量以一次完整验证计算 |
| 集成文档、实时组件与测试 | 自动嵌入、手动 API、服务端示例、真实 Worker 预览、一次性令牌消费测试；浅色/深色/系统主题、六种主题色与自定义 HEX 色、紧凑/标准/自适应尺寸、自动开始 |
| 原登录、个人资料、备用密码 | Euler 与新版通行证统一管理，不新增应用密码 |

管理接口相对当前团队、项目、应用路径。`read` 可查看站点/配置/统计；`write` 可新建、修改配置和域名；`manage` 可删除站点、管理风控和查看/清除 IP；`secrets` 可查看/轮换密钥、执行控制台令牌验证。每个资源 ID 都重新验证 tenant/project/installation，不能通过请求体或 HTTP 头改变归属。业务写入与脱敏审计同事务保存；并发配置更新使用乐观冲突检测。

## 公共验证协议

基地址为部署公开 URL 下 `/public/weauth`：

- `GET /weauth.js`，通过 `weauth.render/reset/execute/remove/getResponse` 控制组件，也支持 `.weauth[data-sitekey]` 自动挂载。
- `GET /widget.html` 是可嵌入验证组件；只加载自身静态资源，不接收人员凭据。消息同时检查 origin 与 iframe source。
- `POST /pow/challenge`：`{sitekey,origin,action}`，返回 `challenge_id,difficulty,batch_mode,batch_total,expires_at`，以及 `challenge` 或 `challenges`。
- `POST /pow/verify`：`{challenge_id,nonce,nonces,batch_mode,iterations,solve_time_ms}`，返回 `token,expires_in`。`iterations/solve_time_ms` 仅为客户端信息，不参与安全判定。
- `POST /pow/siteverify`：`{secret,token,remoteip?}`，也接受 `response` 代替 token。成功返回 `success,challenge_ts,hostname,action`；业务服务端必须核对预期 hostname/action。兼容 `/api/pow/*` 路径。

公开接口不需要人员登录。每次业务请求验证站点和应用是否启用；空域名白名单拒绝挑战。公开 Sitekey 和域名校验不构成人员鉴权；Secret 只应保存在业务服务端。CORS 不授予管理权限，`siteverify` 不开放浏览器跨域调用。

动态模式使用原 WeAuth `SHA256(challenge + nonce)` 的十六进制前导零规则。风控以 `floor((请求数 + 失败数 × 失败权重) / 阈值)` 增加难度或批次。批量证明全部正确后，同一个 SQLite 事务才生成令牌；不能提交一部分后取得成功。挑战不能重复求解。服务端验证以条件更新原子消费令牌，过期、已用、错误站点或可选 IP 不匹配均拒绝。Secret 轮换后旧 Secret 立即失效。

## IP、运行与数据

默认仅使用 TCP `RemoteAddr`，忽略任意客户端伪造的 `X-Forwarded-For`。如部署在反向代理后，明确设置 `EULER_WEAUTH_TRUSTED_PROXIES`，例如本机代理 `127.0.0.1/32,::1/128`。只在直连代理属于配置 CIDR 时，从 XFF 右侧逐跳走到第一个不可信地址。不要配置 `0.0.0.0/0` 或 `::/0`。代理须覆盖/规范化转发头；未正确配置时会以代理 IP 计入风控。

模块新增 `weauth_sites`、`weauth_ip`、`weauth_challenges`、`weauth_events`。站点 Secret 使用 Runtime 的 AES-GCM 加密边界，摘要索引用于查找；验证令牌只保存哈希。备份使用 Euler 数据库与加密主密钥的既有备份流程。不得只恢复数据库而丢失加密密钥。

新挑战创建时清理该站点 24 小时前且已过期的挑战；每日聚合统计保留。IP 记录在后续挑战请求时清理 30 天前记录，控制台最多展示最新 1000 条。每个项目最多 1000 站点，每站点每分钟最多 600 次挑战；IP 还有窗口内突发限制。互联网部署仍需在入口配置全局速率、连接数与请求体限制。高难度可能在超时前无法求解，UI 明确提示难度每增 1，预期计算量增加约 16 倍。

预览会产生真实挑战、统计和 IP 记录；需要显式把控制台域名加入站点白名单。生产浏览器须支持 HTTPS/WebCrypto/Web Worker。控制台测试只接受当前项目站点的令牌，在服务端消费令牌，不将 Secret 放入 iframe。组件 CSS/颜色独立封装；不会放宽管理控制台的嵌入策略。

## 验证与来源

`go test -race ./internal/apps/weauth` 覆盖动态/批量真实 PoW、批次失败后仍可正确完成、并发仅一次消费、挑战/令牌过期、站点间令牌隔离、Secret 轮换/加密/不泄露、viewer 与跨项目拒绝、应用停用、域名规则、可信代理、IP 风险递增、管理生命周期及静态资源。

业务算法来源为 `ctipscn/weauth`，固定版本 `c670c207bade84e7cb680c8d850c5daae0d88f46`，Apache-2.0。保留完整许可证于 `services/app-cloud/internal/apps/weauth/LICENSE.weauth`。主要参考 `backend/services/pow_service.go`、`backend/models/ip_policy.go` 与 `frontend/src/components/pow-verify/pow-worker.ts`。Euler 改写了持久化、授权、密钥管理、批次原子性、过期/重放控制、配置校验、可信代理和界面；没有复制生产配置、固定管理员或演示凭据。
