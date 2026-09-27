package apps

import (
	"context"
	"database/sql"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	_ "modernc.org/sqlite"
	"net/http"
	"reflect"
	"testing"
)

type testModule struct {
	id                 string
	maintained, closed bool
}

func (m *testModule) ID() string                     { return m.id }
func (*testModule) Migrate(context.Context) error    { return nil }
func (*testModule) Handler() http.Handler            { return http.NotFoundHandler() }
func (*testModule) PublicHandler() http.Handler      { return nil }
func (m *testModule) Close() error                   { m.closed = true; return nil }
func (m *testModule) Maintain(context.Context) error { m.maintained = true; return nil }
func (*testModule) DependencyStatus(context.Context) map[string]string {
	return map[string]string{"test": "up"}
}
func descriptor(id string, deps ...string) appkit.Manifest {
	return appkit.Manifest{ID: id, Name: id, Version: "1.0.0", SchemaVersion: 1, APIVersion: "v1", Dependencies: deps}
}
func factory(id string) Factory {
	return func(*appkit.Runtime) appkit.Module { return &testModule{id: id} }
}

func TestRegistryRejectsInvalidGraphBeforeConstruction(t *testing.T) {
	for _, kind := range []string{"missing", "cycle", "duplicate", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			r := NewRegistry()
			built := 0
			add := func(m appkit.Manifest) error {
				return r.Register(m, func(*appkit.Runtime) appkit.Module { built++; return &testModule{id: m.ID} })
			}
			switch kind {
			case "missing":
				_ = add(descriptor("alpha", "missing"))
			case "cycle":
				_ = add(descriptor("alpha", "beta"))
				_ = add(descriptor("beta", "alpha"))
			case "duplicate":
				_ = add(descriptor("alpha"))
				if add(descriptor("alpha")) == nil {
					t.Fatal("duplicate accepted")
				}
				return
			case "invalid":
				m := descriptor("../alpha")
				if add(m) == nil {
					t.Fatal("invalid ID accepted")
				}
				return
			}
			if _, err := r.Build(nil); err == nil {
				t.Fatal("invalid graph accepted")
			}
			if built != 0 {
				t.Fatal("factory ran before graph validation")
			}
		})
	}
}
func TestRegistryDependencyOrderAndImmutableManifest(t *testing.T) {
	r := NewRegistry()
	m := descriptor("alpha", "zeta")
	if err := r.Register(m, factory("alpha")); err != nil {
		t.Fatal(err)
	}
	m.Dependencies[0] = "bad"
	_ = r.Register(descriptor("zeta"), factory("zeta"))
	_ = r.Register(descriptor("beta"), factory("beta"))
	modules, err := r.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, m := range modules {
		ids = append(ids, m.ID())
	}
	if !reflect.DeepEqual(ids, []string{"zeta", "alpha", "beta"}) {
		t.Fatalf("order %v", ids)
	}
	md := modules[1].(appkit.DescribedModule)
	copy := md.Manifest()
	copy.Dependencies[0] = "bad"
	if md.Manifest().Dependencies[0] != "zeta" {
		t.Fatal("mutable metadata")
	}
	wrapped := modules[1].(*described)
	_ = wrapped.Maintain(context.Background())
	_ = wrapped.Close()
	underlying := wrapped.Module.(*testModule)
	if !underlying.maintained || !underlying.closed || wrapped.DependencyStatus(context.Background())["test"] != "up" {
		t.Fatal("optional capability was not forwarded")
	}
}
func TestRegistryFactoryMismatch(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(descriptor("alpha"), factory("beta"))
	if _, err := r.Build(nil); err == nil {
		t.Fatal("mismatch accepted")
	}
}
func TestModuleSchemaDowngradeRequiresSnapshot(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	r := NewRegistry()
	m := descriptor("alpha")
	m.SchemaVersion = 2
	_ = r.Register(m, factory("alpha"))
	modules, _ := r.Build(nil)
	if err = CheckVersions(ctx, db, modules); err != nil {
		t.Fatal(err)
	}
	if err = RecordVersions(ctx, db, modules); err != nil {
		t.Fatal(err)
	}
	modules[0].(*described).metadata.SchemaVersion = 1
	if err = CheckVersions(ctx, db, modules); err == nil {
		t.Fatal("schema downgrade accepted")
	}
	modules[0].(*described).metadata.SchemaVersion = 2
	if err = CheckVersions(ctx, db, modules); err != nil {
		t.Fatal(err)
	}
}
func TestDefaultRegistryHasEightApplications(t *testing.T) {
	r := DefaultRegistry()
	if len(r.entries) != 8 {
		t.Fatalf("apps=%d", len(r.entries))
	}
	if _, err := r.Build(&appkit.Runtime{DataDir: t.TempDir(), DeriveKey: func(string) []byte { return make([]byte, 32) }}); err != nil {
		t.Fatal(err)
	}
}
