package gen

import (
	"os"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/plan"
)

// strippedFixture extracts testdata/stripped and returns one entry's IR plus
// the full file set — the exact shape dir-mode fan-out hands every service
// (all .pc files as FnFiles, PRD-2026-09-06 v0.8.7).
func strippedFixture(t *testing.T, entry string) (*ir.File, []*ir.File) {
	t.Helper()
	files, err := ir.ExtractDir("../../testdata/stripped")
	if err != nil {
		t.Fatal(err)
	}
	var main *ir.File
	for _, f := range files {
		if f.Entry == entry {
			main = f
		}
	}
	if main == nil {
		t.Fatalf("no %s entry in the stripped corpus", entry)
	}
	return main, files
}

// TestStrippedDedupQueriesNoCollision pins the F1 guard: with the whole
// stripped dir as FnFiles (dir fan-out shape), the dedup main's raw q1 stays
// its own SELECT — the fn lib's DECODE(COUNT(*)) query and every other
// unreferenced file's q1-style IDs must stay file-scoped, never overwrite.
func TestStrippedDedupQueriesNoCollision(t *testing.T) {
	main, files := strippedFixture(t, "SVC_MIN_DEDUP")
	m, err := plan.LoadMapping("../../testdata/stripped/mappings/svc_dedup.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(plan.Options{Main: main, FnFiles: files, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(Options{Plan: p, Main: main, FnFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	q := s.Query("q1")
	if q == nil {
		t.Fatal("main q1 missing")
	}
	if !strings.Contains(q.SQL, "FROM MIN_ACCOUNTS") || strings.Contains(q.SQL, "DECODE") {
		t.Errorf("main q1 was overwritten by another file's query:\n%s", q.SQL)
	}
	// The fn lib's query survives only under its file-scoped id — and the
	// dedup main's own q2 (the linked duplicate) is still its SELECT, not
	// the kitchen file's later-walk INSERT.
	if lib := s.Query("fn_min_lib:q1"); lib == nil || !strings.Contains(lib.SQL, "MIN_CLIENT_MAP") {
		t.Errorf("fn lib query not file-scoped: %+v", lib)
	}
	if q := s.Query("q2"); q == nil || q.Type != ir.QuerySelectSingle {
		t.Errorf("MinDedup q2 = %+v, want its own SELECT_SINGLE", q)
	}
}

// TestStrippedKitchenFnNamespace pins the referenced-fn namespace on the
// kitchen fixture: fn_min_check's query resolves as "fn_min_check:q1" —
// the plan's pin namespace — with its own SQL.
func TestStrippedKitchenFnNamespace(t *testing.T) {
	main, files := strippedFixture(t, "SVC_MIN_KITCHEN")
	m, err := plan.LoadMapping("../../testdata/stripped/mappings/svc_kitchen.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(plan.Options{Main: main, FnFiles: files, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(Options{Plan: p, Main: main, FnFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	q := s.Query("fn_min_check:q1")
	if q == nil || !strings.Contains(q.SQL, "MIN_CLIENT_MAP") {
		t.Errorf("fn_min_check:q1 = %+v, want the lib's DECODE SELECT", q)
	}
	if q := s.Query("q1"); q == nil || !strings.Contains(q.SQL, "FROM MIN_ACCOUNTS") {
		t.Errorf("kitchen main q1 overwritten: %+v", q)
	}
}

// TestStrippedDMLQueriesUntouched pins F1 on MinDml: its own q2 is a DELETE,
// and the kitchen file's INSERT (later in walk order) must not replace it.
func TestStrippedDMLQueriesUntouched(t *testing.T) {
	main, files := strippedFixture(t, "SVC_MIN_DML")
	m, err := plan.LoadMapping("../../testdata/stripped/mappings/svc_dml.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(plan.Options{Main: main, FnFiles: files, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(Options{Plan: p, Main: main, FnFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	if q := s.Query("q1"); q == nil || q.Type != ir.QueryUpdate {
		t.Errorf("MinDml q1 = %v, want UPDATE", q)
	}
	if q := s.Query("q2"); q == nil || q.Type != ir.QueryDelete {
		t.Errorf("MinDml q2 = %+v, want its own DELETE", q)
	}
	if q := s.Query("q3"); q == nil || q.Type != ir.QueryMerge {
		t.Errorf("MinDml q3 = %+v, want MERGE", q)
	}
}

// TestStrippedDMLDBMethods pins F2 end-to-end: MinDml's UPDATE/DELETE/MERGE
// render through the DML templates — tx-variant signatures, ExecContext
// bodies, sqlx import — instead of the "not supported" error.
func TestStrippedDMLDBMethods(t *testing.T) {
	main, files := strippedFixture(t, "SVC_MIN_DML")
	m, err := plan.LoadMapping("../../testdata/stripped/mappings/svc_dml.yaml")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(main.Path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(plan.Options{Main: main, Source: string(src), FnFiles: files, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(Options{Plan: p, Main: main, FnFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.DBMethodsFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"github.com/jmoiron/sqlx",
		"func (g *store) UpdateMinAccounts(c context.Context, tx *sqlx.Tx, sqlMinAmt string, sqlMinAcc string) error",
		"tx.ExecContext(c, query, sqlMinAmt, sqlMinAcc)",
		"func (g *store) MergeMinAccounts(c context.Context, sqlMinAcc string, sqlMinAmt string) error",
		"g.db.ExecContext(c, query, sqlMinAcc, sqlMinAmt)",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("db methods file missing %q\n%s", want, file)
		}
	}
	iface, err := s.DBInterface(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(iface, "UpdateMinAccounts(c context.Context, tx *sqlx.Tx, sqlMinAmt string, sqlMinAcc string) error") {
		t.Errorf("db interface missing the tx-variant signature:\n%s", iface)
	}
}

// TestStrippedKitchenDMLDBMethods pins F2 on the kitchen fixture: the
// in-branch INSERT renders through db_method_insert_tx (tx variant,
// error-only), the SELECT single stays read-only.
func TestStrippedDMLKitchenDBMethods(t *testing.T) {
	main, files := strippedFixture(t, "SVC_MIN_KITCHEN")
	m, err := plan.LoadMapping("../../testdata/stripped/mappings/svc_kitchen.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(plan.Options{Main: main, FnFiles: files, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(Options{Plan: p, Main: main, FnFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.DBMethodsFile(p)
	if err != nil {
		t.Fatalf("kitchen db render failed: %v", err)
	}
	for _, want := range []string{
		"tx *sqlx.Tx",
		"INSERT INTO MIN_LEDGER",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("kitchen db methods missing %q\n%s", want, file)
		}
	}
}

// TestNewServiceCollisionGuard pins the F1 defense in depth: two definition
// files reaching one namespaced ID with different SQL hard-error naming both
// files, never a silent walk-order overwrite.
func TestNewServiceCollisionGuard(t *testing.T) {
	main, files := strippedFixture(t, "SVC_MIN_DEDUP")
	dup := &ir.File{
		Path: "other/copy_of_lib.pc",
		Queries: []*ir.Query{
			{ID: "q1", Type: ir.QueryInsert, SQL: "INSERT INTO X VALUES (1)"},
		},
	}
	files = append(files, dup)
	// Two definitions of the same fn name in different files: both exts
	// index "fn_min_check:q1" — the guard must fire on the SQL mismatch.
	libPath := ""
	for _, f := range files {
		if strings.HasSuffix(f.Path, "fn_min_lib.pc") {
			libPath = f.Path
		}
	}
	if libPath == "" {
		t.Fatal("fn_min_lib.pc missing from the corpus")
	}
	main.ExternalFns = append(main.ExternalFns,
		ir.ExternalFn{Name: "fn_min_check", Resolved: true, HasSQL: true, DefinedIn: libPath},
		ir.ExternalFn{Name: "fn_min_check", Resolved: true, HasSQL: true, DefinedIn: dup.Path},
	)
	m, err := plan.LoadMapping("../../testdata/stripped/mappings/svc_dedup.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(plan.Options{Main: main, FnFiles: files, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewService(Options{Plan: p, Main: main, FnFiles: files})
	if err == nil || !strings.Contains(err.Error(), "cross-file collision") || !strings.Contains(err.Error(), "fn_min_lib.pc") || !strings.Contains(err.Error(), "copy_of_lib.pc") {
		t.Fatalf("collision guard error = %v, want a cross-file collision naming both files", err)
	}
}
