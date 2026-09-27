package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type Factory func(*appkit.Runtime) appkit.Module

type registration struct {
	manifest appkit.Manifest
	factory  Factory
}

// Registry validates trusted modules before migrations or background workers run.
// Extensions are linked by the deployer; tenant users cannot install executable code.
type Registry struct{ entries map[string]registration }

func NewRegistry() *Registry { return &Registry{entries: map[string]registration{}} }

func (r *Registry) Register(manifest appkit.Manifest, factory Factory) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if factory == nil {
		return errors.New("application factory is required")
	}
	if _, exists := r.entries[manifest.ID]; exists {
		return fmt.Errorf("duplicate application %s", manifest.ID)
	}
	// Own the slices so a caller cannot mutate a validated dependency graph.
	manifest.Dependencies = append([]string{}, manifest.Dependencies...)
	manifest.Capabilities = append([]string{}, manifest.Capabilities...)
	manifest.Configuration = append([]string{}, manifest.Configuration...)
	r.entries[manifest.ID] = registration{manifest, factory}
	return nil
}

func (r *Registry) Build(rt *appkit.Runtime) ([]appkit.Module, error) {
	ids := make([]string, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	visited, visiting := map[string]bool{}, map[string]bool{}
	order := []string{}
	var visit func(string) error
	visit = func(id string) error {
		entry, ok := r.entries[id]
		if !ok {
			return fmt.Errorf("missing application dependency %s", id)
		}
		if visiting[id] {
			return fmt.Errorf("cyclic application dependency %s", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dependency := range entry.manifest.Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[id], visited[id] = false, true
		order = append(order, id)
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	modules := make([]appkit.Module, 0, len(order))
	for _, id := range order {
		entry := r.entries[id]
		module := entry.factory(rt)
		if module == nil || module.ID() != id {
			return nil, fmt.Errorf("application factory ID does not match %s", id)
		}
		modules = append(modules, &described{Module: module, metadata: entry.manifest})
	}
	return modules, nil
}

type described struct {
	appkit.Module
	metadata appkit.Manifest
}

func (m *described) Migrate(ctx context.Context) error {
	// Each module owns its idempotent migrations. Version checks are performed
	// by the deployment registry before any module migration is started.
	return m.Module.Migrate(ctx)
}

// CheckVersions rejects a known schema downgrade before running any module.
// A release rollback restores its matching snapshot instead of guessing how to
// reverse application data migrations.
func CheckVersions(ctx context.Context, db *sql.DB, modules []appkit.Module) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS application_module_versions(application_id TEXT PRIMARY KEY, version TEXT NOT NULL, schema_version INTEGER NOT NULL)`); err != nil {
		return err
	}
	for _, module := range modules {
		descriptor, ok := module.(appkit.DescribedModule)
		if !ok {
			continue
		}
		manifest := descriptor.Manifest()
		var schema int
		err := db.QueryRowContext(ctx, `SELECT schema_version FROM application_module_versions WHERE application_id=?`, module.ID()).Scan(&schema)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && schema > manifest.SchemaVersion {
			return fmt.Errorf("application %s requires a newer server schema; restore the matching release backup", module.ID())
		}
	}
	return nil
}

func RecordVersions(ctx context.Context, db *sql.DB, modules []appkit.Module) error {
	for _, module := range modules {
		descriptor, ok := module.(appkit.DescribedModule)
		if !ok {
			continue
		}
		manifest := descriptor.Manifest()
		if _, err := db.ExecContext(ctx, `INSERT INTO application_module_versions(application_id,version,schema_version) VALUES(?,?,?) ON CONFLICT(application_id) DO UPDATE SET version=excluded.version,schema_version=excluded.schema_version`, module.ID(), manifest.Version, manifest.SchemaVersion); err != nil {
			return err
		}
	}
	return nil
}

func (m *described) Manifest() appkit.Manifest {
	v := m.metadata
	v.Capabilities = append([]string{}, v.Capabilities...)
	v.Dependencies = append([]string{}, v.Dependencies...)
	v.Configuration = append([]string{}, v.Configuration...)
	return v
}

func (m *described) Close() error {
	if closer, ok := m.Module.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func (m *described) DependencyStatus(ctx context.Context) map[string]string {
	if probe, ok := m.Module.(interface {
		DependencyStatus(context.Context) map[string]string
	}); ok {
		return probe.DependencyStatus(ctx)
	}
	return map[string]string{}
}

func (m *described) Maintain(ctx context.Context) error {
	if worker, ok := m.Module.(interface{ Maintain(context.Context) error }); ok {
		return worker.Maintain(ctx)
	}
	return nil
}
