package analyzer

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

// Marks are the OQ18 scoring knobs. Defaults: +1 per query, +5 per simple
// external fn, +10 per complex external fn, +20 per tpcall, +1 per unit of
// branching factor (each if/else-if header contributes +1 × 2^(number of
// enclosing if/else-if blocks); else headers never contribute). Every mark
// is settable in the generated CSV's `# tuxgo marks:` line (and in the marks
// cells row, which wins) and re-applied via -weights (LoadOptionsCSV) —
// scoring never requires a code change.
type Marks struct {
	Query   int
	Simple  int
	Complex int
	TpCall  int
	Branch  int
}

// DefaultMarks returns the OQ18 rubric defaults.
func DefaultMarks() Marks {
	return Marks{Query: 1, Simple: 5, Complex: 10, TpCall: 20, Branch: 1}
}

// Options carries all analyzer tuning for one run: the rubric Marks plus
// per-fn weight overrides. Precedence when scoring an external fn:
// per-fn override (FnWeights) > tier mark (Marks.Simple/Complex applied via
// classification) > conversion-name fallback classification. A per-fn
// override cleared in the CSV (`name:class:`) re-enables the tier mark.
type Options struct {
	Marks     Marks
	FnWeights map[string]int
}

// DefaultOptions returns the stock configuration.
func DefaultOptions() Options {
	return Options{Marks: DefaultMarks()}
}

// ExternalFnClass classifies a called-but-not-locally-defined function by how
// much conversion work it implies (PRD §4.2.9, §4.8.7).
type ExternalFnClass string

const (
	// ExtComplex — data-access or middleware behaviour: the defining body
	// contains EXEC SQL and/or tpcall, or the definition is unavailable and
	// the name is not conversion-shaped (conservative default). Weight +10.
	ExtComplex ExternalFnClass = "complex"
	// ExtSimple — pure logic/conversion utility: the defining body has no
	// SQL/tpcall, or (unresolved) the name is conversion-shaped
	// (e.g. fn_long_to_int). Weight +5.
	ExtSimple ExternalFnClass = "simple"
)

// ExternalFn describes one called-but-not-locally-defined function.
type ExternalFn struct {
	Name      string
	Class     ExternalFnClass
	Weight    int
	Resolved  bool   // defining file found in the scanned corpus
	DefinedIn string // defining file path when Resolved
	HasSQL    bool   // defining body contains EXEC SQL
}

// isConversionName is the deterministic fallback for unresolved symbols:
// conversion-style names (fn_long_to_int, str_to_date, ...) are utility logic.
func isConversionName(name string) bool {
	return strings.Contains(strings.ToLower(name), "_to_")
}

// fnDefInfo records where a function is defined and whether its body touches SQL/tpcall.
type fnDefInfo struct {
	File      string
	StartLine int
	HasSQL    bool
	HasTpCall bool
}

// corpus maps every function definition discovered across the scanned set.
type corpus map[string]fnDefInfo

// buildCorpus attributes each file's queries and tpcall invocations to the
// function whose body (StartLine .. next function StartLine) contains them.
func buildCorpus(all []*scanner.SourceFacts) corpus {
	c := make(corpus)
	for _, facts := range all {
		fns := make([]scanner.FunctionDef, len(facts.Functions))
		copy(fns, facts.Functions)
		sort.Slice(fns, func(i, j int) bool { return fns[i].StartLine < fns[j].StartLine })

		for i, fn := range fns {
			end := int(^uint(0) >> 1) // +Inf sentinel: body runs to EOF for the last fn
			if i+1 < len(fns) {
				end = fns[i+1].StartLine
			}
			info := fnDefInfo{File: facts.Path, StartLine: fn.StartLine}
			for _, q := range facts.Queries {
				if q.StartLine >= fn.StartLine && q.StartLine < end {
					info.HasSQL = true
					break
				}
			}
			for _, call := range facts.Calls {
				if call.IsTpCall && call.Line >= fn.StartLine && call.Line < end {
					info.HasTpCall = true
					break
				}
			}
			c[fn.Name] = info
		}
	}
	return c
}

// classifyExternal applies the two-tier rule to one external call, using the
// run's marks as tier weights. A per-fn override in opts.FnWeights (edited
// into a previously written CSV and loaded via LoadOptionsCSV) is
// authoritative: chk_session:complex:0 removes the session check from scoring
// without touching code.
func classifyExternal(name string, c corpus, opts Options) ExternalFn {
	var fn ExternalFn
	if def, ok := c[name]; ok {
		fn = ExternalFn{Name: name, Resolved: true, DefinedIn: def.File, HasSQL: def.HasSQL}
		if def.HasSQL || def.HasTpCall {
			fn.Class = ExtComplex
			fn.Weight = opts.Marks.Complex
		} else {
			fn.Class = ExtSimple
			fn.Weight = opts.Marks.Simple
		}
	} else if isConversionName(name) {
		fn = ExternalFn{Name: name, Class: ExtSimple, Weight: opts.Marks.Simple}
	} else {
		fn = ExternalFn{Name: name, Class: ExtComplex, Weight: opts.Marks.Complex}
	}
	if w, ok := opts.FnWeights[name]; ok {
		fn.Weight = w
	}
	return fn
}

// Report holds the triage complexity analysis results for a Pro*C/Tuxedo file.
// Query-type counts (select/insert/update/delete/merge) sum to NumQueries —
// every query unit counts, dedup-inclusive, matching the score's query term.
type Report struct {
	File            string
	NumLines        int
	NumQueries      int
	BranchCount     int // if/else-if headers (else never contributes)
	BranchingFactor int // doubling-weighted: Σ 2^(enclosing-if depth)
	SelectCount     int
	InsertCount     int
	UpdateCount     int
	DeleteCount     int
	MergeCount      int
	HasTpCall       bool
	TpCallCount     int
	FnLocalCount    int
	FnExternalCount int
	LocalFns        []string
	ExternalFns     []ExternalFn
	ComplexityScore int
	Complexity      string // LOW, MEDIUM, HIGH
	Reasons         string
}

// analyzeFacts computes the Report for one file's facts against a corpus of
// known function definitions (may be empty for single-file mode) and the
// run's scoring options (marks + per-fn overrides).
func analyzeFacts(facts *scanner.SourceFacts, c corpus, opts Options) *Report {
	facts = commentLiveFacts(facts)
	// 1. Identify locally defined functions
	localDefMap := make(map[string]bool)
	var localFns []string
	var localFnPrefCount int

	for _, fn := range facts.Functions {
		localDefMap[fn.Name] = true
		localFns = append(localFns, fn.Name)
		if strings.HasPrefix(fn.Name, "fn_") {
			localFnPrefCount++
		}
	}

	// 2. Identify external function calls: only project-convention symbols
	// (fn_*/chk_* prefixes) are conversion-relevant (PRD §4.2.9, §4.8.4) —
	// everything else (C stdlib, POSIX, Tuxedo ATMI, FML buffer ops) is a
	// dropped construct and never counts toward complexity.
	externalCallsMap := make(map[string]bool)
	for _, call := range facts.Calls {
		if !call.IsFnPref && !call.IsChkPref {
			continue
		}
		if localDefMap[call.Name] {
			continue
		}
		externalCallsMap[call.Name] = true
	}

	externalFns := make([]ExternalFn, 0, len(externalCallsMap))
	for name := range externalCallsMap {
		externalFns = append(externalFns, classifyExternal(name, c, opts))
	}
	sort.Slice(externalFns, func(i, j int) bool { return externalFns[i].Name < externalFns[j].Name })
	sort.Strings(localFns)

	numQueries := len(facts.Queries)
	hasTpCall := facts.TpCallCount > 0

	// Per-type query counts (select counts direct SELECTs and flattened
	// cursors alike; every unit counts, dedup-inclusive).
	selectCount, insertCount, updateCount, deleteCount, mergeCount := 0, 0, 0, 0, 0
	for _, q := range facts.Queries {
		switch q.Kind {
		case scanner.SQLSelect, scanner.SQLDeclareCursor:
			selectCount++
		case scanner.SQLInsert:
			insertCount++
		case scanner.SQLUpdate:
			updateCount++
		case scanner.SQLDelete:
			deleteCount++
		case scanner.SQLMerge:
			mergeCount++
		}
	}

	// Branching factor: each if/else-if header contributes +1 × 2^nest
	// (NestDepth = enclosing if/else-if blocks; else headers never
	// contribute).
	branchCount, branchFactor := 0, 0
	for _, b := range facts.Branches {
		if b.Kind == scanner.BranchElse {
			continue
		}
		branchCount++
		branchFactor += 1 << b.NestDepth
	}

	// OQ18 rubric with the run's marks: query/tpcall/branch per count,
	// externals by effective weight (per-fn override or tier mark).
	extWeight := 0
	for _, fn := range externalFns {
		extWeight += fn.Weight
	}
	score := (numQueries * opts.Marks.Query) + extWeight + (facts.TpCallCount * opts.Marks.TpCall) + (branchFactor * opts.Marks.Branch)

	tier := "LOW"
	if score >= 30 {
		tier = "HIGH"
	} else if score >= 10 {
		tier = "MEDIUM"
	}

	var reasonParts []string
	if numQueries == 1 {
		reasonParts = append(reasonParts, fmt.Sprintf("1 query (+%d)", numQueries*opts.Marks.Query))
	} else {
		reasonParts = append(reasonParts, fmt.Sprintf("%d queries (+%d)", numQueries, numQueries*opts.Marks.Query))
	}
	if len(externalFns) > 0 {
		parts := make([]string, len(externalFns))
		for i, fn := range externalFns {
			parts[i] = fmt.Sprintf("%s:%s +%d", fn.Name, fn.Class, fn.Weight)
		}
		reasonParts = append(reasonParts, fmt.Sprintf("%d external fns (%s) (+%d)", len(externalFns), strings.Join(parts, ", "), extWeight))
	} else {
		reasonParts = append(reasonParts, "0 external fns (+0)")
	}
	reasonParts = append(reasonParts, fmt.Sprintf("%d tpcall (+%d)", facts.TpCallCount, facts.TpCallCount*opts.Marks.TpCall))
	reasonParts = append(reasonParts, fmt.Sprintf("branching factor %d over %d branch headers (+%d)", branchFactor, branchCount, branchFactor*opts.Marks.Branch))

	return &Report{
		File:            facts.Path,
		NumLines:        facts.NumLines,
		NumQueries:      numQueries,
		BranchCount:     branchCount,
		BranchingFactor: branchFactor,
		SelectCount:     selectCount,
		InsertCount:     insertCount,
		UpdateCount:     updateCount,
		DeleteCount:     deleteCount,
		MergeCount:      mergeCount,
		HasTpCall:       hasTpCall,
		TpCallCount:     facts.TpCallCount,
		FnLocalCount:    localFnPrefCount,
		FnExternalCount: len(externalFns),
		LocalFns:        localFns,
		ExternalFns:     externalFns,
		ComplexityScore: score,
		Complexity:      tier,
		Reasons:         strings.Join(reasonParts, "; "),
	}
}

// commentLiveFacts excludes any call or SQL statement whose start position
// falls inside a recorded comment span (PF-1.5): with the comment inventory
// in place the exclusion is a deterministic overlap query — testable, not
// incidental. Recorded facts never sit inside comments (the scanner skips
// them), so on healthy inputs this is a no-op and scores cannot drift.
func commentLiveFacts(facts *scanner.SourceFacts) *scanner.SourceFacts {
	if len(facts.Comments) == 0 {
		return facts
	}
	live := *facts
	live.Calls = nil
	live.Queries = nil
	for _, c := range facts.Calls {
		if !facts.InComment(c.Line, c.Col) {
			live.Calls = append(live.Calls, c)
		}
	}
	for _, q := range facts.Queries {
		if !facts.InComment(q.StartLine, q.StartCol) {
			live.Queries = append(live.Queries, q)
		}
	}
	return &live
}

// AnalyzeFile evaluates a single .pc/.pcf file using the OQ18 complexity rubric.
// External functions cannot be resolved against a corpus in this mode: they are
// weighted by the conversion-name fallback (simple) or conservatively complex.
// opts carries the run's marks and per-fn overrides (LoadOptionsCSV / -weights).
func AnalyzeFile(path string, opts Options) (*Report, error) {
	facts, err := scanner.ScanFile(path)
	if err != nil {
		return nil, err
	}
	return analyzeFacts(facts, nil, opts), nil
}

// AnalyzeDir walks a folder recursively, analyzing all .pc and .pcf files found.
// Definitions discovered anywhere in the tree resolve external calls, so a
// fn_* defined in a sibling file is classified by its real body (SQL-bearing
// => complex, pure logic => simple). opts carries the run's marks and per-fn
// overrides (LoadOptionsCSV / -weights).
func AnalyzeDir(dir string, opts Options) ([]*Report, error) {
	var paths []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".pc" || ext == ".pcf" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Pass 1: scan every file and build the definition corpus.
	all := make([]*scanner.SourceFacts, 0, len(paths))
	for _, p := range paths {
		facts, err := scanner.ScanFile(p)
		if err != nil {
			return nil, fmt.Errorf("error scanning %s: %w", p, err)
		}
		all = append(all, facts)
	}
	c := buildCorpus(all)

	// Pass 2: analyze each file with full resolution.
	reports := make([]*Report, 0, len(all))
	for _, facts := range all {
		reports = append(reports, analyzeFacts(facts, c, opts))
	}

	// Sort descending by complexity score, then ascending by filename
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].ComplexityScore != reports[j].ComplexityScore {
			return reports[i].ComplexityScore > reports[j].ComplexityScore
		}
		return reports[i].File < reports[j].File
	})

	return reports, nil
}

// WriteCSV exports analysis reports in the OQ18 CSV format. The CSV is both
// the triage report and the single tuning surface, in four layers:
//
//   - Row 1, the `# tuxgo marks:` comment: every rubric mark in one
//     machine-readable line (edit `query=… tpcall=… branch=…` there).
//   - Row 2, the marks cells: the marks that score a column sit in cells
//     aligned over that column (num_queries, branching_factor, tpcall_count),
//     so the score formulas can reference them. These cells win over the
//     comment line when a CSV is fed back via LoadOptionsCSV / --weights.
//   - Row 3, the header; data rows follow. The external_fns cell names every
//     external call as name:class:weight (edit the weight, or clear it with
//     `name:class:` to fall back to the tier mark).
//   - complexity_score and complexity are spreadsheet formulas (e.g.
//     =C$2*C4+D$2*D4+G$2*G4+K4 and =IF(Q4>=30,"HIGH",…)) so a spreadsheet
//     recalculates when marks cells change. Programmatic consumers recompute
//     the score from the row instead (the formula string is not a number).
//
// Data rows are self-contained — score = num_queries*query +
// branching_factor*branch + external_weight + tpcall_count*tpcall. Tools
// consuming the tabular data can skip the marks line (csv.Reader.Comment =
// '#').
func WriteCSV(w io.Writer, reports []*Report, marks Marks) error {
	writer := csv.NewWriter(w)
	defer writer.Flush()

	marksLine := fmt.Sprintf("# tuxgo marks: query=%d simple=%d complex=%d tpcall=%d branch=%d",
		marks.Query, marks.Simple, marks.Complex, marks.TpCall, marks.Branch)
	if err := writer.Write([]string{marksLine}); err != nil {
		return err
	}

	header := []string{
		"file",
		"num_lines",
		"num_queries",
		"branching_factor",
		"branch_count",
		"has_tpcall",
		"tpcall_count",
		"fn_local_count",
		"fn_external_count",
		"external_fns",
		"external_weight",
		"select_count",
		"insert_count",
		"update_count",
		"delete_count",
		"merge_count",
		"complexity_score",
		"complexity",
		"reasons",
	}

	// Marks cells row (spreadsheet row 2, above the header): the mark that
	// scores each factor column sits over that column, giving the score
	// formulas their absolute references. external_weight has no mark — the
	// per-fn weights are already summed in the cell.
	idx := make(map[string]int, len(header))
	for i, name := range header {
		idx[name] = i + 1
	}
	marksRow := make([]string, len(header))
	for name, mark := range map[string]int{
		"num_queries":      marks.Query,
		"branching_factor": marks.Branch,
		"tpcall_count":     marks.TpCall,
	} {
		marksRow[idx[name]-1] = strconv.Itoa(mark)
	}
	if err := writer.Write(marksRow); err != nil {
		return err
	}

	if err := writer.Write(header); err != nil {
		return err
	}

	qCol := colLetters(idx["num_queries"])
	bCol := colLetters(idx["branching_factor"])
	tCol := colLetters(idx["tpcall_count"])
	wCol := colLetters(idx["external_weight"])
	sCol := colLetters(idx["complexity_score"])

	// Data rows start at spreadsheet row 4 (marks line 1, marks cells 2,
	// header 3); the first data row is row 4.
	for i, r := range reports {
		rowNo := i + 4
		fnCells := make([]string, 0, len(r.ExternalFns))
		extWeight := 0
		for _, fn := range r.ExternalFns {
			fnCells = append(fnCells, fmt.Sprintf("%s:%s:%d", fn.Name, fn.Class, fn.Weight))
			extWeight += fn.Weight
		}
		scoreFormula := fmt.Sprintf("=%s$2*%s%d+%s$2*%s%d+%s$2*%s%d+%s%d",
			qCol, qCol, rowNo,
			bCol, bCol, rowNo,
			tCol, tCol, rowNo,
			wCol, rowNo)
		tierFormula := fmt.Sprintf("=IF(%s%d>=30,\"HIGH\",IF(%s%d>=10,\"MEDIUM\",\"LOW\"))", sCol, rowNo, sCol, rowNo)
		row := []string{
			r.File,
			strconv.Itoa(r.NumLines),
			strconv.Itoa(r.NumQueries),
			strconv.Itoa(r.BranchingFactor),
			strconv.Itoa(r.BranchCount),
			strconv.FormatBool(r.HasTpCall),
			strconv.Itoa(r.TpCallCount),
			strconv.Itoa(r.FnLocalCount),
			strconv.Itoa(r.FnExternalCount),
			strings.Join(fnCells, ";"),
			strconv.Itoa(extWeight),
			strconv.Itoa(r.SelectCount),
			strconv.Itoa(r.InsertCount),
			strconv.Itoa(r.UpdateCount),
			strconv.Itoa(r.DeleteCount),
			strconv.Itoa(r.MergeCount),
			scoreFormula,
			tierFormula,
			r.Reasons,
		}
		if err := writer.Write(row); err != nil {
			return err
		}
	}

	return writer.Error()
}

// colLetters converts a 1-based spreadsheet column index to its letters
// (1→A, 26→Z, 27→AA).
func colLetters(i int) string {
	letters := ""
	for i > 0 {
		i--
		letters = string(rune('A'+i%26)) + letters
		i /= 26
	}
	return letters
}

// LoadOptionsCSV reads a previously generated analysis CSV back into Options:
// the rubric marks — from the marks cells row when present (cells win), else
// the `# tuxgo marks:` line (missing marks keep the defaults) — and the
// per-fn weights from the external_fns column. A per-fn weight cleared in
// the file (`name:class:`) falls back to the tier mark. Formula cells
// (complexity_score/complexity) are ignored: re-scoring recomputes them.
// This is the re-score entry point — edit marks or weights in the CSV and
// re-run analyze with -weights pointing at it. Unknown mark keys and
// malformed values are errors: a typo must never silently keep a default. On
// duplicate fn names the last row wins.
func LoadOptionsCSV(path string) (Options, error) {
	opts := DefaultOptions()
	opts.FnWeights = make(map[string]int)

	f, err := os.Open(path)
	if err != nil {
		return opts, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1 // the leading marks line is a single-cell record
	records, err := reader.ReadAll()
	if err != nil {
		return opts, fmt.Errorf("reading weights CSV %s: %w", path, err)
	}
	if len(records) == 0 {
		return opts, fmt.Errorf("weights CSV %s is empty", path)
	}

	col := -1
	var prev []string
	for _, rec := range records {
		if len(rec) == 0 {
			continue
		}
		first := strings.TrimSpace(rec[0])
		if strings.HasPrefix(first, "#") {
			if err := applyMarksLine(first, &opts.Marks); err != nil {
				return opts, fmt.Errorf("%s: %w", path, err)
			}
			prev = rec
			continue
		}
		if first == "file" {
			// The table-shaped record immediately above the header is the
			// marks cells row; its cells override the comment line.
			if err := applyMarksRow(prev, rec, &opts.Marks); err != nil {
				return opts, fmt.Errorf("%s: %w", path, err)
			}
			for i, name := range rec {
				if name == "external_fns" {
					col = i
					break
				}
			}
			continue
		}
		prev = rec
		if col < 0 || len(rec) <= col {
			continue
		}
		for _, tuple := range strings.Split(rec[col], ";") {
			tuple = strings.TrimSpace(tuple)
			if tuple == "" {
				continue
			}
			parts := strings.Split(tuple, ":")
			if len(parts) < 2 || len(parts) > 3 {
				return opts, fmt.Errorf("malformed external fn %q in %s", tuple, path)
			}
			name := strings.TrimSpace(parts[0])
			if name == "" {
				return opts, fmt.Errorf("malformed external fn %q in %s", tuple, path)
			}
			if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
				w, err := strconv.Atoi(strings.TrimSpace(parts[2]))
				if err != nil {
					return opts, fmt.Errorf("bad weight in %q (%s): %w", tuple, path, err)
				}
				opts.FnWeights[name] = w
			}
			// Two parts or cleared third part: no override — the tier mark
			// applies at classification time (documented precedence).
		}
	}
	if col < 0 {
		return opts, fmt.Errorf("weights CSV %s has no external_fns column (expected a tuxgo analyze CSV)", path)
	}
	return opts, nil
}

// applyMarksRow parses one marks cells row — the record immediately above
// the header whose cells carry the marks aligned over their factor columns
// (num_queries → query, branching_factor → branch, tpcall_count → tpcall).
// A nil row, a comment line, a length mismatch (pre-formula CSV), or an
// all-empty row is a no-op so legacy CSVs keep loading; a non-numeric cell
// under a factor column is an error so mis-edits surface instead of
// silently keeping a default.
func applyMarksRow(row, header []string, m *Marks) error {
	if row == nil || len(row) == 0 || len(row) != len(header) {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(row[0]), "#") {
		return nil
	}
	for i, name := range header {
		var target *int
		switch name {
		case "num_queries":
			target = &m.Query
		case "branching_factor":
			target = &m.Branch
		case "tpcall_count":
			target = &m.TpCall
		default:
			continue
		}
		cell := strings.TrimSpace(row[i])
		if cell == "" {
			continue
		}
		n, err := strconv.Atoi(cell)
		if err != nil {
			return fmt.Errorf("bad mark cell %q in column %s", cell, name)
		}
		*target = n
	}
	return nil
}

// applyMarksLine parses one `# tuxgo marks: query=1 simple=5 …` comment into
// m. Unrelated comment lines are ignored; a malformed or unknown mark is an
// error so mis-edits surface instead of silently keeping a default.
func applyMarksLine(field string, m *Marks) error {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(field), "#"))
	if !strings.HasPrefix(rest, "tuxgo marks:") {
		return nil
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "tuxgo marks:"))
	for _, kv := range strings.Fields(rest) {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("malformed mark %q (want key=value)", kv)
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("bad mark value %q: %w", kv, err)
		}
		switch key {
		case "query":
			m.Query = n
		case "simple":
			m.Simple = n
		case "complex":
			m.Complex = n
		case "tpcall":
			m.TpCall = n
		case "branch":
			m.Branch = n
		default:
			return fmt.Errorf("unknown mark %q", key)
		}
	}
	return nil
}
