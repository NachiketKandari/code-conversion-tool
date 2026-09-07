package goast

import (
	"errors"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// navDBIface mirrors examples/nav/db/interface.txt — the golden shape the
// templates render and goast must accumulate into.
const navDBIface = `package db

import (
	"context"
	"mutual-fund-be/pkg/services/nav/models"
	"time"

	"github.com/jmoiron/sqlx"
	"gorm.io/gorm"
)

type store struct {
	oracle *gorm.DB
	db     *sqlx.DB
}

type NavStore interface {
	GetNavDetails(context.Context, string) ([]*models.NavDetails, error)
	GetNavHistory(context.Context, string, string, time.Time, time.Time) ([]*models.NavHistoryDetail, error)
	GetDateDetails(context.Context) (*models.DateInfo, error)
	GetCount(ctx context.Context, matchAccount string) (int64, error)
	GetSipFreedem(context.Context, string, string, string) ([]*models.SipFreedemDetail, error)
}

func NewNavStore(oracle *gorm.DB, db *sqlx.DB) NavStore {
	return &store{oracle: oracle, db: db}
}
`

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db", "interface.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInspectInterface(t *testing.T) {
	path := writeFixture(t, navDBIface)
	sigs, err := InspectInterface(path, "NavStore")
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"GetNavDetails", "GetNavHistory", "GetDateDetails", "GetCount", "GetSipFreedem"}
	if len(sigs) != len(wantNames) {
		t.Fatalf("got %d signatures, want %d", len(sigs), len(wantNames))
	}
	for i, w := range wantNames {
		if sigs[i].Name != w {
			t.Errorf("signature %d: name = %q, want %q", i, sigs[i].Name, w)
		}
	}
	wantCount := "GetCount(ctx context.Context, matchAccount string) (int64, error)"
	if sigs[3].Text != wantCount {
		t.Errorf("GetCount text = %q, want %q", sigs[3].Text, wantCount)
	}
}

func TestAccumulateInterfaceCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "db", "interface.go")
	sig := "GetDateDetails(context.Context) (*models.DateInfo, error)"
	added, err := AccumulateInterface(path, "db", "NavStore", sig)
	if err != nil || !added {
		t.Fatalf("create: added=%v err=%v", added, err)
	}
	want := "package db\n\ntype NavStore interface {\n\t" + sig + "\n}\n"
	if got := read(t, path); got != want {
		t.Errorf("created file =\n%q\nwant\n%q", got, want)
	}
	added, err = AccumulateInterface(path, "db", "NavStore", sig)
	if err != nil || added {
		t.Errorf("re-accumulate: added=%v err=%v, want false nil", added, err)
	}
	if got := read(t, path); got != want {
		t.Error("re-accumulate changed the file")
	}
}

func TestAccumulateInterfaceIdempotentAndConflict(t *testing.T) {
	path := writeFixture(t, navDBIface)

	added, err := AccumulateInterface(path, "db", "NavStore",
		"GetMarks(ctx context.Context, tx *sqlx.Tx, userId string) error")
	if err != nil || !added {
		t.Fatalf("append: added=%v err=%v", added, err)
	}

	// Identical re-append is a no-op.
	added, err = AccumulateInterface(path, "db", "NavStore",
		"GetMarks(ctx context.Context, tx *sqlx.Tx, userId string) error")
	if err != nil || added {
		t.Errorf("re-append: added=%v err=%v, want false nil", added, err)
	}

	// Renamed parameters are documentation, not identity — structural match.
	added, err = AccumulateInterface(path, "db", "NavStore",
		"GetCount(context.Context, string) (int64, error)")
	if err != nil || added {
		t.Errorf("structural no-op: added=%v err=%v, want false nil", added, err)
	}

	// Same name, different shape — conflict, never silent drift.
	_, err = AccumulateInterface(path, "db", "NavStore",
		"GetMarks(ctx context.Context, tx *sqlx.Tx, userId string) (string, error)")
	if !errors.Is(err, ErrSignatureConflict) {
		t.Errorf("conflict: err = %v, want ErrSignatureConflict", err)
	}

	// Variadic equality is structural too.
	added, err = AccumulateInterface(path, "db", "NavStore",
		"GetCount(ctx context.Context, matchAccounts ...string) (int64, error)")
	if err == nil {
		t.Error("variadic vs fixed arity must conflict")
	}

	sigs, err := InspectInterface(path, "NavStore")
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 6 {
		t.Errorf("inspect after appends: %d signatures, want 6", len(sigs))
	}
}

func TestAccumulateInterfacePreservesFormattingAndComments(t *testing.T) {
	src := `// Package db carries a doc comment that must survive.
package db

// NavStore accumulates the store contract.
type NavStore interface {
	GetDateDetails(context.Context) (*models.DateInfo, error)
	GetCount(ctx context.Context, matchAccount string) (int64, error) // count of active mappings
}
`
	path := writeFixture(t, src)
	if _, err := AccumulateInterface(path, "db", "NavStore",
		"GetMarks(ctx context.Context, tx *sqlx.Tx, userId string) error"); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	want := `// Package db carries a doc comment that must survive.
package db

// NavStore accumulates the store contract.
type NavStore interface {
	GetDateDetails(context.Context) (*models.DateInfo, error)
	GetCount(ctx context.Context, matchAccount string) (int64, error) // count of active mappings
	GetMarks(ctx context.Context, tx *sqlx.Tx, userId string) error
}
`
	if got != want {
		t.Errorf("result =\n%q\nwant\n%q", got, want)
	}
}

func TestAccumulateInterfaceEmptyInterfaceShapes(t *testing.T) {
	multiline := "package db\n\ntype NavStore interface {\n}\n"
	path := writeFixture(t, multiline)
	if _, err := AccumulateInterface(path, "db", "NavStore",
		"GetDateDetails(context.Context) (*models.DateInfo, error)"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "package db\n\ntype NavStore interface {\n\tGetDateDetails(context.Context) (*models.DateInfo, error)\n}\n" {
		t.Errorf("multiline empty: got\n%q", got)
	}

	oneliner := "package db\n\ntype NavStore interface {}\n"
	path = writeFixture(t, oneliner)
	if _, err := AccumulateInterface(path, "db", "NavStore",
		"GetDateDetails(context.Context) (*models.DateInfo, error)"); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	formatted, ferr := format.Source([]byte(got))
	if ferr != nil {
		t.Fatalf("one-liner result does not parse/format: %v\n%q", ferr, got)
	}
	if got != string(formatted) {
		t.Errorf("one-liner result not gofmt-clean:\n%q", got)
	}
}

func TestAccumulateInterfaceErrors(t *testing.T) {
	path := writeFixture(t, navDBIface)
	if _, err := AccumulateInterface(path, "db", "NavStore", "GetNavDetails("); err == nil {
		t.Error("unparsable signature must error")
	}
	if _, err := AccumulateInterface(path, "db", "NavStore", ""); err == nil {
		t.Error("empty signature must error")
	}
	if _, err := AccumulateInterface(path, "db", "Missing", "Get(x int) error"); !errors.Is(err, ErrInterfaceNotFound) {
		t.Errorf("missing interface: err = %v, want ErrInterfaceNotFound", err)
	}
	structIface := writeFixture(t, "package db\n\ntype NavStore struct{ db int }\n")
	if _, err := AccumulateInterface(structIface, "db", "NavStore", "Get(x int) error"); err == nil {
		t.Error("non-interface type must error")
	}
	if _, err := AccumulateInterface(path, "1bad", "NavStore", "Get(x int) error"); err == nil {
		t.Error("invalid package name must error")
	}
	if _, err := AccumulateInterface(path, "db", "Nav Store", "Get(x int) error"); err == nil {
		t.Error("invalid interface name must error")
	}
}

func TestAddImports(t *testing.T) {
	// Missing stdlib path lands in the local group, sorted.
	src := strings.Replace(navDBIface, "\t\"time\"\n", "", 1)
	path := writeFixture(t, src)
	if err := AddImports(path, "time"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != navDBIface {
		t.Errorf("after AddImports(time):\n%q\nwant\n%q", got, navDBIface)
	}

	// Already present — byte-identical no-op.
	before := read(t, path)
	if err := AddImports(path, "time"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != before {
		t.Error("duplicate AddImports changed the file")
	}

	// Dotted path lands in the external group, sorted after sqlx.
	if err := AddImports(path, "go.uber.org/zap"); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	if !strings.Contains(got, "\t\"github.com/jmoiron/sqlx\"\n\t\"go.uber.org/zap\"\n\t\"gorm.io/gorm\"\n") {
		t.Errorf("dotted import not grouped/sorted:\n%q", got)
	}
	if formatted, err := format.Source([]byte(got)); err != nil || got != string(formatted) {
		t.Errorf("result not gofmt-identical: err=%v", err)
	}
}

func TestAddImportsNamedAndMissingBlock(t *testing.T) {
	src := `package db

import (
	"context"
	zap "go.uber.org/zap"
)

type NavStore interface {
	GetDateDetails(context.Context) (*models.DateInfo, error)
}
`
	path := writeFixture(t, src)
	if err := AddImports(path, "go.uber.org/zap", "time", "github.com/jmoiron/sqlx"); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	want := `package db

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	zap "go.uber.org/zap"
)

type NavStore interface {
	GetDateDetails(context.Context) (*models.DateInfo, error)
}
`
	if got != want {
		t.Errorf("named import merge =\n%q\nwant\n%q", got, want)
	}

	noBlock := writeFixture(t, "package db\n\ntype NavStore interface {\n}\n")
	if err := AddImports(noBlock, "context"); err != nil {
		t.Fatal(err)
	}
	formatted, err := format.Source([]byte(read(t, noBlock)))
	if err != nil {
		t.Fatalf("created import block does not format: %v\n%q", err, read(t, noBlock))
	}
	if read(t, noBlock) != string(formatted) {
		t.Errorf("created import block not gofmt-clean:\n%q", read(t, noBlock))
	}
}

// TestGoastGateGoldenCycle is the Phase 3 self-verification gate: the full
// create → accumulate → import lifecycle converges byte-identically on
// re-run and stays gofmt-clean.
func TestGoastGateGoldenCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db", "interface.go")
	run := func() {
		t.Helper()
		sigs := []string{
			"GetNavDetails(context.Context, string) ([]*models.NavDetails, error)",
			"GetNavHistory(context.Context, string, string, time.Time, time.Time) ([]*models.NavHistoryDetail, error)",
			"GetDateDetails(context.Context) (*models.DateInfo, error)",
			"GetCount(ctx context.Context, matchAccount string) (int64, error)",
			"GetSipFreedem(context.Context, string, string, string) ([]*models.SipFreedemDetail, error)",
			"GetMarks(ctx context.Context, tx *sqlx.Tx, userId string) error",
		}
		for _, s := range sigs {
			if _, err := AccumulateInterface(path, "db", "NavStore", s); err != nil {
				t.Fatal(err)
			}
		}
		if err := AddImports(path, "time", "github.com/jmoiron/sqlx", "gorm.io/gorm"); err != nil {
			t.Fatal(err)
		}
	}
	run()
	first := read(t, path)
	run()
	if got := read(t, path); got != first {
		t.Error("second lifecycle run diverged — accumulation is not converged")
	}
	formatted, err := format.Source([]byte(first))
	if err != nil || first != string(formatted) {
		t.Errorf("final file not gofmt-identical: err=%v", err)
	}
	sigs, err := InspectInterface(path, "NavStore")
	if err != nil || len(sigs) != 6 {
		t.Errorf("inspect round-trip: %d signatures err=%v, want 6 nil", len(sigs), err)
	}
}
