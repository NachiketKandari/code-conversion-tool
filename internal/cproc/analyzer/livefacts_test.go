package analyzer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

// TestCommentedOutFactsAgreeAcrossPipelines pins the A2.1 single-home rule:
// the analyzer and the ir extractor must agree that code inside a comment
// is not live code. The two packages used to carry drifted copies of the
// filter (the analyzer's skipped AllSQL); ir.LiveFacts is now the only one.
const commentedSource = `#include <sqlca.h>

void SVC_DEMO() {
	/* EXEC SQL SELECT demo_col INTO :v FROM demo_table; */
	// chk_session("t");
	int live_marker = 1;
	/* EXEC SQL
	   UPDATE demo SET a = 1
	   WHERE b = 2; */
	if (flag == 'Y') {
		// fn_is_demo_active(x);
	}
}
`

func TestCommentedOutFactsAgreeAcrossPipelines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostile.pc")
	if err := os.WriteFile(path, []byte(commentedSource), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := AnalyzeFile(path, DefaultOptions())
	if err != nil {
		t.Fatalf("AnalyzeFile: %v", err)
	}
	if rep.NumQueries != 0 {
		t.Errorf("analyzer: commented-out queries leaked: %d", rep.NumQueries)
	}
	if rep.FnExternalCount != 0 {
		t.Errorf("analyzer: commented-out external fns leaked: %d", rep.FnExternalCount)
	}
	if rep.SelectCount != 0 || rep.UpdateCount != 0 {
		t.Errorf("analyzer: per-type counts leaked: select=%d update=%d", rep.SelectCount, rep.UpdateCount)
	}

	f, err := ir.ExtractFile(path)
	if err != nil {
		t.Fatalf("ir.ExtractFile: %v", err)
	}
	if len(f.Queries) != 0 {
		t.Errorf("ir: commented-out queries leaked: %d", len(f.Queries))
	}
	if len(f.ExternalFns) != 0 {
		t.Errorf("ir: commented-out external fns leaked: %d", len(f.ExternalFns))
	}

	// The shared rule itself: every Calls/Queries/AllSQL fact sits outside
	// the recorded comment spans.
	facts, err := scanner.ScanFile(path)
	if err != nil {
		t.Fatalf("ScanFile: %v", err)
	}
	live := ir.LiveFacts(facts)
	if len(live.AllSQL) != 0 || len(live.Queries) != 0 || len(live.Calls) != 0 {
		t.Errorf("ir.LiveFacts leaked commented facts: sql=%d queries=%d calls=%d",
			len(live.AllSQL), len(live.Queries), len(live.Calls))
	}
	if len(live.Functions) == 0 {
		t.Errorf("live code (functions) must survive the filter")
	}
}
