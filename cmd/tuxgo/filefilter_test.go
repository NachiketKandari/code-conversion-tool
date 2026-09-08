package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/config"
)

// TestFileFilterScoping pins the convert.fileFilter contract: case-
// insensitive substring match on the entry file's base name selects which
// services convert, the full directory set stays ingested as the fn-
// resolution pool (helper libs never need to match), empty = all entries,
// and a filter matching no entry is a visible error.
func TestFileFilterScoping(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "SVC_MF_NAVLIST.pc"), fmt.Sprintf(svcEntrySrc, "SVC_MF_NAVLIST", "navlist"))
	writeFile(t, filepath.Join(dir, "SVC_DEMO_OTHER.pc"), fmt.Sprintf(svcEntrySrc, "SVC_DEMO_OTHER", "other"))
	writeFile(t, filepath.Join(dir, "fn_mf_lib.pc"), "long fn_mf_lib(void)\n{\n    return 1;\n}\n")

	cfg := config.Default()

	// Empty filter (default) = every entry, no behavior change.
	files, mains, excluded, err := extractPlanIR(context.Background(), dir, cfg, false)
	if err != nil {
		t.Fatalf("no filter: %v", err)
	}
	if len(files) != 3 || len(mains) != 2 || len(excluded) != 0 {
		t.Fatalf("no filter: files=%d mains=%d excluded=%d, want 3/2/0", len(files), len(mains), len(excluded))
	}

	// "mf_" selects SVC_MF_NAVLIST.pc case-insensitively; the helper file
	// and the excluded entry stay in the ingested pool.
	cfg.Convert.FileFilter = "mf_"
	files, mains, excluded, err = extractPlanIR(context.Background(), dir, cfg, false)
	if err != nil {
		t.Fatalf("filter mf_: %v", err)
	}
	if len(mains) != 1 || mains[0].Entry != "SVC_MF_NAVLIST" {
		t.Fatalf("filter mf_: mains = %v, want only SVC_MF_NAVLIST", mains)
	}
	if len(files) != 3 {
		t.Errorf("filter mf_: ingested files = %d, want 3 (fn pool intact)", len(files))
	}
	if len(excluded) != 1 || excluded[0] != "svc_demo_other.pc" {
		t.Errorf("filter mf_: excluded = %v, want [svc_demo_other.pc]", excluded)
	}

	// Uppercase filter matches too (the comparison is case-insensitive).
	cfg.Convert.FileFilter = "MF_NAV"
	if _, mains, _, err = extractPlanIR(context.Background(), dir, cfg, false); err != nil || len(mains) != 1 {
		t.Fatalf("filter MF_NAV: mains=%d err=%v, want 1/nil", len(mains), err)
	}

	// A filter matching no entry is a loud, named error.
	cfg.Convert.FileFilter = "no_such_family"
	_, _, _, err = extractPlanIR(context.Background(), dir, cfg, false)
	if err == nil || !strings.Contains(err.Error(), "no_such_family") {
		t.Fatalf("filter no_such_family: err = %v, want a named no-match error", err)
	}

	// An explicitly passed file bypasses the filter (deliberate target).
	_, mains, _, err = extractPlanIR(context.Background(), filepath.Join(dir, "SVC_MF_NAVLIST.pc"), cfg, false)
	if err != nil || len(mains) != 1 {
		t.Fatalf("explicit file with non-matching filter: mains=%d err=%v, want the file to convert", len(mains), err)
	}
}

// TestBatchpyFileFilter pins the batchpy.fileFilter seam end-to-end: a
// directory run with the filter set converts only the matching batch files.
func TestBatchpyFileFilter(t *testing.T) {
	dir := t.TempDir()
	sources := map[string]string{
		"BAT_MF_ONE.pc": "BAT_DEMO_REJECT.pc",
		"BAT_MF_TWO.pc": "BAT_DEMO_RETURNS.pc",
		"BAT_OTHER.pc":  "BAT_DEMO_REJECT.pc", // filtered out before conversion — content irrelevant
	}
	for name, srcName := range sources {
		src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "batch", srcName))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, name), string(src))
	}

	out := filepath.Join(t.TempDir(), "out")
	cfgPath := filepath.Join(t.TempDir(), ".tuxgo.yaml")
	writeFile(t, cfgPath, fmt.Sprintf(`run:
  llm: false
concurrency:
  workers: 2
batchpy:
  input: %s
  outDir: %s
  fileFilter: "bat_mf_"
`, dir, out))

	if err := runBatchpy(context.Background(), []string{"-config", cfgPath, "-no-llm"}); err != nil {
		t.Fatalf("runBatchpy with filter: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	// Module names come from each batch's c_ServiceName literal, not the
	// file name — exactly the two mf files' modules must land.
	if len(entries) != 2 {
		t.Fatalf("modules = %d, want exactly the two bat_mf_ files' modules", len(entries))
	}
	for _, want := range []string{"bat_demo_reject.py", "bat_demo_returns.py"} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}

	// A filter matching nothing errors before any module is written.
	out2 := filepath.Join(t.TempDir(), "out")
	writeFile(t, cfgPath, fmt.Sprintf(`run:
  llm: false
batchpy:
  input: %s
  outDir: %s
  fileFilter: "no_such_family"
`, dir, out2))
	if err := runBatchpy(context.Background(), []string{"-config", cfgPath, "-no-llm"}); err == nil || !strings.Contains(err.Error(), "no_such_family") {
		t.Fatalf("non-matching batchpy filter: err = %v, want a named no-match error", err)
	}
}

// TestConvertFanoutFileFilter pins the convert.fileFilter seam end-to-end on
// the fan-out path: the filter selects which entries convert, the fn lib
// (never matching the filter) still resolves the excluded-from-filter
// service's fn units, and mappings for filter-excluded entries are skipped
// with a warning instead of the orphan-drift error.
func TestConvertFanoutFileFilter(t *testing.T) {
	corpus := convertCorpus(t)
	mappingDir := t.TempDir()
	writeFile(t, filepath.Join(mappingDir, "alpha.yaml"), mappingYAML("SVC_DEMO_LIST.pc", "alpha"))
	writeFile(t, filepath.Join(mappingDir, "beta.yaml"), mappingYAML("SVC_DEMO_TWO.pc", "beta"))

	staged, ledger := filepath.Join(t.TempDir(), "staged"), filepath.Join(t.TempDir(), "ledger")
	cfgPath := writeFanoutConfig(t, 2, staged, ledger)
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfgPath, strings.Replace(string(cfg), "paths:\n", "convert:\n  fileFilter: \"demo_list\"\npaths:\n", 1))

	if err := runFanout(t, cfgPath, mappingDir, corpus); err != nil {
		t.Fatalf("filtered fan-out run: %v", err)
	}

	// Only the selected service landed; the excluded one has no subtree.
	if _, err := os.Stat(filepath.Join(staged, "alpha", "pkg", "services", "alpha", "db", "alpha.go")); err != nil {
		t.Errorf("alpha artifacts missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "beta")); err == nil {
		t.Error("beta was excluded by the filter — no subtree expected")
	}

	// The fn lib never matches the filter but still resolved: alpha's db
	// file carries the fn unit's pinned method.
	data, err := os.ReadFile(filepath.Join(staged, "alpha", "pkg", "services", "alpha", "db", "alpha.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "func (g *store) IsDemoActive(") {
		t.Error("fn-pool resolution broke under the filter — IsDemoActive (fn_is_demo_active:q1) missing")
	}
}
