package scanner

// SQLKind represents the structural classification of an EXEC SQL block.
type SQLKind int

const (
	SQLUnknown SQLKind = iota
	// Logical DB queries (PRD +1 complexity rubric)
	SQLSelect
	SQLDeclareCursor
	SQLInsert
	SQLUpdate
	SQLDelete

	// Cursor and Pro*C control operations (plumbing, not counted as separate queries)
	SQLOpen
	SQLFetch
	SQLClose
	SQLDeclareSection
	SQLInclude
	SQLCommit
	SQLRollback
	SQLConnect
	SQLOther
)

// IsQuery returns true if the SQLKind represents a logical database query.
func (k SQLKind) IsQuery() bool {
	switch k {
	case SQLSelect, SQLDeclareCursor, SQLInsert, SQLUpdate, SQLDelete:
		return true
	default:
		return false
	}
}

func (k SQLKind) String() string {
	switch k {
	case SQLSelect:
		return "SELECT"
	case SQLDeclareCursor:
		return "DECLARE_CURSOR"
	case SQLInsert:
		return "INSERT"
	case SQLUpdate:
		return "UPDATE"
	case SQLDelete:
		return "DELETE"
	case SQLOpen:
		return "OPEN"
	case SQLFetch:
		return "FETCH"
	case SQLClose:
		return "CLOSE"
	case SQLDeclareSection:
		return "DECLARE_SECTION"
	case SQLInclude:
		return "INCLUDE"
	case SQLCommit:
		return "COMMIT"
	case SQLRollback:
		return "ROLLBACK"
	case SQLConnect:
		return "CONNECT"
	default:
		return "OTHER"
	}
}

// ExecSQLStatement encapsulates an extracted EXEC SQL ... ; block.
type ExecSQLStatement struct {
	Raw        string
	Normalized string
	Kind       SQLKind
	StartLine  int
	EndLine    int
	CursorName string // populated for DECLARE CURSOR, OPEN, FETCH, CLOSE
}

// FunctionDef records a function definition at file scope. Body extent is
// derived by consumers (next definition's StartLine), not stored.
type FunctionDef struct {
	Name       string
	ReturnType string
	StartLine  int
}

// FunctionCall records an invocation of a function in live code.
type FunctionCall struct {
	Name      string
	Line      int
	Col       int
	IsTpCall  bool
	IsFnPref  bool // starts with "fn_"
	IsChkPref bool // starts with "chk_"
}

// Directive records a preprocessor directive (#include, #define).
type Directive struct {
	Kind     string // "include", "define", etc.
	Arg      string // e.g. <atmi.h> or "table/mf_navs.h"
	Line     int
	IsHeader bool
	IsSystem bool // angle brackets <...>
}

// SourceFacts represents all structural facts extracted from a Pro*C file.
type SourceFacts struct {
	Path        string
	Directives  []Directive
	Functions   []FunctionDef
	Calls       []FunctionCall
	AllSQL      []ExecSQLStatement
	Queries     []ExecSQLStatement
	TpCallCount int
}
