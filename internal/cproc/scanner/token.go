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
	StartCol   int
	EndLine    int
	CursorName string // populated for DECLARE CURSOR, OPEN, FETCH, CLOSE
	Func       string // enclosing function name ("" when outside any body)
}

// FunctionDef records a function definition at file scope. Body extent is
// brace-matched; consumers may still derive it (next definition's StartLine)
// when the body was not closed.
type FunctionDef struct {
	Name          string
	ReturnType    string
	StartLine     int
	Col           int // column of the definition's name
	BodyStartLine int // line of the opening brace (0 when unresolved)
	BodyEndLine   int // line of the matching closing brace (0 when unresolved)
}

// FunctionCall records an invocation of a function in live code.
type FunctionCall struct {
	Name      string
	Line      int
	Col       int
	Args      string // raw text between the call's parens ("" when unbalanced)
	IsTpCall  bool
	IsFnPref  bool // starts with "fn_"
	IsChkPref bool // starts with "chk_"
	Func      string
}

// Directive records a preprocessor directive (#include, #define).
type Directive struct {
	Kind     string // "include", "define", etc.
	Arg      string // e.g. <atmi.h> or "table/demo_price.h"
	Line     int
	IsHeader bool
	IsSystem bool // angle brackets <...>
}

// BranchKind names the if/else chain role of a branch record.
type BranchKind string

const (
	BranchIf     BranchKind = "if"
	BranchElseIf BranchKind = "elseif"
	BranchElse   BranchKind = "else"
)

// Branch records one if/else-if/else header and its block extent at any
// nesting depth. Top-level chains of the entry function are reconstructed by
// consumers (the IR condition inventory).
type Branch struct {
	Kind       BranchKind
	Cond       string // normalized condition text ("" for else)
	StartLine  int    // line of the if/else keyword
	StartCol   int    // column of the if/else keyword
	BlockStart int    // line of the block's opening brace (0 when unbraced)
	BlockEnd   int    // line of the block's closing brace (0 when unbraced)
	Depth      int    // brace depth at the keyword (function body top level == 1)
	Function   string // enclosing function name ("" when outside any body)
}

// VarDecl records a variable declaration whose base type is one of the
// recognized C/Pro*C base types (char, int, long, short, double, float,
// varchar, …). Types from project headers (e.g. EXEC SQL include table/*.h)
// never appear as declarations and stay untyped in the IR.
type VarDecl struct {
	Type  string
	Name  string
	Line  int
	Col   int // column of the declarator name
	Array bool
	Func  string // enclosing function name ("" for file-scope/params)
}

// CommentKind classifies a recorded comment span (PF-1.1).
type CommentKind string

const (
	// CommentBlock is an ordinary /* … */ block comment.
	CommentBlock CommentKind = "block"
	// CommentLine is a // line comment (ends at the newline).
	CommentLine CommentKind = "line"
	// CommentBanner is a version-marker comment ("Ver X.Y added here",
	// "Ver X.Y comment ends", …) of the project's banner convention.
	CommentBanner CommentKind = "banner"
)

// Comment records one comment span as a first-class scanner fact (PF-1.1).
// Live marks a banner comment that delimits a live code region: single-line
// version markers sit next to live code; the multi-line "commented … comment
// ends" variant wraps dead content, so Live is false there. Comment content
// is opaque — no brace/paren/string inside a span ever affects nesting.
type Comment struct {
	Kind      CommentKind
	StartLine int
	StartCol  int
	EndLine   int
	EndCol    int
	Live      bool
}

// UnbalancedRegion records a construct the scanner could not close
// (PF-1.4): an unterminated block comment, an EXEC SQL block with no
// terminating semicolon, or an unbalanced brace — loud facts, never a
// silently truncated parse.
type UnbalancedRegion struct {
	Kind      string // "block_comment" | "exec_sql" | "braces"
	StartLine int
	StartCol  int
}

// InComment reports whether the 1-based line/col position falls inside any
// recorded comment span (PF-1.3) — the queryable "is this position code?"
// check consumers use instead of re-scanning.
func (f *SourceFacts) InComment(line, col int) bool {
	for _, c := range f.Comments {
		if c.StartLine == line && c.EndLine == line {
			if col >= c.StartCol && col <= c.EndCol {
				return true
			}
			continue
		}
		if line == c.StartLine && col >= c.StartCol {
			return true
		}
		if line == c.EndLine && col <= c.EndCol {
			return true
		}
		if line > c.StartLine && line < c.EndLine {
			return true
		}
	}
	return false
}

// SourceFacts represents all structural facts extracted from a Pro*C file.
// Fragment marks a ScanFragment synthesis (PF-3.2): the facts come from a
// lone code block wrapped as the pseudo-function __fragment, with every
// line number rebased to the original fragment file.
type SourceFacts struct {
	Path        string
	NumLines    int
	Directives  []Directive
	Functions   []FunctionDef
	Calls       []FunctionCall
	AllSQL      []ExecSQLStatement
	Queries     []ExecSQLStatement
	Branches    []Branch
	VarDecls    []VarDecl
	Comments    []Comment
	Unbalanced  []UnbalancedRegion
	TpCallCount int
	Fragment    bool
}
