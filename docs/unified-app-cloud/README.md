> 当前实现进入欧拉独立原生应用阶段。以 [NATIVE-APPS.md](NATIVE-APPS.md) 和各产品功能矩阵为准；以下连接器基线说明保留作为前阶段记录，生产入口已切换到 `/apps/{applicationID}` 原生业务 API。

# 欧拉应用云 · Euler Application Cloud

围绕现有开源产品的统一应用控制台。一个账号可以加入多个团队，在项目中连接身份认证、应用安全、数据库、对象存储和运营工具，并统一管理协作者与操作记录。

本仓库正在由 IaaS Alpha 转向应用云，完整范围与验收条件见 [实施计划](PLAN.md)。新的应用云是默认开发主线，历史计算、网络与计费实现保留供后续演进；它们不代表已经上线的可售公有云服务。

## 产品与职责

| 能力 | 产品 | 接入范围 |
| --- | --- | --- |
| 登录身份 | E时代通行证或可配置身份源 | 独立 OAuth/OIDC 客户端及 Euler 会话 |
| 成员资格 | [EID](https://github.com/miaojilab/emoera-eid) | 当前用户资格摘要、原站办理；旧鉴权保持 |
| 认证结果 | [Trust](https://github.com/miaojilab/trust-center) | 按认证方案查询状态，不汇集 KYC 材料 |
| 站点验证 | [WeAuth](https://github.com/ctipscn/weauth) | 真实站点列表、创建和删除，连接外部用户账号 |
| 数据库 | [ECloud Database](https://github.com/ctipscn/ecloud-database) | 已有数据库摘要与列表，不返回数据库密码 |
| 对象存储 | [ECloud Storage](https://github.com/ctipscn/ecloud-storage) | 存储用量及桶列表，保留原生数据面 |
| 运营工具 | [Statistics](https://github.com/ctipscn/ecloud-statistics)、[Lottery](https://github.com/miaojilab/emoera-lottery-system) | 连接项目独立实例及原生控制台 |
| 服务器安全 | [WitShield](https://github.com/witkitlab/witshield) | 连接独立 Controller，保留原有登录和审批 |

“连接应用”不会自动部署外部产品，也不会为缺少相应接口的产品提供虚假的单点登录。平台会明确展示未配置、不可用和不支持的能力。各产品继续拥有独立仓库、数据库和部署方式。

## 平台行为

- 团队与项目：同一用户可加入不同团队；项目资源和应用连接有明确归属。
- 权限：团队 owner/admin 管理全部项目；其他成员按项目角色访问。移除成员后，已有会话的权限立即重新检查。
- 身份：通行证账号、EID 成员资格、Trust 认证状态和 Euler 管理权限分开处理，不根据邮箱自动合并账号。
- 会话：服务端会话、HttpOnly Cookie、CSRF、短期一次性登录事务，生产不提供假登录回退。
- 连接：凭据加密存储，仅调用部署者允许的外部来源；API 响应不包含原始凭据、KYC 表单或数据库密码。
- 持久化：SQLite 事务与迁移提供单实例部署基线，不把 SQLite 配置成多个写入副本。
- 审计：团队、项目、权限和应用变更保留操作记录。

## 开发与部署

新的控制服务位于 `services/app-cloud`，默认控制台位于 `platform/frontend/apps/console-base`。

生产与本机配置、同源反向代理、卷持久化及维护窗口备份，请按 `deploy/app-cloud/README.md` 操作。真实登录需要创建独立身份源客户端并配置回调；产品连接需要对应产品部署和由用户授权的凭据。

```bash
# 前端工作区
cd platform/frontend
pnpm install --frozen-lockfile
pnpm --filter 'console-base^...' build
pnpm --filter console-base dev

# 后端（另一个终端，先按部署说明配置环境变量）
cd services/app-cloud
go run ./cmd/server
```

API 与字段契约见 [API.md](API.md)。浏览器测试使用独立测试身份源和产品协议测试服务，执行真实的登录交换、签名校验、服务端授权、SQLite 持久化与 HTTP 请求；这不构成生产上游的上线验收。

## 验证

```bash
cd services/app-cloud
go test -race ./...
go vet ./...

cd ../../platform/frontend
pnpm --filter 'console-base^...' build
pnpm --filter console-base typecheck
pnpm --filter console-base build
pnpm exec playwright test --config playwright.app-cloud.config.ts
```

测试环境需 Go、Node.js、pnpm 和 Playwright Chromium。配置 `PLAYWRIGHT_EXECUTABLE_PATH` 可使用本机已安装的兼容 Chromium，避免依赖个人机器绝对路径。

## 文档与历史

- [总体计划、里程碑与完成标准](PLAN.md)
- [接口与模块契约](API.md)
- [验收结果与部署边界](VALIDATION.md)
- `deploy/app-cloud/README.md`：运行、配置、备份、恢复与故障诊断
- `docs/archive/iaas-alpha-readme.md`：历史 IaaS 产品设想，不能作为当前能力清单
- `docs/architecture/`：历史架构资料，冲突处以应用云计划与实现为准

本次集成不改变各接入产品的许可证或其独立部署边界。
