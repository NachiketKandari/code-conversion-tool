package audit

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewCreatesRunFolder(t *testing.T) {
	root := t.TempDir()
	rec, err := New(root, "run-123")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	want := filepath.Join(root, "run-123")
	if rec.Dir() != want {
		t.Errorf("Dir = %q, want %q", rec.Dir(), want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("expected audit folder %s to exist", want)
	}
}

func TestWriteStreamsArtifact(t *testing.T) {
	rec, err := New(t.TempDir(), "run-123")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	path, err := rec.Write("triage_report.csv", func(w io.Writer) error {
		_, err := w.Write([]byte("file,num_queries\n"))
		return err
	})
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading artifact failed: %v", err)
	}
	if !strings.HasPrefix(string(data), "file,num_queries") {
		t.Errorf("unexpected artifact content: %q", string(data))
	}
}

func TestWriteRejectsBadNames(t *testing.T) {
	rec, err := New(t.TempDir(), "run-123")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	for _, name := range []string{"", ".", "..", "a/b", "a\\b", "../escape"} {
		if _, err := rec.Write(name, func(w io.Writer) error { return nil }); err == nil {
			t.Errorf("expected name %q to be rejected", name)
		}
	}
}

func TestNewRejectsEmptyArgs(t *testing.T) {
	if _, err := New("", "run-1"); err == nil {
		t.Error("expected empty root to be rejected")
	}
	if _, err := New(t.TempDir(), ""); err == nil {
		t.Error("expected empty runID to be rejected")
	}
}
