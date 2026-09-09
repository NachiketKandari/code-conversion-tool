package scanner

import "testing"

// TestLoopRecords pins the FLW-1 loop recording semantics: for/while headers
// with cond text and block extents, do-while merged into one record (tail
// while fills Cond/WhileLine, never a second loop), nested loops inside
// branches, and function attribution.
func TestLoopRecords(t *testing.T) {
	src := `void SVC_LOOPS(TPSVCINFO *rqst) {
	while (1) {
		fetch();
		if (done) {
			break;
		}
		for (i = 0; i < N; i++) {
			add(i);
		}
	}
	do {
		work();
	} while (a < b);
	for (;;) {
		spin();
	}
}
`
	facts, err := ScanBytes([]byte(src), "loops.pc")
	if err != nil {
		t.Fatal(err)
	}

	if len(facts.Loops) != 4 {
		t.Fatalf("got %d loops, want 4: %+v", len(facts.Loops), facts.Loops)
	}
	wants := []Loop{
		{Kind: LoopWhile, Cond: "1", Depth: 1, NestDepth: 0},
		{Kind: LoopFor, Cond: "i = 0; i < N; i++", Depth: 2, NestDepth: 1},
		{Kind: LoopDo, Cond: "a < b", Depth: 1, NestDepth: 0},
		{Kind: LoopFor, Cond: ";;", Depth: 1, NestDepth: 0},
	}
	for i, w := range wants {
		l := facts.Loops[i]
		if l.Kind != w.Kind || l.Cond != w.Cond || l.Depth != w.Depth || l.NestDepth != w.NestDepth {
			t.Errorf("loop %d: got %+v, want kind=%s cond=%q depth=%d nest=%d", i, l, w.Kind, w.Cond, w.Depth, w.NestDepth)
		}
		if l.Function != "SVC_LOOPS" {
			t.Errorf("loop %d: function = %q, want SVC_LOOPS", i, l.Function)
		}
		if l.BlockStart == 0 || l.BlockEnd == 0 {
			t.Errorf("loop %d: missing block extent (%d..%d)", i, l.BlockStart, l.BlockEnd)
		}
	}
	if got := facts.Loops[2].WhileLine; got == 0 {
		t.Errorf("do-while tail WhileLine not set: %+v", facts.Loops[2])
	}
	if facts.Loops[2].WhileLine < facts.Loops[2].BlockEnd {
		t.Errorf("do-while tail (line %d) must sit at/after the block close (%d)", facts.Loops[2].WhileLine, facts.Loops[2].BlockEnd)
	}
	// Exactly one standalone while (the while(1)); the do-while tail must
	// not appear as a second while record.
	standalone := 0
	for _, l := range facts.Loops {
		if l.Kind == LoopWhile {
			standalone++
		}
	}
	if standalone != 1 {
		t.Errorf("got %d standalone while records, want 1 (do-while tail leaked?)", standalone)
	}
}

// TestLoopDoWhileNesting pins the stack-based tail matching: a while nested
// inside a do body stays a standalone loop; only the while after the do
// block closes is the tail.
func TestLoopDoWhileNesting(t *testing.T) {
	src := `void SVC_DW(TPSVCINFO *rqst) {
	do {
		while (inner) {
			probe();
		}
		work();
	} while (outer);
}
`
	facts, err := ScanBytes([]byte(src), "dw.pc")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Loops) != 2 {
		t.Fatalf("got %d loops, want 2: %+v", len(facts.Loops), facts.Loops)
	}
	do, inner := facts.Loops[0], facts.Loops[1]
	if do.Kind != LoopDo || do.Cond != "outer" {
		t.Errorf("do record: got kind=%s cond=%q, want do/\"outer\"", do.Kind, do.Cond)
	}
	if inner.Kind != LoopWhile || inner.Cond != "inner" || inner.NestDepth != 1 {
		t.Errorf("inner while: got %+v, want while/\"inner\"/nest 1", inner)
	}
}

// TestReturnRecords pins return-site recording: every C return inside a
// function body is a fact with its function; code outside any body records
// with an empty function; comments never record.
func TestReturnRecords(t *testing.T) {
	src := `int helper(void) {
	if (bad) {
		return -1;
	}
	return 0;
}
void SVC_R(TPSVCINFO *rqst) {
	/* return fake(); */
	x = 1; // return also fake
	if (x) {
		return;
	}
}
`
	facts, err := ScanBytes([]byte(src), "ret.pc")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Returns) != 3 {
		t.Fatalf("got %d returns, want 3: %+v", len(facts.Returns), facts.Returns)
	}
	wantFn := []string{"helper", "helper", "SVC_R"}
	for i, w := range wantFn {
		if facts.Returns[i].Func != w {
			t.Errorf("return %d: function = %q, want %q", i, facts.Returns[i].Func, w)
		}
	}
}

// TestLoopFragmentRebase pins that loop/return line numbers rebase with the
// fragment wrapper removal like every other fact.
func TestLoopFragmentRebase(t *testing.T) {
	frag := `while (1) {
		fetch();
	}
	return;
`
	facts, err := ScanFragment([]byte(frag), "frag.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !facts.Fragment {
		t.Fatal("Fragment flag not set")
	}
	if len(facts.Loops) != 1 || len(facts.Returns) != 1 {
		t.Fatalf("got %d loops / %d returns, want 1/1", len(facts.Loops), len(facts.Returns))
	}
	if facts.Loops[0].StartLine != 1 {
		t.Errorf("loop start = %d, want 1 (rebased)", facts.Loops[0].StartLine)
	}
	if facts.Returns[0].Line != 4 {
		t.Errorf("return line = %d, want 4 (rebased)", facts.Returns[0].Line)
	}
}
