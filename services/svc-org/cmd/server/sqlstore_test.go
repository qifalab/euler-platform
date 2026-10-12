package main

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/storage/sqltest"
)

// newSQLTestStore wires the SQL store to a throwaway account_db, built from the
// service's own DDL directory — the schema of record, not a table invented here.
func newSQLTestStore(t *testing.T) *sqlStore {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-org"))
	store, err := newSQLStore(context.Background(), db)
	if err != nil {
		t.Fatalf("newSQLStore: %v", err)
	}
	return store
}

const testAccount = "100123"

func TestSQLStoreProjectLifecycle(t *testing.T) {
	store := newSQLTestStore(t)

	// The caller supplies a phase-1 UUID; the schema's key is a 号段 id, so the
	// store must replace it and hand the real one back through the pointer.
	p := project{ProjectID: "6d1f8e4a-0000-4000-8000-000000000000", AccountID: testAccount, Name: "生产"}
	if err := store.CreateProject(&p); err != nil {
		t.Fatal(err)
	}
	if p.ProjectID == "6d1f8e4a-0000-4000-8000-000000000000" || p.ProjectID == "" {
		t.Fatalf("project id was not issued by the store: %q", p.ProjectID)
	}

	got, err := store.GetProject(testAccount, p.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "生产" || got.ProjectID != p.ProjectID || got.AccountID != testAccount {
		t.Fatalf("project round trip = %+v", got)
	}
	if got.ParentID != "" {
		t.Fatalf("top-level project has parent %q", got.ParentID)
	}

	// A sub-project carries its parent as a numeric id.
	child := project{AccountID: testAccount, Name: "测试环境", ParentID: p.ProjectID}
	if err := store.CreateProject(&child); err != nil {
		t.Fatal(err)
	}
	gotChild, err := store.GetProject(testAccount, child.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.ParentID != p.ProjectID {
		t.Fatalf("child parent = %q, want %q", gotChild.ParentID, p.ProjectID)
	}

	list, err := store.ListProjects(testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d projects, want 2", len(list))
	}
}

// TestSQLStoreProjectNameIsUniquePerAccount is the idempotency the DDL encodes in
// uk_acc_name: a create retry must not build a second project.
func TestSQLStoreProjectNameIsUniquePerAccount(t *testing.T) {
	store := newSQLTestStore(t)

	first := project{AccountID: testAccount, Name: "生产"}
	if err := store.CreateProject(&first); err != nil {
		t.Fatal(err)
	}
	again := project{AccountID: testAccount, Name: "生产"}
	if err := store.CreateProject(&again); !errors.Is(err, errProjectExists) {
		t.Fatalf("duplicate name = %v, want errProjectExists", err)
	}
	// The same name under another account is a different project.
	other := project{AccountID: "999", Name: "生产"}
	if err := store.CreateProject(&other); err != nil {
		t.Fatalf("another account could not reuse the name: %v", err)
	}
}

func TestSQLStoreUpdateProjectKeepsUntouchedFields(t *testing.T) {
	store := newSQLTestStore(t)

	parent := project{AccountID: testAccount, Name: "父"}
	if err := store.CreateProject(&parent); err != nil {
		t.Fatal(err)
	}
	child := project{AccountID: testAccount, Name: "子", ParentID: parent.ProjectID}
	if err := store.CreateProject(&child); err != nil {
		t.Fatal(err)
	}

	// Rename only: the parent must survive a nil pointer.
	renamed := "子-改名"
	if err := store.UpdateProject(testAccount, child.ProjectID, &renamed, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetProject(testAccount, child.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != renamed || got.ParentID != parent.ProjectID {
		t.Fatalf("after rename = %+v, want name %q with parent %q", got, renamed, parent.ProjectID)
	}

	// Reparent to top level with an explicit empty string.
	empty := ""
	if err := store.UpdateProject(testAccount, child.ProjectID, nil, &empty); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetProject(testAccount, child.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentID != "" || got.Name != renamed {
		t.Fatalf("after reparent = %+v, want no parent and the renamed name", got)
	}

	if err := store.UpdateProject(testAccount, "424242", &renamed, nil); !errors.Is(err, errProjectNotFound) {
		t.Fatalf("update of an unknown project = %v, want errProjectNotFound", err)
	}
}

// TestSQLStoreMoveResource covers the membership rules: first sight registers the
// resource, a wrong from-project is refused, and both ends must belong to the
// caller's account.
func TestSQLStoreMoveResource(t *testing.T) {
	store := newSQLTestStore(t)

	a := project{AccountID: testAccount, Name: "A"}
	b := project{AccountID: testAccount, Name: "B"}
	for _, p := range []*project{&a, &b} {
		if err := store.CreateProject(p); err != nil {
			t.Fatal(err)
		}
	}

	// First sight: the resource is registered in the destination.
	if err := store.MoveResource(testAccount, "res-1", a.ProjectID, b.ProjectID); err != nil {
		t.Fatal(err)
	}
	// It now belongs to B, so a move claiming it is in A must be refused.
	if err := store.MoveResource(testAccount, "res-1", a.ProjectID, b.ProjectID); err == nil {
		t.Fatal("a move that misstates the resource's current project must be refused")
	}
	// The correct move (from B) succeeds.
	if err := store.MoveResource(testAccount, "res-1", b.ProjectID, a.ProjectID); err != nil {
		t.Fatal(err)
	}

	// A project from another account is not a valid endpoint.
	stranger := project{AccountID: "999", Name: "别人的"}
	if err := store.CreateProject(&stranger); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveResource(testAccount, "res-2", a.ProjectID, stranger.ProjectID); !errors.Is(err, errProjectNotFound) {
		t.Fatalf("cross-account move = %v, want errProjectNotFound", err)
	}
	// A non-numeric project id cannot exist in this schema: miss, not error.
	if err := store.MoveResource(testAccount, "res-3", "not-a-number", a.ProjectID); !errors.Is(err, errProjectNotFound) {
		t.Fatalf("non-numeric project = %v, want errProjectNotFound", err)
	}
}

func TestSQLStoreTags(t *testing.T) {
	store := newSQLTestStore(t)

	if err := store.PutTag(tag{AccountID: testAccount, Key: "env", Value: "prod"}); err != nil {
		t.Fatal(err)
	}
	// Same key: an update, not a second tag (uk_acc_key).
	if err := store.PutTag(tag{AccountID: testAccount, Key: "env", Value: "staging"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTag(tag{AccountID: testAccount, Key: "team", Value: "core"}); err != nil {
		t.Fatal(err)
	}

	tags, err := store.ListTags(testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("tags = %+v, want 2 (the same key must not duplicate)", tags)
	}
	if tags[0].Key != "env" || tags[0].Value != "staging" {
		t.Fatalf("tags are not sorted by key or the value did not update: %+v", tags)
	}

	if err := store.DeleteTag(testAccount, "env"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTag(testAccount, "env"); !errors.Is(err, errTagNotFound) {
		t.Fatalf("deleting a missing tag = %v, want errTagNotFound", err)
	}
	// Another account's identically-keyed tag is untouched.
	if err := store.PutTag(tag{AccountID: "999", Key: "env", Value: "prod"}); err != nil {
		t.Fatal(err)
	}
	tags, err = store.ListTags(testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Key != "team" {
		t.Fatalf("tags after delete = %+v, want only team", tags)
	}
}

// TestSQLStoreRejectsNonNumericAccount: every column here is BIGINT UNSIGNED, so a
// non-numeric account cannot be stored — better a refusal than a row on an
// unintended shard.
func TestSQLStoreRejectsNonNumericAccount(t *testing.T) {
	store := newSQLTestStore(t)

	p := project{AccountID: "acct-100123", Name: "x"}
	if err := store.CreateProject(&p); !errors.Is(err, errInvalidAccount) {
		t.Fatalf("non-numeric account = %v, want errInvalidAccount", err)
	}
	if _, err := store.ListProjects("acct-100123"); !errors.Is(err, errInvalidAccount) {
		t.Fatalf("list with non-numeric account = %v, want errInvalidAccount", err)
	}
}

// TestSQLStoreOrgSurvivesRestart is why the org tree is in a database at all.
func TestSQLStoreOrgSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-org"))
	ctx := context.Background()

	first, err := newSQLStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	p := project{AccountID: testAccount, Name: "生产"}
	if err := first.CreateProject(&p); err != nil {
		t.Fatal(err)
	}
	if err := first.PutTag(tag{AccountID: testAccount, Key: "env", Value: "prod"}); err != nil {
		t.Fatal(err)
	}

	restarted, err := newSQLStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.GetProject(testAccount, p.ProjectID)
	if err != nil {
		t.Fatalf("project did not survive the restart: %v", err)
	}
	if got.Name != "生产" {
		t.Fatalf("reloaded project = %+v", got)
	}
	tags, err := restarted.ListTags(testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Value != "prod" {
		t.Fatalf("tags did not survive the restart: %+v", tags)
	}
}

// TestSQLStoreMatchesMemoryStore runs one script through both implementations and
// requires the same observable outcome. Both stores issue their own ids (号段
// numbers vs UUIDs), so projects are compared by name with the parent link
// resolved to the parent's name — which is what the ids encode.
func TestSQLStoreMatchesMemoryStore(t *testing.T) {
	type view struct{ Name, Parent string }
	run := func(store orgStore) (view, []view, []tag) {
		t.Helper()
		p := project{AccountID: testAccount, Name: "生产"}
		if err := store.CreateProject(&p); err != nil {
			t.Fatalf("create: %v", err)
		}
		child := project{AccountID: testAccount, Name: "测试", ParentID: p.ProjectID}
		if err := store.CreateProject(&child); err != nil {
			t.Fatalf("create child: %v", err)
		}
		if err := store.MoveResource(testAccount, "res-1", p.ProjectID, child.ProjectID); err != nil {
			t.Fatalf("move: %v", err)
		}
		if err := store.PutTag(tag{AccountID: testAccount, Key: "env", Value: "prod"}); err != nil {
			t.Fatalf("tag: %v", err)
		}
		projects, err := store.ListProjects(testAccount)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		tags, err := store.ListTags(testAccount)
		if err != nil {
			t.Fatalf("tags: %v", err)
		}
		got, err := store.GetProject(testAccount, child.ProjectID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		names := map[string]string{}
		for _, q := range projects {
			names[q.ProjectID] = q.Name
		}
		views := make([]view, 0, len(projects))
		for _, q := range projects {
			views = append(views, view{q.Name, names[q.ParentID]})
		}
		sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
		return view{got.Name, names[got.ParentID]}, views, tags
	}

	sqlChild, sqlProjects, sqlTags := run(newSQLTestStore(t))
	memChild, memProjects, memTags := run(newMemStore())

	if sqlChild != memChild || sqlChild.Parent != "生产" {
		t.Fatalf("child project: sql %+v, memory %+v", sqlChild, memChild)
	}
	if len(sqlProjects) != len(memProjects) {
		t.Fatalf("sql %d projects, memory %d", len(sqlProjects), len(memProjects))
	}
	for i := range sqlProjects {
		if sqlProjects[i] != memProjects[i] {
			t.Fatalf("project %d: sql %+v, memory %+v", i, sqlProjects[i], memProjects[i])
		}
	}
	if len(sqlTags) != len(memTags) || sqlTags[0] != memTags[0] {
		t.Fatalf("tags: sql %+v, memory %+v", sqlTags, memTags)
	}
}

// bothStores runs fn against the SQL store (when a database is configured) and
// the memory store.
func bothStores(t *testing.T, fn func(t *testing.T, store orgStore)) {
	t.Run("sql", func(t *testing.T) { fn(t, newSQLTestStore(t)) })
	t.Run("memory", func(t *testing.T) { fn(t, newMemStore()) })
}

// Regression: a numeric client-supplied project_id was used as the global
// primary key, so one tenant could take the ids the allocator would hand to
// another tenant's next creates — which then failed as "already exists".
func TestStoresIgnoreClientSuppliedProjectID(t *testing.T) {
	bothStores(t, func(t *testing.T, store orgStore) {
		for i := 1; i <= 5; i++ {
			squat := project{ProjectID: strconv.Itoa(i), AccountID: "999", Name: "squat-" + strconv.Itoa(i)}
			if err := store.CreateProject(&squat); err != nil {
				t.Fatal(err)
			}
		}
		chosen := project{ProjectID: "777777", AccountID: "999", Name: "chosen"}
		if err := store.CreateProject(&chosen); err != nil {
			t.Fatal(err)
		}
		if chosen.ProjectID == "777777" {
			t.Fatal("the store kept the caller's project id")
		}
		for i := 1; i <= 5; i++ {
			p := project{AccountID: testAccount, Name: "p-" + strconv.Itoa(i)}
			if err := store.CreateProject(&p); err != nil {
				t.Fatalf("create %d for another tenant: %v", i, err)
			}
		}
	})
}

// Regression: parent_id was never validated — a project could be its own
// parent, close a cycle, or hang under another account's (or a missing) project.
func TestStoresValidateParent(t *testing.T) {
	bothStores(t, func(t *testing.T, store orgStore) {
		a := project{AccountID: testAccount, Name: "a"}
		if err := store.CreateProject(&a); err != nil {
			t.Fatal(err)
		}
		b := project{AccountID: testAccount, Name: "b", ParentID: a.ProjectID}
		if err := store.CreateProject(&b); err != nil {
			t.Fatal(err)
		}
		c := project{AccountID: testAccount, Name: "c", ParentID: b.ProjectID}
		if err := store.CreateProject(&c); err != nil {
			t.Fatal(err)
		}
		stranger := project{AccountID: "999", Name: "stranger"}
		if err := store.CreateProject(&stranger); err != nil {
			t.Fatal(err)
		}

		for name, parent := range map[string]string{
			"missing":         "424242424",
			"non-numeric":     "not-a-project",
			"another account": stranger.ProjectID,
		} {
			p := project{AccountID: testAccount, Name: "x-" + name, ParentID: parent}
			if err := store.CreateProject(&p); !errors.Is(err, errInvalidParent) {
				t.Errorf("create under %s parent = %v, want errInvalidParent", name, err)
			}
			if err := store.UpdateProject(testAccount, c.ProjectID, nil, &parent); !errors.Is(err, errInvalidParent) {
				t.Errorf("reparent under %s parent = %v, want errInvalidParent", name, err)
			}
		}
		self := a.ProjectID
		if err := store.UpdateProject(testAccount, a.ProjectID, nil, &self); !errors.Is(err, errInvalidParent) {
			t.Errorf("self-parent = %v, want errInvalidParent", err)
		}
		descendant := c.ProjectID
		if err := store.UpdateProject(testAccount, a.ProjectID, nil, &descendant); !errors.Is(err, errInvalidParent) {
			t.Errorf("parent = own grandchild (cycle) = %v, want errInvalidParent", err)
		}
		got, err := store.GetProject(testAccount, a.ProjectID)
		if err != nil || got.ParentID != "" {
			t.Fatalf("a refused reparent changed the project: %+v %v", got, err)
		}
		// A legal reparent still works: c moves directly under a.
		up := a.ProjectID
		if err := store.UpdateProject(testAccount, c.ProjectID, nil, &up); err != nil {
			t.Fatalf("legal reparent: %v", err)
		}
	})
}

// Names are unique per account in both stores, on create and on rename.
func TestStoresProjectNameUniqueOnCreateAndRename(t *testing.T) {
	bothStores(t, func(t *testing.T, store orgStore) {
		a := project{AccountID: testAccount, Name: "a"}
		b := project{AccountID: testAccount, Name: "b"}
		for _, p := range []*project{&a, &b} {
			if err := store.CreateProject(p); err != nil {
				t.Fatal(err)
			}
		}
		dup := project{AccountID: testAccount, Name: "a"}
		if err := store.CreateProject(&dup); !errors.Is(err, errProjectExists) {
			t.Fatalf("duplicate create = %v, want errProjectExists", err)
		}
		name := "a"
		if err := store.UpdateProject(testAccount, b.ProjectID, &name, nil); !errors.Is(err, errProjectExists) {
			t.Fatalf("rename onto an existing name = %v, want errProjectExists", err)
		}
		same := "b"
		if err := store.UpdateProject(testAccount, b.ProjectID, &same, nil); err != nil {
			t.Fatalf("renaming a project to its own name: %v", err)
		}
	})
}

// Regression: the membership row (global uk_resource) was read and updated by
// resource_id alone, so a tenant could move another tenant's resource into its
// own project, and the mismatch error echoed the other tenant's project id.
func TestSQLStoreMoveResourceIsAccountScoped(t *testing.T) {
	store := newSQLTestStore(t)

	theirs := project{AccountID: "999", Name: "theirs"}
	theirs2 := project{AccountID: "999", Name: "theirs-2"}
	mine := project{AccountID: testAccount, Name: "mine"}
	mine2 := project{AccountID: testAccount, Name: "mine-2"}
	for _, p := range []*project{&theirs, &theirs2, &mine, &mine2} {
		if err := store.CreateProject(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MoveResource("999", "res-x", theirs.ProjectID, theirs.ProjectID); err != nil {
		t.Fatal(err)
	}

	for _, from := range []string{mine.ProjectID, mine2.ProjectID} {
		err := store.MoveResource(testAccount, "res-x", from, mine2.ProjectID)
		if !errors.Is(err, errResourceNotFound) {
			t.Fatalf("moving another account's resource = %v, want errResourceNotFound", err)
		}
		if strings.Contains(err.Error(), theirs.ProjectID) {
			t.Fatalf("error leaked the owner's project id: %v", err)
		}
	}
	// The owner's membership is untouched and still movable by the owner.
	if err := store.MoveResource("999", "res-x", theirs.ProjectID, theirs2.ProjectID); err != nil {
		t.Fatalf("owner's move after the foreign attempts: %v", err)
	}
}
