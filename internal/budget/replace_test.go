package budget

import (
	"os"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
)

// sliceLines returns the 1-based inclusive line range of src.
func sliceLines(src string, from, to int) string {
	lines := strings.Split(src, "\n")
	return strings.Join(lines[from-1:to], "\n")
}

// sqlFragment is a distinctive leading chunk of a query's SQL body.
func sqlFragment(sql string) string {
	head := strings.TrimSpace(strings.SplitN(sql, "\n", 2)[0])
	if len(head) > 60 {
		head = head[:60]
	}
	return head
}

// A small deterministic fixture: two SQL regions of different depths.
const replaceSrc = `int svc(void) {
  EXEC SQL
    SELECT X FROM T1
    WHERE ID = :1;
  do_logic();
	if (flag) {
		EXEC SQL UPDATE T2 SET A = :1;
	}
  return 0;
}
`

func replaceFixtureCalls() map[string]DBCall {
	return map[string]DBCall{
		"q1": {Receiver: "store", Name: "GetX", CtxName: "c", Args: []string{"id"}},
		"q2": {Receiver: "store", Name: "UpdateA", CtxName: "c", Args: []string{"a"}},
	}
}

func replaceFixtureQueries() []*ir.Query {
	return []*ir.Query{
		{ID: "q1", Type: ir.QuerySelectMulti, StartLine: 2, EndLine: 4},
		{ID: "q2", Type: ir.QueryUpdate, StartLine: 7, EndLine: 7},
	}
}

func TestReplaceQueries(t *testing.T) {
	view, err := ReplaceQueries(replaceSrc, replaceFixtureQueries(), replaceFixtureCalls())
	if err != nil {
		t.Fatal(err)
	}
	want := `int svc(void) {
  store.GetX(c, id)
  do_logic();
	if (flag) {
		store.UpdateA(c, a)
	}
  return 0;
}
`
	if view.Source != want {
		t.Errorf("view =\n%q\nwant\n%q", view.Source, want)
	}
	if len(view.Report) != 2 {
		t.Fatalf("report entries = %d, want 2", len(view.Report))
	}
	r1 := view.Report[0]
	if r1.QueryID != "q1" || r1.LinesBefore != 3 || r1.ViewLine != 2 {
		t.Errorf("q1 report = %+v", r1)
	}
	if r1.CharsBefore <= r1.CharsAfter {
		t.Errorf("q1 shrink: before %d, after %d", r1.CharsBefore, r1.CharsAfter)
	}
	// ViewLine points at the call line in the rewritten source.
	got := strings.Split(view.Source, "\n")[view.Report[1].ViewLine-1]
	if got != "\t\tstore.UpdateA(c, a)" {
		t.Errorf("q2 view line = %q", got)
	}
	if view.CharsBefore != r1.CharsBefore+view.Report[1].CharsBefore {
		t.Errorf("totals %d != sum of reports", view.CharsBefore)
	}
	if view.ShrinkPct() <= 0 {
		t.Errorf("shrink %v must be positive", view.ShrinkPct())
	}
}

func TestReplaceQueriesErrors(t *testing.T) {
	calls := replaceFixtureCalls()
	queries := replaceFixtureQueries()

	subset := map[string]DBCall{"q1": calls["q1"]}
	if _, err := ReplaceQueries(replaceSrc, queries, subset); err == nil {
		t.Error("missing call for q2 must error")
	}
	badCtx := map[string]DBCall{"q1": {Name: "GetX", CtxName: ""}, "q2": calls["q2"]}
	if _, err := ReplaceQueries(replaceSrc, queries, badCtx); err == nil {
		t.Error("empty context name must error")
	}
	overlap := []*ir.Query{
		{ID: "q1", StartLine: 2, EndLine: 6},
		{ID: "q2", StartLine: 6, EndLine: 6},
	}
	if _, err := ReplaceQueries(replaceSrc, overlap, calls); err == nil {
		t.Error("overlapping ranges must error")
	}
	oob := []*ir.Query{{ID: "q1", StartLine: 0, EndLine: 3}}
	if _, err := ReplaceQueries(replaceSrc, oob, calls); err == nil {
		t.Error("out-of-range start must error")
	}
	oob = []*ir.Query{{ID: "q1", StartLine: 5, EndLine: 99}}
	if _, err := ReplaceQueries(replaceSrc, oob, calls); err == nil {
		t.Error("out-of-range end must error")
	}
}

// TestBudgetGateNavGolden is the Phase 4 self-verification gate over the real
// nav fixture: the rewritten view contains zero SQL, dedup sites call the
// same method, and the cursor-heavy branch view shrinks >60% (architecture.md
// Phase 4 gate).
func TestBudgetGateNavGolden(t *testing.T) {
	const navPath = "../../testdata/nav/SVC_MF_NAV_LIST.pc"
	f, err := ir.ExtractFile(navPath)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(navPath)
	if err != nil {
		t.Fatal(err)
	}
	srcText := string(src)

	// The plan-level call resolution: one method per dedup-collapsed query —
	// q5 (duplicate of q3) resolves to the same GetCount method.
	calls := map[string]DBCall{
		"q1":              {Receiver: "store", Name: "GetDateDetails", CtxName: "c"},
		"cur_mf_nav_hist": {Receiver: "store", Name: "GetNavHistory", CtxName: "c", Args: []string{"compCd", "schCd", "fromDate", "toDate"}},
		"q3":              {Receiver: "store", Name: "GetCount", CtxName: "c", Args: []string{"matchAcc"}},
		"cur_mf_freed":    {Receiver: "store", Name: "GetSipFreedem", CtxName: "c", Args: []string{"compCd", "matchAcc", "d2uFlg"}},
		"q5":              {Receiver: "store", Name: "GetCount", CtxName: "c", Args: []string{"matchAcc"}},
		"cur_mf_nav":      {Receiver: "store", Name: "GetNavList", CtxName: "c", Args: []string{"compCd", "asOf"}},
		"cur_mf_nav_list": {Receiver: "store", Name: "GetNavDetails", CtxName: "c", Args: []string{"compCd"}},
	}

	view, err := ReplaceQueries(srcText, f.Queries, calls)
	if err != nil {
		t.Fatal(err)
	}

	// Zero query SQL leaks into the view — the controller-prompt contract.
	// (Pro*C directives like EXEC SQL DECLARE SECTION / include and
	// non-query EXEC constructs — COMMIT/ROLLBACK tx markers the crux-flow
	// analyzer later consumes — legitimately remain.)
	for _, q := range f.Queries {
		frag := sqlFragment(q.SQL)
		if strings.Contains(view.Source, frag) {
			t.Errorf("query %q SQL fragment %q still present in the view", q.ID, frag)
		}
	}
	if len(view.Report) != 7 {
		t.Fatalf("report entries = %d, want 7", len(view.Report))
	}

	// Dedup call sites: q3 and q5 render the identical GetCount call
	// (indentation differs per branch depth — compare the call itself).
	lines := strings.Split(view.Source, "\n")
	q3, q5 := view.Report[2], view.Report[4]
	if q3.QueryID != "q3" || q5.QueryID != "q5" {
		t.Fatalf("report order changed: %+v %+v", q3, q5)
	}
	if got, want := strings.TrimSpace(lines[q3.ViewLine-1]), strings.TrimSpace(lines[q5.ViewLine-1]); got != want {
		t.Errorf("dedup sites differ:\n%q\n%q", got, want)
	}
	if !strings.Contains(lines[q3.ViewLine-1], "store.GetCount(c, matchAcc)") {
		t.Errorf("q3 line = %q", lines[q3.ViewLine-1])
	}

	// Whole-file shrink of the SQL mass.
	t.Logf("whole-file: %d chars → %d chars (SQL regions %.1f%% smaller)",
		view.CharsBefore, view.CharsAfter, view.ShrinkPct())

	// Cursor-heavy branch: the default branch carries cur_mf_nav_list
	// (L836-965 inside an ~164-line branch). The branch view is what the
	// controller unit consumes (§4.2.4) — it must shrink >60%.
	var def *ir.Condition
	for i := range f.Conditions {
		if f.Conditions[i].IsDefault {
			def = &f.Conditions[i]
		}
	}
	if def == nil {
		t.Fatal("nav IR has no default condition")
	}
	branch := sliceLines(srcText, def.StartLine, def.EndLine)
	var branchQueries []*ir.Query
	for _, q := range f.Queries {
		if q.StartLine >= def.StartLine && q.EndLine <= def.EndLine {
			local := *q
			local.StartLine -= def.StartLine - 1
			local.EndLine -= def.StartLine - 1
			branchQueries = append(branchQueries, &local)
		}
	}
	bview, err := ReplaceQueries(branch, branchQueries, calls)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("default branch: %d chars of SQL → %d chars, view shrink %.1f%%",
		bview.CharsBefore, bview.CharsAfter, bview.ShrinkPct())
	before := len(branch)
	after := len(bview.Source)
	shrink := (1 - float64(after)/float64(before)) * 100
	t.Logf("default branch view: %d → %d chars total (%.1f%% smaller)", before, after, shrink)
	if shrink <= 60 {
		t.Errorf("cursor-heavy branch view shrank %.1f%%, want >60%%", shrink)
	}
}
