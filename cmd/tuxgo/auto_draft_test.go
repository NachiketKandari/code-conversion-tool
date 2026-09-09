package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/config"
)

// chdirTemp moves the test into a fresh working directory — the mappings/
// convention is cwd-relative, so the auto-draft loop needs a clean home.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

// TestConvertAutoDraftThenStop pins the draft-then-stop journey: with no
// mapping anywhere, convert scans the target, writes the draft to the
// mappings/ convention, and converts nothing; the re-run of the SAME
// command finds the draft (deterministic names are loader-legal) and
// converts for real.
func TestConvertAutoDraftThenStop(t *testing.T) {
	corpus := convertCorpus(t) // fixture reads are cwd-relative: build before chdir
	chdirTemp(t)
	target := filepath.Join(corpus, "SVC_DEMO_LIST.pc")
	cfg := writeFanoutConfig(t, 1, filepath.Join(t.TempDir(), "staged"), filepath.Join(t.TempDir(), "ledger"))

	// Run 1: draft, then stop — no output tree.
	if err := runConvert(context.Background(), []string{"-config", cfg, "-no-llm", target}); err != nil {
		t.Fatalf("convert (no mapping): %v", err)
	}
	draftPath := filepath.Join("mappings", "SVC_DEMO_LIST.mapping.yaml")
	data, err := os.ReadFile(draftPath)
	if err != nil {
		t.Fatalf("draft not written to the mappings/ convention: %v", err)
	}
	draft := string(data)
	if !strings.Contains(draft, "service: svc_demo_list") {
		t.Errorf("draft service not prefilled:\n%s", draft)
	}
	if !strings.Contains(draft, "name: \"") || strings.Contains(draft, `name: ""`) {
		t.Errorf("draft names not pre-filled by the deterministic picker:\n%s", draft)
	}
	if strings.Contains(draft, "# module: your-app") || strings.Contains(draft, "routeGroup") {
		t.Errorf("draft carries stale manual-edit scaffolding:\n%s", draft)
	}

	// Run 2: the same command converts.
	staged := filepath.Join(t.TempDir(), "staged")
	cfg = writeFanoutConfig(t, 1, staged, filepath.Join(t.TempDir(), "ledger"))
	if err := runConvert(context.Background(), []string{"-config", cfg, "-no-llm", target}); err != nil {
		t.Fatalf("convert (mapping from mappings/): %v", err)
	}
	// module defaults to the service name, so the staged tree is rooted at
	// the service: staged/svc_demo_list/{models,db,controller,handler}.
	matches, err := filepath.Glob(filepath.Join(staged, "svc_demo_list", "models", "*.go"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("second run did not convert: glob=%v err=%v", matches, err)
	}
}

// TestConvertDirAutoDraftThenStop pins the multi-service loop: a directory
// target drafts one mapping per entry (dir-mode source: lines), and the
// re-run fans out one worker per service — zero hand-written yamls.
func TestConvertDirAutoDraftThenStop(t *testing.T) {
	corpus := convertCorpus(t)
	chdirTemp(t)
	cfg := writeFanoutConfig(t, 1, filepath.Join(t.TempDir(), "staged"), filepath.Join(t.TempDir(), "ledger"))

	if err := runConvert(context.Background(), []string{"-config", cfg, "-no-llm", corpus}); err != nil {
		t.Fatalf("convert (no mapping, dir target): %v", err)
	}
	for _, base := range []string{"SVC_DEMO_LIST.mapping.yaml", "SVC_DEMO_TWO.mapping.yaml"} {
		data, err := os.ReadFile(filepath.Join("mappings", base))
		if err != nil {
			t.Fatalf("dir draft %s missing: %v", base, err)
		}
		if !strings.Contains(string(data), "source: ") {
			t.Errorf("dir draft %s missing its source: line:\n%s", base, data)
		}
	}

	staged := filepath.Join(t.TempDir(), "staged")
	cfg = writeFanoutConfig(t, 1, staged, filepath.Join(t.TempDir(), "ledger"))
	if err := runConvert(context.Background(), []string{"-config", cfg, "-no-llm", corpus}); err != nil {
		t.Fatalf("convert (mapping dir from mappings/): %v", err)
	}
	for _, svc := range []string{"svc_demo_list", "svc_demo_two"} {
		if _, err := os.Stat(filepath.Join(staged, svc, "db", "interface.go")); err != nil {
			t.Errorf("service %s did not convert on the re-run: %v", svc, err)
		}
	}
}

// TestMappingForEntry pins the single-entry mapping pick inside a mapping
// directory: source: match wins over stem match, and a mismatched
// directory is an error naming both sides.
func TestMappingForEntry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "alpha.yaml"), mappingYAML("SVC_DEMO_LIST.pc", "alpha"))
	writeFile(t, filepath.Join(dir, "SVC_DEMO_TWO.mapping.yaml"), mappingYAML("SVC_DEMO_TWO.pc", "beta"))

	got, err := mappingForEntry(dir, "SVC_DEMO_LIST.pc")
	if err != nil || filepath.Base(got) != "alpha.yaml" {
		t.Errorf("source: match = %s, %v; want alpha.yaml", got, err)
	}
	got, err = mappingForEntry(dir, "SVC_DEMO_TWO.pc")
	if err != nil || filepath.Base(got) != "SVC_DEMO_TWO.mapping.yaml" {
		t.Errorf("stem fallback = %s, %v; want the stem-named yaml", got, err)
	}
	if _, err := mappingForEntry(dir, "SVC_OTHER.pc"); err == nil || !strings.Contains(err.Error(), "no mapping in") {
		t.Errorf("unmatched entry: err = %v, want directory guidance", err)
	}
}

// TestMappingForConvention pins the resolution chain: flag → config →
// mappings/ convention, and "" when nothing exists.
func TestMappingForConvention(t *testing.T) {
	chdirTemp(t)
	cfg := config.Default() // Convert.Mapping is empty by default

	if got := mappingFor("", cfg); got != "" {
		t.Errorf("no mapping anywhere: got %q, want \"\"", got)
	}
	writeFile(t, filepath.Join("mappings", "x.mapping.yaml"), mappingYAML("SVC_DEMO_LIST.pc", "svc_demo_list"))
	if got := mappingFor("", cfg); got != "mappings" {
		t.Errorf("mappings/ convention: got %q, want mappings", got)
	}
	if got := mappingFor("explicit.yaml", cfg); got != "explicit.yaml" {
		t.Errorf("flag must win: got %q", got)
	}
}
