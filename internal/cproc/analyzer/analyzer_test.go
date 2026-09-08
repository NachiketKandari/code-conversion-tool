package analyzer

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyzeNavFixture(t *testing.T) {
	pcPath := filepath.Join("..", "..", "..", "testdata", "nav", "SVC_DEMO_LIST.pc")
	rep, err := AnalyzeFile(pcPath, DefaultOptions())
	if err != nil {
		t.Fatalf("AnalyzeFile failed: %v", err)
	}

	if rep.NumQueries != 7 {
		t.Errorf("expected 7 queries, got %d", rep.NumQueries)
	}
	if rep.HasTpCall {
		t.Errorf("expected HasTpCall = false")
	}
	if rep.FnExternalCount != 3 {
		t.Errorf("expected 3 external functions, got %d (%v)", rep.FnExternalCount, rep.ExternalFns)
	}
	// chk_session: unresolved, non-conversion name => complex +10.
	// fn_is_demo_active: unresolved in single-file mode, non-conversion name => complex +10.
	// fn_long_to_int: unresolved but conversion-shaped name (_to_) => simple +5.
	// Score = 7 queries + (10 + 10 + 5) external + 142 branching factor = 174.
	if rep.BranchCount != 75 {
		t.Errorf("expected 75 branch headers, got %d", rep.BranchCount)
	}
	if rep.BranchingFactor != 142 {
		t.Errorf("expected branching factor 142, got %d", rep.BranchingFactor)
	}
	if rep.ComplexityScore != 174 {
		t.Errorf("expected complexity score = 174, got %d", rep.ComplexityScore)
	}
	if rep.Complexity != "HIGH" {
		t.Errorf("expected complexity 'HIGH', got %s", rep.Complexity)
	}
	byName := externalByName(rep)
	if fn := byName["fn_long_to_int"]; fn == nil || fn.Class != ExtSimple || fn.Weight != 5 {
		t.Errorf("expected fn_long_to_int to be simple +5, got %+v", fn)
	}
	if fn := byName["chk_session"]; fn == nil || fn.Class != ExtComplex || fn.Weight != 10 {
		t.Errorf("expected chk_session to be complex +10, got %+v", fn)
	}
}

func TestAnalyzeDirResolvesExternalFns(t *testing.T) {
	dirPath := filepath.Join("..", "..", "..", "testdata", "nav")
	reports, err := AnalyzeDir(dirPath, DefaultOptions())
	if err != nil {
		t.Fatalf("AnalyzeDir failed: %v", err)
	}

	var nav *Report
	for _, r := range reports {
		if strings.HasSuffix(r.File, "SVC_DEMO_LIST.pc") {
			nav = r
		}
	}
	if nav == nil {
		t.Fatalf("SVC_DEMO_LIST.pc report not found")
	}

	byName := externalByName(nav)
	// fn_is_demo_active resolves to fn_demo_lib.pc, whose defining body carries an
	// EXEC SQL fetch => complex +10.
	fn := byName["fn_is_demo_active"]
	if fn == nil {
		t.Fatalf("expected fn_is_demo_active among external fns, got %v", nav.ExternalFns)
	}
	if !fn.Resolved {
		t.Errorf("expected fn_is_demo_active to be resolved via the corpus")
	}
	if !fn.HasSQL || fn.Class != ExtComplex || fn.Weight != 10 {
		t.Errorf("expected resolved fn_is_demo_active (SQL-bearing, complex +10), got %+v", fn)
	}
	if !strings.HasSuffix(fn.DefinedIn, "fn_demo_lib.pc") {
		t.Errorf("expected fn_is_demo_active defined in fn_demo_lib.pc, got %s", fn.DefinedIn)
	}
	// Corpus resolution must not change the fixture score: 7 + (10 + 10 + 5) + 142 = 174.
	if nav.ComplexityScore != 174 {
		t.Errorf("expected complexity score = 174 in dir mode, got %d", nav.ComplexityScore)
	}
}

// TestAnalyzerCountsOnlyProjectExternalCalls guards the fn_*/chk_* prefix
// rule (v0.6.3, corpus finding): C stdlib/POSIX/FML calls are dropped
// constructs and must never score as external fns.
func TestAnalyzerCountsOnlyProjectExternalCalls(t *testing.T) {
	src := `#include <stdio.h>
static long fn_local(int x) { return x; }
void SVC_TEST(TPCFB *rqst) {
	char buf[64];
	sscanf(buf, "%d", &i);
	double v = sqrt(2.0);
	time_t t = time(NULL);
	Fadd(fbfr, 1, "x", 0);
	long a = fn_helper(buf);
	long b = fn_local(1);
	chk_foo(fbfr);
	EXEC SQL SELECT COUNT(*) INTO :c FROM DUAL;
}
`
	path := filepath.Join(t.TempDir(), "noise.pc")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("writing fixture failed: %v", err)
	}
	rep, err := AnalyzeFile(path, DefaultOptions())
	if err != nil {
		t.Fatalf("AnalyzeFile failed: %v", err)
	}

	if rep.NumQueries != 1 {
		t.Errorf("expected 1 query, got %d", rep.NumQueries)
	}
	if rep.FnLocalCount != 1 {
		t.Errorf("expected 1 local fn (fn_local), got %d", rep.FnLocalCount)
	}
	byName := externalByName(rep)
	for _, noise := range []string{"sscanf", "sqrt", "time", "Fadd", "fn_local"} {
		if _, ok := byName[noise]; ok {
			t.Errorf("dropped construct %q must not count as an external fn (got %+v)", noise, byName[noise])
		}
	}
	for _, want := range []string{"fn_helper", "chk_foo"} {
		if byName[want] == nil {
			t.Errorf("expected project external fn %q, got %v", want, rep.ExternalFns)
		}
	}
	if rep.FnExternalCount != 2 {
		t.Errorf("expected 2 external fns, got %d (%v)", rep.FnExternalCount, rep.ExternalFns)
	}
	// 1 query + 2 unresolved complex externals (neither is conversion-named)
	if rep.ComplexityScore != 21 {
		t.Errorf("expected complexity score 21, got %d", rep.ComplexityScore)
	}
}

func externalByName(rep *Report) map[string]*ExternalFn {
	m := make(map[string]*ExternalFn, len(rep.ExternalFns))
	for i := range rep.ExternalFns {
		m[rep.ExternalFns[i].Name] = &rep.ExternalFns[i]
	}
	return m
}

// TestAnalyzeMergeFixture pins the MERGE rubric: the merge counts as one
// query (merge_count), the nested sqlcode checks double per the nesting
// rule, and the score = 1 query + 4 branching factor.
func TestAnalyzeMergeFixture(t *testing.T) {
	pcPath := filepath.Join("..", "..", "..", "testdata", "merge", "SVC_DEMO_MERGE.pc")
	rep, err := AnalyzeFile(pcPath, DefaultOptions())
	if err != nil {
		t.Fatalf("AnalyzeFile failed: %v", err)
	}

	if rep.NumQueries != 1 {
		t.Errorf("expected 1 query, got %d", rep.NumQueries)
	}
	if rep.MergeCount != 1 || rep.SelectCount != 0 || rep.InsertCount != 0 ||
		rep.UpdateCount != 0 || rep.DeleteCount != 0 {
		t.Errorf("expected merge_count=1 and all other counts 0, got select=%d insert=%d update=%d delete=%d merge=%d",
			rep.SelectCount, rep.InsertCount, rep.UpdateCount, rep.DeleteCount, rep.MergeCount)
	}
	if rep.BranchCount != 3 || rep.BranchingFactor != 4 {
		t.Errorf("expected branch count/factor 3/4, got %d/%d", rep.BranchCount, rep.BranchingFactor)
	}
	if rep.ComplexityScore != 5 {
		t.Errorf("expected complexity score 5 (1 query + 4 branching), got %d", rep.ComplexityScore)
	}
	if rep.Complexity != "LOW" {
		t.Errorf("expected tier LOW, got %s", rep.Complexity)
	}
	if !strings.Contains(rep.Reasons, "branching factor 4 over 3 branch headers (+4)") {
		t.Errorf("reasons must reflect the branching factor: %s", rep.Reasons)
	}
}

func TestAnalyzeFnDemoFixture(t *testing.T) {
	pcPath := filepath.Join("..", "..", "..", "testdata", "nav", "fn_demo_lib.pc")
	rep, err := AnalyzeFile(pcPath, DefaultOptions())
	if err != nil {
		t.Fatalf("AnalyzeFile failed: %v", err)
	}

	if rep.NumQueries != 1 {
		t.Errorf("expected 1 query, got %d", rep.NumQueries)
	}
	if rep.HasTpCall {
		t.Errorf("expected HasTpCall = false")
	}
	if rep.FnLocalCount != 1 {
		t.Errorf("expected 1 local fn, got %d", rep.FnLocalCount)
	}
	if rep.FnExternalCount != 0 {
		t.Errorf("expected 0 external functions, got %d (%v)", rep.FnExternalCount, rep.ExternalFns)
	}
	// 1 query + 1 top-level if in fn_is_demo_active (branching factor 1).
	if rep.BranchCount != 1 || rep.BranchingFactor != 1 {
		t.Errorf("expected branch count/factor 1/1, got %d/%d", rep.BranchCount, rep.BranchingFactor)
	}
	if rep.ComplexityScore != 2 {
		t.Errorf("expected complexity score = 2, got %d", rep.ComplexityScore)
	}
	if rep.Complexity != "LOW" {
		t.Errorf("expected complexity 'LOW', got %s", rep.Complexity)
	}
}

func TestAnalyzeDirAndCSV(t *testing.T) {
	dirPath := filepath.Join("..", "..", "..", "testdata", "nav")
	reports, err := AnalyzeDir(dirPath, DefaultOptions())
	if err != nil {
		t.Fatalf("AnalyzeDir failed: %v", err)
	}

	if len(reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(reports))
	}

	// Verify ranking: highest complexity first
	if reports[0].ComplexityScore < reports[1].ComplexityScore {
		t.Errorf("expected descending sort by complexity score")
	}
	if !strings.HasSuffix(reports[0].File, "SVC_DEMO_LIST.pc") {
		t.Errorf("expected SVC_DEMO_LIST.pc to be ranked first, got %s", reports[0].File)
	}

	// Test CSV export. The marks comment line leads the file; the marks
	// cells row follows; the tabular reader skips comment lines.
	var buf bytes.Buffer
	if err := WriteCSV(&buf, reports, DefaultMarks()); err != nil {
		t.Fatalf("WriteCSV failed: %v", err)
	}
	if want := "# tuxgo marks: query=1 simple=5 complex=10 tpcall=20 branch=1"; !strings.Contains(buf.String(), want) {
		t.Errorf("CSV missing marks line %q:\n%s", want, buf.String())
	}

	reader := csv.NewReader(&buf)
	reader.Comment = '#'
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("reading generated CSV failed: %v", err)
	}

	if len(records) != 4 { // 1 marks cells row + 1 header + 2 rows
		t.Fatalf("expected 4 CSV records, got %d", len(records))
	}

	expectedHeader := []string{
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
	for i, col := range expectedHeader {
		if records[1][i] != col {
			t.Errorf("header col %d: expected %s, got %s", i, col, records[1][i])
		}
	}

	// The marks cells row carries each scored column's mark over that
	// column (spreadsheet row 2): query=1 under num_queries, branch=1
	// under branching_factor, tpcall=20 under tpcall_count.
	marksRow := records[0]
	if marksRow[2] != "1" || marksRow[3] != "1" || marksRow[6] != "20" {
		t.Errorf("marks cells row misaligned (want num_queries=1, branching_factor=1, tpcall_count=20): %v", marksRow)
	}

	// The nav row (highest complexity, first) names every external call with
	// class and weight, so the CSV is a self-contained tuning surface.
	navRow := records[2]
	if navRow[1] != "982" {
		t.Errorf("expected nav fixture num_lines 982, got %s", navRow[1])
	}
	for _, want := range []string{
		"chk_session:complex:10",
		"fn_is_demo_active:complex:10",
		"fn_long_to_int:simple:5",
	} {
		if !strings.Contains(navRow[9], want) {
			t.Errorf("external_fns cell missing %q: %q", want, navRow[9])
		}
	}
	if navRow[10] != "25" {
		t.Errorf("expected external_weight 25, got %s", navRow[10])
	}
	if navRow[6] != "0" {
		t.Errorf("expected tpcall_count 0, got %s", navRow[6])
	}
	// Branching factor and per-type query counts. All 7 nav units are
	// SELECTs (3 singles + 4 flattened cursors); the counts sum to
	// num_queries.
	if navRow[3] != "142" || navRow[4] != "75" {
		t.Errorf("expected branching_factor/branch_count 142/75, got %s/%s", navRow[3], navRow[4])
	}
	for i, want := range map[int]string{2: "7", 11: "7", 12: "0", 13: "0", 14: "0", 15: "0"} {
		if navRow[i] != want {
			t.Errorf("count col %d: expected %s, got %s", i, want, navRow[i])
		}
	}

	// complexity_score and complexity are spreadsheet formulas over the
	// marks cells row (absolute $2 refs) and the row's own factor cells.
	if navRow[16] != "=C$2*C4+D$2*D4+G$2*G4+K4" {
		t.Errorf("unexpected score formula: %q", navRow[16])
	}
	if navRow[17] != `=IF(Q4>=30,"HIGH",IF(Q4>=10,"MEDIUM","LOW"))` {
		t.Errorf("unexpected tier formula: %q", navRow[17])
	}
	fnRow := records[3]
	if fnRow[16] != "=C$2*C5+D$2*D5+G$2*G5+K5" {
		t.Errorf("unexpected score formula on second data row: %q", fnRow[16])
	}
	if fnRow[17] != `=IF(Q5>=30,"HIGH",IF(Q5>=10,"MEDIUM","LOW"))` {
		t.Errorf("unexpected tier formula on second data row: %q", fnRow[17])
	}
}

func TestLoadOptionsCSVAndRescore(t *testing.T) {
	dirPath := filepath.Join("..", "..", "..", "testdata", "nav")

	// 1. Baseline: write the standard CSV to disk.
	reports, err := AnalyzeDir(dirPath, DefaultOptions())
	if err != nil {
		t.Fatalf("AnalyzeDir failed: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, reports, DefaultMarks()); err != nil {
		t.Fatalf("WriteCSV failed: %v", err)
	}
	basePath := filepath.Join(t.TempDir(), "base.csv")
	if err := os.WriteFile(basePath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing baseline CSV failed: %v", err)
	}

	// 2. Round-trip: the written CSV is a valid options input — marks
	// defaults plus the explicit per-fn weights.
	opts, err := LoadOptionsCSV(basePath)
	if err != nil {
		t.Fatalf("LoadOptionsCSV failed: %v", err)
	}
	if opts.Marks != DefaultMarks() {
		t.Errorf("round-tripped marks %v, want defaults %v", opts.Marks, DefaultMarks())
	}
	if opts.FnWeights["chk_session"] != 10 || opts.FnWeights["fn_is_demo_active"] != 10 || opts.FnWeights["fn_long_to_int"] != 5 {
		t.Errorf("unexpected round-tripped weights: %v", opts.FnWeights)
	}

	// 3. Per-fn edit (drop the session check from scoring) and re-score.
	edited := strings.Replace(buf.String(), "chk_session:complex:10", "chk_session:complex:0", 1)
	opts, err = LoadOptionsCSV(writeTemp(t, edited))
	if err != nil {
		t.Fatalf("LoadOptionsCSV(edited) failed: %v", err)
	}
	nav := navReport(t, dirPath, opts)
	if nav.ComplexityScore != 164 {
		t.Errorf("expected re-scored complexity 164 (174 with chk_session weight 0), got %d", nav.ComplexityScore)
	}
	if nav.Complexity != "HIGH" {
		t.Errorf("expected re-scored complexity tier HIGH, got %s", nav.Complexity)
	}
	if fn := externalByName(nav)["chk_session"]; fn == nil || fn.Weight != 0 {
		t.Errorf("expected chk_session weight 0 after re-score, got %+v", fn)
	}
	// Counts stay factual: chk_session is still an external call, only its
	// scoring weight changed.
	if nav.FnExternalCount != 3 {
		t.Errorf("fn_external_count must stay factual (3), got %d", nav.FnExternalCount)
	}

	// 4. Marks cell edit: num_queries mark 1 → 2 doubles the query
	// contribution (7×2 + 25 + 142 = 181). The marks cells row is the
	// editing surface; a comment-line-only edit would be ignored (cells
	// win, step 6).
	edited = editMarksCell(buf.String(), 3, "2")
	opts, err = LoadOptionsCSV(writeTemp(t, edited))
	if err != nil {
		t.Fatalf("LoadOptionsCSV(marks cell edited) failed: %v", err)
	}
	nav = navReport(t, dirPath, opts)
	if nav.ComplexityScore != 181 {
		t.Errorf("expected re-scored complexity 181 with query mark 2, got %d", nav.ComplexityScore)
	}
	if !strings.Contains(nav.Reasons, "7 queries (+14)") {
		t.Errorf("reasons must reflect the query mark: %s", nav.Reasons)
	}

	// 5. Marks cells row: editing the branching_factor cell (column 4) to 2
	// doubles the branch contribution (7 + 25 + 142×2 = 316).
	edited = editMarksCell(buf.String(), 4, "2")
	opts, err = LoadOptionsCSV(writeTemp(t, edited))
	if err != nil {
		t.Fatalf("LoadOptionsCSV(marks cell edited) failed: %v", err)
	}
	nav = navReport(t, dirPath, opts)
	if nav.ComplexityScore != 316 {
		t.Errorf("expected re-scored complexity 316 with branch mark 2, got %d", nav.ComplexityScore)
	}

	// 6. Cells win over the comment line: a hand-edited comment tpcall mark
	// is ignored while the marks cells row is present.
	edited = strings.Replace(buf.String(), "tpcall=20", "tpcall=5", 1)
	opts, err = LoadOptionsCSV(writeTemp(t, edited))
	if err != nil {
		t.Fatalf("LoadOptionsCSV(comment edited) failed: %v", err)
	}
	if opts.Marks.TpCall != 20 {
		t.Errorf("marks cells row must win over the comment line: got tpcall %d", opts.Marks.TpCall)
	}

	// 7. Cleared per-fn weight falls back to the tier mark: fn_long_to_int
	// (simple) with simple=3 → +3; score = 7 + 10 + 10 + 142 + 3 = 172.
	edited = strings.Replace(buf.String(), "fn_long_to_int:simple:5", "fn_long_to_int:simple:", 1)
	edited = strings.Replace(edited, "simple=5", "simple=3", 1)
	opts, err = LoadOptionsCSV(writeTemp(t, edited))
	if err != nil {
		t.Fatalf("LoadOptionsCSV(cleared weight) failed: %v", err)
	}
	nav = navReport(t, dirPath, opts)
	if fn := externalByName(nav)["fn_long_to_int"]; fn == nil || fn.Weight != 3 {
		t.Errorf("expected fn_long_to_int to fall back to simple mark 3, got %+v", fn)
	}
	if nav.ComplexityScore != 172 {
		t.Errorf("expected re-scored complexity 172, got %d", nav.ComplexityScore)
	}

	// 8. A typo'd mark is an error, never a silent default.
	edited = strings.Replace(buf.String(), "query=1", "quer=2", 1)
	if _, err := LoadOptionsCSV(writeTemp(t, edited)); err == nil {
		t.Error("expected unknown mark 'quer' to be rejected")
	}
	edited = strings.Replace(buf.String(), "tpcall=20", "tpcall=lots", 1)
	if _, err := LoadOptionsCSV(writeTemp(t, edited)); err == nil {
		t.Error("expected malformed mark value to be rejected")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "edited.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing CSV failed: %v", err)
	}
	return path
}

// editMarksCell rewrites one cell of the marks cells row (spreadsheet row 2,
// the second CSV line): col is the 1-based column index.
func editMarksCell(csv string, col int, value string) string {
	lines := strings.Split(csv, "\n")
	cells := strings.Split(lines[1], ",")
	cells[col-1] = value
	lines[1] = strings.Join(cells, ",")
	return strings.Join(lines, "\n")
}

// TestBranchingFactorNesting pins the doubling semantics: each if/else-if
// header contributes +1 × 2^(enclosing if blocks); else contributes 0; an
// else body does not nest its chain; an else-if is a sibling of its chain.
func TestBranchingFactorNesting(t *testing.T) {
	src := `void SVC_NEST(TPSVCINFO *rqst) {
	if (a) {
		if (b) {
			if (c) {
				work();
			}
		}
	}
	if (d) {
		work();
	} else if (e) {
		work();
	} else {
		if (f) {
			work();
		}
	}
}
`
	path := filepath.Join(t.TempDir(), "nest.pc")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("writing fixture failed: %v", err)
	}
	rep, err := AnalyzeFile(path, DefaultOptions())
	if err != nil {
		t.Fatalf("AnalyzeFile failed: %v", err)
	}
	// a=1, b=2, c=4, d=1, e=1 (else-if sibling), f=1 (inside an else body —
	// else never nests): factor 10 over 6 headers.
	if rep.BranchCount != 6 {
		t.Errorf("expected 6 branch headers, got %d", rep.BranchCount)
	}
	if rep.BranchingFactor != 10 {
		t.Errorf("expected branching factor 10 (1+2+4+1+1+1), got %d", rep.BranchingFactor)
	}
	if rep.ComplexityScore != 10 {
		t.Errorf("expected complexity score 10 (branching only), got %d", rep.ComplexityScore)
	}
	if rep.Complexity != "MEDIUM" {
		t.Errorf("expected tier MEDIUM (score 10 hits the threshold), got %s", rep.Complexity)
	}
}

func navReport(t *testing.T, dirPath string, opts Options) *Report {
	t.Helper()
	rescored, err := AnalyzeDir(dirPath, opts)
	if err != nil {
		t.Fatalf("AnalyzeDir with overrides failed: %v", err)
	}
	for _, r := range rescored {
		if strings.HasSuffix(r.File, "SVC_DEMO_LIST.pc") {
			return r
		}
	}
	t.Fatalf("nav report missing after re-score")
	return nil
}
