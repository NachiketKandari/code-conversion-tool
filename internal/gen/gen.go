// Package gen is the deterministic generation core (PRD §4.2, plan-conversion
// §5.4): plan + IR + templates → real Go code. Models, DB methods, and every
// interface/handler/router artifact render from templates with zero LLM
// calls — the SQL is already known and the shapes are fixed by the FML ops.
// The LLM fills exactly one gap per endpoint: the controller body (Part 2b).
package gen

import (
	"fmt"
	"go/format"
	"path/filepath"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/cproc/flow"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/plan"
	"github.com/Public/convert-tux-to-go/internal/templates"
)

// Options carries the generation inputs: the plan (units, naming pins), the
// main IR, and the fn-file IRs backing external-fn units.
type Options struct {
	Plan    *plan.Plan
	Main    *ir.File
	FnFiles []*ir.File
	// Source is the entry file's text — required only when the mapping
	// references discovery candidates (conditionRef): their conditions are
	// re-derived from the flow tree (PRD-2026-09-10 endpoint discovery).
	Source string
	// WithGorm renders the store with the legacy *gorm.DB handle alongside
	// sqlx (db.withGorm); default is the plain sqlx-only store.
	WithGorm bool
}

// Service bundles the derived naming context shared by all generators.
type Service struct {
	Mapping     *plan.Mapping
	Main        *ir.File
	ModelsPkg   string               // mutual-fund-be/pkg/services/nav/models
	Module      string               // mutual-fund-be
	If          string               // Nav — exported service name for interface names
	WithGorm    bool                 // store carries the legacy gorm handle (db.withGorm)
	structLower string               // navController / navHandler receiver base
	queries     map[string]*ir.Query // namespaced ID → query (main + fn files)
	hostVars    map[string]ir.HostVar
	source      string     // entry source text (conditionRef resolution)
	flowTree    *flow.Tree // lazily built when a conditionRef endpoint appears
}

// NewService derives the naming context from the plan and IR.
//
// Query indexing (severity F1): raw IDs belong to the main file alone.
// Referenced fn files are namespaced `fn:<name>:<qID>` — exactly the plan's
// pin namespace — and every other FnFile is scoped under its own base name,
// so an unreferenced file's `q1` can never silently overwrite the main's
// `q1`. Cross-file definitions sharing one ID with different SQL are a hard
// error, never a walk-order-dependent overwrite.
func NewService(o Options) (*Service, error) {
	if o.Plan == nil || o.Main == nil {
		return nil, fmt.Errorf("gen: plan and main IR are required")
	}
	s := &Service{Mapping: o.Plan.Mapping, Main: o.Main, WithGorm: o.WithGorm, source: o.Source}
	s.ModelsPkg = s.Mapping.ImportPath("models")
	s.Module = strings.SplitN(s.Mapping.Module, "/", 2)[0]
	s.If = exportName(s.Mapping.Service)
	s.structLower = s.Mapping.Service
	s.queries = make(map[string]*ir.Query, len(o.Main.Queries))
	s.hostVars = make(map[string]ir.HostVar, len(o.Main.HostVars))
	origin := map[string]string{} // query id → defining file path
	index := func(id string, q *ir.Query, path string) error {
		if prev, dup := s.queries[id]; dup {
			if prev.SQL == q.SQL {
				return nil // same SQL under one id — benign re-index
			}
			return fmt.Errorf("gen: query %q defined by both %s and %s with different SQL — cross-file collision (severity F1 guard)", id, origin[id], path)
		}
		s.queries[id] = q
		origin[id] = path
		return nil
	}
	for _, q := range o.Main.Queries {
		if err := index(q.ID, q, o.Main.Path); err != nil {
			return nil, err
		}
	}

	fnIRByPath := make(map[string]*ir.File, len(o.FnFiles))
	handled := map[string]bool{o.Main.Path: true}
	for _, f := range o.FnFiles {
		fnIRByPath[f.Path] = f
	}
	for _, ext := range o.Main.ExternalFns {
		if strings.HasPrefix(ext.Name, "chk_") {
			continue // dropped construct at plan time (§4.8.4.1)
		}
		f := fnIRByPath[ext.DefinedIn]
		if f == nil {
			continue // unresolved fn — plan already blocks its endpoints
		}
		handled[f.Path] = true
		for _, q := range f.Queries {
			if err := index(ext.Name+":"+q.ID, q, f.Path); err != nil {
				return nil, err
			}
		}
		for _, hv := range f.HostVars {
			s.hostVars[hv.Name] = hv
		}
	}
	for _, f := range o.FnFiles {
		if handled[f.Path] {
			continue
		}
		// Unreferenced file — scoped, inert, still collision-guarded.
		base := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		for _, q := range f.Queries {
			if err := index(base+":"+q.ID, q, f.Path); err != nil {
				return nil, err
			}
		}
	}
	for _, hv := range o.Main.HostVars {
		s.hostVars[hv.Name] = hv
	}
	return s, nil
}

// Query resolves a namespaced query ID: main-file IDs as-is, referenced fn
// queries as "<fn>:<id>", other files' queries as "<file base>:<id>".
func (s *Service) Query(id string) *ir.Query { return s.queries[id] }

// Pin returns the user's method pin for a query ID, if any.
func (s *Service) Pin(queryID string) (plan.MethodPin, bool) {
	pin, ok := s.Mapping.DBMethods[queryID]
	return pin, ok
}

func exportName(service string) string {
	r := []rune(service)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// lowerFirst renders navController/navHandler-style receiver type names.
func lowerFirst(s string) string {
	r := []rune(s)
	return strings.ToLower(string(r[0])) + string(r[1:])
}

// fieldFromFML derives a Go field name from an FML field name:
// FML_COMP_CD → CompCd (deterministic; reference prettiness is a §4.8.5 note).
func fieldFromFML(fml string) string {
	return exportName(camelLower(strings.ToLower(strings.TrimPrefix(fml, "FML_"))))
}

// camelLower renders snake_case host var names as lowerCamelCase Go names.
func camelLower(s string) string {
	parts := strings.Split(s, "_")
	var sb strings.Builder
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i == 0 {
			sb.WriteString(p)
			continue
		}
		r := []rune(p)
		sb.WriteRune([]rune(strings.ToUpper(string(r[0])))[0])
		sb.WriteString(string(r[1:]))
	}
	return sb.String()
}

// requestType / responseType / rowType naming conventions.
func (s *Service) requestType(endpoint string) string  { return endpoint + "Request" }
func (s *Service) responseType(endpoint string) string { return endpoint + "Response" }

// RowName derives the models struct carrying one query's result row: the
// mapping pin's Row when set, else the method name minus its verb. The verb
// list mirrors plan.methodName's emitted prefixes (Get/Insert/Update/Delete/
// Merge — A2.6 added Merge, which plan emits for unpinned MERGE units).
func (s *Service) RowName(queryID, methodName string) string {
	if pin, ok := s.Pin(queryID); ok && pin.Row != "" {
		return pin.Row
	}
	for _, verb := range []string{"Get", "Insert", "Update", "Delete", "Merge"} {
		if strings.HasPrefix(methodName, verb) {
			return strings.TrimPrefix(methodName, verb)
		}
	}
	return methodName
}

// rowFields derives the row struct's fields from the FETCH-INTO host vars.
// The db tag is the SELECT alias when the source SQL carries one
// (`Query.Aliases`, position-aligned); otherwise the Pro*C table-header
// convention names host vars after their columns (`sql_demo_comp_cd` →
// DEMO_COMP_CD), which is exactly what Oracle returns for unaliased
// selects — so the tag is the uppercased, sql_-stripped host var.
// Field formats are uniformly sql.NullString.
func (s *Service) rowFields(q *ir.Query) ([]templates.FieldSpec, error) {
	fields := make([]templates.FieldSpec, 0, len(q.RowShape))
	for i, hvName := range q.RowShape {
		spec := templates.FieldSpec{}
		if i < len(q.Aliases) {
			alias := q.Aliases[i]
			spec.DBTag = alias
			spec.Name = exportName(camelLower(strings.ToLower(alias)))
		} else {
			spec.DBTag = strings.ToUpper(strings.TrimPrefix(hvName, "sql_"))
			spec.Name = exportName(camelLower(strings.TrimPrefix(hvName, "sql_")))
		}
		spec.Type = s.hostType(hvName, spec.DBTag)
		fields = append(fields, spec)
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("gen: query %s has a row shape but no FETCH-INTO fields", q.ID)
	}
	return fields, nil
}

// hostType is the uniform field-format rule (user directive, 2026-09-07):
// every DB-backed model field is sql.NullString — no per-type guessing
// (NullTime/int64 derivations removed). Conversions happen in the controller
// layer where the business logic lives. The declared C type stays available
// in the IR for the controller prompt.
func (s *Service) hostType(hostVar, dbTag string) string {
	return "sql.NullString"
}

// requestFields / responseFields derive the endpoint's contract structs from
// the branch's FML ops (decision 10: Fget32 = inputs, Fadd32 = outputs;
// dropped session/error plumbing never reaches models, §4.8.4). Error ops
// (PRD-2026-09-10 discovery) are error emissions — "fadd err = returning
// error" — never contract fields.
func contractFields(ops []ir.FmlOp, kind ir.FmlOpKind) []templates.FieldSpec {
	fields := make([]templates.FieldSpec, 0, len(ops))
	seen := map[string]bool{}
	for _, op := range ops {
		if op.Dropped || op.Error || op.Kind != kind || seen[op.Field] {
			continue
		}
		seen[op.Field] = true
		spec := templates.FieldSpec{
			Name:    fieldFromFML(op.Field),
			Type:    "string",
			JSONTag: op.Field,
		}
		if kind == ir.FmlGet {
			if !op.Optional {
				spec.Binding = "required"
			}
		} else {
			spec.OmitEmpty = true
		}
		fields = append(fields, spec)
	}
	return fields
}

// ModelFile derives the models.go content: per-endpoint request/response
// structs plus every DB method's row struct.
func (s *Service) ModelFile(p *plan.Plan) (string, error) {
	data := templates.ModelFileData{Package: "models"}
	endpoints := map[string]bool{}
	for _, e := range s.Mapping.Endpoints {
		endpoints[e.Name] = true
	}
	for _, e := range s.Mapping.Endpoints {
		c := s.conditionOf(e)
		if c == nil {
			continue
		}
		data.Structs = append(data.Structs, templates.StructSpec{
			Name:   s.requestType(e.Name),
			Fields: contractFields(c.FmlOps, ir.FmlGet),
		})
		data.Structs = append(data.Structs, templates.StructSpec{
			Name:   s.responseType(e.Name),
			Fields: contractFields(c.FmlOps, ir.FmlAdd),
		})
	}
	for _, u := range p.Units {
		if u.Kind != plan.KindDBMethod || len(u.QueryIDs) == 0 {
			continue
		}
		q := s.Query(u.QueryIDs[0])
		if q == nil || q.Type != ir.QuerySelectMulti {
			continue
		}
		fields, err := s.rowFields(q)
		if err != nil {
			return "", err
		}
		data.Structs = append(data.Structs, templates.StructSpec{Name: s.RowName(u.QueryIDs[0], u.Name), Fields: fields})
	}
	// Single-row SELECT units produce a row struct too (non-COUNT singles).
	for _, u := range p.Units {
		if u.Kind != plan.KindDBMethod || len(u.QueryIDs) == 0 {
			continue
		}
		q := s.Query(u.QueryIDs[0])
		if q == nil || q.Type != ir.QuerySelectSingle || isCountQuery(q) {
			continue
		}
		fields, err := s.rowFields(q)
		if err != nil {
			// Host vars declared in Pro*C table headers may leave the row
			// shape unnamed; the pipeline flags it instead of guessing.
			return "", fmt.Errorf("gen: %w", err)
		}
		data.Structs = append(data.Structs, templates.StructSpec{Name: s.RowName(u.QueryIDs[0], u.Name), Fields: fields})
	}
	return render(templates.ModelFile, data)
}

func (s *Service) conditionOf(e plan.Endpoint) *ir.Condition {
	if e.ConditionRef != "" {
		// Discovery candidate (PRD-2026-09-10): re-derive the synthesized
		// condition from the flow tree (the shared flow.TreeFor derivation).
		// Degrade to nil (caller skips the endpoint's structs) on any
		// failure — the plan already validated the ref, so this is
		// defensive only.
		if s.flowTree == nil {
			if strings.TrimSpace(s.source) == "" {
				return nil
			}
			t, err := flow.TreeFor(s.source, s.Main)
			if err != nil {
				return nil
			}
			s.flowTree = t
		}
		c, err := flow.ConditionFor(s.flowTree, s.Main.Conditions, e.ConditionRef)
		if err != nil {
			return nil
		}
		return c
	}
	return s.Main.Condition(e.Condition)
}

// isCountQuery reports whether a SELECT single's select list is exactly a
// scalar COUNT — DECODE(COUNT(*),…) wrappers return strings, not counts.
func isCountQuery(q *ir.Query) bool {
	head := strings.TrimSpace(q.SQL)
	head = strings.TrimPrefix(strings.TrimPrefix(head, "SELECT"), "select")
	return strings.HasPrefix(strings.TrimSpace(head), "COUNT(")
}

// DBMethod derives one store method: signature, rendered body, and the
// interface signature line — all from the query descriptor + pin (zero LLM).
func (s *Service) DBMethod(u plan.Unit) (body, signature string, needsSQL bool, err error) {
	if len(u.QueryIDs) == 0 {
		return "", "", false, fmt.Errorf("gen: db unit %s has no query", u.ID)
	}
	q := s.Query(u.QueryIDs[0])
	if q == nil {
		return "", "", false, fmt.Errorf("gen: db unit %s references unknown query %q", u.ID, u.QueryIDs[0])
	}
	pin, _ := s.Pin(u.QueryIDs[0])
	errOnly := false

	params, err := s.dbParams(q, pin)
	if err != nil {
		return "", "", false, err
	}
	d := templates.DBMethodData{
		Receiver:  "g",
		StoreType: "store",
		Name:      u.Name,
		CtxName:   "c",
		Params:    params,
		Query:     strings.TrimRight(q.SQL, " \t\n;"),
	}
	needsSQL = q.Type == ir.QuerySelectSingle

	switch q.Type {
	case ir.QuerySelectMulti:
		row := s.RowName(u.QueryIDs[0], u.Name)
		d.Multi, d.RowType = true, "models."+row
		d.VarName = lowerFirst(row)
		if !strings.HasSuffix(d.VarName, "s") {
			d.VarName += "s"
		}
	case ir.QuerySelectSingle:
		if isCountQuery(q) {
			d.Scalar, d.VarName = "int64", "count"
		} else {
			row := s.RowName(u.QueryIDs[0], u.Name)
			d.RowType, d.VarName = "models."+row, lowerFirst(row)
		}
	case ir.QueryInsert, ir.QueryUpdate, ir.QueryDelete, ir.QueryMerge:
		// DML contract (severity F2): INSERT/UPDATE/DELETE render through
		// the tx-variant templates (explicit `tx *sqlx.Tx` per decision 27 —
		// in-branch DML is transactional; standalone DML never forms a unit
		// under the current mapping model), MERGE through db_method_merge.
		// All four are error-only: ExecContext + RowsAffected (the delete
		// variant logs and returns without a RowsAffected check).
		errOnly = true
		d.TxParam = q.Type != ir.QueryMerge
		d.SuccessMsg = u.Name + " executed successfully"
	default:
		return "", "", false, fmt.Errorf("gen: query %s type %s not supported by the deterministic db generator yet", q.ID, q.Type)
	}

	// Template resolution honors the IR's per-type choice (q.TemplateID is
	// passed through the plan as u.TemplateID); when the plan's ID is absent
	// or disagrees with the query type, the type's canonical template wins —
	// never a cross-type fallback.
	tmpl := templates.ID(u.TemplateID)
	if want := templates.ID(q.Type.TemplateID()); want != "" && tmpl != want {
		tmpl = want
	}
	if tmpl == "" {
		tmpl = templates.DBMethodSelectMulti
	}
	body, err = render(tmpl, d)
	if err != nil {
		return "", "", false, err
	}
	sig := u.Name + "(c context.Context"
	if d.TxParam {
		sig += ", tx *sqlx.Tx"
	}
	for _, p := range params {
		sig += ", " + p.Name + " " + p.Type
	}
	if errOnly {
		sig += ") error"
	} else {
		sig += ") (" + d.ReturnType() + ", error)"
	}
	return body, sig, needsSQL, nil
}

// dbParams derives the method parameters from the query's binds: pinned
// "name:Type" entries win; the fallback is a lowerCamel name and the uniform
// string type (FML values arrive as strings; Oracle coerces — the C decl's
// long/varchar distinction is procedural, not semantic here).
func (s *Service) dbParams(q *ir.Query, pin plan.MethodPin) ([]templates.ParamSpec, error) {
	specs := make([]templates.ParamSpec, 0, len(q.Binds))
	for i, bind := range q.Binds {
		spec := templates.ParamSpec{Name: camelLower(bind), Type: "string"}
		if i < len(pin.Params) {
			name, typ, hasType := strings.Cut(pin.Params[i], ":")
			if name != "" {
				spec.Name = name
			}
			if hasType && typ != "" {
				spec.Type = typ
			}
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func hasTimeParam(params []templates.ParamSpec) bool {
	for _, p := range params {
		if p.Type == "time.Time" {
			return true
		}
	}
	return false
}

// render executes one embedded template. Go artifacts (anything starting
// with a package clause) are normalized with go/format so generated files
// pass the Tier-A gofmt check byte-for-byte; the router snippet (not Go)
// passes through untouched.
func render(id templates.ID, data any) (string, error) {
	out, err := templates.NewEmbeddedProvider().Render(id, data)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(out, "package ") {
		formatted, ferr := format.Source([]byte(out))
		if ferr != nil {
			return "", fmt.Errorf("gen: %s output does not parse: %w", id, ferr)
		}
		out = string(formatted)
	}
	return out, nil
}
