package batchflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

func buildFlow(t *testing.T, path string) (*ir.File, *Flow) {
	t.Helper()
	facts, err := scanner.ScanFile(path)
	if err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	f, err := ir.ExtractFile(path)
	if err != nil {
		t.Fatalf("extract %s: %v", path, err)
	}
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return f, Build(f, facts, string(src))
}

func TestBuildSimpleRubric(t *testing.T) {
	_, flow := buildFlow(t, filepath.Join("..", "..", "..", "testdata", "batch", "BAT_DEMO_REJECT.pc"))
	if flow.ServiceName != "bat_demo_reject" {
		t.Errorf("service name = %q", flow.ServiceName)
	}
	if flow.Entry != "main" {
		t.Errorf("entry = %q", flow.Entry)
	}
	if flow.Shape != ShapeSimple {
		t.Errorf("shape = %q, want simple", flow.Shape)
	}
	if len(flow.CursorGroups) != 2 {
		t.Fatalf("cursor groups = %d, want 2", len(flow.CursorGroups))
	}
	for _, g := range flow.CursorGroups {
		if g.Select == nil || g.DML == nil {
			t.Errorf("group %s: select=%v dml=%v — the rubric pairs both", g.CursorName, g.Select != nil, g.DML != nil)
		}
	}
	if len(flow.Steps) != 0 {
		t.Errorf("steps = %d, want 0 (fully covered)", len(flow.Steps))
	}
	if len(flow.Loops) != 2 {
		t.Errorf("loops = %d, want 2", len(flow.Loops))
	}
	if len(flow.Dropped) == 0 {
		t.Error("dropped sites empty — tp*/errlog/MEMSET/SETNULL must be inventoried")
	}
	kinds := map[DropKind]bool{}
	for _, d := range flow.Dropped {
		kinds[d.Kind] = true
	}
	for _, want := range []DropKind{DropTuxedo, DropErrorLog, DropRegistration} {
		if !kinds[want] {
			t.Errorf("dropped kinds missing %q", want)
		}
	}
}

func TestBuildStatefulRubric(t *testing.T) {
	_, flow := buildFlow(t, filepath.Join("..", "..", "..", "testdata", "batch", "BAT_DEMO_RETURNS.pc"))
	if flow.Shape != ShapeStateful {
		t.Errorf("shape = %q, want stateful", flow.Shape)
	}
	if len(flow.Steps) == 0 {
		t.Error("steps empty — the in-loop single-row SELECT must surface as an uncovered step")
	}
	foundRebuild := false
	for _, s := range flow.Steps {
		if s.TruncateSQL != "" {
			foundRebuild = true
		}
	}
	if !foundRebuild {
		t.Error("truncate+insert rebuild pairing not detected")
	}
}
