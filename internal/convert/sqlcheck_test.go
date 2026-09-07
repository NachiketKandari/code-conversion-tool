package convert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/gen"
	"github.com/Public/convert-tux-to-go/internal/ledger"
	"github.com/Public/convert-tux-to-go/internal/plan"
)

// TestConvertSQLFidelityGate is the PF-6 gate: the nav run reports zero
// deviations (generated SQL is the source SQL verbatim), and a planted
// table rename surfaces as exactly one typed deviation in summary + ledger.
func TestConvertSQLFidelityGate(t *testing.T) {
	opts, _ := convertFixture(t)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SQLDeviations) != 0 {
		t.Fatalf("nav run reported deviations: %v", res.SQLDeviations)
	}
	if _, _, _, _, _, deviated := opts.Ledger.Counts(); deviated != 0 {
		t.Fatalf("deviated = %d, want 0", deviated)
	}

	dbPath := filepath.Join(opts.BaseDir, "pkg/services/nav/db/nav.go")
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(raw), "DEMO_ACCOUNT_MAP", "DEMO_ACCOUNT_MAP_X", 1)
	if mutated == string(raw) {
		t.Fatal("plant failed: DEMO_ACCOUNT_MAP not found in the db file")
	}
	if err := os.WriteFile(dbPath, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, u := range unitsOf(opts.Plan, plan.KindDBMethod) {
		opts.Ledger.Set(u.ID, ledger.StatusGenerated, "")
	}
	svc, err := gen.NewService(gen.Options{Plan: opts.Plan, Main: opts.Main, FnFiles: opts.FnFiles})
	if err != nil {
		t.Fatal(err)
	}
	res2 := &Result{}
	checkDBFidelity(context.Background(), opts, res2, svc, dbPath, unitsOf(opts.Plan, plan.KindDBMethod))

	if len(res2.SQLDeviations) != 1 {
		t.Fatalf("deviations = %v, want exactly the mutated method", res2.SQLDeviations)
	}
	if !strings.Contains(res2.SQLDeviations[0], "tables") {
		t.Errorf("deviation = %q, want the tables kind", res2.SQLDeviations[0])
	}
	var deviated []string
	for _, e := range opts.Ledger.Units {
		if e.Status == ledger.StatusDeviated {
			deviated = append(deviated, e.Name)
		}
	}
	if len(deviated) != 1 {
		t.Fatalf("deviated units = %v, want exactly one", deviated)
	}
}

// TestConvertSQLFreeLeakGate: a SQL keyword inside a SQL-free artifact's
// string literal flags as sql-leak and flips the owning controller unit to
// deviated; ordinary prose stays invisible to the check.
func TestConvertSQLFreeLeakGate(t *testing.T) {
	opts, _ := convertFixture(t)
	ctrlPath := filepath.Join(opts.BaseDir, "pkg/services/nav/controller/nav.go")
	if err := os.MkdirAll(filepath.Dir(ctrlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	leak := "package controller\n\nfunc NavHistory(c context.Context) error {\n\tq := \"SELECT comp_cd FROM demo_price\"\n\t_ = q\n\treturn nil\n}\n"
	if err := os.WriteFile(ctrlPath, []byte(leak), 0o644); err != nil {
		t.Fatal(err)
	}
	res := &Result{Files: []string{ctrlPath}}
	checkSQLFreeArtifacts(context.Background(), opts, res, filepath.Join(opts.BaseDir, "pkg/services/nav/db/nav.go"))

	if len(res.SQLDeviations) != 1 {
		t.Fatalf("deviations = %v, want exactly the leak", res.SQLDeviations)
	}
	var u plan.Unit
	for _, cand := range opts.Plan.Units {
		if cand.Kind == plan.KindControllerMethod && cand.Name == "NavHistory" {
			u = cand
			break
		}
	}
	e := opts.Ledger.Get(u.ID, string(u.Kind), u.Name)
	if e.Status != ledger.StatusDeviated || !strings.Contains(e.Error, "sql-leak") {
		t.Errorf("NavHistory ledger = %s (%s), want deviated with sql-leak", e.Status, e.Error)
	}
}
