package connectors

// Catalog describes implemented adapter capabilities, never provisioned resources.
func Catalog() []Application {
	return []Application{
		{ID: "eid", Name: "EID 成员资格", Category: "身份与信任", Description: "查看当前账号的成员资格记录。", Color: "#4f46e5", Homepage: "https://esd-id.emoera.com", Repository: "https://github.com/miaojilab/emoera-eid", Capabilities: []string{"summary", "console:open"}, ConnectionMode: "identity", Limitations: []string{"仅查询当前登录主体，不授予 Euler 角色。", "旧接口仅返回最新已通过记录，不代表完整有效期或撤销状态。"}},
		{ID: "trust", Name: "Trust 信任中心", Category: "身份与信任", Description: "按已配置认证方案查看当前账号的认证状态。", Color: "#0d9488", Homepage: "https://trust.emoera.com", Repository: "https://github.com/miaojilab/trust-center", Capabilities: []string{"summary", "console:open"}, ConnectionMode: "identity", Limitations: []string{"不复制认证材料，不把认证状态作为团队授权。"}},
		{ID: "weauth", Name: "WeAuth 人机验证", Category: "开发服务", Description: "连接独立外部账号，管理项目的人机验证站点。", Color: "#7c3aed", Homepage: "https://github.com/ctipscn/weauth", Repository: "https://github.com/ctipscn/weauth", Capabilities: []string{"summary", "resources:list", "resources:create", "resources:delete", "console:open"}, ConnectionMode: "bearer", Limitations: []string{"凭据是上游用户会话，并非受限服务令牌；到期后需更新。", "一个外部账号仅绑定一个项目。站点密钥在原控制台管理。"}},
		{ID: "database", Name: "ECloud Database", Category: "开发服务", Description: "查看外部账号已有数据库的状态与用量。", Color: "#2563eb", Homepage: "https://github.com/ctipscn/ecloud-database", Repository: "https://github.com/ctipscn/ecloud-database", Capabilities: []string{"summary", "resources:list", "console:open"}, ConnectionMode: "bearer", Limitations: []string{"只读接入；用户会话到期后需更新。", "数据库连接密码不会进入 Euler 摘要。一个外部账号仅绑定一个项目。"}},
		{ID: "storage", Name: "ECloud Storage", Category: "开发服务", Description: "读取已有存储桶及账号配额。", Color: "#0891b2", Homepage: "https://github.com/ctipscn/ecloud-storage", Repository: "https://github.com/ctipscn/ecloud-storage", Capabilities: []string{"summary", "resources:list", "console:open"}, ConnectionMode: "access_key_pair", Limitations: []string{"上游密钥为账号级；Euler 仅调用只读接口，不代理文件。", "配额接口返回上游账号基础配额；不推算套餐或计费。"}},
		{ID: "statistics", Name: "ECloud Statistics", Category: "应用工具", Description: "连接项目独立统计面板。", Color: "#d97706", Homepage: "https://github.com/ctipscn/ecloud-statistics", Repository: "https://github.com/ctipscn/ecloud-statistics", Capabilities: []string{"console:open"}, ConnectionMode: "external", Limitations: []string{"仅提供入口，不共享全局管理员令牌或统计数据。需在原应用登录。"}},
		{ID: "lottery", Name: "Lottery 活动抽奖", Category: "应用工具", Description: "连接项目独立部署的活动系统。", Color: "#e11d48", Homepage: "https://github.com/miaojilab/emoera-lottery-system", Repository: "https://github.com/miaojilab/emoera-lottery-system", Capabilities: []string{"console:open"}, ConnectionMode: "external", Limitations: []string{"仅支持独立实例入口，不提供共享多租户管理或自动登录。"}},
		{ID: "witshield", Name: "WitShield 服务器安全", Category: "安全运维", Description: "检查独立 Controller 健康状态并进入原生控制台。", Color: "#059669", Homepage: "https://witshield.witkitlab.com", Repository: "https://github.com/witkitlab/witshield", Capabilities: []string{"summary", "console:open"}, ConnectionMode: "external", Limitations: []string{"上游为单管理员、多设备，不共享管理员会话。", "修复审批、回滚和审计仍由原控制台执行；Docker 仅支持只读观察。"}},
	}
}

func (m *Manager) Catalog() []Application { return Catalog() }
func known(id string) bool {
	for _, app := range Catalog() {
		if app.ID == id {
			return true
		}
	}
	return false
}
