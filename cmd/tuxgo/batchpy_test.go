package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// batchpyConfig writes a run config whose batchpy.input/outDir point into
// the test's temp tree — the yaml-driven I/O seam with no CLI positional.
func batchpyConfig(t *testing.T, input, outDir string, workers int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".tuxgo.yaml")
	writeFile(t, path, fmt.Sprintf(`run:
  llm: false
concurrency:
  workers: %d
batchpy:
  input: %s
  outDir: %s
`, workers, input, outDir))
	return path
}

// TestBatchpyModuleCollisionGate: distinct files can declare the same batch
// service (the module name comes from the c_ServiceName literal) — that must
// hard-error before anything is written, never race one output file.
func TestBatchpyModuleCollisionGate(t *testing.T) {
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "batch", "BAT_DEMO_REJECT.pc"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "BAT_ONE.pc"), string(src))
	writeFile(t, filepath.Join(dir, "BAT_TWO.pc"), string(src))

	out := filepath.Join(t.TempDir(), "out")
	cfgPath := batchpyConfig(t, dir, out, 2)

	err = runBatchpy(context.Background(), []string{"-config", cfgPath, "-no-llm"})
	if err == nil || !strings.Contains(err.Error(), "both produce module") {
		t.Fatalf("err = %v, want the module-collision gate", err)
	}
	entries, rerr := os.ReadDir(out)
	if rerr == nil && len(entries) > 0 {
		t.Errorf("collision run wrote %d files; nothing must be written", len(entries))
	}
}

// TestBatchpyDirConcurrentDistinct: distinct batch files convert end-to-end
// on their own goroutines (workers=2) and both modules land.
func TestBatchpyDirConcurrentDistinct(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"BAT_DEMO_REJECT.pc", "BAT_DEMO_RETURNS.pc"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "batch", name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, name), string(src))
	}

	out := filepath.Join(t.TempDir(), "out")
	cfgPath := batchpyConfig(t, dir, out, 2)

	if err := runBatchpy(context.Background(), []string{"-config", cfgPath, "-no-llm"}); err != nil {
		t.Fatalf("runBatchpy: %v", err)
	}
	for _, mod := range []string{"bat_demo_reject.py", "bat_demo_returns.py"} {
		if _, err := os.Stat(filepath.Join(out, mod)); err != nil {
			t.Errorf("missing module %s: %v", mod, err)
		}
	}
}

// TestBatchpyWrongPipelineGuard pins severity F4: a Tuxedo service file
// (SVC_* entry) fed to the batch pipeline is skipped with a visible reason —
// no plausible-looking empty module, no run failure.
func TestBatchpyWrongPipelineGuard(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"BAT_DEMO_REJECT.pc"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "batch", name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, name), string(src))
	}
	svcSrc, err := os.ReadFile(filepath.Join("..", "..", "testdata", "adversarial", "BAT_ADV_SVC_FILE.pc"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "ADV_SERVICE.pc"), string(svcSrc))

	out := filepath.Join(t.TempDir(), "out")
	cfgPath := batchpyConfig(t, dir, out, 2)

	if err := runBatchpy(context.Background(), []string{"-config", cfgPath, "-no-llm"}); err != nil {
		t.Fatalf("wrong-pipeline skip must not fail the run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "adv_service.py")); err == nil {
		t.Error("service file must not produce a batch module")
	}
	if _, err := os.Stat(filepath.Join(out, "bat_demo_reject.py")); err != nil {
		t.Errorf("real batch module missing: %v", err)
	}
}
