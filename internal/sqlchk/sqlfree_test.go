package sqlchk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckSQLFree(t *testing.T) {
	src := `package controller

func NavHistory(c context.Context) error {
	q := "SELECT comp_cd FROM demo_price WHERE stat = 'A'"
	_ = q
	return nil
}

func NavList(c context.Context) error {
	msg := "please select an option"
	_ = msg
	return nil
}

func SipInsurance(c context.Context) error {
	const cleanup = "DELETE FROM demo_tmp"
	_ = cleanup
	return nil
}
`
	path := filepath.Join(t.TempDir(), "nav.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := CheckSQLFree([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 file flagged", len(results))
	}
	var fns []string
	for _, d := range results[0].Deviations {
		if d.Kind != DevSQLLeak {
			t.Errorf("kind = %s, want sql-leak", d.Kind)
		}
		fns = append(fns, d.Fn)
	}
	if len(fns) != 2 || fns[0] != "NavHistory" || fns[1] != "SipInsurance" {
		t.Errorf("leaks attributed to %v, want [NavHistory SipInsurance] — the plain message must not flag", fns)
	}
	if !strings.Contains(results[0].Deviations[0].Detail, "nav.go:4") {
		t.Errorf("detail = %q, want the literal's line", results[0].Deviations[0].Detail)
	}
}

func TestCheckSQLFreeClean(t *testing.T) {
	src := `package controller

func NavHistory(c context.Context, s Store) ([]*models.Row, error) {
	rows, err := s.GetNavHistory(c, "A")
	if err != nil {
		return nil, err
	}
	return rows, nil
}
`
	path := filepath.Join(t.TempDir(), "nav.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := CheckSQLFree([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("clean file flagged: %+v", results)
	}
}
