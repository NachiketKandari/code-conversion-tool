package scanner

import "testing"

// TestBranchNestDepth pins the if-nesting semantics behind the branching
// factor: NestDepth counts enclosing if/else-if block extents only (loops
// and else bodies never nest); an else-if is a sibling of its chain, even
// when its header shares the closing-brace line (`} else if`); unbraced
// parents create no nesting level.
func TestBranchNestDepth(t *testing.T) {
	src := `void SVC_NEST(TPSVCINFO *rqst) {
	if (a) {
		if (b) {
			if (c) {
				work();
			}
		}
	}
	while (1) {
		if (d) {
			work();
		}
	}
	if (e) {
		work();
	} else if (f) {
		if (g) {
			work();
		}
	} else {
		if (h) {
			work();
		}
	}
	if (i)
		if (j)
			work();
}
`
	facts, err := ScanBytes([]byte(src), "nest.pc")
	if err != nil {
		t.Fatal(err)
	}

	type want struct {
		cond  string
		kind  BranchKind
		depth int
	}
	wants := []want{
		{"a", BranchIf, 0},
		{"b", BranchIf, 1},
		{"c", BranchIf, 2},
		{"d", BranchIf, 0}, // inside a while — loops never nest
		{"e", BranchIf, 0},
		{"f", BranchElseIf, 0}, // sibling of e, not nested in its block
		{"g", BranchIf, 1},     // inside f's block
		{"", BranchElse, 0},    // the else header itself is recorded
		{"h", BranchIf, 0},     // inside an else body — else never nests
		{"i", BranchIf, 0},
		{"j", BranchIf, 0}, // unbraced parent i creates no level
	}
	if len(facts.Branches) != len(wants) {
		t.Fatalf("got %d branches, want %d: %+v", len(facts.Branches), len(wants), facts.Branches)
	}
	for i, w := range wants {
		b := facts.Branches[i]
		if b.Cond != w.cond || b.Kind != w.kind {
			t.Errorf("branch %d: got %s(%s), want %s(%s)", i, b.Kind, b.Cond, w.kind, w.cond)
		}
		if b.NestDepth != w.depth {
			t.Errorf("branch %s: NestDepth = %d, want %d", w.cond, b.NestDepth, w.depth)
		}
	}
}

// TestBlockExtentColumns pins the brace-position facts that separate a
// header inside a block from a sibling on the closing-brace line.
func TestBlockExtentColumns(t *testing.T) {
	src := `void SVC_COLS(TPSVCINFO *rqst) {
	if (a) {
		work();
	} else if (b) {
		work();
	}
}
`
	facts, err := ScanBytes([]byte(src), "cols.pc")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Branches) != 2 {
		t.Fatalf("got %d branches, want 2", len(facts.Branches))
	}
	a, b := facts.Branches[0], facts.Branches[1]
	if a.BlockEnd != 4 || a.BlockEndCol == 0 {
		t.Errorf("branch a: block end %d (col %d), want line 4 with a column", a.BlockEnd, a.BlockEndCol)
	}
	if b.StartLine != 4 || b.StartCol <= a.BlockEndCol {
		t.Errorf("else-if must start after the closing brace on line 4: got col %d, brace col %d", b.StartCol, a.BlockEndCol)
	}
	if b.NestDepth != 0 {
		t.Errorf("else-if sibling must not nest in its chain: NestDepth = %d", b.NestDepth)
	}
}
