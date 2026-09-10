package flow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/pred"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

// extractTestIR writes src to a temp .pc file and extracts its IR (the
// fresh-clone-safe shape of ir.ExtractFile on synthetic sources).
func extractTestIR(t *testing.T, src string) *ir.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "defines_flow.pc")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := ir.ExtractFile(path)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return f
}

// defineSrc is the DEF-2 synthetic corpus: a file-scope define, a chained
// define, a compound define, a function-like macro, an in-body define-led
// run, and conditions over all of them.
const defineSrc = `#define FLAG_H 'H'
#define LIMIT 6
#define ALIAS LIMIT
#define COMPOUND (LIMIT*3)
#define MACRO(x) ((x)*2)
void SVC_DEMO(TPSVCINFO *rqst) {
	char c_flag;
	int i;
#define LOCAL_LIMIT 2
	if (c_flag == FLAG_H) {
		while (i < ALIAS) {
			work(i);
		}
		for (i = 0; i < LOCAL_LIMIT; i++) {
			count();
		}
	} else if (i < COMPOUND) {
		other();
	}
#undef LOCAL_LIMIT
	if (i < LOCAL_LIMIT) {
		tail();
	}
#include <weird.h>
}
`

func buildDefinesTree(t *testing.T) (*Tree, *ir.File) {
	t.Helper()
	facts, err := scanner.ScanBytes([]byte(defineSrc), "defines.pc")
	if err != nil {
		t.Fatal(err)
	}
	f := extractTestIR(t, defineSrc)
	tree := Build([]byte(defineSrc), facts, "SVC_DEMO", f)
	if tree == nil || len(tree.Root) == 0 {
		t.Fatal("empty tree")
	}
	return tree, f
}

func TestPredicateDefineSubstitution(t *testing.T) {
	tree, _ := buildDefinesTree(t)

	var flag, loop, compound, tail *Node
	var walk func(ns []*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			switch {
			case n.Kind == KindBranch && strings.Contains(n.Cond, "FLAG_H") && flag == nil:
				flag = n
			case n.Kind == KindLoop && strings.Contains(n.Cond, "ALIAS"):
				loop = n
			case n.Kind == KindBranch && strings.Contains(n.Cond, "COMPOUND"):
				compound = n
			case n.Kind == KindBranch && strings.Contains(n.Cond, "LOCAL_LIMIT"):
				tail = n
			}
			walk(n.Children)
		}
	}
	walk(tree.Root)
	if flag == nil || loop == nil || compound == nil || tail == nil {
		t.Fatalf("expected branches/loop missing: flag=%v loop=%v compound=%v tail=%v", flag, loop, compound, tail)
	}

	// Char-literal define resolves (FLAG_H → 'H'; rendered "H" by exprGo).
	if got := exprGo(flag.Predicate); got != `c_flag == "H"` {
		t.Errorf("FLAG_H resolved condition = %q, want c_flag == \"H\"", got)
	}
	// Chain resolution: ALIAS → LIMIT → 6 (while-header conditions).
	if got := exprGo(loop.Predicate); !strings.Contains(got, "< 6") || strings.Contains(got, "ALIAS") {
		t.Errorf("ALIAS resolved loop condition = %q, want i < 6", got)
	}
	// Compound values never substitute — the ident stays (DEF-D2).
	if got := exprGo(compound.Predicate); !strings.Contains(got, "COMPOUND") {
		t.Errorf("COMPOUND must stay unresolved, got %q", got)
	}
	// The raw Cond text is untouched — the audit trail (DEF-D2).
	if !strings.Contains(flag.Cond, "FLAG_H") || !strings.Contains(compound.Cond, "COMPOUND") {
		t.Error("raw Cond text must keep the define names")
	}
	// For-loop headers keep their whole `init; cond; incr` text — outside
	// the substitution grammar (documented limitation: bounds stay the
	// renderer's TODO; while/if conditions resolve).
	// #undef clears the name after its line: LOCAL_LIMIT stays unresolved
	// in the tail branch (the undefined Go symbol → TODO).
	if got := exprGo(tail.Predicate); !strings.Contains(got, "LOCAL_LIMIT") {
		t.Errorf("post-undef LOCAL_LIMIT must stay, got %q", got)
	}
}

func TestDefineRunClassifiesAsDecl(t *testing.T) {
	tree, _ := buildDefinesTree(t)

	var defineRuns, unknowns int
	var walk func(ns []*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			if n.Kind == KindDecl && n.Sub == "define" {
				defineRuns++
			}
			if n.Kind == KindUnknown {
				unknowns++
				t.Logf("unknown residue: %q", n.Text)
			}
			walk(n.Children)
		}
	}
	walk(tree.Root)

	// The in-body #define LOCAL_LIMIT + #undef LOCAL_LIMIT lines classify
	// (two runs, singles here); the #include stays loud unknown (G-DEF4).
	if defineRuns < 2 {
		t.Errorf("define runs = %d, want >= 2", defineRuns)
	}
	foundInclude := false
	var walkU func(ns []*Node)
	walkU = func(ns []*Node) {
		for _, n := range ns {
			if n.Kind == KindUnknown && strings.Contains(n.Text, "#include") {
				foundInclude = true
			}
			walkU(n.Children)
		}
	}
	walkU(tree.Root)
	if !foundInclude {
		t.Error("non-define directive (#include) must stay loud unknown")
	}
}

func TestDefineResolverCycleGuard(t *testing.T) {
	// A self-referential define never hangs, never resolves.
	src := "#define LOOP_A LOOP_B\n#define LOOP_B LOOP_A\nvoid SVC_C(TPSVCINFO *rqst) {\n if (x == LOOP_A) {\n  a();\n }\n}\n"
	facts, err := scanner.ScanBytes([]byte(src), "cycle.pc")
	if err != nil {
		t.Fatal(err)
	}
	f := extractTestIR(t, src)
	tree := Build([]byte(src), facts, "SVC_C", f)
	var cond *Node
	for _, n := range tree.Root {
		if n.Kind == KindBranch {
			cond = n
		}
	}
	if cond == nil {
		t.Fatal("branch not found")
	}
	if got := exprGo(cond.Predicate); !strings.Contains(got, "LOOP_A") {
		t.Errorf("cycle must keep the ident, got %q", got)
	}
}

func TestPredSubstituteShape(t *testing.T) {
	e := pred.Parse("a == 1 && b != 'x' || !(c < d)")
	res := func(name string) (string, bool) {
		if name == "a" {
			return "7", true
		}
		if name == "b" {
			return "'y'", true
		}
		return "", false
	}
	got := exprGo2(pred.Substitute(&e, res))
	want := `7 == 1 && "y" != "x" || !(c < d)`
	if got != want {
		t.Errorf("substituted = %q, want %q", got, want)
	}
	if !pred.IsLitText("6144") || !pred.IsLitText("'H'") || !pred.IsLitText(`"msg"`) {
		t.Error("literals must pass IsLitText")
	}
	if pred.IsLitText("(LIMIT*3)") || pred.IsLitText("") || pred.IsLitText("-1") {
		t.Error("compound/empty/negative must fail IsLitText")
	}
	if !pred.IsBareIdent("BUF_LEN") || pred.IsBareIdent("(x)") || pred.IsBareIdent("A B") {
		t.Error("IsBareIdent misclassifies")
	}
}

// exprGo2 renders a value Expr (test convenience mirroring exprGo).
func exprGo2(e pred.Expr) string { return exprGo(&e) }

// TestRendererRetainsErrorCodes pins G-DEF6: the TPFAIL draft line carries
// the legacy codes the node's FML ops ship, harvested from the errlog→add
// convention.
func TestRendererRetainsErrorCodes(t *testing.T) {
	src := `void SVC_E(TPSVCINFO *rqst) {
	char c_errmsg[256];
	if (x == 1) {
		errlog(c_ServiceName,"S31005",FMLMSG,DEF_USR,DEF_SSSN,c_errmsg) ;
		Fadd32(ptr_fml_Ibuffer,FML_ERR_MSG,c_errmsg,0) ;
		tpreturn(TPFAIL,0L,(char *)ptr_fml_Ibuffer,0L,0) ;
	}
}
`
	f := extractTestIR(t, src)
	facts, err := scanner.ScanBytes([]byte(src), "errcode_render.pc")
	if err != nil {
		t.Fatal(err)
	}
	tree := Build([]byte(src), facts, "SVC_E", f)
	out := RenderTree(tree, nil)
	if !strings.Contains(out.Body, "legacy error code(s): S31005") {
		t.Errorf("TPFAIL line missing the code comment:\n%s", out.Body)
	}
}
