// Package plan builds the deterministic decomposition plan (PRD F2,
// plan-conversion §3): IR + the user's endpoint mapping → ordered generation
// units with template marking, target paths, dependency order, and token
// estimates. The tool never invents endpoints or their names — the mapping
// is user data (§4.2.8); DB method names get deterministic fallbacks the
// mapping may pin. The LLM is not involved in planning.
package plan

import (
	"fmt"
	"go/token"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// MethodPin is the user's optional pin for one DB method: the method name
// and, per bind, "name" or "name:Type" — reference-quality signatures
// without an LLM. Unpinned methods fall back to deterministic naming.
type MethodPin struct {
	Name   string   `yaml:"name"`
	Params []string `yaml:"params"`
}

// Endpoint is one user-mapped condition: the IR condition inventory index
// (1-based) promoted to an API endpoint with a user-chosen Go method name
// and route path (§4.2.8 — the tool never decides endpoint-ness).
type Endpoint struct {
	Condition int    `yaml:"condition"`
	Name      string `yaml:"name"`
	Route     string `yaml:"route"`
}

// Mapping is the user-specified conversion mapping (F3 run input): the
// target service identity plus which conditions become endpoints. Loaded
// from a small YAML file the user edits.
type Mapping struct {
	Service    string               `yaml:"service"`
	Module     string               `yaml:"module"`
	ReadDBs    []string             `yaml:"readDBs"`
	RouteGroup string               `yaml:"routeGroup"`
	Endpoints  []Endpoint           `yaml:"endpoints"`
	DBMethods  map[string]MethodPin `yaml:"dbMethods"`
}

// LoadMapping reads and validates a mapping YAML file.
func LoadMapping(path string) (*Mapping, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plan: read mapping %s: %w", path, err)
	}
	var m Mapping
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("plan: parse mapping %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("plan: %s: %w", path, err)
	}
	return &m, nil
}

// Validate enforces the mapping invariants: service identity present, at
// least one endpoint, unique Go method names and condition indices, valid
// identifiers, routes under the group.
func (m *Mapping) Validate() error {
	if m.Service == "" {
		return fmt.Errorf("service must not be empty")
	}
	if !token.IsIdentifier(m.Service) {
		return fmt.Errorf("service %q is not a valid Go identifier", m.Service)
	}
	if m.Module == "" {
		return fmt.Errorf("module must not be empty (import path of the service, e.g. mutual-fund-be/pkg/services/nav)")
	}
	if !strings.HasPrefix(m.Module, strings.SplitN(m.Module, "/", 2)[0]) {
		return fmt.Errorf("module %q is malformed", m.Module)
	}
	if len(m.Endpoints) == 0 {
		return fmt.Errorf("at least one endpoint must be mapped — the tool never invents endpoints (§4.2.8)")
	}
	conds := map[int]bool{}
	names := map[string]bool{}
	for i, e := range m.Endpoints {
		if e.Condition < 1 {
			return fmt.Errorf("endpoints[%d].condition must be a 1-based inventory index", i)
		}
		if conds[e.Condition] {
			return fmt.Errorf("endpoints[%d]: condition %d mapped twice", i, e.Condition)
		}
		conds[e.Condition] = true
		if !token.IsIdentifier(e.Name) {
			return fmt.Errorf("endpoints[%d].name %q is not a valid Go identifier", i, e.Name)
		}
		if names[e.Name] {
			return fmt.Errorf("endpoints[%d]: endpoint name %q used twice", i, e.Name)
		}
		names[e.Name] = true
		if !strings.HasPrefix(e.Route, "/") {
			return fmt.Errorf("endpoints[%d].route %q must start with /", i, e.Route)
		}
	}
	for qid, pin := range m.DBMethods {
		if !token.IsIdentifier(pin.Name) {
			return fmt.Errorf("dbMethods[%q].name %q is not a valid Go identifier", qid, pin.Name)
		}
	}
	return nil
}

// ImportPath returns the module path with the service folder appended when
// the last segment is not already the service name.
func (m *Mapping) ImportPath(folder string) string {
	if folder == "" || strings.HasSuffix(m.Module, "/"+folder) {
		return m.Module
	}
	return m.Module + "/" + folder
}
