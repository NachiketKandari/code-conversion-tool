package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/flow"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
)

// Kind classifies a generation unit.
type Kind string

const (
	KindModels              Kind = "models"
	KindDBMethod            Kind = "db_method"
	KindDBInterface         Kind = "db_interface"
	KindControllerMethod    Kind = "controller_method"
	KindControllerInterface Kind = "controller_interface"
	KindHandlerMethod       Kind = "handler_method"
	KindHandlerInterface    Kind = "handler_interface"
	KindRouter              Kind = "router"
	KindMocks               Kind = "mocks"
	// KindTPCall is one tpcall site of a mapped endpoint (PF-4.4): it
	// renders as a compilable placeholder stub carrying the send/recv FML
	// contract — never invented outbound scaffolding (R8).
	KindTPCall Kind = "tpcall"
)

// Unit is one step of the decomposition plan (plan-conversion §3): what to
// generate, from which source, into which target file, with which template,
// whether the LLM fills the body, and what must exist first. TP carries the
// tpcall site's full contract for KindTPCall units.
type Unit struct {
	ID            string     `json:"id"`
	Kind          Kind       `json:"kind"`
	Name          string     `json:"name"`
	SourceFile    string     `json:"source_file,omitempty"`
	SourceLines   string     `json:"source_lines,omitempty"`
	QueryIDs      []string   `json:"query_ids,omitempty"`
	TargetPath    string     `json:"target_path"`
	TemplateID    string     `json:"template_id,omitempty"`
	LLM           bool       `json:"llm"`
	TokenEstimate int        `json:"token_estimate"`
	Deps          []string   `json:"deps,omitempty"`
	TP            *ir.TPCall `json:"tp,omitempty"`
}

// Skipped is an IR unit deliberately not converted — a query belonging only
// to unmapped conditions (§4.2.8: conditions not mapped stay unconverted).
type Skipped struct {
	QueryID string `json:"query_id"`
	Reason  string `json:"reason"`
}

// Blocker is an unresolved external fn: generation of the endpoints that
// call it blocks pending its defining file — visible in the plan, never a
// silent stub (§4.2.9.4).
type Blocker struct {
	Fn        string   `json:"fn"`
	Reason    string   `json:"reason"`
	Endpoints []string `json:"endpoints"`
}

// Plan is the deterministic decomposition of one conversion run.
type Plan struct {
	Service  string    `json:"service"`
	Module   string    `json:"module"`
	Source   string    `json:"source"`
	Mapping  *Mapping  `json:"mapping"`
	Units    []Unit    `json:"units"`
	Skipped  []Skipped `json:"skipped,omitempty"`
	Blockers []Blocker `json:"blockers,omitempty"`
	Dropped  []string  `json:"dropped,omitempty"`
	Orphans  []string  `json:"orphans,omitempty"`
}

// Options carries the plan inputs: the main file's IR and source text, the
// fn-file IRs backing resolved external fns, the user mapping, and the
// budget used for token estimates.
type Options struct {
	Main    *ir.File
	Source  string
	FnFiles []*ir.File
	Mapping *Mapping
	Budget  budget.Budget
}

// Build constructs the plan. Deterministic: identical inputs produce a
// byte-identical plan.
func Build(opts Options) (*Plan, error) {
	if opts.Main == nil || opts.Mapping == nil {
		return nil, fmt.Errorf("plan: main IR and mapping are required")
	}
	m := opts.Mapping
	p := &Plan{Service: m.Service, Module: m.Module, Source: opts.Main.Path, Mapping: m}

	// Condition lookup — inventory indices are 1-based.
	condBy := make(map[int]*ir.Condition, len(opts.Main.Conditions))
	for i := range opts.Main.Conditions {
		condBy[opts.Main.Conditions[i].Index] = &opts.Main.Conditions[i]
	}
	// Discovery resolution (PRD-2026-09-10): conditionRef endpoints resolve
	// through the flow tree, built lazily and at most once (flow.TreeFor is
	// the shared derivation; plan's policy is to hard-error).
	var flowTree *flow.Tree
	condRef := func(ref string) (*ir.Condition, error) {
		if flowTree == nil {
			if strings.TrimSpace(opts.Source) == "" {
				return nil, fmt.Errorf("plan: endpoint references candidate %s but no source is available to re-derive the flow tree", ref)
			}
			t, err := flow.TreeFor(opts.Source, opts.Main)
			if err != nil {
				return nil, fmt.Errorf("plan: flow tree for candidate %s: %w", ref, err)
			}
			flowTree = t
		}
		c, err := flow.ConditionFor(flowTree, opts.Main.Conditions, ref)
		if err != nil {
			return nil, fmt.Errorf("plan: %w", err)
		}
		return c, nil
	}
	cond := func(e Endpoint) (*ir.Condition, error) {
		if e.ConditionRef != "" {
			return condRef(e.ConditionRef)
		}
		c, ok := condBy[e.Condition]
		if !ok {
			err := fmt.Errorf("plan: endpoint %s maps condition %d — inventory has %d conditions",
				e.Name, e.Condition, len(opts.Main.Conditions))
			if n := len(opts.Main.Unbalanced); n > 0 {
				// The condition inventory may be short because the parse
				// was truncated (severity F3) — say so, never a bare count.
				err = fmt.Errorf("%w — the file carries %d unbalanced region(s) (%s), so the parse may be truncated",
					err, n, unbalancedSummary(opts.Main.Unbalanced))
			}
			return nil, err
		}
		return c, nil
	}

	queryByID := make(map[string]*ir.Query, len(opts.Main.Queries))
	for _, q := range opts.Main.Queries {
		queryByID[q.ID] = q
	}

	// Queries per mapped endpoint, deduplicated to canonical units.
	canonical := map[string]*ir.Query{}
	var dbQueries []*ir.Query
	seen := map[string]bool{}
	endpointQueries := make(map[string][]string, len(m.Endpoints)) // endpoint name -> query IDs
	for _, e := range m.Endpoints {
		c, err := cond(e)
		if err != nil {
			return nil, err
		}
		ids := c.QueryIDs
		if len(ids) == 0 {
			ids = queriesIn(opts.Main, c)
		}
		for _, id := range ids {
			q := queryByID[id]
			if q == nil {
				return nil, fmt.Errorf("plan: condition %d references unknown query %q", c.Index, id)
			}
			if q.DuplicateOf != "" {
				q = queryByID[q.DuplicateOf]
			}
			endpointQueries[e.Name] = append(endpointQueries[e.Name], q.ID)
			if !seen[q.ID] {
				seen[q.ID] = true
				canonical[q.ID] = q
				dbQueries = append(dbQueries, q)
			}
		}
	}

	// Queries belonging to no mapped endpoint are deliberate skips.
	mappedIDs := map[string]bool{}
	for _, q := range dbQueries {
		mappedIDs[q.ID] = true
	}
	for _, q := range opts.Main.Queries {
		if q.DuplicateOf != "" || mappedIDs[q.ID] {
			continue
		}
		p.Skipped = append(p.Skipped, Skipped{QueryID: q.ID, Reason: "belongs only to unmapped conditions"})
	}

	// External fns: chk_* are dropped constructs at conversion time
	// (§4.8.4.1); resolved SQL-bearing fns contribute their own db units;
	// everything else blocks the endpoints that call it (§4.2.9.4).
	fnIRByPath := make(map[string]*ir.File, len(opts.FnFiles))
	for _, f := range opts.FnFiles {
		fnIRByPath[f.Path] = f
	}
	var fnQueries []*ir.Query
	for _, fn := range opts.Main.ExternalFns {
		if strings.HasPrefix(fn.Name, "chk_") {
			p.Dropped = append(p.Dropped, fn.Name+" (session/error plumbing — middleware owns it, §4.8.4.1)")
			continue
		}
		switch {
		case fn.Resolved && fn.HasSQL && fnIRByPath[fn.DefinedIn] != nil:
			f := fnIRByPath[fn.DefinedIn]
			for _, q := range f.Queries {
				ns := fn.Name + ":" + q.ID
				if seen[ns] {
					continue
				}
				seen[ns] = true
				qq := *q
				qq.ID = ns
				fnQueries = append(fnQueries, &qq)
			}
		case fn.Resolved:
			p.Dropped = append(p.Dropped, fn.Name+" (no SQL in its body — pure logic, inlined by the controller)")
		default:
			b := Blocker{Fn: fn.Name, Reason: "defining file not provided — generation of the calling endpoints is blocked (§4.2.9.4)"}
			for _, e := range m.Endpoints {
				if c, err := cond(e); err == nil && callsiteIn(fn.Callsites, c) {
					b.Endpoints = append(b.Endpoints, e.Name)
				}
			}
			p.Blockers = append(p.Blockers, b)
		}
	}

	// Units — generation order: models → db methods → db interface →
	// controllers → controller interface → handlers → handler interface →
	// router → mocks.
	add := func(u Unit) { p.Units = append(p.Units, u) }
	dbUnitIDs := []string{}

	add(Unit{
		ID: "u01", Kind: KindModels, Name: m.Service + "Models",
		SourceFile: opts.Main.Path, TargetPath: m.ImportPath("models") + "/" + m.Service + ".go",
		TemplateID: "model_file", TokenEstimate: opts.Budget.Count(modelsRepr(opts.Main)),
	})
	for _, q := range dbQueries {
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		name := methodName(m, q)
		add(Unit{
			ID: id, Kind: KindDBMethod, Name: name,
			SourceFile: opts.Main.Path, SourceLines: lineSpan(q.StartLine, q.EndLine),
			QueryIDs:   []string{q.ID},
			TargetPath: m.ImportPath("db") + "/" + m.Service + ".go",
			TemplateID: q.TemplateID, LLM: false,
			TokenEstimate: opts.Budget.Count(q.SQL),
			Deps:          []string{"u01"},
		})
		dbUnitIDs = append(dbUnitIDs, id)
	}
	for _, q := range fnQueries {
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		name := methodName(m, q)
		add(Unit{
			ID: id, Kind: KindDBMethod, Name: name,
			SourceFile: definedInOf(opts, q.ID), SourceLines: lineSpan(q.StartLine, q.EndLine),
			QueryIDs:   []string{q.ID},
			TargetPath: m.ImportPath("db") + "/" + m.Service + ".go",
			TemplateID: q.TemplateID, LLM: false,
			TokenEstimate: opts.Budget.Count(q.SQL),
			Deps:          []string{"u01"},
		})
		dbUnitIDs = append(dbUnitIDs, id)
	}
	ifaceID := fmt.Sprintf("u%02d", len(p.Units)+1)
	add(Unit{
		ID: ifaceID, Kind: KindDBInterface, Name: interfaceName(m.Service) + "Store",
		TargetPath: m.ImportPath("db") + "/interface.go",
		TemplateID: "db_interface_file",
		Deps:       dbUnitIDs,
	})

	ctrlIDs := []string{}
	for _, e := range m.Endpoints {
		c, err := cond(e)
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		add(Unit{
			ID: id, Kind: KindControllerMethod, Name: e.Name,
			SourceFile: opts.Main.Path, SourceLines: lineSpan(c.StartLine, c.EndLine),
			QueryIDs:   endpointQueries[e.Name],
			TargetPath: m.ImportPath("controller") + "/" + m.Service + ".go",
			TemplateID: "controller_method", LLM: true,
			TokenEstimate: opts.Budget.Count(branchSource(opts.Source, c.StartLine, c.EndLine)),
			Deps:          []string{ifaceID},
		})
		ctrlIDs = append(ctrlIDs, id)
	}
	ctrlIfaceID := fmt.Sprintf("u%02d", len(p.Units)+1)
	add(Unit{
		ID: ctrlIfaceID, Kind: KindControllerInterface, Name: interfaceName(m.Service) + "Controller",
		TargetPath: m.ImportPath("controller") + "/interface.go",
		TemplateID: "controller_interface_file",
		Deps:       ctrlIDs,
	})

	// TPCall units — one per call site under the owning endpoint (PF-4.4).
	// Sites outside every mapped condition are recorded skips (§4.2.8: only
	// mapped conditions convert).
	tpcallContract := func(tp *ir.TPCall) string {
		var sb strings.Builder
		for _, op := range tp.SendFML {
			fmt.Fprintf(&sb, "send %s %s\n", op.Field, op.Target)
		}
		for _, op := range tp.RecvFML {
			fmt.Fprintf(&sb, "recv %s %s\n", op.Field, op.Target)
		}
		return sb.String()
	}
	tpNameSeen := map[string]int{}
	for i := range opts.Main.TPCalls {
		tp := &opts.Main.TPCalls[i]
		owner := ""
		for _, e := range m.Endpoints {
			if c, err := cond(e); err == nil && tp.StartLine >= c.StartLine && tp.StartLine <= c.EndLine {
				owner = e.Name
				break
			}
		}
		if owner == "" {
			p.Skipped = append(p.Skipped, Skipped{QueryID: "tpcall:" + tp.Service, Reason: "belongs only to unmapped conditions"})
			continue
		}
		name := "TPCall" + camel(tp.Service)
		tpNameSeen[name]++
		if n := tpNameSeen[name]; n > 1 {
			name = fmt.Sprintf("%s%d", name, n)
		}
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		add(Unit{
			ID: id, Kind: KindTPCall, Name: name,
			SourceFile: opts.Main.Path, SourceLines: lineSpan(tp.StartLine, tp.EndLine),
			TargetPath: m.ImportPath("controller") + "/tpcall_placeholders.go",
			TemplateID: "tpcall_placeholder", LLM: false,
			TokenEstimate: opts.Budget.Count(tpcallContract(tp)),
			Deps:          []string{ctrlIfaceID},
			TP:            tp,
		})
	}

	handlerIDs := []string{}
	for _, e := range m.Endpoints {
		id := fmt.Sprintf("u%02d", len(p.Units)+1)
		add(Unit{
			ID: id, Kind: KindHandlerMethod, Name: e.Name,
			TargetPath: m.ImportPath("handler") + "/" + m.Service + ".go",
			TemplateID: "handler_method", LLM: false,
			Deps: []string{ctrlIfaceID},
		})
		handlerIDs = append(handlerIDs, id)
	}
	handlerIfaceID := fmt.Sprintf("u%02d", len(p.Units)+1)
	add(Unit{
		ID: handlerIfaceID, Kind: KindHandlerInterface, Name: interfaceName(m.Service) + "Handler",
		TargetPath: m.ImportPath("handler") + "/interface.go",
		TemplateID: "handler_interface_file",
		Deps:       handlerIDs,
	})
	add(Unit{
		ID: fmt.Sprintf("u%02d", len(p.Units)+1), Kind: KindRouter, Name: m.RouteGroup,
		TargetPath: m.ImportPath("handler") + "/router_snippet.txt",
		TemplateID: "router_snippet",
		Deps:       []string{handlerIfaceID},
	})
	add(Unit{
		ID: fmt.Sprintf("u%02d", len(p.Units)+1), Kind: KindMocks, Name: "mockgen " + interfaceName(m.Service) + "Store/" + interfaceName(m.Service) + "Controller",
		TargetPath: m.ImportPath("db") + "/mock_store.go",
		TemplateID: "(mockgen)", LLM: false,
		Deps: []string{ifaceID, ctrlIfaceID},
	})

	return p, nil
}

// methodName resolves a DB method name: the mapping pin when present, else
// the deterministic fallback (cursor/table-derived, §3 of plan-conversion).
func methodName(m *Mapping, q *ir.Query) string {
	if pin, ok := m.DBMethods[q.ID]; ok && pin.Name != "" {
		return pin.Name
	}
	if q.CursorName != "" {
		return "Get" + camel(strings.TrimPrefix(q.CursorName, "cur_"))
	}
	table := "Row"
	if len(q.Tables) > 0 && q.Tables[0] != "" {
		table = q.Tables[0]
	}
	switch q.Type {
	case ir.QueryInsert:
		return "Insert" + camel(table)
	case ir.QueryUpdate:
		return "Update" + camel(table)
	case ir.QueryDelete:
		return "Delete" + camel(table)
	case ir.QueryMerge:
		return "Merge" + camel(table)
	default:
		return "Get" + camel(table)
	}
}

// camel turns snake/dotted segments into exported CamelCase (DEMO_ACC_X → DemoAccX).
func camel(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '.' || r == ' ' })
	var sb strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(strings.ToLower(p))
		sb.WriteRune([]rune(strings.ToUpper(string(r[0])))[0])
		sb.WriteString(string(r[1:]))
	}
	return sb.String()
}

func interfaceName(service string) string {
	r := []rune(service)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// queriesIn derives a condition's query IDs by line overlap when the
// inventory's explicit references are absent.
func queriesIn(f *ir.File, c *ir.Condition) []string {
	var ids []string
	for _, q := range f.Queries {
		if q.StartLine >= c.StartLine && q.StartLine <= c.EndLine {
			ids = append(ids, q.ID)
		}
	}
	return ids
}

func callsiteIn(sites []int, c *ir.Condition) bool {
	for _, s := range sites {
		if s >= c.StartLine && s <= c.EndLine {
			return true
		}
	}
	return false
}

func lineSpan(from, to int) string {
	if from == to {
		return fmt.Sprintf("%d", from)
	}
	return fmt.Sprintf("%d-%d", from, to)
}

// unbalancedSummary renders the IR's unbalanced regions as a compact
// "kind@line" list for error messages (severity F3).
func unbalancedSummary(u []ir.Unbalanced) string {
	parts := make([]string, 0, len(u))
	for _, r := range u {
		parts = append(parts, fmt.Sprintf("%s@%d", r.Kind, r.Line))
	}
	return strings.Join(parts, ", ")
}

// branchSource slices the 1-based inclusive line range out of src.
func branchSource(src string, from, to int) string {
	if src == "" {
		return ""
	}
	lines := strings.Split(src, "\n")
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return ""
	}
	return strings.Join(lines[from-1:to], "\n")
}

// modelsRepr is the deterministic token-estimate input for the models unit:
// the FML field inventory plus every row shape.
func modelsRepr(f *ir.File) string {
	var sb strings.Builder
	for _, op := range f.FmlOps {
		if op.Dropped {
			continue
		}
		fmt.Fprintf(&sb, "%s %s %s\n", op.Kind, op.Field, op.Target)
	}
	for _, c := range f.Conditions {
		for _, op := range c.FmlOps {
			if op.Dropped {
				continue
			}
			fmt.Fprintf(&sb, "%s %s %s\n", op.Kind, op.Field, op.Target)
		}
	}
	for _, q := range f.UniqueQueries() {
		sb.WriteString(strings.Join(q.RowShape, " "))
		sb.WriteString("\n")
	}
	return sb.String()
}

// definedInOf finds the source file backing a namespaced fn-query ID.
func definedInOf(opts Options, nsID string) string {
	fn := strings.SplitN(nsID, ":", 2)[0]
	for _, ext := range opts.Main.ExternalFns {
		if ext.Name == fn && ext.DefinedIn != "" {
			return ext.DefinedIn
		}
	}
	return ""
}

// SortUnits keeps plan output stable regardless of builder internals.
func SortUnits(units []Unit) {
	sort.SliceStable(units, func(i, j int) bool { return units[i].ID < units[j].ID })
}
