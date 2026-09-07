package sqlchk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDBFileExtraction(t *testing.T) {
	src := `package db

import "context"

func GetX(c context.Context, cd string) ([]*models.X, error) {
	query := ` + "`" + `SELECT A FROM DEMO_PRICE WHERE B = :1` + "`" + `
	_ = query
	return nil, nil
}

func GetY(c context.Context) (int64, error) {
	q := "SELECT COUNT(*) FROM DEMO_PRICE"
	_ = q
	return 0, nil
}

func GetZ(c context.Context) error {
	return nil
}
`
	path := filepath.Join(t.TempDir(), "nav.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := CheckDBFile(path, []Target{
		{Method: "GetX", QueryID: "q1", Source: "SELECT A FROM DEMO_PRICE WHERE B = :1"},
		{Method: "GetY", QueryID: "q2", Source: "SELECT COUNT(*) FROM DEMO_PRICE"},
		{Method: "GetZ", QueryID: "q3", Source: "SELECT 1 FROM DUAL"},
		{Method: "GetMissing", QueryID: "q4", Source: "SELECT 1 FROM DUAL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	if results[0].Status != StatusMatch {
		t.Errorf("GetX = %s (%+v), want match", results[0].Status, results[0].Deviations)
	}
	if results[1].Status != StatusUnverifiable {
		t.Errorf("GetY = %s, want unverifiable — interpreted strings are not the embedded SQL", results[1].Status)
	}
	if results[2].Status != StatusUnverifiable {
		t.Errorf("GetZ = %s, want unverifiable — no literal at all", results[2].Status)
	}
	if results[3].Status != StatusUnverifiable {
		t.Errorf("GetMissing = %s, want unverifiable", results[3].Status)
	}
}

func TestCheckDBFileParseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.go")
	if err := os.WriteFile(path, []byte("not go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckDBFile(path, nil); err == nil {
		t.Fatal("expected a parse error, got none")
	}
}
