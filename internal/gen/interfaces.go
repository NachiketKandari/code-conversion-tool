package gen

import (
	"errors"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/goast"
	"github.com/Public/convert-tux-to-go/internal/plan"
	"github.com/Public/convert-tux-to-go/internal/templates"
)

// sortedUnits returns the plan's units in ID order (deterministic).
func sortedUnits(p *plan.Plan) []plan.Unit {
	units := make([]plan.Unit, len(p.Units))
	copy(units, p.Units)
	sort.SliceStable(units, func(i, j int) bool { return units[i].ID < units[j].ID })
	return units
}

// DBMethodsFile assembles db/<service>.go: package header, the exact import
// set the rendered bodies need (unused imports are compile errors, so the
// set is derived per method kind), and every db unit's method body.
func (s *Service) DBMethodsFile(p *plan.Plan) (string, error) {
	var bodies []string
	need := struct{ sql, errors, fmt, time bool }{}
	for _, u := range sortedUnits(p) {
		if u.Kind != plan.KindDBMethod {
			continue
		}
		q := s.Query(u.QueryIDs[0])
		if q == nil {
			return "", fmt.Errorf("gen: db unit %s references unknown query %q", u.ID, u.QueryIDs[0])
		}
		body, _, _, err := s.DBMethod(u)
		if err != nil {
			return "", err
		}
		bodies = append(bodies, body)
		pin, _ := s.Pin(u.QueryIDs[0])
		for _, pp := range pin.Params {
			if strings.HasSuffix(pp, ":time.Time") {
				need.time = true
			}
		}
		switch q.Type {
		case ir.QuerySelectSingle:
			need.sql, need.errors = true, true
		case ir.QuerySelectMulti:
			need.fmt = true
		}
		if hasTimeParam(methodParams(s, q)) {
			need.time = true
		}
	}
	var sb strings.Builder
	sb.WriteString("package db\n\nimport (\n")
	writeImport := func(path string) { sb.WriteString("\t\"" + path + "\"\n") }
	writeImport("context")
	if need.sql {
		writeImport("database/sql")
	}
	if need.errors {
		writeImport("errors")
	}
	if need.fmt {
		writeImport("fmt")
	}
	writeImport(s.Module + "/pkg/logger")
	writeImport(s.ModelsPkg)
	if need.time {
		writeImport("time")
	}
	if need.fmt || need.errors || need.sql {
		sb.WriteString("\n")
	}
	writeImport("go.uber.org/zap")
	sb.WriteString(")\n")
	for _, b := range bodies {
		sb.WriteString("\n" + strings.TrimRight(b, "\n") + "\n")
	}
	return gofmt(sb.String())
}

// gofmt normalizes manually assembled Go sources so generated files pass
// the Tier-A gofmt check byte-for-byte.
func gofmt(src string) (string, error) {
	formatted, err := format.Source([]byte(src))
	if err != nil {
		return "", fmt.Errorf("gen: assembled source does not parse: %w", err)
	}
	return string(formatted), nil
}

// methodParams resolves a query's parameter specs (binds + pin) without
// rendering — used for import derivation.
func methodParams(s *Service, q *ir.Query) []templates.ParamSpec {
	pin, _ := s.Pin(q.ID)
	params, err := s.dbParams(q, pin)
	if err != nil {
		return nil
	}
	return params
}

// DBInterface renders db/interface.go: the store struct, the accumulating
// <Service>Store interface (one signature line per db unit), and the
// constructor (PRD §4.2.3).
func (s *Service) DBInterface(p *plan.Plan) (string, error) {
	data := templates.DBInterfaceData{
		Package:   "db",
		StoreType: "store",
		IfaceName: s.If + "Store",
		CtorName:  "New" + s.If + "Store",
		WithGorm:  s.WithGorm,
		ModelsPkg: s.ModelsPkg,
	}
	for _, u := range sortedUnits(p) {
		if u.Kind != plan.KindDBMethod {
			continue
		}
		_, sig, _, err := s.DBMethod(u)
		if err != nil {
			return "", err
		}
		data.Methods = append(data.Methods, sig)
	}
	if len(data.Methods) == 0 {
		return "", fmt.Errorf("gen: plan has no db units")
	}
	return render(templates.DBInterfaceFile, data)
}

// AccumulateDBInterface appends one method signature to db/interface.go —
// the accumulating-interface contract (§4.2.3, OQ14). A missing file is
// created from the template skeleton first, so single-run and incremental
// paths converge on the same shape.
func (s *Service) AccumulateDBInterface(path, signature string) error {
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		skeleton, err := s.DBInterfaceSkeleton()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("gen: create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(skeleton), 0o644); err != nil {
			return fmt.Errorf("gen: write %s: %w", path, err)
		}
	}
	_, err := goast.AccumulateInterface(path, "db", s.If+"Store", signature)
	return err
}

func (s *Service) DBInterfaceSkeleton() (string, error) {
	return render(templates.DBInterfaceFile, templates.DBInterfaceData{
		Package:   "db",
		StoreType: "store",
		IfaceName: s.If + "Store",
		CtorName:  "New" + s.If + "Store",
		WithGorm:  s.WithGorm,
		ModelsPkg: s.ModelsPkg,
	})
}

// ControllerInterface renders controller/interface.go with one method
// signature per mapped endpoint (PRD §4.8.6).
func (s *Service) ControllerInterface(p *plan.Plan) (string, error) {
	data := templates.ControllerInterfaceData{
		Package:    "controller",
		StructName: lowerFirst(s.Mapping.Service) + "Controller",
		IfaceName:  s.If + "Controller",
		CtorName:   "New" + s.If + "Controller",
		DBPkg:      s.Mapping.ImportPath("db"),
		ModelsPkg:  s.ModelsPkg,
		StoreIface: "db." + s.If + "Store",
	}
	for _, e := range s.Mapping.Endpoints {
		data.Methods = append(data.Methods,
			fmt.Sprintf("%s(ctx context.Context, request *models.%s) (data []*models.%s, err error)",
				e.Name, s.requestType(e.Name), s.responseType(e.Name)))
	}
	return render(templates.ControllerInterfaceFile, data)
}

// HandlerInterface renders handler/interface.go: handler struct + interface
// + constructor + the wiring function building the store from the existing
// repo (OQ11).
func (s *Service) HandlerInterface(p *plan.Plan) (string, error) {
	data := templates.HandlerInterfaceData{
		Package:         "handler",
		Module:          s.Module,
		Service:         s.Mapping.Service,
		StructName:      lowerFirst(s.Mapping.Service) + "Handler",
		IfaceName:       s.If + "Handler",
		CtorName:        "New" + s.If + "Handler",
		WiringFnName:    s.If + "Controller",
		ControllerIface: s.If + "Controller",
		ControllerCtor:  "New" + s.If + "Controller",
		StoreCtor:       "New" + s.If + "Store",
		ReadDBs:         s.Mapping.ReadDBs,
	}
	for _, e := range s.Mapping.Endpoints {
		data.Methods = append(data.Methods, e.Name)
	}
	return render(templates.HandlerInterfaceFile, data)
}

// HandlerMethodsFile assembles handler/<service>.go: the deterministic gin
// glue per endpoint (bind → controller call → response shaping).
func (s *Service) HandlerMethodsFile() (string, error) {
	var sb strings.Builder
	sb.WriteString("package handler\n\nimport (\n")
	sb.WriteString("\t\"" + s.Module + "/pkg/logger\"\n")
	sb.WriteString("\t\"" + s.Module + "/pkg/network\"\n")
	sb.WriteString("\t\"" + s.ModelsPkg + "\"\n\n")
	sb.WriteString("\t\"github.com/gin-gonic/gin\"\n")
	sb.WriteString(")\n")
	for _, e := range s.Mapping.Endpoints {
		body, err := render(templates.HandlerMethod, templates.HandlerMethodData{
			StructName:  lowerFirst(s.Mapping.Service) + "Handler",
			Name:        e.Name,
			RequestType: "models." + s.requestType(e.Name),
		})
		if err != nil {
			return "", err
		}
		sb.WriteString("\n" + strings.TrimRight(body, "\n") + "\n")
	}
	return gofmt(sb.String())
}

// Router renders the router snippet for the user's transport layer (R8: the
// transport exists — tuxgo emits the registration lines, never scaffolding).
func (s *Service) Router() (string, error) {
	data := templates.RouterData{Service: s.Mapping.Service}
	for _, e := range s.Mapping.Endpoints {
		data.Routes = append(data.Routes, templates.RouteSpec{Path: e.Route, Handler: e.Name})
	}
	return render(templates.RouterSnippet, data)
}

// ControllerSignature renders one controller method's interface line.
func (s *Service) ControllerSignature(e plan.Endpoint) string {
	return fmt.Sprintf("%s(ctx context.Context, request *models.%s) (data []*models.%s, err error)",
		e.Name, s.requestType(e.Name), s.responseType(e.Name))
}

// sortedEndpoints returns the mapping endpoints in mapped order (stable).
func sortedEndpoints(m *plan.Mapping) []plan.Endpoint {
	out := make([]plan.Endpoint, len(m.Endpoints))
	copy(out, m.Endpoints)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Condition < out[j].Condition })
	return out
}
