// Package pyplan turns the batch flow skeleton into the Python generation
// plan (PRD 2026-09-08 BP-3): mechanical, deterministic naming for the SQL
// constants, the DAL functions / repository methods, and the service class —
// plus the CodeView (SQL regions replaced by call placeholders, dropped
// constructs elided) that the LLM seam consumes for stateful service bodies.
// Identical inputs produce a byte-identical plan.
package pyplan

import (
	"strconv"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/cproc/batchflow"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
)

// Wrapper carries the configurable db-router conventions (BP-7): the import
// path, the router class, and the read/write mode spellings the generated
// code uses. Transactions are wrapper-owned (BP-5) — the generated code
// never calls commit/rollback itself.
type Wrapper struct {
	Import      string
	RouterClass string
	ReadMode    string
	WriteMode   string
}

// Options are the run's shape/loop knobs (BP-4) and naming conventions.
type Options struct {
	Shape        string // auto | repo (forced repository shape)
	DMLLoop      string // batch | rowbyrow
	ChunkSize    int
	LoggerPrefix string
	Entrypoint   string
	Wrapper      Wrapper
}

// QueryConst is one module-level SQL constant.
type QueryConst struct {
	ID    string
	Name  string
	Kind  string
	SQL   string
	Binds []string // textual order, unique
}

// DALFn is one pure cursor function of the simple shape (mockable with a
// plain cursor double, the pythonEqTux1 reference shape).
type DALFn struct {
	Name     string
	Kind     string // fetch | dml
	Const    string
	BindIdx  []int // DML: fetch-row indexes for the bind projection
	RowShape []string
}

// Phase is one cursor-batch phase of the simple-shape service.
type Phase struct {
	Index   int
	Method  string
	FetchFn string
	DMLFn   string
	Cursor  string
	Table   string
}

// RepoMethod is one repository method of the repository shape.
type RepoMethod struct {
	Name      string
	Kind      string // fetch | dml | rebuild
	QueryKind string // ir.QueryType of the backing query ("" for truncate halves)
	Consts    []string
	Binds     []string // kwarg bind names
	RowShape  []string
	InLoop    bool
	SrcLine   int // source line of the backing query (orchestration-block assignment)
}

// Plan is the deterministic Python generation plan for one batch program.
type Plan struct {
	Module      string
	ServiceName string
	ClassName   string
	RepoName    string
	RepoUsed    bool
	Shape       string // simple | repo
	DMLLoop     string
	ChunkSize   int
	LoggerName  string
	Entrypoint  string
	Wrapper     Wrapper
	Consts      []QueryConst
	DAL         []DALFn
	Phases      []Phase
	Repo        []RepoMethod
	Flow        *batchflow.Flow

	constByQuery map[string]string // IR query id → constant name
	callByQuery  map[string]string // IR query id → DAL fn or repo method name
}

// Build assembles the plan from the flow and the run options. Shape: auto
// follows the rubric (simple → function DAL, stateful → repository); repo
// forces the repository shape (BP-4).
func Build(flow *batchflow.Flow, opts Options) *Plan {
	p := &Plan{
		Module:       flow.ServiceName,
		ServiceName:  flow.ServiceName,
		ClassName:    Camel(flow.ServiceName) + "Service",
		RepoName:     Camel(flow.ServiceName) + "Repository",
		Shape:        flow.Shape,
		DMLLoop:      opts.DMLLoop,
		ChunkSize:    opts.ChunkSize,
		LoggerName:   opts.LoggerPrefix + flow.ServiceName,
		Entrypoint:   opts.Entrypoint,
		Wrapper:      opts.Wrapper,
		Flow:         flow,
		constByQuery: map[string]string{},
		callByQuery:  map[string]string{},
	}
	if opts.Shape == "repo" || flow.Shape == batchflow.ShapeStateful {
		p.Shape = "repo"
	}
	if p.ChunkSize <= 0 {
		p.ChunkSize = 1000
	}

	used := map[string]bool{}
	take := func(base string) string {
		name := base
		for i := 2; used[strings.ToLower(name)]; i++ {
			name = base + "_" + strconv.Itoa(i)
		}
		used[strings.ToLower(name)] = true
		return name
	}

	for _, g := range flow.CursorGroups {
		sel := g.Select
		sc := take("SELECT_" + strings.ToUpper(g.CursorName) + "_QUERY")
		p.Consts = append(p.Consts, QueryConst{ID: sel.ID, Name: sc, Kind: string(sel.Type), SQL: emitSQL(sel), Binds: bindOrder(sel.SQL)})
		p.constByQuery[sel.ID] = sc
		if g.DML == nil {
			continue
		}
		dc := take(strings.ToUpper(verbOf(g.DML.Type)) + "_" + strings.ToUpper(g.CursorName) + "_QUERY")
		p.Consts = append(p.Consts, QueryConst{ID: g.DML.ID, Name: dc, Kind: string(g.DML.Type), SQL: emitSQL(g.DML), Binds: bindOrder(g.DML.SQL)})
		p.constByQuery[g.DML.ID] = dc
		if p.Shape == "repo" {
			continue // groups become repository methods below, not DAL phases
		}
		fname := "fetch_" + g.CursorName
		dname := verbOf(g.DML.Type) + "_" + g.CursorName
		p.DAL = append(p.DAL,
			DALFn{Name: fname, Kind: "fetch", Const: sc, RowShape: sel.RowShape},
			DALFn{Name: dname, Kind: "dml", Const: dc, BindIdx: bindIdx(g.DML.Binds, sel.RowShape)},
		)
		p.callByQuery[sel.ID] = fname
		p.callByQuery[g.DML.ID] = dname
		p.Phases = append(p.Phases, Phase{
			Index: len(p.Phases) + 1, Method: "process_" + g.CursorName + "_phase",
			FetchFn: fname, DMLFn: dname, Cursor: g.CursorName, Table: firstTable(g.DML),
		})
	}

	if p.Shape != "repo" {
		return p
	}
	p.RepoUsed = true
	// Cursor groups → repository methods (forced-repo over a simple rubric
	// flow keeps every query, but the orchestration becomes the LLM seam's).
	for _, g := range flow.CursorGroups {
		sel := g.Select
		name := take(verbOf(sel.Type) + "_" + strings.ToLower(g.CursorName))
		p.Repo = append(p.Repo, RepoMethod{
			Name: name, Kind: "fetch", QueryKind: string(sel.Type),
			Consts: []string{p.constByQuery[sel.ID]}, Binds: bindOrder(sel.SQL), RowShape: sel.RowShape,
			InLoop: false, SrcLine: sel.StartLine,
		})
		p.callByQuery[sel.ID] = name
		if g.DML == nil {
			continue
		}
		dname := take(verbOf(g.DML.Type) + "_" + strings.ToLower(g.CursorName))
		p.Repo = append(p.Repo, RepoMethod{
			Name: dname, Kind: verbOf(g.DML.Type), QueryKind: string(g.DML.Type),
			Consts: []string{p.constByQuery[g.DML.ID]}, Binds: bindOrder(g.DML.SQL),
			InLoop: inLoops(flow.Loops, g.DML.StartLine), SrcLine: g.DML.StartLine,
		})
		p.callByQuery[g.DML.ID] = dname
	}
	for _, s := range flow.Steps {
		q := s.Query
		var tc string
		if s.TruncateSQL != "" && q.Type == ir.QueryInsert {
			table := firstTable(q)
			tc = take("TRUNCATE_" + strings.ToUpper(table) + "_QUERY")
			p.Consts = append(p.Consts, QueryConst{ID: q.ID + "-trunc", Name: tc, Kind: "TRUNCATE", SQL: s.TruncateSQL})
		}
		cn := take(strings.ToUpper(verbOf(q.Type)) + "_" + strings.ToUpper(firstTable(q)) + "_QUERY")
		p.Consts = append(p.Consts, QueryConst{ID: q.ID, Name: cn, Kind: string(q.Type), SQL: emitSQL(q), Binds: bindOrder(q.SQL)})
		p.constByQuery[q.ID] = cn
		m := RepoMethod{
			Name: take(verbOf(q.Type) + "_" + strings.ToLower(firstTable(q))),
			Kind: verbOf(q.Type), QueryKind: string(q.Type), Consts: []string{cn},
			Binds: bindOrder(q.SQL), RowShape: q.RowShape, InLoop: s.InLoop, SrcLine: q.StartLine,
		}
		if tc != "" && q.Type == ir.QueryInsert {
			m.Kind = "rebuild"
			m.Consts = []string{tc, cn}
			m.Name = take("rebuild_" + strings.ToLower(firstTable(q)))
		}
		p.Repo = append(p.Repo, m)
		p.callByQuery[q.ID] = m.Name
	}
	return p
}

func inLoops(loops []batchflow.Loop, line int) bool {
	for _, l := range loops {
		if line >= l.StartLine && line <= l.EndLine {
			return true
		}
	}
	return false
}

// ConstName returns the SQL constant backing an IR query id.
func (p *Plan) ConstName(queryID string) string { return p.constByQuery[queryID] }

// CallName returns the DAL fn / repo method name backing an IR query id.
func (p *Plan) CallName(queryID string) string { return p.callByQuery[queryID] }

// verbOf maps a query type to its Python verb (fetch for SELECTs).
func verbOf(t ir.QueryType) string {
	switch t {
	case ir.QuerySelectSingle, ir.QuerySelectMulti:
		return "fetch"
	default:
		return strings.ToLower(string(t)) // update/delete/insert/merge
	}
}

func firstTable(q *ir.Query) string {
	if len(q.Tables) == 0 {
		return "row"
	}
	// DB-link-qualified tables (SCHEME@CONTENT_DB) and schema-qualified
	// names sanitize to Python-safe identifiers.
	return strings.ToLower(pyIdent(q.Tables[0]))
}

// pyIdent coerces a C/SQL identifier fragment into a Python-safe one:
// non-alphanumerics become underscores.
func pyIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// emitSQL renders the IR's canonical SQL as executable oracledb text (the
// documented BP-3 rubric transforms, tolerance-matched in sqlchk's
// normalizer): Pro*C host-variable INTO lists are stripped from SELECTs and
// `: name` collapses to `:name`.
func emitSQL(q *ir.Query) string {
	sql := q.SQL
	switch q.Type {
	case ir.QuerySelectSingle, ir.QuerySelectMulti:
		sql = stripInto(sql)
	}
	return collapseBinds(sql)
}

// stripInto removes the `INTO :a, :b …` span from a SELECT (the host-var
// output list — rows come back as tuples in oracledb).
func stripInto(sql string) string {
	lower := strings.ToLower(sql)
	for i := 0; i+4 <= len(lower); i++ {
		if lower[i:i+4] != "into" || !wordBoundary(sql, i, 4) {
			continue
		}
		j := i + 4
		for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t' || sql[j] == '\n' || sql[j] == '\r') {
			j++
		}
		if j >= len(sql) || sql[j] != ':' {
			continue // INSERT INTO t — not a host-var list
		}
		depth := 0
		for k := j; k < len(sql); k++ {
			switch sql[k] {
			case '\'':
				k = skipLit(sql, k)
			case '(':
				depth++
			case ')':
				depth--
			case 'f', 'F':
				if depth == 0 && k+4 < len(sql) && strings.EqualFold(sql[k:k+4], "from") && wordBoundary(sql, k, 4) {
					return strings.TrimSpace(sql[:i] + sql[k:])
				}
			}
		}
		return strings.TrimSpace(sql[:i])
	}
	return sql
}

func wordBoundary(sql string, i, n int) bool {
	before := i == 0 || !isIdentByte(sql[i-1])
	after := i+n >= len(sql) || !isIdentByte(sql[i+n])
	return before && after
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func skipLit(sql string, i int) int {
	for j := i + 1; j < len(sql); j++ {
		if sql[j] == '\'' {
			if j+1 < len(sql) && sql[j+1] == '\'' {
				j++
				continue
			}
			return j
		}
	}
	return len(sql) - 1
}

// collapseBinds rewrites `: name` → `:name` (Pro*C tolerates the space;
// oracledb named binds do not).
func collapseBinds(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if c == ':' && i+1 < len(sql) {
			j := i + 1
			for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t') {
				j++
			}
			if j > i+1 && j < len(sql) && isIdentByte(sql[j]) {
				b.WriteByte(':')
				i = j - 1
				continue
			}
		}
		if c == '\'' {
			k := skipLit(sql, i)
			b.WriteString(sql[i : k+1])
			i = k
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// bindOrder returns the unique bind names in textual order of appearance
// (`: name` with whitespace counts — Pro*C tolerates the space).
func bindOrder(sql string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(sql); i++ {
		if sql[i] != ':' {
			continue
		}
		j := i + 1
		for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t') {
			j++
		}
		nameStart := j
		for j < len(sql) && isIdent(sql[j]) {
			j++
		}
		if j == nameStart {
			continue // positional :1/:2 binds keep no name
		}
		name := sql[nameStart:j]
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
		i = j - 1
	}
	return out
}

func isIdent(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// bindIdx maps the DML's textual bind order onto the fetch row tuple.
func bindIdx(binds, rowShape []string) []int {
	idx := map[string]int{}
	for i, r := range rowShape {
		idx[strings.ToLower(r)] = i
	}
	out := make([]int, 0, len(binds))
	for _, b := range binds {
		out = append(out, idx[strings.ToLower(b)])
	}
	return out
}

// Camel converts snake/dashed identifiers to CamelCase (bat_mf_mbm_rt →
// BatMfMbmRt).
func Camel(s string) string {
	parts := strings.FieldsFunc(pyIdent(s), func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	var sb strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		sb.WriteString(strings.ToUpper(p[:1]))
		sb.WriteString(p[1:])
	}
	return sb.String()
}

// CodeView renders the entry body for the LLM seam (BP-6, the batch analogue
// of the query-replaced branch view): every EXEC SQL span becomes one
// placeholder line naming the deterministic call or cursor op, dropped
// constructs collapse to marked comments, and the surrounding control flow
// (loops, branches, assignments, logs) stays verbatim.
func (p *Plan) CodeView(src string) string {
	entries := p.codeViewEntries(src)
	texts := make([]string, len(entries))
	for i, e := range entries {
		texts[i] = e.Text
	}
	return strings.Join(texts, "\n")
}

// viewEntry is one CodeView line with its source line number.
type viewEntry struct {
	Line int
	Text string
}

// OrchestrationBlock is one phase of the orchestration contract (BP-6): a
// contiguous control region of the entry body — the setup before any loop,
// then each loop — with the repository methods whose source lines fall
// inside it. The seam must implement every block, in order, calling every
// listed method under the condition the view shows.
type OrchestrationBlock struct {
	Index   int
	Name    string
	Methods []string
	View    string
}

// Orchestration slices the CodeView into the per-block contract.
func (p *Plan) Orchestration(src string) []OrchestrationBlock {
	entries := p.codeViewEntries(src)
	type rng struct {
		start, end int
		name       string
	}
	var ranges []rng
	if len(p.Flow.Loops) == 0 {
		ranges = append(ranges, rng{p.Flow.BodyStart, p.Flow.BodyEnd, "body"})
	} else {
		if first := p.Flow.Loops[0].StartLine; first-1 >= p.Flow.BodyStart {
			ranges = append(ranges, rng{p.Flow.BodyStart, first - 1, "setup (before any loop)"})
		}
		for _, l := range p.Flow.Loops {
			ranges = append(ranges, rng{l.StartLine, l.EndLine,
				"loop `" + l.Header + "` (source " + strconv.Itoa(l.StartLine) + "-" + strconv.Itoa(l.EndLine) + ")"})
		}
	}
	blocks := make([]OrchestrationBlock, 0, len(ranges))
	for i, r := range ranges {
		b := OrchestrationBlock{Index: i + 1, Name: r.name}
		var texts []string
		for _, e := range entries {
			if e.Line >= r.start && e.Line <= r.end {
				texts = append(texts, e.Text)
			}
		}
		b.View = strings.Join(texts, "\n")
		for _, m := range p.Repo {
			if m.SrcLine >= r.start && m.SrcLine <= r.end {
				b.Methods = append(b.Methods, m.Name)
			}
		}
		if b.View != "" || len(b.Methods) > 0 {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// codeViewEntries renders the entry body line-by-line with EXEC SQL spans
// replaced by placeholders and dropped statements elided; Line is the source
// line each output line came from.
func (p *Plan) codeViewEntries(src string) []viewEntry {
	lines := strings.Split(src, "\n")
	placeholder := map[int]string{}
	sqlLine := map[int]bool{}
	for _, s := range p.Flow.SQLSpans {
		if s.StartLine < p.Flow.BodyStart || s.StartLine > p.Flow.BodyEnd {
			continue
		}
		if text := p.sqlPlaceholder(s); text != "" {
			placeholder[s.StartLine] = text
		}
		for l := s.StartLine; l <= s.EndLine; l++ {
			sqlLine[l] = true
		}
	}
	hidden := map[int]bool{}
	for _, d := range p.Flow.Dropped {
		start, end := dropSpan(lines, d.Line)
		for l := start; l <= end; l++ {
			hidden[l] = true
		}
		placeholder[start] = "# dropped [" + string(d.Kind) + "]: " + d.Call
	}
	var out []viewEntry
	for l := p.Flow.BodyStart; l <= p.Flow.BodyEnd && l-1 < len(lines); l++ {
		if sqlLine[l] || hidden[l] {
			continue
		}
		if text, ok := placeholder[l]; ok {
			out = append(out, viewEntry{Line: l, Text: leading(lines[l-1]) + text})
			continue
		}
		out = append(out, viewEntry{Line: l, Text: lines[l-1]})
	}
	return out
}

// sqlPlaceholder maps one EXEC SQL span to its CodeView placeholder line.
func (p *Plan) sqlPlaceholder(s batchflow.SQLSpan) string {
	switch s.Kind {
	case "DECLARE_CURSOR":
		q := p.cursorSelect(s.CursorName)
		if q == nil {
			return "# cursor " + s.CursorName + ": DECLARE"
		}
		return "# cursor " + s.CursorName + ": DECLARE → const " + p.ConstName(q.ID) +
			" (row: " + rowNames(q) + ")"
	case "OPEN":
		return "# cursor " + s.CursorName + ": OPEN"
	case "FETCH":
		q := p.cursorSelect(s.CursorName)
		call := ""
		if q != nil {
			call = " (calls " + p.CallName(q.ID) + ")"
		}
		return "# cursor " + s.CursorName + ": FETCH row" + call
	case "CLOSE":
		return "# cursor " + s.CursorName + ": CLOSE"
	case "COMMIT", "ROLLBACK":
		return "# " + strings.ToLower(s.Kind) + " — owned by " + p.Wrapper.RouterClass + " context manager"
	}
	// Query spans: match the IR unit by start line.
	for _, q := range p.Flow.Queries {
		if q.StartLine != s.StartLine {
			continue
		}
		if call := p.CallName(q.ID); call != "" {
			return "# " + call + "(" + strings.Join(kwargBinds(bindOrder(q.SQL)), ", ") + ")"
		}
		return "# SQL " + string(q.Type)
	}
	if strings.Contains(strings.ToUpper(s.Normal), "TRUNCATE") {
		return "# TRUNCATE — folded into the paired rebuild method"
	}
	return ""
}

func rowNames(q *ir.Query) string {
	if len(q.RowShape) == 0 {
		return "row"
	}
	return strings.Join(q.RowShape, ", ")
}

func kwargBinds(binds []string) []string {
	out := make([]string, len(binds))
	for i, b := range binds {
		out[i] = b + "=" + b
	}
	return out
}

func (p *Plan) cursorSelect(name string) *ir.Query {
	for _, g := range p.Flow.CursorGroups {
		if g.CursorName == name {
			return g.Select
		}
	}
	return nil
}

// dropSpan returns the statement extent of a dropped call: from its line
// until parens rebalance and a statement terminator appears.
func dropSpan(lines []string, line int) (int, int) {
	depth := 0
	seen := false
	for l := line; l <= len(lines); l++ {
		text := lines[l-1]
		for i := 0; i < len(text); i++ {
			switch text[i] {
			case '"', '\'':
				i = skipQuoted(text, i)
			case '(':
				depth++
				seen = true
			case ')':
				depth--
			case ';':
				if seen && depth <= 0 {
					return line, l
				}
			}
		}
		if seen && depth <= 0 {
			return line, l
		}
	}
	return line, line
}

func skipQuoted(text string, i int) int {
	q := text[i]
	for j := i + 1; j < len(text); j++ {
		if text[j] == '\\' {
			j++
			continue
		}
		if text[j] == q {
			return j
		}
	}
	return len(text) - 1
}

func leading(line string) string {
	for i, r := range line {
		if r != ' ' && r != '\t' {
			return line[:i]
		}
	}
	return line
}
