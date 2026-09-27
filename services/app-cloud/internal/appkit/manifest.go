package appkit

import (
	"fmt"
	"regexp"
)

// Manifest describes a trusted, compiled module. It never grants permissions or
// causes code to be fetched from a user-supplied URL at runtime.
type Manifest struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	APIVersion    string   `json:"apiVersion"`
	SchemaVersion int      `json:"schemaVersion"`
	Category      string   `json:"category"`
	Description   string   `json:"description"`
	Color         string   `json:"color"`
	Repository    string   `json:"repository,omitempty"`
	Capabilities  []string `json:"capabilities"`
	Dependencies  []string `json:"dependencies"`
	Configuration []string `json:"configuration"`
}

var manifestID = regexp.MustCompile(`^[a-z][a-z0-9-]{1,47}$`)
var moduleVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?$`)

func (m Manifest) Validate() error {
	if !manifestID.MatchString(m.ID) || m.Name == "" || len(m.Name) > 120 || !moduleVersion.MatchString(m.Version) || m.APIVersion != "v1" || m.SchemaVersion < 1 {
		return fmt.Errorf("invalid application manifest")
	}
	seen := map[string]bool{}
	for _, id := range m.Dependencies {
		if !manifestID.MatchString(id) || id == m.ID || seen[id] {
			return fmt.Errorf("invalid or duplicate application dependency")
		}
		seen[id] = true
	}
	return nil
}

type DescribedModule interface {
	Module
	Manifest() Manifest
}
