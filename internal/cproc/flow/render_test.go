package flow

import (
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

func buildDemoLinked(t *testing.T) *Tree {
	t.Helper()
	facts, err := scanner.ScanBytes([]byte(demoSrc), "demo.pc")
	if err != nil {
		t.Fatal(err)
	}
	irFile := &ir.File{Queries: []*ir.Query{{
		// classifySQL uppercases cursor names — match that exactly.
		ID: "cur_q1", CursorName: "CUR_DEMO", CursorFlattened: true,
		StartLine: 10, EndLine: 12,
	}}}
	return Build([]byte(demoSrc), facts, "SVC_DEMO", irFile)
}

type fakeResolver struct{}

func (fakeResolver) StoreCall(queryID string) (string, bool) {
	return "s.store.GetDemoRows(c, request.CompCd)", true
}

func (fakeResolver) RowType(queryID string) (string, bool) {
	return "*models.DemoRow", true
}

func TestRenderFetchIterate(t *testing.T) {
	tree := buildDemoLinked(t)
	flag := flagBranch(t, tree)
	// The flag branch legitimately carries the cursor query too (the
	// DECLARE sits inside its span); the fetch loop also links it via the
	// FETCH cursor name.
	if len(flag.QueryIDs) == 0 {
		t.Fatal("flag branch should link the cursor query by line overlap")
	}
	var loop *Node
	for _, c := range flag.Children {
		if c.Kind == KindLoop && c.Sub == "while" {
			loop = c
		}
	}
	if loop == nil {
		t.Fatal("while loop not found")
	}
	if len(loop.QueryIDs) != 1 || loop.QueryIDs[0] != "cur_q1" {
		t.Fatalf("loop QueryIDs = %v, want [cur_q1] (cursor-flattened attach)", loop.QueryIDs)
	}

	out := RenderTree(tree, fakeResolver{})
	for _, want := range []string{
		"rows, err := s.store.GetDemoRows(c, request.CompCd)",
		"if err != nil {",
		"for _, row := range rows { // []*models.DemoRow",
		"if c_flag == \"H\" {",
	} {
		if !strings.Contains(out.Body, want) {
			t.Errorf("draft missing %q\n---\n%s", want, out.Body)
		}
	}
	if out.Elided == 0 {
		t.Error("expected dropped-construct elisions to be counted")
	}
	if len(out.TODOs) == 0 {
		t.Error("expected explicit TODO residue")
	}
	if strings.Contains(out.Body, "tpalloc") || strings.Contains(out.Body, "userlog") {
		t.Errorf("dropped constructs leaked into the draft:\n%s", out.Body)
	}
	if strings.Contains(out.Body, "'H'") {
		t.Errorf("C char literal leaked into the draft:\n%s", out.Body)
	}
}

func TestRenderGuardsCollapse(t *testing.T) {
	tree := buildDemoLinked(t)
	out := RenderTree(tree, fakeResolver{})
	if strings.Contains(out.Body, "FML_ERR_MSG") {
		t.Errorf("request guard should collapse, but its error add leaked:\n%s", out.Body)
	}
	if strings.Contains(out.Body, "i_err_op[0] :=") {
		t.Errorf("Fadd-result check assign leaked as Go assignment:\n%s", out.Body)
	}
}

func TestRenderSpanIsolatesEndpoint(t *testing.T) {
	tree := buildDemoLinked(t)
	flag := flagBranch(t, tree)
	out := RenderSpan(tree, fakeResolver{}, flag.Line, flag.EndLine, 1)
	if !strings.Contains(out.Body, "if c_flag == \"H\" {") {
		t.Errorf("span draft missing the branch header:\n%s", out.Body)
	}
	if strings.Contains(out.Body, "tpreturn(TPSUCCESS") {
		t.Errorf("span draft leaked the entry-level tail:\n%s", out.Body)
	}
}

func TestRenderPlaceholdersWithoutResolver(t *testing.T) {
	tree := buildDemoLinked(t)
	out := RenderTree(tree, nil)
	if !strings.Contains(out.Body, "TODO name for cur_q1") {
		t.Errorf("missing store-call placeholder:\n%s", out.Body)
	}
}

func TestRenderDoWhile(t *testing.T) {
	src := `void SVC_DW(TPSVCINFO *rqst) {
	do {
		work();
	} while (a < b);
}
`
	facts, err := scanner.ScanBytes([]byte(src), "dw.pc")
	if err != nil {
		t.Fatal(err)
	}
	out := RenderTree(Build([]byte(src), facts, "SVC_DW", nil), nil)
	for _, want := range []string{"for {", "if !(a < b) {", "break"} {
		if !strings.Contains(out.Body, want) {
			t.Errorf("do-while draft missing %q:\n%s", want, out.Body)
		}
	}
}

func TestMatchHints(t *testing.T) {
	tree := buildDemoLinked(t)
	hints := Match(tree)
	byKind := map[HintKind]int{}
	for _, h := range hints {
		byKind[h.Kind]++
	}
	if byKind[HintFetchIterate] != 1 {
		t.Errorf("fetch-then-iterate hints = %d, want 1", byKind[HintFetchIterate])
	}
	if byKind[HintRequestGuard] != 1 {
		t.Errorf("request-guard hints = %d, want 1", byKind[HintRequestGuard])
	}
	if byKind[HintErrOpLoop] != 1 {
		t.Errorf("err-op-check-loop hints = %d, want 1", byKind[HintErrOpLoop])
	}
	if byKind[HintDebugIf] != 1 {
		t.Errorf("debug-only-if hints = %d, want 1", byKind[HintDebugIf])
	}
	if byKind[HintResponseFanout] < 1 {
		t.Errorf("response-fanout hints = %d, want >= 1", byKind[HintResponseFanout])
	}
}
