package database

import (
	"net/url"
	"testing"
)

// Adminer reads host:port from one parameter; an IPv6 literal must be
// bracketed or the port is indistinguishable from the address.
func TestAdminerLinkBracketsIPv6Host(t *testing.T) {
	m, s, _, _ := databaseFixture(t)
	t.Setenv("EULER_DATABASE_ADMINER_URL", "https://adminer.example/")
	for _, kind := range []string{"mysql", "postgresql"} {
		cfg := m.engines[kind]
		cfg.Host = "2001:db8::10"
		m.engines[kind] = cfg
	}
	for kind, param := range map[string]string{"mysql": "server", "postgresql": "pgsql"} {
		v := createDatabase(t, m, s, kind)
		w := databaseCall(m.Handler(), &s, "GET", "/databases/"+v.ID+"/credentials", nil)
		databaseStatus(t, w, 200)
		out := decodeResponse[struct {
			Port  int `json:"port"`
			Tools []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"tools"`
		}](t, w)
		var link string
		for _, tool := range out.Tools {
			if tool.Name == "Adminer" {
				link = tool.URL
			}
		}
		u, e := url.Parse(link)
		if e != nil || link == "" {
			t.Fatalf("%s: missing Adminer link %q: %v", kind, link, e)
		}
		want := "[2001:db8::10]:" + map[string]string{"mysql": "3306", "postgresql": "5432"}[kind]
		if got := u.Query().Get(param); got != want {
			t.Fatalf("%s: Adminer %s=%q, want %q", kind, param, got, want)
		}
	}
}
