package database

import (
	"reflect"
	"testing"
)

// POST /admin/fix-pg-permissions re-applies grants through ReadOnly, the call
// quota enforcement uses; the engine has no separate Repair operation.
func TestFixPGPermissionsReappliesGrantsThroughReadOnly(t *testing.T) {
	if _, ok := reflect.TypeOf((*Engine)(nil)).Elem().MethodByName("Repair"); ok {
		t.Fatal("Engine declares Repair, which nothing calls")
	}
	m, s, fake, _ := databaseFixture(t)
	my := createDatabase(t, m, s, "mysql")
	pg := createDatabase(t, m, s, "postgresql")
	// Grants on both engines drifted to read-only outside Euler.
	fake.mu.Lock()
	for _, db := range []string{my.Database, pg.Database} {
		v := fake.resources[db]
		v.readOnly = true
		fake.resources[db] = v
	}
	before := fake.readonlyCalls
	fake.mu.Unlock()
	databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/admin/fix-pg-permissions", nil), 200)
	fake.mu.Lock()
	calls := fake.readonlyCalls - before
	fake.mu.Unlock()
	if fake.state(pg.Database).readOnly || calls != 1 {
		t.Fatalf("PostgreSQL grants not re-applied once: readOnly=%v calls=%d", fake.state(pg.Database).readOnly, calls)
	}
	if !fake.state(my.Database).readOnly {
		t.Fatal("repair changed a MySQL database whose quota state did not change")
	}
	var ops int
	if e := m.rt.DB.QueryRow("SELECT COUNT(*) FROM database_operations WHERE resource_id=? AND action='quota.enforce' AND state='completed'", pg.ID).Scan(&ops); e != nil || ops != 1 {
		t.Fatalf("repair not recorded as one completed operation: %d %v", ops, e)
	}
}
