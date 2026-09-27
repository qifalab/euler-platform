// Package apps composes the independent Euler application suite. No original
// product management service or original product database is used at runtime.
package apps

import (
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/database"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/eid"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/lottery"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/statistics"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/storage"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/trust"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/weauth"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/witshield"
)

func New(rt *appkit.Runtime) []appkit.Module {
	modules, err := DefaultRegistry().Build(rt)
	if err != nil {
		// Built-in registrations are compile-time constants. A malformed extension
		// fails startup before any migration or worker can change persisted data.
		panic(err)
	}
	return modules
}

func DefaultRegistry() *Registry {
	r := NewRegistry()
	entries := []struct {
		id, name, category, description, color    string
		capabilities, dependencies, configuration []string
		factory                                   Factory
	}{
		{"trust", "Trust 信任中心", "身份与信任", "配置认证方案、保护材料、审核资格并触发业务流程。", "#0d9488", []string{"schemes", "materials", "review", "qualification", "events"}, nil, []string{"notifications_optional"}, func(rt *appkit.Runtime) appkit.Module { return trust.New(rt) }},
		{"eid", "EID 成员资格", "身份与信任", "管理成员身份、招募与录取，按 Trust 资格办理业务。", "#4f46e5", []string{"membership", "recruitment", "offers", "qualification-gate", "events"}, []string{"trust"}, []string{"smtp_optional"}, func(rt *appkit.Runtime) appkit.Module { return eid.New(rt) }},
		{"weauth", "WeAuth 人机验证", "开发服务", "站点验证、域名策略、工作量证明及访问风险管理。", "#7c3aed", []string{"sites", "proof-of-work", "risk", "widget"}, nil, nil, func(rt *appkit.Runtime) appkit.Module { return weauth.New(rt) }},
		{"database", "ECloud Database", "开发服务", "项目数据库、连接凭据、容量配额与资源权益。", "#2563eb", []string{"mysql", "postgresql", "credentials", "quota", "packages"}, nil, []string{"mysql_or_postgresql"}, func(rt *appkit.Runtime) appkit.Module { return database.New(rt) }},
		{"storage", "ECloud Storage", "开发服务", "项目文件、访问策略、上传会话与资源配额。", "#0891b2", []string{"objects", "signed-uploads", "multipart", "versions", "lifecycle", "quota", "packages"}, nil, []string{"s3"}, func(rt *appkit.Runtime) appkit.Module { return storage.New(rt) }},
		{"statistics", "ECloud Statistics", "应用工具", "访问趋势、来源、事件与转化分析。", "#d97706", []string{"pageviews", "visitors", "trends", "events", "funnels", "export"}, nil, nil, func(rt *appkit.Runtime) appkit.Module { return statistics.New(rt) }},
		{"lottery", "Lottery 活动抽奖", "应用工具", "活动报名、资格与防刷、现场抽奖及结果联动。", "#e11d48", []string{"rooms", "signup", "qualification-gate", "draws", "events"}, nil, []string{"qualification_optional", "verification_optional"}, func(rt *appkit.Runtime) appkit.Module { return lottery.New(rt) }},
		{"witshield", "WitShield 服务器安全", "安全运维", "设备接入、风险调查、修复审批和回滚。", "#059669", []string{"devices", "scans", "investigations", "approval", "rollback"}, nil, []string{"devices", "ai_optional", "notifications_optional"}, func(rt *appkit.Runtime) appkit.Module { return witshield.New(rt) }},
	}
	for _, entry := range entries {
		err := r.Register(appkit.Manifest{ID: entry.id, Name: entry.name, Version: "0.2.0", APIVersion: "v1", SchemaVersion: 2, Category: entry.category, Description: entry.description, Color: entry.color, Repository: "https://github.com/qifalab/euler-platform", Capabilities: entry.capabilities, Dependencies: entry.dependencies, Configuration: entry.configuration}, entry.factory)
		if err != nil {
			panic(err)
		}
	}
	return r
}
