// Package pychk is the batchpy validation gate (PRD-2026-09-08 BP-8): a
// structural Python syntax check (indentation blocks, bracket/quote balance)
// plus the SQL fidelity check, which extracts the generated module's
// triple-quoted string literals and reuses sqlchk.Compare unchanged — the
// same normalized structural projection the Go target is gated with. When a
// python3 interpreter is available, the ast.parse check upgrades the
// structural gate; both are degrade-safe observations.
package pychk

import (
	"os"
	"os/exec"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/sqlchk"
)

// Issue is one structural syntax problem with its 1-based line.
type Issue struct {
	Line int
	Msg  string
}

// Check performs the structural syntax scan: indentation blocks, bracket and
// quote balance, compound-statement headers. A clean result is necessary but
// not sufficient — CheckWithInterpreter upgrades the gate when python3 is on
// PATH.
func Check(src string) []Issue {
	var issues []Issue
	issue := func(line int, msg string) { issues = append(issues, Issue{Line: line, Msg: msg}) }

	lines := strings.Split(src, "\n")
	quote := ""            // active triple-quote marker
	depth := 0             // bracket depth carried across continuation lines
	prevOpener := false    // previous statement line opened a block
	prevOpenerIndent := "" // indent of the block-opening line
	blocks := []string{""} // open block body levels; the module level ("") is the outermost

	for i, raw := range lines {
		lineNo := i + 1
		startedInQuote := quote != ""
		code, newQuote, d := scanLine(raw, quote)
		quote = newQuote

		// A line that begins inside a triple-quoted string carries no
		// block structure — even when the string closes mid-line, the
		// remainder is a statement continuation at most.
		if startedInQuote {
			continue
		}
		depth += d

		indent := leading(raw)
		trimmed := strings.TrimSpace(code)

		// Statement-level analysis only applies when no brackets are open.
		if depth == 0 && trimmed != "" {
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if prevOpener {
				if indent <= prevOpenerIndent {
					issue(lineNo, "block opener not followed by deeper indent")
				}
				prevOpener = false
				// The block's body level is the next statement's indent —
				// pushed here, not the opener's own line indent.
				blocks = append(blocks, indent)
			} else if len(blocks) > 0 {
				top := blocks[len(blocks)-1]
				switch {
				case indent > top:
					// Deeper than the innermost open block without an
					// opener — Python raises IndentationError here.
					issue(lineNo, "unexpected indent")
				case indent < top:
					for len(blocks) > 0 && indent < blocks[len(blocks)-1] {
						blocks = blocks[:len(blocks)-1]
					}
					if len(blocks) == 0 || indent != blocks[len(blocks)-1] {
						issue(lineNo, "unindent does not match any outer indentation level")
					}
				}
			}
			if strings.ContainsRune(indent, '\t') {
				issue(lineNo, "tab in indentation")
			}
			if isBlockOpener(trimmed) {
				prevOpener = true
				prevOpenerIndent = indent
			}
		}
	}

	last := len(lines)
	if quote != "" {
		issue(last, "unterminated triple-quoted string")
	}
	if depth != 0 {
		issue(last, "unbalanced brackets at end of file")
	}
	if prevOpener {
		issue(last, "file ends with an unterminated block")
	}
	return issues
}

// scanLine returns the code portion of one line (string/comment segments
// blanked), the triple-quote state after the line, and the net bracket delta.
func scanLine(raw, quote string) (code string, newQuote string, depth int) {
	var sb strings.Builder
	i := 0
	for i < len(raw) {
		// Inside a triple-quoted string: scan for the closer.
		if quote != "" {
			if end := strings.Index(raw[i:], quote); end >= 0 {
				i += end + len(quote)
				quote = ""
				continue
			}
			return sb.String(), quote, depth
		}
		c := raw[i]
		switch {
		case (i+2 < len(raw)+1) && (strings.HasPrefix(raw[i:], `"""`) || strings.HasPrefix(raw[i:], "'''")):
			q := raw[i : i+3]
			rest := raw[i+3:]
			if end := strings.Index(rest, q); end >= 0 {
				sb.WriteString(strings.Repeat(" ", 3+end)) // blank the literal
				i = i + 3 + end + 3
				continue
			}
			sb.WriteString("   ")
			quote = q
			i += 3
			continue
		case c == '#':
			return sb.String(), quote, depth // comment to EOL
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(raw) {
				if raw[j] == '\\' {
					j += 2
					continue
				}
				if raw[j] == c {
					break
				}
				j++
			}
			sb.WriteString(strings.Repeat(" ", j-i+1))
			i = j + 1
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		}
		sb.WriteByte(c)
		i++
	}
	return sb.String(), quote, depth
}

// isBlockOpener recognizes compound-statement headers (the trimmed code ends
// with ':' and starts with a block keyword).
func isBlockOpener(trimmed string) bool {
	if !strings.HasSuffix(trimmed, ":") {
		return false
	}
	for _, kw := range []string{"if ", "elif ", "else", "for ", "while ", "def ", "class ", "try", "except", "finally", "with ", "match "} {
		if strings.HasPrefix(trimmed, kw) {
			return true
		}
	}
	return false
}

func leading(line string) string {
	for i, r := range line {
		if r != ' ' && r != '\t' {
			return line[:i]
		}
	}
	return line
}

// CheckWithInterpreter runs the real syntax gate: python3's ast.parse over
// the written file. mode reports "ast" (interpreter ran), or "unavailable"
// with the structural gate as the only evidence.
func CheckWithInterpreter(path string) (ok bool, mode string, detail string) {
	py, err := exec.LookPath("python3")
	if err != nil {
		return false, "unavailable", "python3 not found on PATH"
	}
	cmd := exec.Command(py, "-c", "import ast,sys; ast.parse(open(sys.argv[1], encoding='utf-8').read())", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, "ast", strings.TrimSpace(string(out))
	}
	return true, "ast", ""
}

// CheckSource is the content-level syntax gate (A2.7): the one owner of the
// temp-file + interpreter dance — callers pass module content and never
// touch the interpreter themselves.
func CheckSource(content string) (ok bool, mode string, detail string) {
	if _, err := exec.LookPath("python3"); err != nil {
		return false, "unavailable", "python3 not found on PATH"
	}
	tmp, err := os.CreateTemp("", "batchpy-*.py")
	if err != nil {
		return false, "unavailable", err.Error()
	}
	path := tmp.Name()
	if _, werr := tmp.WriteString(content); werr != nil {
		tmp.Close()
		os.Remove(path)
		return false, "unavailable", werr.Error()
	}
	tmp.Close()
	defer os.Remove(path)
	return CheckWithInterpreter(path)
}

// Literal is one extracted triple-quoted string with its assignment name
// ("" for inline literals).
type Literal struct {
	Name string
	SQL  string
}

// SQLLiterals extracts the module's triple-quoted string literals in source
// order, capturing the assignment target when the literal opens a
// `NAME = """..."""` statement (the py_batch_const shape).
func SQLLiterals(src string) []Literal {
	lines := strings.Split(src, "\n")
	var out []Literal
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		name := ""
		for _, q := range []string{`"""`, "'''"} {
			idx := strings.Index(trimmed, q)
			if idx < 0 {
				continue
			}
			head := strings.TrimSpace(trimmed[:idx])
			if eq := strings.LastIndex(head, "="); eq >= 0 && isIdentStr(strings.TrimSpace(head[:eq])) {
				name = strings.TrimSpace(head[:eq])
			}
			body, end := capture(lines, i, idx+len(q), q)
			out = append(out, Literal{Name: name, SQL: body})
			i = end
			break
		}
	}
	return out
}

// capture reads a triple-quoted body starting at (line, col) until the
// closing marker, returning the body text and the closing line index.
func capture(lines []string, line, col int, q string) (string, int) {
	var sb strings.Builder
	first := lines[line][col:]
	if end := strings.Index(first, q); end >= 0 {
		return first[:end], line
	}
	sb.WriteString(first)
	for l := line + 1; l < len(lines); l++ {
		if end := strings.Index(lines[l], q); end >= 0 {
			sb.WriteString("\n")
			sb.WriteString(lines[l][:end])
			return sb.String(), l
		}
		sb.WriteString("\n")
		sb.WriteString(lines[l])
	}
	return sb.String(), len(lines) - 1
}

func isIdentStr(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// FidelityTarget is one SQL constant's fidelity input (const name, IR query
// id, source SQL).
type FidelityTarget struct {
	Const   string
	QueryID string
	Source  string
}

// Fidelity compares every target's source SQL against the generated module's
// literal of the same const name through sqlchk.Compare — the same
// structural projection used for the Go target (PF-6 semantics, BP-8).
// The compare skeleton is the shared sqlchk.CompareTargets; only the
// Python triple-quoted literal extractor is local.
func Fidelity(targets []FidelityTarget, generated string) []sqlchk.Result {
	byName := map[string]string{}
	for _, lit := range SQLLiterals(generated) {
		if lit.Name != "" {
			if _, dup := byName[lit.Name]; !dup {
				byName[lit.Name] = lit.SQL
			}
		}
	}
	return sqlchk.CompareTargets(targets,
		func(t FidelityTarget) (string, bool) { gen, ok := byName[t.Const]; return gen, ok },
		func(t FidelityTarget) string { return t.Const },
		func(t FidelityTarget) string { return t.QueryID },
		func(t FidelityTarget) string { return t.Source })
}
