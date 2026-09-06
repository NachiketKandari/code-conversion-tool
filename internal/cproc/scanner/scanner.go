package scanner

import (
	"bytes"
	"os"
	"strings"
	"unicode"
)

var cKeywordsWithParen = map[string]bool{
	"if":     true,
	"while":  true,
	"for":    true,
	"switch": true,
	"sizeof": true,
	"return": true,
}

// ScanFile parses a Pro*C file from disk and returns its SourceFacts.
func ScanFile(filePath string) (*SourceFacts, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	return ScanBytes(data, filePath)
}

// ScanBytes parses Pro*C source bytes into SourceFacts.
func ScanBytes(src []byte, path string) (*SourceFacts, error) {
	s := &scannerState{
		src:  src,
		path: path,
		len:  len(src),
		pos:  0,
		line: 1,
		col:  1,
	}

	// Tokenize and extract facts. Dead code is whatever sits inside standard C
	// comments (block or line); version banner comments ("Ver X.Y added here",
	// "Ver X.Y comment ends") delimit *live* regions and must not hide code.
	s.scan()

	return &SourceFacts{
		Path:        path,
		Directives:  s.directives,
		Functions:   s.functions,
		Calls:       s.calls,
		AllSQL:      s.allSQL,
		Queries:     s.queries,
		Branches:    s.branches,
		VarDecls:    s.varDecls,
		TpCallCount: s.tpcallCount,
	}, nil
}

type scannerState struct {
	src  []byte
	path string
	len  int
	pos  int
	line int
	col  int

	directives  []Directive
	functions   []FunctionDef
	calls       []FunctionCall
	allSQL      []ExecSQLStatement
	queries     []ExecSQLStatement
	branches    []Branch
	varDecls    []VarDecl
	tpcallCount int

	pendingFn   int    // index into functions awaiting its body brace (-1 none)
	curFn       int    // index of the function whose body we are inside (-1 none)
	pendingType string // base type seen, awaiting a declarator ("" none)
}

func (s *scannerState) fnName(idx int) string {
	if idx < 0 || idx >= len(s.functions) {
		return ""
	}
	return s.functions[idx].Name
}

func (s *scannerState) peek() byte {
	if s.pos < s.len {
		return s.src[s.pos]
	}
	return 0
}

func (s *scannerState) advance() byte {
	if s.pos >= s.len {
		return 0
	}
	b := s.src[s.pos]
	s.pos++
	if b == '\n' {
		s.line++
		s.col = 1
	} else {
		s.col++
	}
	return b
}

func (s *scannerState) skipWhitespace() {
	for s.pos < s.len && unicode.IsSpace(rune(s.src[s.pos])) {
		s.advance()
	}
}

func (s *scannerState) scan() {
	braceDepth := 0
	var prevIdent string

	for s.pos < s.len {
		// 1. Handle standard C comments.
		if s.pos+1 < s.len && s.src[s.pos] == '/' && s.src[s.pos+1] == '*' {
			s.skipBlockComment()
			continue
		}

		// 2. Handle C++ line comments.
		if s.pos+1 < s.len && s.src[s.pos] == '/' && s.src[s.pos+1] == '/' {
			s.skipLineComment()
			continue
		}

		// 3. Handle String Literals.
		if s.src[s.pos] == '"' {
			s.skipStringLiteral()
			prevIdent = ""
			continue
		}

		// 4. Handle Char Literals.
		if s.src[s.pos] == '\'' {
			s.skipCharLiteral()
			prevIdent = ""
			continue
		}

		// 5. Handle Preprocessor Directives.
		if s.src[s.pos] == '#' && s.isLineStart() {
			s.scanDirective()
			prevIdent = ""
			continue
		}

		// 6. Handle EXEC SQL block.
		if s.matchesExecSQL() {
			s.scanExecSQL()
			prevIdent = ""
			continue
		}

		// 7. Track braces.
		if s.src[s.pos] == '{' {
			if braceDepth == 0 && s.pendingFn >= 0 {
				// The function definition's body opens here.
				s.functions[s.pendingFn].BodyStartLine = s.line
				s.curFn = s.pendingFn
				s.pendingFn = -1
			}
			braceDepth++
			s.advance()
			s.pendingType = ""
			prevIdent = ""
			continue
		}
		if s.src[s.pos] == '}' {
			if braceDepth > 0 {
				braceDepth--
			}
			if braceDepth == 0 && s.curFn >= 0 {
				s.functions[s.curFn].BodyEndLine = s.line
				s.curFn = -1
			}
			s.advance()
			s.pendingType = ""
			prevIdent = ""
			continue
		}

		// 8. Handle Identifiers and Function Calls / Definitions.
		if isIdentStart(s.src[s.pos]) {
			startLine := s.line
			startCol := s.col
			ident := s.scanIdent()

			// Check if next non-whitespace token is '('
			savedPos := s.pos
			savedLine := s.line
			savedCol := s.col
			s.skipWhitespace()

			if ident == "if" {
				kind := BranchIf
				if prevIdent == "else" {
					kind = BranchElseIf
				}
				s.recordIfBranch(kind, startLine, braceDepth)
			} else if ident == "else" {
				s.recordElseBranch(startLine, braceDepth)
			} else if s.pos < s.len && s.src[s.pos] == '(' {
				// A call or a definition — either way any pending type
				// context is done (e.g. `long fn_x(...)` is a def).
				s.pendingType = ""
				if !cKeywordsWithParen[ident] {
					if braceDepth == 0 && isLikelyFuncDef(prevIdent, ident, s.src, s.pos) {
						s.functions = append(s.functions, FunctionDef{
							Name:       ident,
							ReturnType: prevIdent,
							StartLine:  startLine,
						})
						s.pendingFn = len(s.functions) - 1
					} else {
						isTp := ident == "tpcall"
						if isTp {
							s.tpcallCount++
						}
						s.calls = append(s.calls, FunctionCall{
							Name:      ident,
							Line:      startLine,
							Col:       startCol,
							Args:      s.balancedParenText(s.pos),
							IsTpCall:  isTp,
							IsFnPref:  strings.HasPrefix(ident, "fn_"),
							IsChkPref: strings.HasPrefix(ident, "chk_"),
							Func:      s.fnName(s.curFn),
						})
					}
				}
			} else if isTypeKeyword(ident) {
				if s.isCastContext() {
					// `(char*)x` — a cast, not a declaration type.
					s.pendingType = ""
				} else if endsWithModifier(s.pendingType) {
					// `unsigned long`, `long long` — accumulate modifiers.
					s.pendingType += " " + ident
				} else {
					// A new type restarts the declarator context
					// (`char* a, char* b` is two `char` decls, not `char char`).
					s.pendingType = ident
				}
			} else if s.pendingType != "" && !cKeywords[ident] {
				s.recordVarDecl(ident, braceDepth)
			}

			// Restore pos if we skipped whitespace
			s.pos = savedPos
			s.line = savedLine
			s.col = savedCol

			prevIdent = ident
			continue
		}

		// Normal punctuation / bytes
		if !unicode.IsSpace(rune(s.src[s.pos])) {
			// Semicolon resets prevIdent, closes any pending declaration
			// list, and turns an unbraced (prototype) definition back into
			// a non-definition.
			if s.src[s.pos] == ';' {
				prevIdent = ""
				s.pendingType = ""
				if braceDepth == 0 {
					s.pendingFn = -1
				}
			}
		}
		s.advance()
	}
}

func (s *scannerState) isLineStart() bool {
	// Look back in current line for any non-space
	for i := s.pos - 1; i >= 0 && s.src[i] != '\n'; i-- {
		if !unicode.IsSpace(rune(s.src[i])) {
			return false
		}
	}
	return true
}

func (s *scannerState) skipBlockComment() {
	s.advance() // /
	s.advance() // *
	for s.pos < s.len {
		if s.pos+1 < s.len && s.src[s.pos] == '*' && s.src[s.pos+1] == '/' {
			s.advance() // *
			s.advance() // /
			return
		}
		s.advance()
	}
}

func (s *scannerState) skipLineComment() {
	s.advance() // /
	s.advance() // /
	for s.pos < s.len && s.src[s.pos] != '\n' {
		s.advance()
	}
}

func (s *scannerState) skipStringLiteral() {
	s.advance() // opening "
	for s.pos < s.len {
		b := s.advance()
		if b == '\\' && s.pos < s.len {
			s.advance() // skip escaped char
			continue
		}
		if b == '"' {
			return
		}
	}
}

func (s *scannerState) skipCharLiteral() {
	s.advance() // opening '
	for s.pos < s.len {
		b := s.advance()
		if b == '\\' && s.pos < s.len {
			s.advance()
			continue
		}
		if b == '\'' {
			return
		}
	}
}

func (s *scannerState) scanDirective() {
	startLine := s.line
	s.advance() // #
	s.skipWhitespace()

	dirName := s.scanIdent()
	s.skipWhitespace()

	var argBuf bytes.Buffer
	for s.pos < s.len && s.src[s.pos] != '\n' {
		// Handle comments on directive line
		if s.pos+1 < s.len && s.src[s.pos] == '/' && (s.src[s.pos+1] == '*' || s.src[s.pos+1] == '/') {
			break
		}
		argBuf.WriteByte(s.advance())
	}

	arg := strings.TrimSpace(argBuf.String())
	isHeader := dirName == "include"
	isSystem := isHeader && strings.HasPrefix(arg, "<") && strings.HasSuffix(arg, ">")

	s.directives = append(s.directives, Directive{
		Kind:     dirName,
		Arg:      arg,
		Line:     startLine,
		IsHeader: isHeader,
		IsSystem: isSystem,
	})
}

func (s *scannerState) matchesExecSQL() bool {
	if s.pos+7 > s.len {
		return false
	}
	sub := string(s.src[s.pos : s.pos+8])
	if strings.EqualFold(sub, "exec sql") {
		// Verify word boundaries
		after := s.pos + 8
		if after < s.len && (unicode.IsSpace(rune(s.src[after])) || s.src[after] == '\n' || s.src[after] == ';') {
			return true
		}
	}
	return false
}

func (s *scannerState) scanExecSQL() {
	startLine := s.line
	// Skip "EXEC SQL"
	for s.pos < s.len && !unicode.IsSpace(rune(s.src[s.pos])) {
		s.advance() // skip EXEC
	}
	s.skipWhitespace()
	for s.pos < s.len && !unicode.IsSpace(rune(s.src[s.pos])) {
		s.advance() // skip SQL
	}

	var raw bytes.Buffer
	for s.pos < s.len {
		// Comments inside SQL
		if s.pos+1 < s.len && s.src[s.pos] == '/' && s.src[s.pos+1] == '*' {
			s.skipBlockComment()
			raw.WriteByte(' ')
			continue
		}
		if s.pos+1 < s.len && s.src[s.pos] == '/' && s.src[s.pos+1] == '/' {
			s.skipLineComment()
			raw.WriteByte(' ')
			continue
		}
		// String literal inside SQL (single quotes)
		if s.src[s.pos] == '\'' {
			raw.WriteByte(s.advance())
			for s.pos < s.len {
				b := s.advance()
				raw.WriteByte(b)
				if b == '\'' {
					if s.peek() == '\'' {
						// SQL escaped quote ''
						raw.WriteByte(s.advance())
						continue
					}
					break
				}
			}
			continue
		}
		// End of SQL statement
		if s.src[s.pos] == ';' {
			s.advance() // consume ;
			break
		}
		raw.WriteByte(s.advance())
	}

	endLine := s.line
	rawStr := strings.TrimSpace(raw.String())
	normalized := strings.Join(strings.Fields(rawStr), " ")

	kind, cursorName := classifySQL(normalized)

	stmt := ExecSQLStatement{
		Raw:        rawStr,
		Normalized: normalized,
		Kind:       kind,
		StartLine:  startLine,
		EndLine:    endLine,
		CursorName: cursorName,
		Func:       s.fnName(s.curFn),
	}

	s.allSQL = append(s.allSQL, stmt)
	if kind.IsQuery() {
		s.queries = append(s.queries, stmt)
	}
}

func classifySQL(sql string) (SQLKind, string) {
	upper := strings.ToUpper(sql)
	fields := strings.Fields(upper)
	if len(fields) == 0 {
		return SQLOther, ""
	}

	first := fields[0]

	switch first {
	case "SELECT":
		return SQLSelect, ""
	case "INSERT":
		return SQLInsert, ""
	case "UPDATE":
		return SQLUpdate, ""
	case "DELETE":
		return SQLDelete, ""
	case "DECLARE":
		// Check for DECLARE <cursor> CURSOR FOR SELECT ...
		if len(fields) >= 4 && fields[2] == "CURSOR" && fields[3] == "FOR" {
			return SQLDeclareCursor, fields[1]
		}
		return SQLOther, ""
	case "OPEN":
		cursorName := ""
		if len(fields) >= 2 {
			cursorName = fields[1]
		}
		return SQLOpen, cursorName
	case "FETCH":
		cursorName := ""
		if len(fields) >= 2 {
			cursorName = fields[1]
		}
		return SQLFetch, cursorName
	case "CLOSE":
		cursorName := ""
		if len(fields) >= 2 {
			cursorName = fields[1]
		}
		return SQLClose, cursorName
	case "BEGIN":
		if len(fields) >= 3 && fields[1] == "DECLARE" && fields[2] == "SECTION" {
			return SQLDeclareSection, ""
		}
		return SQLOther, ""
	case "END":
		if len(fields) >= 3 && fields[1] == "DECLARE" && fields[2] == "SECTION" {
			return SQLDeclareSection, ""
		}
		return SQLOther, ""
	case "INCLUDE":
		return SQLInclude, ""
	case "COMMIT":
		return SQLCommit, ""
	case "ROLLBACK":
		return SQLRollback, ""
	case "CONNECT":
		return SQLConnect, ""
	default:
		return SQLOther, ""
	}
}

func (s *scannerState) scanIdent() string {
	start := s.pos
	for s.pos < s.len && isIdentChar(s.src[s.pos]) {
		s.advance()
	}
	return string(s.src[start:s.pos])
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentChar(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

var cKeywords = map[string]bool{
	"return": true, "if": true, "else": true, "while": true, "for": true,
	"do": true, "switch": true, "case": true, "default": true, "break": true,
	"continue": true, "goto": true, "sizeof": true, "typedef": true,
	"struct": true, "union": true, "enum": true, "extern": true,
	"static": true, "register": true, "volatile": true, "const": true,
}

var typeKeywords = map[string]bool{
	"char": true, "int": true, "long": true, "short": true, "double": true,
	"float": true, "unsigned": true, "signed": true, "void": true,
	"varchar": true, // Pro*C host type
}

func isTypeKeyword(ident string) bool { return typeKeywords[ident] }

// endsWithModifier reports whether a pending type ends in a modifier that
// another type keyword can extend (unsigned long, long long, …).
func endsWithModifier(pending string) bool {
	switch pending {
	case "unsigned", "signed", "long", "short", "unsigned long", "long long":
		return true
	}
	return false
}

// isCastContext reports whether the type keyword just scanned opens a cast —
// an optional run of `*`s followed by `)` (read-only lookahead).
func (s *scannerState) isCastContext() bool {
	off := s.skipReadOnly(s.pos)
	for off < s.len && s.src[off] == '*' {
		off = s.skipReadOnly(off + 1)
	}
	return off < s.len && s.src[off] == ')'
}

// skipReadOnly returns the offset of the next significant byte at or after
// off, skipping whitespace and comments. Read-only: never mutates scan state.
func (s *scannerState) skipReadOnly(off int) int {
	for off < s.len {
		b := s.src[off]
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			off++
			continue
		}
		if b == '/' && off+1 < s.len {
			if s.src[off+1] == '*' {
				off += 2
				for off < s.len {
					if s.src[off] == '*' && off+1 < s.len && s.src[off+1] == '/' {
						off += 2
						break
					}
					off++
				}
				continue
			}
			if s.src[off+1] == '/' {
				for off < s.len && s.src[off] != '\n' {
					off++
				}
				continue
			}
		}
		return off
	}
	return off
}

// lineAt computes the line number of an offset that is at or after the
// current scan position (read-only forward scans only).
func (s *scannerState) lineAt(off int) int {
	line := s.line
	for i := s.pos; i < off && i < s.len; i++ {
		if s.src[i] == '\n' {
			line++
		}
	}
	return line
}

// balancedParenText returns the raw text between the parenthesis at off and
// its match ("" when off is not '(' or the parens never balance). Strings,
// chars, and comments inside are carried through verbatim.
func (s *scannerState) balancedParenText(off int) string {
	if off >= s.len || s.src[off] != '(' {
		return ""
	}
	depth := 0
	for i := off; i < s.len; i++ {
		switch b := s.src[i]; b {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return string(s.src[off+1 : i])
			}
		case '"':
			i = s.skipQuotedReadOnly(i, '"')
		case '\'':
			i = s.skipQuotedReadOnly(i, '\'')
		case '/':
			if i+1 < s.len {
				if s.src[i+1] == '*' {
					i = s.skipBlockCommentReadOnly(i)
				} else if s.src[i+1] == '/' {
					for i < s.len && s.src[i] != '\n' {
						i++
					}
				}
			}
		}
	}
	return ""
}

// skipQuotedReadOnly returns the index of the closing quote for the literal
// opening at i (handling backslash escapes).
func (s *scannerState) skipQuotedReadOnly(i int, quote byte) int {
	for j := i + 1; j < s.len; j++ {
		if s.src[j] == '\\' {
			j++
			continue
		}
		if s.src[j] == quote {
			return j
		}
	}
	return s.len
}

func (s *scannerState) skipBlockCommentReadOnly(i int) int {
	for j := i + 2; j < s.len; j++ {
		if s.src[j] == '*' && j+1 < s.len && s.src[j+1] == '/' {
			return j + 1
		}
	}
	return s.len
}

// blockExtent returns the line numbers of the opening brace at off and its
// matching close (ok=false when off is not '{' or the block never closes).
func (s *scannerState) blockExtent(off int) (start, end int, ok bool) {
	if off >= s.len || s.src[off] != '{' {
		return 0, 0, false
	}
	start = s.lineAt(off)
	depth := 0
	for i := off; i < s.len; i++ {
		switch b := s.src[i]; b {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return start, s.lineAt(i), true
			}
		case '"':
			i = s.skipQuotedReadOnly(i, '"')
		case '\'':
			i = s.skipQuotedReadOnly(i, '\'')
		case '/':
			if i+1 < s.len {
				if s.src[i+1] == '*' {
					i = s.skipBlockCommentReadOnly(i)
				} else if s.src[i+1] == '/' {
					for i < s.len && s.src[i] != '\n' {
						i++
					}
				}
			}
		}
	}
	return 0, 0, false
}

// recordIfBranch captures one if/else-if header and its block. Read-only:
// the main loop rescans the region normally afterwards.
func (s *scannerState) recordIfBranch(kind BranchKind, startLine, depth int) {
	off := s.skipReadOnly(s.pos)
	if off >= s.len || s.src[off] != '(' {
		return
	}
	inner := s.balancedParenText(off)
	cond := strings.Join(strings.Fields(inner), " ")
	blockStart, blockEnd := 0, 0
	// off: '(' at off, inner between, close paren at off+1+len(inner).
	if end := off + 2 + len(inner); end <= s.len {
		if open := s.skipReadOnly(end); open < s.len && s.src[open] == '{' {
			blockStart, blockEnd, _ = s.blockExtent(open)
		}
	}
	s.branches = append(s.branches, Branch{
		Kind:       kind,
		Cond:       cond,
		StartLine:  startLine,
		BlockStart: blockStart,
		BlockEnd:   blockEnd,
		Depth:      depth,
		Function:   s.fnName(s.curFn),
	})
}

// recordElseBranch captures a plain else block; an `else if` is recorded by
// recordIfBranch as BranchElseIf instead.
func (s *scannerState) recordElseBranch(startLine, depth int) {
	off := s.skipReadOnly(s.pos)
	if off >= s.len {
		return
	}
	if s.src[off] != '{' {
		return // `else if` chain link or `else` + label — the if-record handles it
	}
	blockStart, blockEnd, ok := s.blockExtent(off)
	if !ok {
		return
	}
	s.branches = append(s.branches, Branch{
		Kind:       BranchElse,
		StartLine:  startLine,
		BlockStart: blockStart,
		BlockEnd:   blockEnd,
		Depth:      depth,
		Function:   s.fnName(s.curFn),
	})
}

// recordVarDecl records a declarator for the pending base type when the next
// significant byte continues a declaration (; [ = , ).
func (s *scannerState) recordVarDecl(name string, depth int) {
	off := s.skipReadOnly(s.pos)
	if off >= s.len {
		return
	}
	next := s.src[off]
	switch next {
	case ';', '=', ',', ')':
		s.varDecls = append(s.varDecls, VarDecl{
			Type: s.pendingType,
			Name: name,
			Line: s.line,
			Func: s.fnName(s.curFn),
		})
	case '[':
		s.varDecls = append(s.varDecls, VarDecl{
			Type:  s.pendingType,
			Name:  name,
			Line:  s.line,
			Array: true,
			Func:  s.fnName(s.curFn),
		})
	}
}

func isLikelyFuncDef(prevIdent, ident string, src []byte, openParenPos int) bool {
	if prevIdent == "" {
		return false
	}
	// Skip common type qualifiers or keywords that can't be return types
	if prevIdent == "return" || prevIdent == "case" || prevIdent == "goto" {
		return false
	}

	// Look past the balanced parameter list (...)
	depth := 0
	pos := openParenPos
	for pos < len(src) {
		if src[pos] == '(' {
			depth++
		} else if src[pos] == ')' {
			depth--
			if depth == 0 {
				pos++
				break
			}
		}
		pos++
	}

	// Skip spaces
	for pos < len(src) && unicode.IsSpace(rune(src[pos])) {
		pos++
	}

	// If followed by '{' it's definitely a function definition
	if pos < len(src) && src[pos] == '{' {
		return true
	}
	return false
}
