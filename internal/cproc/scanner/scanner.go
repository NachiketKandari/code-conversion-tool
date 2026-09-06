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
	tpcallCount int
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
			braceDepth++
			s.advance()
			prevIdent = ""
			continue
		}
		if s.src[s.pos] == '}' {
			if braceDepth > 0 {
				braceDepth--
			}
			s.advance()
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

			if s.pos < s.len && s.src[s.pos] == '(' {
				// It is a call or a definition
				if !cKeywordsWithParen[ident] {
					if braceDepth == 0 && isLikelyFuncDef(prevIdent, ident, s.src, s.pos) {
						s.functions = append(s.functions, FunctionDef{
							Name:       ident,
							ReturnType: prevIdent,
							StartLine:  startLine,
						})
					} else {
						isTp := ident == "tpcall"
						if isTp {
							s.tpcallCount++
						}
						s.calls = append(s.calls, FunctionCall{
							Name:      ident,
							Line:      startLine,
							Col:       startCol,
							IsTpCall:  isTp,
							IsFnPref:  strings.HasPrefix(ident, "fn_"),
							IsChkPref: strings.HasPrefix(ident, "chk_"),
						})
					}
				}
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
			// Semicolon resets prevIdent
			if s.src[s.pos] == ';' {
				prevIdent = ""
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
