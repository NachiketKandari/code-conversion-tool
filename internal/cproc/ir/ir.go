// Package ir builds the deterministic Pro*C/Tuxedo intermediate
// representation (PRD Phase 2, architecture.md §3): query units with
// QueryType + template marking, the entry function's endpoint condition
// inventory, FML input/output ops with buffer roles, host variables,
// external-fn references, and correlated tpcall sites. Extraction is 100%
// tool work — no LLM involvement.
package ir

import (
	"strings"

	"github.com/Public/convert-tux-to-go/internal/cproc/pred"
)

// QueryType is the deterministic classification of one logical query unit
// (PRD §4.2.2: SELECT single-value / SELECT multi-row / INSERT / UPDATE /
// DELETE / MERGE; flattened cursors are SELECT multi-row).
type QueryType string

const (
	QuerySelectSingle QueryType = "SELECT_SINGLE"
	QuerySelectMulti  QueryType = "SELECT_MULTI"
	QueryInsert       QueryType = "INSERT"
	QueryUpdate       QueryType = "UPDATE"
	QueryDelete       QueryType = "DELETE"
	QueryMerge        QueryType = "MERGE"
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
	TemplateMerge        = "db_method_merge"
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
	case QueryMerge:
		return TemplateMerge
	default:
		return ""
	}
}

// IsDML reports whether the type is a data-modification statement (INSERT,
// UPDATE, DELETE, MERGE) — the tx-variant family. One vocabulary for the
// batch rubric, the plan, and the generators (A2.5).
func (q QueryType) IsDML() bool {
	switch q {
	case QueryInsert, QueryUpdate, QueryDelete, QueryMerge:
		return true
	}
	return false
}

// IsErrField reports whether an FML field is the project's error-emission
// convention (any field whose name carries "ERR") — the one home of the
// rule the flow matchers and the discovery census re-derived four times.
func IsErrField(field string) bool {
	return strings.Contains(strings.ToUpper(field), "ERR")
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
// (§4.8.4: FML_USER_ID/FML_SESSION_ID handled by middleware, FML_ERR_MSG becomes
// the returned error). Buffer names the buffer variable the op targeted
// (PF-4.2), resolvable against File.Buffers for its role. Error marks an
// error-emission add (PRD-2026-09-10 endpoint discovery): the op writes an
// ERR field or targets the input/send buffer — "fadd err = returning error".
// Only flow-discovered candidate conditions set it (DIS-D1); inventory
// conditions keep the flag unset so existing outputs never change.
type FmlOp struct {
	Kind     FmlOpKind `json:"kind"`
	Field    string    `json:"field"`
	Target   string    `json:"target,omitempty"`
	Buffer   string    `json:"buffer,omitempty"`
	Line     int       `json:"line"`
	Optional bool      `json:"optional,omitempty"`
	Dropped  bool      `json:"dropped,omitempty"`
	Error    bool      `json:"error,omitempty"`
}

// FmlBufferRole names the data-flow role of an FML buffer variable
// (PF-4.1): input (Ibuffer — the endpoint's request), output (Obuffer — the
// response), send/recv (the two buffers of a tpcall), or unknown-role when
// the naming convention does not recognize the variable (a visible fact,
// never a guess).
type FmlBufferRole string

const (
	BufferInput  FmlBufferRole = "input"
	BufferOutput FmlBufferRole = "output"
	BufferSend   FmlBufferRole = "send"
	BufferRecv   FmlBufferRole = "recv"
	// BufferUnknown marks a buffer variable outside the recognized naming
	// convention — recorded, never guessed.
	BufferUnknown FmlBufferRole = "unknown-role"
)

// BufferRole is one FML buffer variable's recorded convention role (PF-4.1).
type BufferRole struct {
	Name string        `json:"name"`
	Role FmlBufferRole `json:"role"`
}

// TPCall is one correlated tpcall site (PF-4.3): the outbound service name,
// the FML contract built from the surrounding block — Fadd32 ops into the
// send buffer before the call, Fget32 ops from the receive buffer after —
// and the site extent. Ambiguous marks a site whose send/recv buffer
// variables could not be identified (contracts degrade to empty, visibly).
type TPCall struct {
	Service    string  `json:"service"`
	SendBuffer string  `json:"send_buffer,omitempty"`
	RecvBuffer string  `json:"recv_buffer,omitempty"`
	SendFML    []FmlOp `json:"send_fml,omitempty"`
	RecvFML    []FmlOp `json:"recv_fml,omitempty"`
	StartLine  int     `json:"start_line"`
	EndLine    int     `json:"end_line"`
	Function   string  `json:"function,omitempty"`
	Ambiguous  bool    `json:"ambiguous,omitempty"`
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
// default/else branch. Predicate is the parsed condition tree (PF-2.3) —
// the raw Expr stays the audit trail, the tree is the query surface.
type Condition struct {
	Index     int        `json:"index"`
	Kind      string     `json:"kind"`
	Expr      string     `json:"expr,omitempty"`
	Predicate *pred.Expr `json:"predicate,omitempty"`
	FlagVars  []string   `json:"flag_vars,omitempty"`
	StartLine int        `json:"start_line"`
	EndLine   int        `json:"end_line"`
	FmlOps    []FmlOp    `json:"fml_ops,omitempty"`
	QueryIDs  []string   `json:"query_ids,omitempty"`
	IsDefault bool       `json:"is_default,omitempty"`
}

// ContainsLine reports whether the 1-based line falls inside the
// condition's inclusive source span — the one ownership predicate for
// associating queries, calls, and tpcall sites with a condition (A2.5).
func (c *Condition) ContainsLine(line int) bool {
	return line >= c.StartLine && line <= c.EndLine
}

// Condition returns the inventory condition with the given 1-based index,
// or nil. The one lookup for plan (map building) and gen (linear scans).
func (f *File) Condition(index int) *Condition {
	for i := range f.Conditions {
		if f.Conditions[i].Index == index {
			return &f.Conditions[i]
		}
	}
	return nil
}

// SameCursor compares two cursor names under the house convention: the
// scanner uppercases cursor names while the IR keeps raw casing, so all
// consumers fold case. The one comparison rule (A2.5).
func SameCursor(a, b string) bool {
	return strings.EqualFold(a, b)
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

// File is the IR of one scanned .pc/.pcf file. Fragment marks a lone code
// block converted under the standard rubric (PF-3): Entry is the synthesized
// pseudo-function __fragment and every line number is the fragment file's
// own. Buffers records the FML buffer-role facts (PF-4.1); TPCalls records
// the correlated outbound-service call sites (PF-4.3). BranchCount counts
// the file's if/else-if headers (else never contributes) and
// BranchingFactor is the doubling-weighted total: each header contributes
// +1 × 2^(number of enclosing if/else-if blocks); loops and else bodies do
// not nest (documented approximation for unbraced parents).
type File struct {
	Path            string       `json:"path"`
	Entry           string       `json:"entry,omitempty"`
	Fragment        bool         `json:"fragment,omitempty"`
	Functions       []string     `json:"functions"`
	BranchCount     int          `json:"branch_count"`
	BranchingFactor int          `json:"branching_factor"`
	Conditions      []Condition  `json:"conditions,omitempty"`
	FmlOps          []FmlOp      `json:"fml_ops,omitempty"`
	Buffers         []BufferRole `json:"buffers,omitempty"`
	TPCalls         []TPCall     `json:"tpcalls,omitempty"`
	Queries         []*Query     `json:"queries"`
	HostVars        []HostVar    `json:"host_vars"`
	ExternalFns     []ExternalFn `json:"external_fns,omitempty"`
	Unbalanced      []Unbalanced `json:"unbalanced,omitempty"`
}

// Unbalanced marks a construct the scanner could not close (PF-1.4,
// severity F3): an unterminated block comment, an EXEC SQL block with no
// terminating semicolon, or an unbalanced brace. The parse continues
// leniently past such regions, so these facts must ride along on the IR —
// loud in every summary, never a silent truncation.
type Unbalanced struct {
	Kind string `json:"kind"` // block_comment | exec_sql | braces
	Line int    `json:"line"`
	Col  int    `json:"col"`
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
