package ir

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/cproc/pred"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

// droppedFmlFields are session/error plumbing constructs (§4.8.4): read by
// the existing service's middleware or translated into Go error returns, so
// they never become request/response fields.
var droppedFmlFields = map[string]bool{
	"FML_USER_ID":    true,
	"FML_SESSION_ID": true,
	"FML_ERR_MSG":    true,
}

var (
	hostRefRe    = regexp.MustCompile(`:([A-Za-z_][A-Za-z0-9_]*)`)
	positionalRe = regexp.MustCompile(`:\d+`)
	intoRe       = regexp.MustCompile(`(?i)\bINTO\b`)
	fromRe       = regexp.MustCompile(`(?i)\bFROM\b`)
	whereRe      = regexp.MustCompile(`(?i)\bWHERE\b|\bGROUP\b|\bHAVING\b|\bORDER\b|\bFOR UPDATE\b`)
	orderByRe    = regexp.MustCompile(`(?i)\bORDER BY\b`)
	cursorNameRe = regexp.MustCompile(`(?i)\bDECLARE\s+(\w+)\s+CURSOR\b`)
	aliasRe      = regexp.MustCompile(`(?i)AS\s+"([A-Za-z0-9_]+)"`)
)

// defaultBufferRoles is the project's buffer naming convention (PF-4.1):
// Ibuffer is the endpoint's FML input, Obuffer its output, and Sbuffer/
// Rbuffer the send/receive buffers of a tpcall. Matching is case-insensitive
// on the whole variable name or on its `_`-delimited tail segment
// (ptr_fml_Ibuffer → Ibuffer).
var defaultBufferRoles = map[string]FmlBufferRole{
	"ibuffer": BufferInput,
	"obuffer": BufferOutput,
	"sbuffer": BufferSend,
	"rbuffer": BufferRecv,
}

// Options carries extraction-time configuration that is data, not code
// (PF-4.1): the buffer-role registry flows from .tuxgo.yaml so renamed or
// additional buffer conventions need no code change. ForceFragment runs the
// fragment rubric on a file whose automatic detection is ambiguous (PF-3.1).
type Options struct {
	BufferRoles   map[string]string
	ForceFragment bool
}

// DefaultOptions returns the stock extraction configuration (the project
// naming convention registry).
func DefaultOptions() Options {
	roles := make(map[string]string, len(defaultBufferRoles))
	for k, v := range defaultBufferRoles {
		roles[k] = string(v)
	}
	return Options{BufferRoles: roles}
}

// roleOf resolves a buffer variable's convention role: exact (case-
// insensitive) name first, then the last `_`-delimited segment; unknown
// names degrade to the visible unknown-role fact (PF-4.1).
func roleOf(bufVar string, roles map[string]FmlBufferRole) FmlBufferRole {
	name := strings.ToLower(strings.TrimSpace(bufVar))
	if name == "" {
		return BufferUnknown
	}
	if r, ok := roles[name]; ok {
		return r
	}
	if i := strings.LastIndex(name, "_"); i >= 0 {
		if r, ok := roles[name[i+1:]]; ok {
			return r
		}
	}
	return BufferUnknown
}

// ExtractFile scans one Pro*C file and builds its IR. Files holding no
// SVC_* entry function and no function definitions convert as fragments
// (PF-3.1: automatic detection — a lone block wrapped as __fragment);
// -fragment forces the fragment rubric when detection is ambiguous.
func ExtractFile(path string) (*File, error) {
	return ExtractFileOpts(path, Options{})
}

// ExtractFileOpts is ExtractFile with the run's extraction options (buffer
// registry, forced fragment mode).
func ExtractFileOpts(path string, opts Options) (*File, error) {
	facts, err := scanner.ScanFile(path)
	if err != nil {
		return nil, err
	}
	if !opts.ForceFragment && !isFragmentFacts(facts) {
		return build(facts, opts), nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	facts, err = scanner.ScanFragment(src, path)
	if err != nil {
		return nil, err
	}
	return build(facts, opts), nil
}

// isFragmentFacts applies the automatic fragment detection (PF-3.1): no
// SVC_* entry and no function definitions means the file is a lone code
// block; a file with helper definitions (fn_*.pc) is a full helper file.
func isFragmentFacts(facts *scanner.SourceFacts) bool {
	for _, fn := range facts.Functions {
		if strings.HasPrefix(fn.Name, "SVC_") {
			return false
		}
	}
	return len(facts.Functions) == 0
}

// ExtractDir walks a folder recursively and extracts every .pc/.pcf file.
// Directory mode is corpus mode: no fragment detection (helper files keep
// their full-file semantics). External fn symbols (called but not defined
// locally) resolve against the definitions found anywhere in the scanned
// corpus (§4.2.9): a resolved fn_'s SQL units live in the defining file's
// IR and are referenced by ID.
func ExtractDir(dir string) ([]*File, error) {
	return ExtractDirOpts(dir, Options{})
}

// ExtractDirOpts is ExtractDir with the run's extraction options.
func ExtractDirOpts(dir string, opts Options) ([]*File, error) {
	var paths []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".pc", ".pcf":
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	units := make([]*fileUnit, 0, len(paths))
	for _, p := range paths {
		facts, err := scanner.ScanFile(p)
		if err != nil {
			return nil, fmt.Errorf("error scanning %s: %w", p, err)
		}
		units = append(units, &fileUnit{facts: facts, ir: build(facts, opts)})
	}

	// Definition corpus: fn name → defining file + body traits (same shape
	// as the analyzer's two-pass resolution).
	type defSite struct {
		unit      *fileUnit
		hasSQL    bool
		hasTpCall bool
	}
	corpus := map[string]*defSite{}
	for _, u := range units {
		ranges := fnRanges(u.facts)
		for name, r := range ranges {
			site := &defSite{unit: u}
			for _, q := range u.facts.Queries {
				if q.StartLine >= r.start && q.StartLine < r.end {
					site.hasSQL = true
					break
				}
			}
			for _, c := range u.facts.Calls {
				if c.IsTpCall && c.Line >= r.start && c.Line < r.end {
					site.hasTpCall = true
					break
				}
			}
			corpus[name] = site
		}
	}

	for _, u := range units {
		for i := range u.ir.ExternalFns {
			ext := &u.ir.ExternalFns[i]
			site, ok := corpus[ext.Name]
			if !ok {
				continue
			}
			ext.Resolved = true
			ext.DefinedIn = site.unit.ir.Path
			ext.HasSQL = site.hasSQL || site.hasTpCall
			for _, q := range site.unit.ir.Queries {
				if q.OwningFunction == ext.Name {
					ext.QueryIDs = append(ext.QueryIDs, q.ID)
				}
			}
		}
	}

	return filesOf(units), nil
}

type fileUnit struct {
	facts *scanner.SourceFacts
	ir    *File
}

func filesOf(units []*fileUnit) []*File {
	out := make([]*File, 0, len(units))
	for _, u := range units {
		out = append(out, u.ir)
	}
	return out
}

// fnRange is the body extent of one function definition.
type fnRange struct{ start, end int }

// fnRanges maps function name → body extent (brace-matched when the scanner
// resolved it, next-definition heuristic otherwise).
func fnRanges(facts *scanner.SourceFacts) map[string]fnRange {
	fns := make([]scanner.FunctionDef, len(facts.Functions))
	copy(fns, facts.Functions)
	sort.Slice(fns, func(i, j int) bool { return fns[i].StartLine < fns[j].StartLine })

	out := make(map[string]fnRange, len(fns))
	for i, fn := range fns {
		end := int(^uint(0) >> 1)
		if fn.BodyStartLine > 0 && fn.BodyEndLine > 0 {
			end = fn.BodyEndLine + 1
		} else if i+1 < len(fns) {
			end = fns[i+1].StartLine
		}
		out[fn.Name] = fnRange{start: fn.StartLine, end: end}
	}
	return out
}

// build derives the IR from scanned facts. Pure: same facts → same IR.
func build(facts *scanner.SourceFacts, opts Options) *File {
	facts = commentLiveFacts(facts)
	f := &File{Path: facts.Path, Fragment: facts.Fragment}
	for _, u := range facts.Unbalanced {
		f.Unbalanced = append(f.Unbalanced, Unbalanced{Kind: u.Kind, Line: u.StartLine, Col: u.StartCol})
	}
	for _, fn := range facts.Functions {
		f.Functions = append(f.Functions, fn.Name)
	}

	entry := ""
	for _, fn := range facts.Functions {
		if strings.HasPrefix(fn.Name, "SVC_") {
			entry = fn.Name
			break
		}
	}
	if entry == "" && facts.Fragment {
		// The synthesized pseudo-function is the entry (PF-3.2).
		entry = "__fragment"
	}
	f.Entry = entry

	f.Queries = buildQueries(facts)
	f.BranchCount, f.BranchingFactor = branchingOf(facts)

	if entry != "" {
		f.Conditions = buildConditions(facts, entry, f.Queries)
		f.FmlOps = entryFmlOps(facts, entry, f.Conditions)
	}

	f.ExternalFns = buildExternalFns(facts)
	f.HostVars = buildHostVars(facts, f)
	f.Buffers = buildBuffers(facts, opts)
	f.TPCalls = buildTPCalls(facts, opts, f.Buffers)

	linkDuplicates(f.Queries)
	return f
}

// branchingOf totals the file's if/else-if header count (else never
// contributes) and the doubling-weighted branching factor: each header adds
// 1 << NestDepth, where NestDepth is the scanner's count of enclosing
// if/else-if blocks.
func branchingOf(facts *scanner.SourceFacts) (count, factor int) {
	for _, b := range facts.Branches {
		if b.Kind == scanner.BranchElse {
			continue
		}
		count++
		factor += 1 << b.NestDepth
	}
	return count, factor
}

// commentLiveFacts excludes any call or SQL statement whose start position
// falls inside a recorded comment span (PF-1.5) — with the comment inventory
// in place the exclusion is a deterministic overlap query instead of a
// scanner incident. Recorded facts never sit inside comments (the scanner
// skips them), so on healthy inputs this is a no-op; on malformed ones it is
// the loud, testable rule.
func commentLiveFacts(facts *scanner.SourceFacts) *scanner.SourceFacts {
	if len(facts.Comments) == 0 {
		return facts
	}
	live := *facts
	live.Calls = nil
	live.AllSQL = nil
	live.Queries = nil
	for _, c := range facts.Calls {
		if !facts.InComment(c.Line, c.Col) {
			live.Calls = append(live.Calls, c)
		}
	}
	for _, q := range facts.AllSQL {
		if !facts.InComment(q.StartLine, q.StartCol) {
			live.AllSQL = append(live.AllSQL, q)
		}
	}
	for _, q := range facts.Queries {
		if !facts.InComment(q.StartLine, q.StartCol) {
			live.Queries = append(live.Queries, q)
		}
	}
	return &live
}

// roleRegistry resolves the run's buffer-role registry (config-extensible,
// PF-4.1); an absent registry falls back to the project convention.
func roleRegistry(opts Options) map[string]FmlBufferRole {
	if len(opts.BufferRoles) == 0 {
		return defaultBufferRoles
	}
	roles := make(map[string]FmlBufferRole, len(opts.BufferRoles))
	for name, role := range opts.BufferRoles {
		roles[strings.ToLower(name)] = FmlBufferRole(role)
	}
	return roles
}

// buildBuffers records every FML buffer variable observed in live code with
// its convention role (PF-4.1): Fget32/Fadd32 first arguments plus tpcall
// send/receive buffer arguments. Unknown names degrade to the visible
// unknown-role fact, never a guess.
func buildBuffers(facts *scanner.SourceFacts, opts Options) []BufferRole {
	registry := roleRegistry(opts)
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for i := range facts.Calls {
		call := &facts.Calls[i]
		switch {
		case call.Name == "Fget32" || call.Name == "Fadd32":
			args := splitArgs(call.Args)
			if len(args) > 0 {
				add(normalizeTarget(args[0]))
			}
		case call.IsTpCall:
			args := splitArgs(call.Args)
			if len(args) > 1 {
				add(normalizeTarget(args[1]))
			}
			if len(args) > 3 {
				add(normalizeTarget(args[3]))
			}
		}
	}
	sort.Strings(names)
	out := make([]BufferRole, 0, len(names))
	for _, name := range names {
		out = append(out, BufferRole{Name: name, Role: roleOf(name, registry)})
	}
	return out
}

// svcNameRe extracts the quoted service name of a tpcall site.
var svcNameRe = regexp.MustCompile(`"([^"]+)"`)

// buildTPCalls correlates every live tpcall site with its FML contract
// (PF-4.3): the send contract is the Fadd32 ops into the send buffer inside
// the enclosing block before the call, the receive contract the Fget32 ops
// from the receive buffer after it. Sites whose buffer variables cannot be
// identified degrade to an Ambiguous fact — visible, never a guess.
func buildTPCalls(facts *scanner.SourceFacts, opts Options, buffers []BufferRole) []TPCall {
	if facts.TpCallCount == 0 {
		return nil
	}
	var out []TPCall
	for i := range facts.Calls {
		call := &facts.Calls[i]
		if !call.IsTpCall {
			continue
		}
		args := splitArgs(call.Args)
		tp := TPCall{
			StartLine: call.Line,
			EndLine:   call.Line,
			Function:  call.Func,
			Ambiguous: true,
		}
		if len(args) > 0 {
			if m := svcNameRe.FindStringSubmatch(args[0]); m != nil {
				tp.Service = m[1]
			}
		}
		if len(args) > 1 {
			tp.SendBuffer = normalizeTarget(args[1])
		}
		if len(args) > 3 {
			tp.RecvBuffer = normalizeTarget(args[3])
		}
		if tp.Service == "" || tp.SendBuffer == "" || tp.RecvBuffer == "" {
			out = append(out, tp)
			continue
		}
		tp.Ambiguous = false

		winStart, winEnd := enclosingWindow(facts, call)
		for j := range facts.Calls {
			other := &facts.Calls[j]
			if other.Func != call.Func {
				continue
			}
			switch other.Name {
			case "Fadd32":
				if op, ok := fmlOpOf(other, facts); ok && op.Buffer == tp.SendBuffer &&
					other.Line >= winStart && other.Line < call.Line {
					tp.SendFML = append(tp.SendFML, op)
				}
			case "Fget32":
				if op, ok := fmlOpOf(other, facts); ok && op.Buffer == tp.RecvBuffer &&
					other.Line > call.Line && other.Line <= winEnd {
					tp.RecvFML = append(tp.RecvFML, op)
					if other.Line > tp.EndLine {
						tp.EndLine = other.Line
					}
				}
			}
		}
		// Identified buffers but an empty contract — no FML ops in the
		// call's window on either side (severity F5): the contract is
		// silently empty, so the site degrades to an ambiguous fact the
		// placeholder must state, never an implicit guess.
		if len(tp.SendFML) == 0 && len(tp.RecvFML) == 0 {
			tp.Ambiguous = true
		}
		out = append(out, tp)
	}
	return out
}

// enclosingWindow returns the line extent of the tpcall site's surrounding
// block (PF-4.3): the innermost recorded branch block containing the call,
// else the function body, else the file.
func enclosingWindow(facts *scanner.SourceFacts, call *scanner.FunctionCall) (start, end int) {
	bestStart, bestEnd := 0, 0
	for i := range facts.Branches {
		b := &facts.Branches[i]
		if b.Function != call.Func || b.BlockStart == 0 {
			continue
		}
		if b.BlockStart <= call.Line && call.Line <= b.BlockEnd {
			if bestStart == 0 || (b.BlockStart >= bestStart && b.BlockEnd <= bestEnd) {
				bestStart, bestEnd = b.BlockStart, b.BlockEnd
			}
		}
	}
	if bestStart != 0 {
		return bestStart, bestEnd
	}
	lo, hi := 1, facts.NumLines
	for _, fn := range facts.Functions {
		if fn.Name == call.Func && fn.BodyStartLine > 0 && fn.BodyEndLine > 0 {
			lo, hi = fn.BodyStartLine, fn.BodyEndLine
		}
	}
	return lo, hi
}

// buildQueries turns EXEC SQL statements into logical query units. Cursor
// groups flatten into one SELECT_MULTI unit whose extent spans the DECLARE
// through the last CLOSE (§4.8.2).
func buildQueries(facts *scanner.SourceFacts) []*Query {
	var out []*Query
	ordinal := 0
	for i := range facts.AllSQL {
		stmt := &facts.AllSQL[i]
		switch stmt.Kind {
		case scanner.SQLDeclareCursor:
			ordinal++
			out = append(out, cursorQuery(facts, stmt, ordinal))
		case scanner.SQLSelect:
			ordinal++
			out = append(out, directQuery(stmt, ordinal))
		case scanner.SQLInsert:
			ordinal++
			out = append(out, dmlQuery(stmt, QueryInsert, ordinal))
		case scanner.SQLUpdate:
			ordinal++
			out = append(out, dmlQuery(stmt, QueryUpdate, ordinal))
		case scanner.SQLDelete:
			ordinal++
			out = append(out, dmlQuery(stmt, QueryDelete, ordinal))
		case scanner.SQLMerge:
			ordinal++
			out = append(out, dmlQuery(stmt, QueryMerge, ordinal))
		}
	}
	return out
}

// cursorQuery flattens one DECLARE CURSOR group. The SQL body is everything
// after CURSOR FOR; the row shape comes from the first FETCH; the extent
// ends at the last CLOSE of the cursor inside the same function. Cursor
// grouping uses the scanner's uppercase key; the IR keeps the source spelling.
func cursorQuery(facts *scanner.SourceFacts, stmt *scanner.ExecSQLStatement, ordinal int) *Query {
	normalized := stmt.Normalized
	lower := strings.ToLower(normalized)
	idx := strings.Index(lower, "cursor for")
	body := ""
	if idx >= 0 {
		body = strings.TrimSpace(normalized[idx+len("cursor for"):])
	}
	name := stmt.CursorName
	if m := cursorNameRe.FindStringSubmatch(normalized); m != nil {
		name = m[1]
	}

	q := &Query{
		ID:              name,
		Type:            QuerySelectMulti,
		TemplateID:      QuerySelectMulti.TemplateID(),
		SQL:             body,
		StartLine:       stmt.StartLine,
		EndLine:         stmt.EndLine,
		OwningFunction:  stmt.Func,
		CursorName:      name,
		CursorFlattened: true,
		Tables:          tablesFromSelect(body),
		OrderBy:         orderByOf(body),
		Sites:           []int{stmt.StartLine},
	}
	q.DedupKey = body

	var lastClose int
	for j := range facts.AllSQL {
		other := &facts.AllSQL[j]
		if other.CursorName != stmt.CursorName || other.Func != stmt.Func || other.StartLine <= stmt.StartLine {
			continue
		}
		switch other.Kind {
		case scanner.SQLFetch:
			if len(q.RowShape) == 0 {
				q.RowShape = intoList(other.Normalized)
			}
			if other.EndLine > q.EndLine {
				q.EndLine = other.EndLine
			}
		case scanner.SQLClose:
			lastClose = other.EndLine
		case scanner.SQLOpen:
			if other.EndLine > q.EndLine && lastClose == 0 {
				q.EndLine = other.EndLine
			}
		}
	}
	if lastClose > q.EndLine {
		q.EndLine = lastClose
	}

	q.Binds, q.BindArity = bindsOf(bindSource(body))
	q.Aliases = aliasesOf(stmt.Raw)
	return q
}

// directQuery classifies a bare SELECT: INTO marks a single-value read
// (scalar/one-row), no INTO a multi-row read.
func directQuery(stmt *scanner.ExecSQLStatement, ordinal int) *Query {
	q := &Query{
		ID:             fmt.Sprintf("q%d", ordinal),
		Type:           QuerySelectMulti,
		TemplateID:     QuerySelectMulti.TemplateID(),
		SQL:            stmt.Normalized,
		StartLine:      stmt.StartLine,
		EndLine:        stmt.EndLine,
		OwningFunction: stmt.Func,
		Tables:         tablesFromSelect(stmt.Normalized),
		OrderBy:        orderByOf(stmt.Normalized),
		Sites:          []int{stmt.StartLine},
	}
	if intoRe.MatchString(stmt.Normalized) {
		q.Type = QuerySelectSingle
		q.TemplateID = QuerySelectSingle.TemplateID()
		q.RowShape = intoList(stmt.Normalized)
	}
	q.DedupKey = q.SQL
	q.Binds, q.BindArity = bindsOf(bindSource(q.SQL))
	q.Aliases = aliasesOf(stmt.Raw)
	return q
}

func dmlQuery(stmt *scanner.ExecSQLStatement, qt QueryType, ordinal int) *Query {
	q := &Query{
		ID:             fmt.Sprintf("q%d", ordinal),
		Type:           qt,
		TemplateID:     qt.TemplateID(),
		SQL:            stmt.Normalized,
		StartLine:      stmt.StartLine,
		EndLine:        stmt.EndLine,
		OwningFunction: stmt.Func,
		Tables:         tablesFromDML(stmt.Normalized, qt),
		Sites:          []int{stmt.StartLine},
	}
	q.DedupKey = q.SQL
	q.Binds, q.BindArity = bindsOf(q.SQL)
	return q
}

// linkDuplicates sets DedupKey/DuplicateOf over identical normalized SQL
// (§4.2.8.7: cross-branch duplicates eventually collapse to one DB method).
func linkDuplicates(queries []*Query) {
	first := map[string]string{}
	for _, q := range queries {
		if q.DedupKey == "" {
			continue
		}
		if id, ok := first[q.DedupKey]; ok {
			q.DuplicateOf = id
		} else {
			first[q.DedupKey] = q.ID
		}
	}
}

// bindsOf extracts the named host binds in first-appearance order plus the
// total bind arity (named + positional :N placeholders).
func bindsOf(sql string) (binds []string, arity int) {
	seen := map[string]bool{}
	for _, m := range hostRefRe.FindAllStringSubmatch(sql, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			binds = append(binds, m[1])
		}
	}
	arity = len(binds) + len(positionalRe.FindAllString(sql, -1))
	return binds, arity
}

// bindSource removes the INTO clause of a SELECT before bind extraction:
// INTO targets are outputs (RowShape), never input binds.
func bindSource(sql string) string {
	loc := intoRe.FindStringIndex(sql)
	if loc == nil {
		return sql
	}
	end := len(sql)
	if loc2 := fromRe.FindStringIndex(sql[loc[1]:]); loc2 != nil {
		end = loc[1] + loc2[0]
	}
	return sql[:loc[0]] + " " + sql[end:]
}

// intoList parses an INTO/FETCH-INTO host list: everything after INTO up to
// FROM or end, one host var per comma (indicator suffixes dropped).
func intoList(sql string) []string {
	loc := intoRe.FindStringIndex(sql)
	if loc == nil {
		return nil
	}
	rest := sql[loc[1]:]
	if loc2 := fromRe.FindStringIndex(rest); loc2 != nil {
		rest = rest[:loc2[0]]
	}
	var out []string
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, ":")
		// :name:indicator → name
		if i := strings.Index(part, ":"); i >= 0 {
			part = part[:i]
		}
		if part != "" {
			out = append(out, strings.TrimSpace(part))
		}
	}
	return out
}

// aliasesOf extracts the SELECT list's `AS "X"` column aliases from the raw
// SQL, in select order — position-aligned with the FETCH-INTO row shape so
// the generated row models carry the alias as their db tag (§4.8.2).
func aliasesOf(rawSQL string) []string {
	var out []string
	for _, m := range aliasRe.FindAllStringSubmatch(rawSQL, -1) {
		out = append(out, m[1])
	}
	return out
}

// orderByOf returns the ORDER BY tail of a SELECT ("" when absent).
func orderByOf(sql string) string {
	loc := orderByRe.FindStringIndex(sql)
	if loc == nil {
		return ""
	}
	return strings.TrimSpace(sql[loc[1]:])
}

// tablesFromSelect parses the FROM list of a SELECT (comma-separated tables,
// aliases dropped).
func tablesFromSelect(sql string) []string {
	loc := fromRe.FindStringIndex(sql)
	if loc == nil {
		return nil
	}
	rest := sql[loc[1]:]
	if loc2 := whereRe.FindStringIndex(rest); loc2 != nil {
		rest = rest[:loc2[0]]
	}
	var out []string
	for _, part := range strings.Split(rest, ",") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		out = append(out, fields[0])
	}
	return out
}

// tablesFromDML parses the target table of INSERT/UPDATE/DELETE/MERGE
// (MERGE accepts both `MERGE INTO t` and bare `MERGE t`).
func tablesFromDML(sql string, qt QueryType) []string {
	upper := strings.ToUpper(sql)
	var markers []string
	switch qt {
	case QueryInsert:
		markers = []string{"INSERT INTO"}
	case QueryUpdate:
		markers = []string{"UPDATE"}
	case QueryDelete:
		markers = []string{"DELETE FROM"}
	case QueryMerge:
		markers = []string{"MERGE INTO", "MERGE"}
	}
	for _, marker := range markers {
		idx := strings.Index(upper, marker)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(sql[idx+len(marker):])
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil
		}
		return []string{fields[0]}
	}
	return nil
}

// buildConditions reconstructs the entry function's top-level if/else-if/else
// chains (scanner branch records at body depth 1; C syntax guarantees an
// else/elseif continues the most recent same-depth branch in its function)
// and keeps only multi-branch chains as endpoint candidates (§4.2.8). For
// fragments (PF-3.3) a single-branch chain is kept too — a lone if is a
// one-endpoint fragment — and a fragment with no top-level chain at all
// becomes one endpoint covering the whole block (design decision 3).
func buildConditions(facts *scanner.SourceFacts, entry string, queries []*Query) []Condition {
	minChain := 2
	if facts.Fragment {
		minChain = 1
	}
	var chain []Condition
	var conditions []Condition
	flush := func() {
		if len(chain) >= minChain {
			for i := range chain {
				chain[i].Index = len(conditions) + i + 1
			}
			conditions = append(conditions, chain...)
		}
		chain = nil
	}

	decls := declNames(facts)
	for _, b := range facts.Branches {
		if b.Function != entry || b.Depth != 1 || b.BlockStart == 0 {
			continue
		}
		switch b.Kind {
		case scanner.BranchIf:
			flush()
			cond := conditionOfBranch(b, decls)
			chain = append(chain, cond)
		case scanner.BranchElseIf:
			if len(chain) == 0 {
				flush()
			}
			cond := conditionOfBranch(b, decls)
			chain = append(chain, cond)
		case scanner.BranchElse:
			if len(chain) == 0 {
				continue
			}
			chain = append(chain, Condition{Kind: string(b.Kind), StartLine: b.StartLine, EndLine: b.BlockEnd, IsDefault: true})
		}
	}
	flush()

	if facts.Fragment && len(conditions) == 0 {
		// A chainless fragment is one endpoint unit over its whole body.
		conditions = append(conditions, Condition{
			Index:     1,
			Kind:      "else",
			IsDefault: true,
			StartLine: 1,
			EndLine:   facts.NumLines,
		})
	}

	for i := range conditions {
		c := &conditions[i]
		c.FmlOps = fmlOpsInRange(facts, entry, c.StartLine, c.EndLine)
		c.QueryIDs = queryIDsInRange(queries, c.StartLine, c.EndLine)
	}
	return conditions
}

// conditionOfBranch builds one condition record: raw text, the parsed
// predicate tree (PF-2), and the flag vars resolved from the tree's
// Ident/comparison leaves against the file's declared vars (PF-2.2). Raw
// conditions keep the substring fallback so no result regresses.
func conditionOfBranch(b scanner.Branch, decls map[string]bool) Condition {
	cond := Condition{Kind: string(b.Kind), Expr: b.Cond, StartLine: b.StartLine, EndLine: b.BlockEnd}
	if b.Cond != "" {
		tree := pred.Parse(b.Cond)
		cond.Predicate = &tree
		cond.FlagVars = flagVarsOf(&tree, b.Cond, decls)
	}
	return cond
}

// flagVarsOf picks the condition's flag variables from the predicate tree
// (PF-2.2): Ident leaves — including the Ident sides of comparison leaves —
// count iff they resolve against the declared vars. Raw-degraded conditions
// fall back to the substring tokenization of the raw text.
func flagVarsOf(tree *pred.Expr, rawCond string, decls map[string]bool) []string {
	var out []string
	if tree != nil && !tree.IsRaw() {
		for _, name := range pred.Idents(tree) {
			if decls[name] {
				out = append(out, name)
			}
		}
		return out
	}
	for _, tok := range strings.FieldsFunc(rawCond, func(r rune) bool {
		return !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		if decls[tok] {
			out = append(out, tok)
		}
	}
	return out
}

// entryFmlOps collects the FML ops the entry function performs outside the
// candidate branches (the preamble: session reads, the flag read, error
// adds). Branch-internal ops live on their Condition.
func entryFmlOps(facts *scanner.SourceFacts, entry string, conditions []Condition) []FmlOp {
	covered := func(line int) bool {
		for _, c := range conditions {
			if line >= c.StartLine && line <= c.EndLine {
				return true
			}
		}
		return false
	}
	lo, hi := 0, int(^uint(0)>>1)
	for _, fn := range facts.Functions {
		if fn.Name == entry {
			lo = fn.StartLine
			if fn.BodyEndLine > 0 {
				hi = fn.BodyEndLine
			}
		}
	}
	var out []FmlOp
	for i := range facts.Calls {
		call := &facts.Calls[i]
		if call.Func != entry || call.Line < lo || call.Line > hi || covered(call.Line) {
			continue
		}
		if op, ok := fmlOpOf(call, facts); ok {
			out = append(out, op)
		}
	}
	return out
}

// fmlOpsInRange builds the FML ops of one branch body.
func fmlOpsInRange(facts *scanner.SourceFacts, entry string, start, end int) []FmlOp {
	var out []FmlOp
	for i := range facts.Calls {
		call := &facts.Calls[i]
		if call.Func != entry || call.Line < start || call.Line > end {
			continue
		}
		if op, ok := fmlOpOf(call, facts); ok {
			out = append(out, op)
		}
	}
	return out
}

// FmlOpOf exports the Fget32/Fadd32 classifier for consumers outside the ir
// package (FLW-2: the flow statement tree tags FML ops without re-deriving
// the call shape). See fmlOpOf for the argument conventions.
func FmlOpOf(call *scanner.FunctionCall, facts *scanner.SourceFacts) (FmlOp, bool) {
	return fmlOpOf(call, facts)
}

// fmlOpOf converts an Fget32/Fadd32 call into an FmlOp: field = arg 2,
// target = the host variable written (get, arg 4) or read (add, arg 3),
// buffer = the buffer variable the op ran on (PF-4.2, arg 1).
// Optional marks FNOTPRES-guarded reads (§4.8.3).
func fmlOpOf(call *scanner.FunctionCall, facts *scanner.SourceFacts) (FmlOp, bool) {
	var kind FmlOpKind
	switch call.Name {
	case "Fget32":
		kind = FmlGet
	case "Fadd32":
		kind = FmlAdd
	default:
		return FmlOp{}, false
	}
	args := splitArgs(call.Args)
	if len(args) < 2 {
		return FmlOp{}, false
	}
	field := strings.TrimSpace(args[1])
	op := FmlOp{Kind: kind, Field: field, Line: call.Line, Dropped: droppedFmlFields[field]}
	if len(args) > 0 {
		op.Buffer = normalizeTarget(args[0])
	}

	idx := 2
	if kind == FmlGet && len(args) > 3 {
		idx = 3 // (buf, field, occurrence, value, len)
	}
	if idx < len(args) {
		op.Target = normalizeTarget(args[idx])
	}
	if kind == FmlGet {
		op.Optional = fnotpresGuard(facts, call)
	}
	return op, true
}

// fnotpresGuard reports whether the ==-1 guard wrapping this Fget32 reads
// Ferror32 == FNOTPRES in its block (deterministic optionality mark).
func fnotpresGuard(facts *scanner.SourceFacts, call *scanner.FunctionCall) bool {
	var guard *scanner.Branch
	for i := range facts.Branches {
		b := &facts.Branches[i]
		if b.Function != call.Func || !strings.Contains(b.Cond, "Fget32") {
			continue
		}
		if call.Line >= b.StartLine && call.Line <= b.StartLine+3 {
			guard = b
			break
		}
	}
	if guard == nil || guard.BlockStart == 0 {
		return false
	}
	for i := range facts.Branches {
		b := &facts.Branches[i]
		if b.Function == call.Func && strings.Contains(b.Cond, "FNOTPRES") &&
			b.StartLine > guard.StartLine && b.StartLine <= guard.BlockEnd {
			return true
		}
	}
	return false
}

// splitArgs splits a raw call argument list on top-level commas.
func splitArgs(args string) []string {
	if strings.TrimSpace(args) == "" {
		return nil
	}
	var out []string
	depth := 0
	var cur strings.Builder
	inStr, inChar := false, false
	for i := 0; i < len(args); i++ {
		c := args[i]
		switch {
		case inStr:
			if c == '\\' && i+1 < len(args) {
				cur.WriteByte(c)
				i++
				cur.WriteByte(args[i])
				continue
			}
			if c == '"' {
				inStr = false
			}
		case inChar:
			if c == '\\' && i+1 < len(args) {
				cur.WriteByte(c)
				i++
				cur.WriteByte(args[i])
				continue
			}
			if c == '\'' {
				inChar = false
			}
		case c == '"':
			inStr = true
		case c == '\'':
			inChar = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}

// normalizeTarget reduces a call argument to the bare host-variable name:
// casts dropped, & dropped, varchar .arr dropped, literals become "".
func normalizeTarget(arg string) string {
	s := strings.TrimSpace(arg)
	for strings.HasPrefix(s, "(") {
		end := strings.Index(s, ")")
		if end < 0 {
			break
		}
		s = strings.TrimSpace(s[end+1:])
	}
	s = strings.TrimPrefix(s, "&")
	if i := strings.Index(s, "."); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" || isNumericLiteral(s) {
		return ""
	}
	if !isIdentLike(s) {
		return ""
	}
	return s
}

func isNumericLiteral(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != '-' {
			return false
		}
	}
	return true
}

func isIdentLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// declNames maps every declared variable name in the file.
func declNames(facts *scanner.SourceFacts) map[string]bool {
	out := make(map[string]bool, len(facts.VarDecls))
	for _, d := range facts.VarDecls {
		out[d.Name] = true
	}
	return out
}

// queryIDsInRange links a branch to the query units whose sites fall in its
// body.
func queryIDsInRange(queries []*Query, start, end int) []string {
	var out []string
	for _, q := range queries {
		for _, site := range q.Sites {
			if site >= start && site <= end {
				out = append(out, q.ID)
				break
			}
		}
	}
	return out
}

// buildExternalFns lists called-but-undefined project symbols (fn_*/chk_*),
// comment-aware via the scanner.
func buildExternalFns(facts *scanner.SourceFacts) []ExternalFn {
	local := map[string]bool{}
	for _, fn := range facts.Functions {
		local[fn.Name] = true
	}
	seen := map[string]*ExternalFn{}
	var names []string
	for i := range facts.Calls {
		call := &facts.Calls[i]
		if !call.IsFnPref && !call.IsChkPref {
			continue
		}
		if local[call.Name] {
			continue
		}
		if _, ok := seen[call.Name]; !ok {
			seen[call.Name] = &ExternalFn{Name: call.Name}
			names = append(names, call.Name)
		}
		seen[call.Name].Callsites = append(seen[call.Name].Callsites, call.Line)
	}
	sort.Strings(names)
	out := make([]ExternalFn, 0, len(names))
	for _, name := range names {
		out = append(out, *seen[name])
	}
	return out
}

// buildHostVars assembles every host variable referenced by a query bind,
// row shape, or FML target, typed from scanned declarations when available.
func buildHostVars(facts *scanner.SourceFacts, f *File) []HostVar {
	sections := declareSections(facts)
	decls := map[string]scanner.VarDecl{}
	for _, d := range facts.VarDecls {
		decls[d.Name] = d
	}

	referenced := map[string]bool{}
	add := func(name string) {
		if name != "" {
			referenced[name] = true
		}
	}
	for _, q := range f.Queries {
		for _, b := range q.Binds {
			add(b)
		}
		for _, r := range q.RowShape {
			add(r)
		}
	}
	for _, op := range f.FmlOps {
		add(op.Target)
	}
	for _, c := range f.Conditions {
		for _, op := range c.FmlOps {
			add(op.Target)
		}
		for _, fv := range c.FlagVars {
			add(fv)
		}
	}

	names := make([]string, 0, len(referenced))
	for name := range referenced {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]HostVar, 0, len(names))
	for _, name := range names {
		d, ok := decls[name]
		hv := HostVar{Name: name, FromHeader: !ok}
		if ok {
			hv.CType = d.Type
			hv.GoHint = goHintOf(d.Type)
			hv.Array = d.Array
			hv.Nullable = d.Type == "varchar"
			for _, s := range sections {
				if d.Line > s.start && d.Line < s.end {
					hv.InDeclareSection = true
					break
				}
			}
		}
		out = append(out, hv)
	}
	return out
}

// declareSections pairs EXEC SQL BEGIN/END DECLARE SECTION extents.
func declareSections(facts *scanner.SourceFacts) []fnRange {
	var out []fnRange
	var open *scanner.ExecSQLStatement
	for i := range facts.AllSQL {
		stmt := &facts.AllSQL[i]
		if stmt.Kind != scanner.SQLDeclareSection {
			continue
		}
		upper := strings.ToUpper(stmt.Normalized)
		if strings.Contains(upper, "BEGIN") {
			open = stmt
		} else if strings.Contains(upper, "END") && open != nil {
			out = append(out, fnRange{start: open.StartLine, end: stmt.EndLine})
			open = nil
		}
	}
	return out
}

// goHintOf maps a scanned C/Pro*C base type to a Go type hint.
func goHintOf(ctype string) string {
	switch {
	case strings.Contains(ctype, "varchar"):
		return "string"
	case strings.Contains(ctype, "char"):
		return "string"
	case strings.Contains(ctype, "double"):
		return "float64"
	case strings.Contains(ctype, "float"):
		return "float32"
	case strings.Contains(ctype, "short"):
		return "int16"
	case strings.Contains(ctype, "long"):
		return "int64"
	case strings.Contains(ctype, "int"):
		return "int"
	default:
		return ""
	}
}
