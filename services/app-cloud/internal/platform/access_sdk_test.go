package platform

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/weauth"
)

// Exercise the shipped Python SDK over TCP against the real Go handler and
// WeAuth SQLite module. Tokens go through stdin and are never in command args.
func TestPythonSDKAgainstNativeMachineAPI(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required for the SDK integration test")
	}
	s := testStore(t)
	owner := testUser(t, s, "sdk-owner")
	tenant := mustTenant(t, s, owner.ID)
	project := mustProject(t, s, owner.ID, tenant.ID)
	other := mustProject(t, s, owner.ID, tenant.ID)
	if _, err = s.EnableApplication(testContext, owner.ID, tenant.ID, project.ID, "weauth"); err != nil {
		t.Fatal(err)
	}
	mod := weauth.New(s.ApplicationRuntime(t.TempDir(), "http://127.0.0.1"))
	if err = mod.Migrate(testContext); err != nil {
		t.Fatal(err)
	}
	_, readToken, err := s.CreateServiceAccount(testContext, owner.ID, tenant.ID, project.ID, ServiceAccountInput{"SDK read", time.Now().Add(time.Hour), []ServiceScope{{"weauth", []string{"read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	writeAccount, writeToken, err := s.CreateServiceAccount(testContext, owner.ID, tenant.ID, project.ID, ServiceAccountInput{"SDK write", time.Now().Add(time.Hour), []ServiceScope{{"weauth", []string{"read", "write"}}}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHandler(s, nil, managerFor(t), WithApplications([]appkit.Module{mod}, nil)))
	defer server.Close()
	_, source, _, _ := runtime.Caller(0)
	sdkRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../sdk/app-cloud-python/src"))
	run := func(phase string, additional map[string]string) {
		t.Helper()
		config := map[string]string{"origin": server.URL, "tenant": tenant.ID, "project": project.ID, "other": other.ID, "read": readToken, "write": writeToken, "phase": phase}
		for k, v := range additional {
			config[k] = v
		}
		raw, _ := json.Marshal(config)
		ctx, cancel := context.WithTimeout(testContext, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, python, "-c", pythonSDKScenario)
		cmd.Env = append(os.Environ(), "PYTHONPATH="+sdkRoot, "PYTHONDONTWRITEBYTECODE=1")
		cmd.Stdin = strings.NewReader(string(raw))
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Python SDK integration %s failed: %v\n%s", phase, err, output)
		}
	}
	run("create", nil)
	next, err := s.RotateServiceAccount(testContext, owner.ID, tenant.ID, project.ID, writeAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	run("rotate", map[string]string{"next": next})
	if err = s.RevokeServiceAccount(testContext, owner.ID, tenant.ID, project.ID, writeAccount.ID); err != nil {
		t.Fatal(err)
	}
	run("revoke", map[string]string{"next": next})
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM weauth_sites WHERE tenant_id=? AND project_id=?`, tenant.ID, project.ID).Scan(&n); err != nil || n != 1 {
		t.Fatal("SDK did not persist exactly one native site", n, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM audit WHERE project_id=? AND actor_id=? AND action='weauth.site.created'`, project.ID, writeAccount.ID).Scan(&n); err != nil || n != 1 {
		t.Fatal("SDK write lacks service-account attribution", n, err)
	}
}

const pythonSDKScenario = `
import json, sys, urllib.request, urllib.error
from euler_cloud import Client, APIError
c = json.load(sys.stdin)
def client(key, project=None):
    return Client(c['origin'], c['tenant'], project or c['project'], c[key], allow_insecure_loopback=True)
def denied(fn, status):
    try:
        fn()
    except APIError as e:
        assert e.status == status, (e.status, e.code)
    else:
        raise AssertionError('Expected denied request')
if c['phase'] == 'create':
    reader, writer = client('read'), client('write')
    assert reader.request('weauth', path='/sites').data == {'items': []}
    denied(lambda: reader.request('weauth','POST','/sites',data={'name':'Denied'}),403)
    site=writer.request('weauth','POST','/sites',data={'name':'SDK integration','domains':['example.test']})
    assert site.status == 201
    assert reader.request('weauth',path='/sites').data['items'][0]['id'] == site.data['id']
    denied(lambda: writer.request('weauth',path='/sites/'+site.data['id']+'/secret'),403)
    denied(lambda: client('read',c['other']).request('weauth',path='/sites'),404)
    denied(lambda: writer.request('trust','POST','/review',data={}),404)
    request=urllib.request.Request(c['origin']+'/api/v1/machine/tenants/'+c['tenant']+'/projects/'+c['project']+'/apps/weauth/sites',headers={'Authorization':'Bearer '+c['read'],'Cookie':'euler_session=not-a-session'})
    try:
        urllib.request.urlopen(request)
    except urllib.error.HTTPError as e:
        assert e.code==403
        e.close()
    else:
        raise AssertionError('Machine endpoint accepted browser Cookie')
elif c['phase'] == 'rotate':
    denied(lambda: client('write').request('weauth',path='/sites'),401)
    assert len(client('next').request('weauth',path='/sites').data['items']) == 1
elif c['phase'] == 'revoke':
    denied(lambda: client('next').request('weauth',path='/sites'),401)
`
