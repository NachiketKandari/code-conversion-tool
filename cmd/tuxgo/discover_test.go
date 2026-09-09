package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/flow"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
	"github.com/Public/convert-tux-to-go/internal/plan"
)

const navFixture = "../../testdata/nav/SVC_DEMO_LIST.pc"

func discoverDraft(t *testing.T) string {
	t.Helper()
	f, err := ir.ExtractFileOpts(navFixture, ir.Options{})
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(navFixture)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := scanner.ScanBytes(src, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	tree := flow.Build(src, facts, f.Entry, f)
	candidates := flow.Discover(tree, f.Conditions)
	if len(candidates) == 0 {
		t.Fatal("no candidates discovered for the nav fixture")
	}
	return renderDraft(f, candidates, false)
}

// TestDiscoverDraftRoundTrip pins the scan-then-tag contract: the untagged
// draft fails mapping validation (the tag instruction), and after tagging
// names/routes the same bytes load through the strict loader unchanged.
func TestDiscoverDraftRoundTrip(t *testing.T) {
	draft := discoverDraft(t)
	if !strings.Contains(draft, `name: ""`) || !strings.Contains(draft, `route: ""`) {
		t.Errorf("draft missing tag placeholders:\n%s", draft)
	}
	if strings.Contains(draft, "service: \n") {
		t.Error("service must be prefilled from the entry name")
	}

	// Untagged: the strict loader rejects the empty identifiers.
	untagged := filepath.Join(t.TempDir(), "untagged.yaml")
	if err := os.WriteFile(untagged, []byte(draft), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.LoadMapping(untagged); err == nil {
		t.Error("untagged draft must fail LoadMapping")
	}

	// Tagged: same draft with the placeholders filled and module uncommented
	// — exactly the flow the draft header instructs — loads clean. Names are
	// unique per endpoint, as the validation requires.
	tagged := draft
	tagged = strings.Replace(tagged, "# module: your-app/", "module: your-app/", 1)
	names := []string{"NavHistory", "SipFreedem", "SipInsurance", "NavList", "Extra"}
	for _, n := range names {
		tagged = strings.Replace(tagged, `name: ""`, `name: `+n, 1)
	}
	tagged = strings.ReplaceAll(tagged, `route: ""`, `route: /route`)
	taggedPath := filepath.Join(t.TempDir(), "tagged.yaml")
	if err := os.WriteFile(taggedPath, []byte(tagged), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := plan.LoadMapping(taggedPath)
	if err != nil {
		t.Fatalf("tagged draft must load: %v", err)
	}
	if m.Service != "svc_demo_list" || len(m.Endpoints) == 0 {
		t.Errorf("loaded mapping = %+v", m)
	}
	for _, e := range m.Endpoints {
		if e.Name == "" || !strings.HasPrefix(e.Route, "/") {
			t.Errorf("endpoint not tagged: %+v", e)
		}
	}
}

// TestDiscoverDirMode runs the command over the real corpus directory: one
// draft per service entry, batch/fn-lib files skipped loudly.
func TestDiscoverDirMode(t *testing.T) {
	out := t.TempDir()
	if err := runDiscover(context.Background(), []string{"../../tuxExamples", "-out", out}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var drafts int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".mapping.yaml") {
			drafts++
		}
	}
	if drafts != 1 {
		t.Fatalf("drafts written = %d, want 1 (mainTux.pc)", drafts)
	}
}

// TestDiscoverDirModeHonestZero pins the rubric on the stripped corpus: its
// condition bodies only write (reads live in the entry preamble), so zero
// candidates is the CORRECT answer — the skip lines say so, never a guess.
func TestDiscoverDirModeHonestZero(t *testing.T) {
	out := t.TempDir()
	if err := runDiscover(context.Background(), []string{"../../testdata/stripped", "-out", out}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".mapping.yaml") {
			t.Errorf("unexpected draft %s (stripped corpus has no qualifying candidates)", e.Name())
		}
	}
}

// TestDiscoverDirModeRequiresOut pins the guard: a directory target without
// -out is an error, never a silent no-op.
func TestDiscoverDirModeRequiresOut(t *testing.T) {
	if err := runDiscover(context.Background(), []string{"../../testdata/stripped"}); err == nil {
		t.Error("dir mode without -out must fail")
	}
}
