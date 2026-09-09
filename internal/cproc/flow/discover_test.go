package flow

import (
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

// buildDemoConditions fabricates the condition inventory the way ir
// extraction would for demoSrc: one chain (if/elseif/else) + the debug if.
func buildDemoConditions(t *testing.T, tree *Tree) []ir.Condition {
	t.Helper()
	var conds []ir.Condition
	idx := 0
	for _, n := range tree.Root {
		if n.Kind != KindBranch || isDebugCond(n) {
			continue
		}
		idx++
		conds = append(conds, ir.Condition{
			Index: idx, Kind: n.Sub, Expr: n.Cond,
			StartLine: n.Line, EndLine: n.EndLine,
			FmlOps: n.FmlOps, QueryIDs: n.QueryIDs,
		})
	}
	if idx != 3 {
		t.Fatalf("demo inventory = %d conditions, want 3", idx)
	}
	return conds
}

func isDebugCond(n *Node) bool { return n.Cond == "DEBUG_MSG_LVL_3" }

func TestDiscoverCandidates(t *testing.T) {
	facts, err := scanner.ScanBytes([]byte(demoSrc), "demo.pc")
	if err != nil {
		t.Fatal(err)
	}
	tree := Build([]byte(demoSrc), facts, "SVC_DEMO", nil)
	conds := buildDemoConditions(t, tree)
	got := Discover(tree, conds)

	// c1: the flag branch (1 get + 2 response adds). The request guard
	// (1 get + only error adds) and the SQLCODE branches (no reads) must
	// NOT appear. elseif/else carry no FML → not candidates.
	if len(got) != 1 {
		t.Fatalf("candidates = %+v, want 1", got)
	}
	c := got[0]
	if c.Key != "c1" || c.ParentCondition != 1 {
		t.Errorf("key/parent = %s/%d, want c1/1", c.Key, c.ParentCondition)
	}
	if len(c.Gets) != 1 || c.Gets[0] != "FML_COMP_CD" {
		t.Errorf("gets = %v, want [FML_COMP_CD]", c.Gets)
	}
	if len(c.Adds) != 2 {
		t.Errorf("adds = %v, want the two response fields", c.Adds)
	}
	if len(c.ErrorAdds) == 0 {
		t.Errorf("error adds not recorded: %+v", c)
	}
}

func TestConditionForRoundTrip(t *testing.T) {
	facts, err := scanner.ScanBytes([]byte(demoSrc), "demo.pc")
	if err != nil {
		t.Fatal(err)
	}
	tree := Build([]byte(demoSrc), facts, "SVC_DEMO", nil)
	conds := buildDemoConditions(t, tree)

	synth, err := ConditionFor(tree, conds, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if synth.Index != 0 {
		t.Errorf("synthesized Index = %d, want 0", synth.Index)
	}
	if synth.StartLine != conds[0].StartLine || synth.EndLine != conds[0].EndLine {
		t.Errorf("synthesized span %d..%d != condition span %d..%d", synth.StartLine, synth.EndLine, conds[0].StartLine, conds[0].EndLine)
	}
	// The guard branch inside c1 is a qualifying nested candidate? No — its
	// adds are all error emissions, so it fails the rubric; there is no
	// c1.1 to resolve.
	if _, err := ConditionFor(tree, conds, "c1.1"); err == nil {
		t.Error("c1.1 should not resolve (the guard is not a candidate)")
	}
	// Out-of-range and malformed refs fail loudly.
	for _, bad := range []string{"c9", "c0", "x1", "c1.z"} {
		if _, err := ConditionFor(tree, conds, bad); err == nil {
			t.Errorf("ref %q resolved but must fail", bad)
		}
	}
}

func TestDiscoverNestedAndRedundant(t *testing.T) {
	src := `void SVC_NEST(TPSVCINFO *rqst) {
	if (flag == 'A') {
		if (Fget32(buf, FML_A, 0, (char *)&a, 0) == -1) {
			Fadd32(ibuf, FML_ERR_MSG, msg, 0);
			tpreturn(TPFAIL, 0, ibuf, 0, 0);
		}
		Fadd32(obuf, FML_A, (char *)&a, 0);
		Fadd32(obuf, FML_B, (char *)&b, 0);
	}
}
`
	facts, err := scanner.ScanBytes([]byte(src), "nest.pc")
	if err != nil {
		t.Fatal(err)
	}
	tree := Build([]byte(src), facts, "SVC_NEST", nil)
	var conds []ir.Condition
	for _, n := range tree.Root {
		if n.Kind == KindBranch {
			conds = append(conds, ir.Condition{Index: 1, Kind: n.Sub, StartLine: n.Line, EndLine: n.EndLine})
		}
	}
	got := Discover(tree, conds)
	if len(got) != 1 || got[0].Key != "c1" {
		t.Fatalf("candidates = %+v, want only c1 (the inner guard is error-only)", got)
	}
	if got[0].Redundant {
		t.Errorf("c1 marked redundant without a qualifying parent: %+v", got[0])
	}
}
