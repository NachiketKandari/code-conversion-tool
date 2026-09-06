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
	pcPath := filepath.Join("..", "..", "..", "testdata", "nav", "SVC_MF_NAV_LIST.pc")
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
	// chk_sssn: unresolved, non-conversion name => complex +10.
	// fn_is_d2u_active: unresolved in single-file mode, non-conversion name => complex +10.
	// fn_long_to_int: unresolved but conversion-shaped name (_to_) => simple +5.
	// Score = 7 queries + (10 + 10 + 5) = 32.
	if rep.ComplexityScore != 32 {
		t.Errorf("expected complexity score = 32, got %d", rep.ComplexityScore)
	}
	if rep.Complexity != "HIGH" {
		t.Errorf("expected complexity 'HIGH', got %s", rep.Complexity)
	}
	byName := externalByName(rep)
	if fn := byName["fn_long_to_int"]; fn == nil || fn.Class != ExtSimple || fn.Weight != 5 {
		t.Errorf("expected fn_long_to_int to be simple +5, got %+v", fn)
	}
	if fn := byName["chk_sssn"]; fn == nil || fn.Class != ExtComplex || fn.Weight != 10 {
		t.Errorf("expected chk_sssn to be complex +10, got %+v", fn)
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
		if strings.HasSuffix(r.File, "SVC_MF_NAV_LIST.pc") {
			nav = r
		}
	}
	if nav == nil {
		t.Fatalf("SVC_MF_NAV_LIST.pc report not found")
	}

	byName := externalByName(nav)
	// fn_is_d2u_active resolves to fn_d2u_mf.pc, whose defining body carries an
	// EXEC SQL fetch => complex +10.
	fn := byName["fn_is_d2u_active"]
	if fn == nil {
		t.Fatalf("expected fn_is_d2u_active among external fns, got %v", nav.ExternalFns)
	}
	if !fn.Resolved {
		t.Errorf("expected fn_is_d2u_active to be resolved via the corpus")
	}
	if !fn.HasSQL || fn.Class != ExtComplex || fn.Weight != 10 {
		t.Errorf("expected resolved fn_is_d2u_active (SQL-bearing, complex +10), got %+v", fn)
	}
	if !strings.HasSuffix(fn.DefinedIn, "fn_d2u_mf.pc") {
		t.Errorf("expected fn_is_d2u_active defined in fn_d2u_mf.pc, got %s", fn.DefinedIn)
	}
	// Corpus resolution must not change the fixture score: 7 + (10 + 10 + 5) = 32.
	if nav.ComplexityScore != 32 {
		t.Errorf("expected complexity score = 32 in dir mode, got %d", nav.ComplexityScore)
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

func TestAnalyzeFnD2uFixture(t *testing.T) {
	pcPath := filepath.Join("..", "..", "..", "testdata", "nav", "fn_d2u_mf.pc")
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
	if rep.ComplexityScore != 1 {
		t.Errorf("expected complexity score = 1, got %d", rep.ComplexityScore)
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
	if !strings.HasSuffix(reports[0].File, "SVC_MF_NAV_LIST.pc") {
		t.Errorf("expected SVC_MF_NAV_LIST.pc to be ranked first, got %s", reports[0].File)
	}

	// Test CSV export. The marks line leads the file; the tabular reader
	// skips comment lines.
	var buf bytes.Buffer
	if err := WriteCSV(&buf, reports, DefaultMarks()); err != nil {
		t.Fatalf("WriteCSV failed: %v", err)
	}
	if want := "# tuxgo marks: query=1 simple=5 complex=10 tpcall=20"; !strings.Contains(buf.String(), want) {
		t.Errorf("CSV missing marks line %q:\n%s", want, buf.String())
	}

	reader := csv.NewReader(&buf)
	reader.Comment = '#'
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("reading generated CSV failed: %v", err)
	}

	if len(records) != 3 { // 1 header + 2 rows
		t.Fatalf("expected 3 CSV records, got %d", len(records))
	}

	expectedHeader := []string{
		"file",
		"num_queries",
		"has_tpcall",
		"tpcall_count",
		"fn_local_count",
		"fn_external_count",
		"external_fns",
		"external_weight",
		"complexity_score",
		"complexity",
		"reasons",
	}
	for i, col := range expectedHeader {
		if records[0][i] != col {
			t.Errorf("header col %d: expected %s, got %s", i, col, records[0][i])
		}
	}

	// The nav row (highest complexity, first) names every external call with
	// class and weight, so the CSV is a self-contained tuning surface.
	navRow := records[1]
	for _, want := range []string{
		"chk_sssn:complex:10",
		"fn_is_d2u_active:complex:10",
		"fn_long_to_int:simple:5",
	} {
		if !strings.Contains(navRow[6], want) {
			t.Errorf("external_fns cell missing %q: %q", want, navRow[6])
		}
	}
	if navRow[7] != "25" {
		t.Errorf("expected external_weight 25, got %s", navRow[7])
	}
	if navRow[3] != "0" {
		t.Errorf("expected tpcall_count 0, got %s", navRow[3])
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
	if opts.FnWeights["chk_sssn"] != 10 || opts.FnWeights["fn_is_d2u_active"] != 10 || opts.FnWeights["fn_long_to_int"] != 5 {
		t.Errorf("unexpected round-tripped weights: %v", opts.FnWeights)
	}

	// 3. Per-fn edit (drop the session check from scoring) and re-score.
	edited := strings.Replace(buf.String(), "chk_sssn:complex:10", "chk_sssn:complex:0", 1)
	opts, err = LoadOptionsCSV(writeTemp(t, edited))
	if err != nil {
		t.Fatalf("LoadOptionsCSV(edited) failed: %v", err)
	}
	nav := navReport(t, dirPath, opts)
	if nav.ComplexityScore != 22 {
		t.Errorf("expected re-scored complexity 22 (32 with chk_sssn weight 0), got %d", nav.ComplexityScore)
	}
	if nav.Complexity != "MEDIUM" {
		t.Errorf("expected re-scored complexity tier MEDIUM, got %s", nav.Complexity)
	}
	if fn := externalByName(nav)["chk_sssn"]; fn == nil || fn.Weight != 0 {
		t.Errorf("expected chk_sssn weight 0 after re-score, got %+v", fn)
	}
	// Counts stay factual: chk_sssn is still an external call, only its
	// scoring weight changed.
	if nav.FnExternalCount != 3 {
		t.Errorf("fn_external_count must stay factual (3), got %d", nav.FnExternalCount)
	}

	// 4. Marks edit: query=1 → query=2 doubles the query contribution
	// (7×2 + 25 = 39).
	edited = strings.Replace(buf.String(), "query=1", "query=2", 1)
	opts, err = LoadOptionsCSV(writeTemp(t, edited))
	if err != nil {
		t.Fatalf("LoadOptionsCSV(marks edited) failed: %v", err)
	}
	nav = navReport(t, dirPath, opts)
	if nav.ComplexityScore != 39 {
		t.Errorf("expected re-scored complexity 39 with query=2, got %d", nav.ComplexityScore)
	}
	if !strings.Contains(nav.Reasons, "7 queries (+14)") {
		t.Errorf("reasons must reflect the query mark: %s", nav.Reasons)
	}

	// 5. Cleared per-fn weight falls back to the tier mark: fn_long_to_int
	// (simple) with simple=3 → +3; score = 7 + 10 + 10 + 3 = 30.
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
	if nav.ComplexityScore != 30 {
		t.Errorf("expected re-scored complexity 30, got %d", nav.ComplexityScore)
	}

	// 6. A typo'd mark is an error, never a silent default.
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

func navReport(t *testing.T, dirPath string, opts Options) *Report {
	t.Helper()
	rescored, err := AnalyzeDir(dirPath, opts)
	if err != nil {
		t.Fatalf("AnalyzeDir with overrides failed: %v", err)
	}
	for _, r := range rescored {
		if strings.HasSuffix(r.File, "SVC_MF_NAV_LIST.pc") {
			return r
		}
	}
	t.Fatalf("nav report missing after re-score")
	return nil
}
