package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestWriteConcurrentArtifacts pins the fan-out contract: one Recorder is
// shared across service workers, so concurrent Writes must all land intact.
func TestWriteConcurrentArtifacts(t *testing.T) {
	rec, err := New(t.TempDir(), "run-123")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	const n = 32
	var wg sync.WaitGroup
	paths := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("svc-%02d-ledger.json", i)
			paths[i], errs[i] = rec.Write(name, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "{\"service\":%d}", i)
				return err
			})
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("write %d failed: %v", i, errs[i])
		}
		data, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatalf("read artifact %d: %v", i, err)
		}
		if want := fmt.Sprintf("{\"service\":%d}", i); string(data) != want {
			t.Errorf("artifact %d = %q, want %q", i, data, want)
		}
	}
}

// TestWriteExchange pins the shared LLM-trace artifact: the default name is
// <kind>-<name>-attempt<attempt>.json (sanitized), File overrides it, and
// the payload carries the full prompt + raw response + gate errors.
func TestWriteExchange(t *testing.T) {
	dir := t.TempDir()
	rec, err := New(dir, "run-exchange")
	if err != nil {
		t.Fatal(err)
	}
	path, err := rec.WriteExchange(Exchange{
		Unit: "u01", Kind: "controller", Name: "NavHistory", Attempt: 1,
		Template: "controller_method", Prompt: "PROMPT", Response: "RESPONSE",
		Errors: []string{"parse: unexpected }"}, Outcome: "failed",
	})
	if err != nil {
		t.Fatalf("WriteExchange: %v", err)
	}
	if filepath.Base(path) != "controller-NavHistory-attempt1.json" {
		t.Errorf("default artifact name = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e Exchange
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("payload is not an Exchange: %v", err)
	}
	if e.Prompt != "PROMPT" || e.Response != "RESPONSE" || e.Outcome != "failed" || len(e.Errors) != 1 {
		t.Errorf("exchange = %+v", e)
	}

	// File override (convert's pinned unit-<id>-attempt<n> convention) and
	// path-unsafe names are sanitized.
	path, err = rec.WriteExchange(Exchange{Name: "svc/x", Kind: "batchpy", File: "unit-u09-attempt0.json"})
	if err != nil || filepath.Base(path) != "unit-u09-attempt0.json" {
		t.Errorf("File override = %q, %v", path, err)
	}
	path, err = rec.WriteExchange(Exchange{Name: "a/b c", Kind: "discover"})
	if err != nil || strings.Contains(filepath.Base(path), "/") {
		t.Errorf("unsanitized name: %q, %v", path, err)
	}
}
