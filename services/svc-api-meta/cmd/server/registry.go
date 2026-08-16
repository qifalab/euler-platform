// Package main: console sub-app registry (02-frontend-architecture.md §4.1).
//
// The registry is a backend-served, frontend-consumed dynamic list of which
// sub-apps the console shell should load. The shell fetches it at boot
// (02§4.2) and caches in sessionStorage (5 min), falling back to a built-in
// list on failure.
//
// Production source: Nacos config + MySQL mf_app_registry rows, managed by an
// internal ops console. Phase-1 keeps an in-memory store seeded with the
// canonical app set so the frontend pulls a real endpoint (no hardcoded
// fallback in the shell).
package main

import (
	"encoding/json"
	"net/http"
	"sync"
)

// RegistryApp mirrors the JSON the console shell expects (02§4.1 API example).
type RegistryApp struct {
	AppCode      string         `json:"appCode"`
	AppTitle     string         `json:"appTitle"`
	ProductCodes []string       `json:"productCodes"`
	ActiveRules  []string       `json:"activeRules"`
	EntryURL     string         `json:"entryUrl"`
	Version      string         `json:"version"`
	KeepAlive    bool           `json:"keepAlive"`
	Preload      bool           `json:"preload"`
	Status       string         `json:"status"`
	Gray         *RegistryGray  `json:"gray,omitempty"`
}

type RegistryGray struct {
	Version  string         `json:"version"`
	EntryURL string         `json:"entryUrl"`
	Rule     map[string]any `json:"rule"`
}

// Registry is the full response: { apps, revision, ttl }.
type Registry struct {
	Apps    []RegistryApp `json:"apps"`
	Revision int          `json:"revision"`
	TTL      int          `json:"ttl"`
}

// registryStore is the in-memory registry (MySQL + Nacos in production).
type registryStore struct {
	mu       sync.RWMutex
	registry Registry
}

func newRegistryStore() *registryStore {
	return &registryStore{registry: seedRegistry()}
}

// seedRegistry returns the canonical phase-1 sub-app set. EntryURLs point at
// the dev servers; production swaps these for versioned static-domain entries
// (https://static.starcloud.cn/apps/{app}/{ver}/index.html).
func seedRegistry() Registry {
	return Registry{
		Revision: 1,
		TTL:      300,
		Apps: []RegistryApp{
			{AppCode: "console-ecs", AppTitle: "云服务器 ECS", ProductCodes: []string{"scecs"}, ActiveRules: []string{"/scecs"}, EntryURL: "http://localhost:5174/", Version: "0.1.0", KeepAlive: true, Preload: true, Status: "online"},
			{AppCode: "console-storage", AppTitle: "对象存储 OSS", ProductCodes: []string{"scoss"}, ActiveRules: []string{"/scoss"}, EntryURL: "http://localhost:5176/", Version: "0.1.0", KeepAlive: true, Preload: false, Status: "online"},
			{AppCode: "console-network", AppTitle: "专有网络 VPC", ProductCodes: []string{"scvpc", "sceip"}, ActiveRules: []string{"/scvpc", "/sceip"}, EntryURL: "http://localhost:5177/", Version: "0.1.0", KeepAlive: false, Preload: false, Status: "online"},
			{AppCode: "console-database", AppTitle: "云数据库 RDS", ProductCodes: []string{"scrds"}, ActiveRules: []string{"/scrds"}, EntryURL: "http://localhost:5178/", Version: "0.1.0", KeepAlive: false, Preload: false, Status: "online"},
			{AppCode: "console-monitor", AppTitle: "云监控", ProductCodes: []string{"scmon"}, ActiveRules: []string{"/scmon"}, EntryURL: "http://localhost:5179/", Version: "0.1.0", KeepAlive: false, Preload: false, Status: "online"},
			{AppCode: "web-billing", AppTitle: "费用中心", ProductCodes: []string{}, ActiveRules: []string{"/billing"}, EntryURL: "http://localhost:5180/", Version: "0.1.0", KeepAlive: false, Preload: true, Status: "online"},
			{AppCode: "web-account", AppTitle: "账号与访问控制", ProductCodes: []string{}, ActiveRules: []string{"/account"}, EntryURL: "http://localhost:5175/", Version: "0.1.0", KeepAlive: false, Preload: false, Status: "online"},
			{AppCode: "web-ticket", AppTitle: "工单支持", ProductCodes: []string{}, ActiveRules: []string{"/ticket"}, EntryURL: "http://localhost:5181/", Version: "0.1.0", KeepAlive: false, Preload: false, Status: "online"},
			{AppCode: "devops-explorer", AppTitle: "OpenAPI Explorer", ProductCodes: []string{}, ActiveRules: []string{"/explorer"}, EntryURL: "http://localhost:5182/", Version: "0.1.0", KeepAlive: false, Preload: false, Status: "online"},
		},
	}
}

// handleConsoleApps returns the sub-app registry. This endpoint is public to
// the console shell (fetched in parallel with /api/iam/session at boot, before
// login may be established, 02§4.2 sequence) — no account header required.
// ETag + Cache-Control: no-cache so the shell can revalidate cheaply.
func (s *registryStore) handleConsoleApps(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	reg := s.registry
	s.mu.RUnlock()

	// The shell reads revision for change detection (02§4.1).
	body := map[string]any{
		"RequestId": w.Header().Get("X-Sc-TraceId"),
		"Code":      "OK",
		"Data":      reg,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(body)
}
