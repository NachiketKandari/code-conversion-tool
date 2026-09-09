package flow

import (
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

const demoSrc = `void SVC_DEMO(TPSVCINFO *rqst) {
	char c_flag;
	c_flag = 'H';
	if (c_flag == 'H') {
		if (Fget32(ptr_fml_Ibuffer, FML_COMP_CD, 0, (char *)&sql_comp, 0) == -1) {
			errlog(c_ServiceName, "S1", FMLMSG, c_user_id, DEF_SSSN, c_errmsg);
			Fadd32(ptr_fml_Ibuffer, FML_ERR_MSG, c_errmsg, 0);
			tpreturn(TPFAIL, 0L, (char *)ptr_fml_Ibuffer, 0L, 0);
		}
		EXEC SQL
		DECLARE cur_demo CURSOR FOR
		SELECT A, B FROM T WHERE K = :sql_comp;
		EXEC SQL OPEN cur_demo;
		while (1) {
			EXEC SQL FETCH cur_demo INTO :a, :b;
			if (SQLCODE == NO_DATA_FOUND)
				break;
			if (SQLCODE) {
				Fadd32(ptr_fml_Ibuffer, FML_ERR_MSG, c_errmsg, 0);
				tpreturn(TPFAIL, 0L, (char *)ptr_fml_Ibuffer, 0L, 0);
			}
			i_err_op[0] = Fadd32(ptr_fml_Obuffer, FML_A, (char *)&a, 0);
			i_err_op[1] = Fadd32(ptr_fml_Obuffer, FML_B, (char *)&b, 0);
			for (i = 0; i < 2; i++) {
				if (i_err_op[i] == -1) {
					tpreturn(TPFAIL, 0L, (char *)ptr_fml_Ibuffer, 0L, 0);
				}
			}
		}
		EXEC SQL CLOSE cur_demo;
	} else if (c_flag == 'F') {
		work();
	} else {
		other();
	}
	if (DEBUG_MSG_LVL_3) {
		userlog("done");
	}
	tpreturn(TPSUCCESS, 0, (char *)ptr_fml_Obuffer, 0L, 0);
}
`

func buildDemo(t *testing.T) *Tree {
	t.Helper()
	facts, err := scanner.ScanBytes([]byte(demoSrc), "demo.pc")
	if err != nil {
		t.Fatal(err)
	}
	tree := Build([]byte(demoSrc), facts, "SVC_DEMO", nil)
	if tree == nil || len(tree.Root) == 0 {
		t.Fatal("empty tree")
	}
	return tree
}

func TestBuildShape(t *testing.T) {
	tree := buildDemo(t)

	// Root: decl, assign(c_flag), the if/elseif/else chain, the debug if,
	// and the final tpreturn stmt. Chain siblings all sit at root level.
	if len(tree.Root) != 7 {
		var kinds []string
		for _, n := range tree.Root {
			kinds = append(kinds, string(n.Kind)+":"+n.Sub)
		}
		t.Fatalf("root nodes = %v, want 7", kinds)
	}
	if tree.Root[0].Kind != KindDecl {
		t.Errorf("root[0] = %s:%s, want decl", tree.Root[0].Kind, tree.Root[0].Sub)
	}
	if tree.Root[1].Kind != KindStmt || tree.Root[1].Sub != SubAssign {
		t.Errorf("root[1] = %s:%s, want stmt:assign", tree.Root[1].Kind, tree.Root[1].Sub)
	}
	chain := tree.Root[2:5]
	wantSubs := []string{"if", "elseif", "else"}
	for i, w := range wantSubs {
		if chain[i].Sub != w {
			t.Errorf("chain[%d].Sub = %q, want %q", i, chain[i].Sub, w)
		}
	}
	if chain[0].Cond != "c_flag == 'H'" {
		t.Errorf("chain[0].Cond = %q", chain[0].Cond)
	}
	if chain[0].Predicate == nil || chain[0].Predicate.Kind == "raw" {
		t.Errorf("chain[0] predicate not parsed: %+v", chain[0].Predicate)
	}
}

// flagBranch returns the root if-node guarding on c_flag == 'H'.
func flagBranch(t *testing.T, tree *Tree) *Node {
	t.Helper()
	for _, n := range tree.Root {
		if n.Kind == KindBranch && n.Sub == "if" && strings.Contains(n.Cond, "c_flag == 'H'") {
			return n
		}
	}
	t.Fatal("flag-ladder branch not found")
	return nil
}

func TestBuildLoopAndGuards(t *testing.T) {
	tree := buildDemo(t)
	flag := flagBranch(t, tree)

	var guard, cursor, fetch, loop, fanout *Node
	var find func(ns []*Node)
	find = func(ns []*Node) {
		for _, n := range ns {
			switch {
			case n.Kind == KindBranch && strings.Contains(n.Cond, "Fget32"):
				guard = n
			case n.Kind == KindSQL && n.Sub == "DECLARE_CURSOR":
				cursor = n
			case n.Kind == KindSQL && n.Sub == "FETCH":
				fetch = n
			case n.Kind == KindLoop && n.Sub == "while":
				loop = n
			case n.Kind == KindLoop && n.Sub == "for":
				fanout = n
			}
			find(n.Children)
		}
	}
	find(flag.Children)

	if guard == nil {
		t.Fatal("Fget32 guard branch not found")
	}
	if len(guard.FmlOps) != 2 || guard.FmlOps[0].Kind != ir.FmlGet || guard.FmlOps[1].Field != "FML_ERR_MSG" {
		t.Errorf("guard FmlOps = %+v", guard.FmlOps)
	}
	hasTPReturn := false
	for _, c := range guard.Calls {
		if c == "tpreturn" {
			hasTPReturn = true
		}
	}
	if !hasTPReturn {
		t.Errorf("guard calls missing tpreturn: %v", guard.Calls)
	}
	if cursor == nil || fetch == nil {
		t.Fatal("DECLARE/FETCH sql nodes not found")
	}
	if loop == nil {
		t.Fatal("while(1) loop not found")
	}
	if len(loop.FmlOps) != 3 {
		t.Errorf("loop FmlOps = %d, want 3 (1 err + 2 response)", len(loop.FmlOps))
	}
	if fanout == nil {
		t.Error("for-loop not nested inside the while loop")
	}
	if fetch.Line <= loop.Line || fetch.EndLine >= loop.EndLine {
		t.Errorf("FETCH span (%d..%d) not inside while span (%d..%d)", fetch.Line, fetch.EndLine, loop.Line, loop.EndLine)
	}
}

func TestBuildCoverage(t *testing.T) {
	tree := buildDemo(t)
	cov := tree.Coverage
	if cov.CodeLines == 0 {
		t.Fatal("no code lines counted")
	}
	// Exactly one honest residue: the `break;` after the unbraced
	// NO_DATA_FOUND if (the structural parse did not anchor it).
	if cov.Unknown != 1 || len(cov.Residue) != 1 {
		t.Errorf("unexpected residue: unknown=%d residue=%v", cov.Unknown, cov.Residue)
	}
	if cov.Classified != cov.CodeLines-1 {
		t.Errorf("classified %d != code %d - 1", cov.Classified, cov.CodeLines)
	}
}

func TestBuildFragment(t *testing.T) {
	facts, err := scanner.ScanFragment([]byte("if (x) {\n\twork();\n}\n"), "frag.txt")
	if err != nil {
		t.Fatal(err)
	}
	tree := Build([]byte("if (x) {\n\twork();\n}\n"), facts, "", nil)
	if tree.Function != "__fragment" {
		t.Errorf("function = %q, want __fragment", tree.Function)
	}
	if len(tree.Root) != 1 || tree.Root[0].Kind != KindBranch {
		t.Fatalf("root = %+v, want one branch", tree.Root)
	}
	if len(tree.Root[0].Children) != 1 || tree.Root[0].Children[0].Sub != SubCall {
		t.Errorf("branch children = %+v, want one call stmt", tree.Root[0].Children)
	}
}

func TestBuildUnknownFunction(t *testing.T) {
	facts, err := scanner.ScanBytes([]byte(demoSrc), "demo.pc")
	if err != nil {
		t.Fatal(err)
	}
	tree := Build([]byte(demoSrc), facts, "NOPE", nil)
	if len(tree.Root) != 0 {
		t.Errorf("unknown function should give an empty tree, got %d nodes", len(tree.Root))
	}
}

func TestStripComments(t *testing.T) {
	cases := map[string]string{
		`x = 1; /* c */`:      `x = 1; `,
		`x = 1; // tail`:      `x = 1; `,
		`/* multi */ y = 2;`:  ` y = 2;`,
		`s = "a/*b*/c";`:      `s = "a/*b*/c";`,
		`/* unterminated`:     ``,
		`url = "http://x/y";`: `url = "http://x/y";`,
	}
	for in, want := range cases {
		if got := stripComments(in); got != want {
			t.Errorf("stripComments(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasTopLevelAssign(t *testing.T) {
	cases := map[string]bool{
		"i_err_op[0] = Fadd32(a, b)":        true,
		"c_flag = 'H';":                     true,
		"if (a == b) work();":               false,
		"errlog(c_ServiceName, \"S1\");":    false,
		"x != y;":                           false,
		"while (1)":                         false,
		"if (Fget32(b, f, 0, &v, 0) == -1)": false,
	}
	for in, want := range cases {
		if got := hasTopLevelAssign(in); got != want {
			t.Errorf("hasTopLevelAssign(%q) = %v, want %v", in, got, want)
		}
	}
}
