package flow

import (
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

// The fault-tolerance contract (PRD-2026-09-10, user directive): the flow
// parser degrades to loud residue on malformed input — it never panics,
// never returns nil for a scanned file, and never silently drops what it
// cannot classify.

func mustTolerate(t *testing.T, name, src string) *Tree {
	t.Helper()
	facts, err := scanner.ScanBytes([]byte(src), name)
	if err != nil {
		t.Fatalf("%s: scan failed: %v", name, err)
	}
	tree := Build([]byte(src), facts, "", nil)
	if tree == nil {
		t.Fatalf("%s: Build returned nil", name)
	}
	return tree
}

func TestTolerateUnbalancedBraces(t *testing.T) {
	src := `void SVC_BAD(TPSVCINFO *rqst) {
	while (1) {
		fetch();
	if (x) {
		work();
}
`
	tree := mustTolerate(t, "unbalanced.pc", src)
	// Whatever the scanner could close stays classified; the rest is loud
	// residue, not a crash.
	if len(tree.Root) == 0 {
		t.Fatal("expected nodes or residue, got nothing")
	}
}

func TestTolerateParenlessHeaders(t *testing.T) {
	src := `void SVC_PARENS(TPSVCINFO *rqst) {
	if x {
		work();
	}
	while y {
		more();
	}
	do {
		thing();
	}
}
`
	tree := mustTolerate(t, "parens.pc", src)
	// Unbraced/parenless headers are not loop/branch anchors — the lines
	// fall to residue and are reported, never dropped.
	if tree.Coverage.Unknown == 0 {
		t.Errorf("parenless headers should surface as residue: %+v", tree.Coverage)
	}
}

func TestTolerateUnterminatedCommentAndString(t *testing.T) {
	src := `void SVC_CUT(TPSVCINFO *rqst) {
	s = "never closed...
	work();
	/* unterminated comment
	more();
}
`
	tree := mustTolerate(t, "cut.pc", src)
	if tree == nil {
		t.Fatal("nil tree")
	}
}

func TestTolerateEmptyBodies(t *testing.T) {
	src := `void SVC_EMPTY(TPSVCINFO *rqst) {
}
void SVC_ALSO_EMPTY(TPSVCINFO *rqst)
{
}
`
	facts, err := scanner.ScanBytes([]byte(src), "empty.pc")
	if err != nil {
		t.Fatal(err)
	}
	tree := Build([]byte(src), facts, "SVC_EMPTY", nil)
	if len(tree.Root) != 0 || tree.Coverage.CodeLines != 0 {
		t.Errorf("empty body should give an empty tree, got %+v", tree.Coverage)
	}
	tree = Build([]byte(src), facts, "GHOST", nil)
	if len(tree.Root) != 0 {
		t.Errorf("unknown function should give an empty tree, got %d nodes", len(tree.Root))
	}
}

func TestTolerateGarbageBytes(t *testing.T) {
	src := "\x00\x01{}}{ ;; \x02\nvoid (\n{{{\n\"\"\n"
	tree := mustTolerate(t, "garbage.pc", src)
	_ = tree
}

func TestTolerateDoWithoutTail(t *testing.T) {
	src := `void SVC_NOTAIL(TPSVCINFO *rqst) {
	do {
		work();
	}
	work();
}
`
	tree := mustTolerate(t, "notail.pc", src)
	var found bool
	var walk func(ns []*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			if n.Kind == KindLoop && n.Sub == "do" {
				found = true
			}
			walk(n.Children)
		}
	}
	walk(tree.Root)
	if !found {
		t.Error("do record missing (tail-less do must still be a loop node)")
	}
}

func TestTolerateOutOfRangeQueries(t *testing.T) {
	src := `void SVC_Q(TPSVCINFO *rqst) {
	work();
}
`
	facts, err := scanner.ScanBytes([]byte(src), "q.pc")
	if err != nil {
		t.Fatal(err)
	}
	irFile := &ir.File{Queries: []*ir.Query{
		{ID: "way_out", StartLine: 9999, EndLine: 9999},
		{ID: "neg", StartLine: -5, EndLine: -1},
	}}
	tree := Build([]byte(src), facts, "SVC_Q", irFile)
	for _, n := range tree.Root {
		if len(n.QueryIDs) != 0 {
			t.Errorf("out-of-range query leaked onto node: %v", n.QueryIDs)
		}
	}
}

func TestTolerateFragmentOfGarbage(t *testing.T) {
	facts, err := scanner.ScanFragment([]byte("while {\n}}}\n"), "frag.txt")
	if err != nil {
		t.Fatal(err)
	}
	tree := Build([]byte("while {\n}}}\n"), facts, "", nil)
	if tree == nil {
		t.Fatal("nil tree")
	}
}

func TestResidueIsLoud(t *testing.T) {
	src := `void SVC_RES(TPSVCINFO *rqst) {
	#pragma something weird
	switch (x) {
	case 1:
		work();
	}
}
`
	tree := mustTolerate(t, "res.pc", src)
	if tree.Coverage.Unknown == 0 {
		t.Fatalf("switch/pragma should be loud residue: %+v", tree.Coverage)
	}
	if len(tree.Coverage.Residue) == 0 {
		t.Error("residue lines not reported")
	}
	// And the renderer turns residue into explicit TODOs, never silence.
	out := RenderTree(tree, nil)
	if !strings.Contains(out.Body, "TODO") {
		t.Errorf("renderer dropped residue without a TODO:\n%s", out.Body)
	}
}
