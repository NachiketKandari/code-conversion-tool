// Package ir builds the deterministic Pro*C/Tuxedo intermediate
// representation (PRD Phase 2, architecture.md §3): query units with
// QueryType + template marking, the entry function's endpoint condition
// inventory, FML input/output ops, host variables, and external-fn
// references. Extraction is 100% tool work — no LLM involvement.
package ir

// QueryType is the deterministic classification of one logical query unit
// (PRD §4.2.2: SELECT single-value / SELECT multi-row / INSERT / UPDATE /
// DELETE; flattened cursors are SELECT multi-row).
type QueryType string

const (
	QuerySelectSingle QueryType = "SELECT_SINGLE"
	QuerySelectMulti  QueryType = "SELECT_MULTI"
	QueryInsert       QueryType = "INSERT"
	QueryUpdate       QueryType = "UPDATE"
	QueryDelete       QueryType = "DELETE"
)

// Template IDs of the embedded v1 DB-method set (internal/templates) keyed
// by query type. Mirrored as plain strings to keep cproc a stdlib-only leaf;
// ir_test asserts the mirror against the real templates.ID constants so the
// two cannot drift. DML marks the tx variant per decision 27 — standalone
// DML downgrades to db_method_dml_plain at plan time.
const (
	TemplateSelectSingle = "db_method_select_single"
	TemplateSelectMulti  = "db_method_select_multi"
	TemplateInsertTx     = "db_method_insert_tx"
	TemplateUpdateTx     = "db_method_update_tx"
	TemplateDeleteTx     = "db_method_delete_tx"
)

// TemplateID maps the query type to its generation template.
func (q QueryType) TemplateID() string {
	switch q {
	case QuerySelectSingle:
		return TemplateSelectSingle
	case QuerySelectMulti:
		return TemplateSelectMulti
	case QueryInsert:
		return TemplateInsertTx
	case QueryUpdate:
		return TemplateUpdateTx
	case QueryDelete:
		return TemplateDeleteTx
	default:
		return ""
	}
}

// FmlOpKind separates API inputs from API outputs (decision 10: Fget32 =
// request body, Fadd32 = response body).
type FmlOpKind string

const (
	FmlGet FmlOpKind = "get" // Fget32 — API input
	FmlAdd FmlOpKind = "add" // Fadd32 — API output
)

// FmlOp records one FML buffer manipulation. Optional marks a field whose
// legacy read is guarded by FNOTPRES (defaults applied — PRD §4.8.3).
// Dropped marks session/error plumbing that never reaches generated models
// (§4.8.4: FML_USR_ID/FML_SSSN_ID handled by middleware, FML_ERR_MSG becomes
// the returned error).
type FmlOp struct {
	Kind     FmlOpKind `json:"kind"`
	Field    string    `json:"field"`
	Target   string    `json:"target,omitempty"`
	Line     int       `json:"line"`
	Optional bool      `json:"optional,omitempty"`
	Dropped  bool      `json:"dropped,omitempty"`
}

// HostVar is one host/bind variable referenced by queries or FML ops.
// CType comes from a scanned declaration; variables declared in Pro*C table
// headers (EXEC SQL include "table/*.h") are FromHeader with no type — the
// generation phase resolves them with the LLM's row-shape context.
type HostVar struct {
	Name             string `json:"name"`
	CType            string `json:"c_type,omitempty"`
	GoHint           string `json:"go_hint,omitempty"`
	Array            bool   `json:"array,omitempty"`
	Nullable         bool   `json:"nullable,omitempty"`
	FromHeader       bool   `json:"from_header,omitempty"`
	InDeclareSection bool   `json:"in_declare_section,omitempty"`
}

// Query is one logical query unit. Cursor units are flattened
// (DECLARE/OPEN/FETCH/CLOSE → one SELECT_MULTI unit, §4.8.2) and carry the
// FETCH-INTO list as RowShape. Aliases holds the SELECT list's `AS "X"`
// column aliases from the raw SQL, position-aligned with RowShape for row
// models (§4.8.2: the alias is the db tag). Duplicate units (identical SQL
// in different branches) stay in the IR — factual, one row per site —
// linked by DedupKey/DuplicateOf; the plan level collapses them to one DB
// method (§4.2.8.7).
type Query struct {
	ID              string    `json:"id"`
	Type            QueryType `json:"type"`
	TemplateID      string    `json:"template_id"`
	SQL             string    `json:"sql"`
	Aliases         []string  `json:"aliases,omitempty"`
	StartLine       int       `json:"start_line"`
	EndLine         int       `json:"end_line"`
	OwningFunction  string    `json:"owning_function"`
	CursorName      string    `json:"cursor_name,omitempty"`
	CursorFlattened bool      `json:"cursor_flattened,omitempty"`
	Tables          []string  `json:"tables"`
	Binds           []string  `json:"binds"`
	BindArity       int       `json:"bind_arity"`
	RowShape        []string  `json:"row_shape,omitempty"`
	OrderBy         string    `json:"order_by,omitempty"`
	Sites           []int     `json:"sites"`
	DedupKey        string    `json:"dedup_key"`
	DuplicateOf     string    `json:"duplicate_of,omitempty"`
	DefinedBy       string    `json:"defined_by,omitempty"`
}

// Condition is one endpoint candidate from the condition inventory (§4.2.8).
// Extraction emits the inventory for every run; only user-mapped conditions
// become endpoints. Kind is if | elseif | else; IsDefault marks the
// default/else branch.
type Condition struct {
	Index     int      `json:"index"`
	Kind      string   `json:"kind"`
	Expr      string   `json:"expr,omitempty"`
	FlagVars  []string `json:"flag_vars,omitempty"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	FmlOps    []FmlOp  `json:"fml_ops,omitempty"`
	QueryIDs  []string `json:"query_ids,omitempty"`
	IsDefault bool     `json:"is_default,omitempty"`
}

// ExternalFn is one called-but-not-defined project symbol (fn_*/chk_*).
// Resolved/DefinedIn are filled by corpus resolution (dir mode); QueryIDs
// point at the query units in the defining file owned by this fn (§4.2.9).
type ExternalFn struct {
	Name      string   `json:"name"`
	Resolved  bool     `json:"resolved,omitempty"`
	DefinedIn string   `json:"defined_in,omitempty"`
	HasSQL    bool     `json:"has_sql,omitempty"`
	Callsites []int    `json:"callsites"`
	QueryIDs  []string `json:"query_ids,omitempty"`
}

// File is the IR of one scanned .pc/.pcf file.
type File struct {
	Path        string       `json:"path"`
	Entry       string       `json:"entry,omitempty"`
	Functions   []string     `json:"functions"`
	Conditions  []Condition  `json:"conditions,omitempty"`
	FmlOps      []FmlOp      `json:"fml_ops,omitempty"`
	Queries     []*Query     `json:"queries"`
	HostVars    []HostVar    `json:"host_vars"`
	ExternalFns []ExternalFn `json:"external_fns,omitempty"`
}

// UniqueQueries returns the queries that survive duplicate collapsing
// (first unit of every DedupKey group), in IR order.
func (f *File) UniqueQueries() []*Query {
	out := make([]*Query, 0, len(f.Queries))
	for _, q := range f.Queries {
		if q.DuplicateOf == "" {
			out = append(out, q)
		}
	}
	return out
}
