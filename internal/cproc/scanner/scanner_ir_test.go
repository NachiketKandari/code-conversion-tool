package scanner

import (
	"strings"
	"testing"
)

func TestScanFunctionBodyAndAttribution(t *testing.T) {
	src := `void SVC_DEMO(TPSVCINFO* rqst)
{
    char c_flag;
    int cnt;
    EXEC SQL SELECT COUNT(*) INTO :cnt FROM DUAL;
    if(Fget32(buf,FIELD,0,(char*)&c_flag,0) == -1)
    {
        Fadd32(out,FIELD,(char*)&c_flag,0);
    }
    else if(c_flag == 'H')
    {
        EXEC SQL UPDATE T SET X = :cnt WHERE Y = :c_flag;
    }
    else
    {
        helper(c_flag);
    }
}
int fn_helper(char* msg)
{
    return 0;
}
`
	facts, err := ScanBytes([]byte(src), "demo.pc")
	if err != nil {
		t.Fatalf("ScanBytes failed: %v", err)
	}

	if len(facts.Functions) != 2 {
		t.Fatalf("expected 2 function defs, got %d", len(facts.Functions))
	}
	svc := facts.Functions[0]
	if svc.Name != "SVC_DEMO" || svc.BodyStartLine != 2 || svc.BodyEndLine != 18 {
		t.Errorf("unexpected SVC_DEMO body extent: %+v", svc)
	}
	helper := facts.Functions[1]
	if helper.Name != "fn_helper" || helper.BodyStartLine != 20 || helper.BodyEndLine != 22 {
		t.Errorf("unexpected fn_helper body extent: %+v", helper)
	}

	// SQL statements and calls must carry their enclosing function.
	for _, q := range facts.AllSQL {
		if q.Func != "SVC_DEMO" {
			t.Errorf("SQL at line %d attributed to %q, want SVC_DEMO", q.StartLine, q.Func)
		}
	}
	var fget, fadd *FunctionCall
	for i := range facts.Calls {
		switch facts.Calls[i].Name {
		case "Fget32":
			fget = &facts.Calls[i]
		case "Fadd32":
			fadd = &facts.Calls[i]
		case "helper":
			if facts.Calls[i].Func != "SVC_DEMO" {
				t.Errorf("helper attributed to %q, want SVC_DEMO", facts.Calls[i].Func)
			}
		}
	}
	if fget == nil || fget.Func != "SVC_DEMO" {
		t.Fatalf("Fget32 call missing or misattributed")
	}
	if fadd == nil || fadd.Func != "SVC_DEMO" {
		t.Fatalf("Fadd32 call missing or misattributed")
	}

	// Call args carry the raw paren text.
	if want := "buf,FIELD,0,(char*)&c_flag,0"; fget.Args != want {
		t.Errorf("Fget32 args = %q, want %q", fget.Args, want)
	}

	// Variable declarations capture base types and arrays.
	decls := map[string]VarDecl{}
	for _, d := range facts.VarDecls {
		decls[d.Name] = d
	}
	if d := decls["c_flag"]; d.Type != "char" || d.Func != "SVC_DEMO" {
		t.Errorf("c_flag decl = %+v", d)
	}
	if d := decls["cnt"]; d.Type != "int" {
		t.Errorf("cnt decl = %+v", d)
	}
	if d := decls["msg"]; d.Type != "char" || d.Func != "" {
		// Params sit in the signature, before the body opens — Func is
		// legitimately empty for them.
		t.Errorf("msg param decl = %+v", d)
	}

	// Branches: if / elseif / else at depth 1, with condition text and blocks.
	if len(facts.Branches) != 3 {
		t.Fatalf("expected 3 branch records, got %d: %+v", len(facts.Branches), facts.Branches)
	}
	if facts.Branches[0].Kind != BranchIf || facts.Branches[0].Cond != "Fget32(buf,FIELD,0,(char*)&c_flag,0) == -1" {
		t.Errorf("branch 0 = %+v", facts.Branches[0])
	}
	if facts.Branches[1].Kind != BranchElseIf || facts.Branches[1].Cond != "c_flag == 'H'" {
		t.Errorf("branch 1 = %+v", facts.Branches[1])
	}
	if facts.Branches[2].Kind != BranchElse || facts.Branches[2].Cond != "" {
		t.Errorf("branch 2 = %+v", facts.Branches[2])
	}
	for i, b := range facts.Branches {
		if b.Depth != 1 || b.Function != "SVC_DEMO" {
			t.Errorf("branch %d depth/function = %+v", i, b)
		}
		if b.BlockStart == 0 || b.BlockEnd <= b.BlockStart {
			t.Errorf("branch %d block extent unresolved: %+v", i, b)
		}
	}
}

func TestScanVarDeclForms(t *testing.T) {
	src := `void F(void)
{
    varchar ls_acc[12];
    long l1, l2;
    char *p, c = 'x';
    unsigned long ul;
    EXEC SQL BEGIN DECLARE SECTION;
    varchar c_from_date[12];
    EXEC SQL END DECLARE SECTION;
    int arr[10];
}
`
	facts, err := ScanBytes([]byte(src), "decls.pc")
	if err != nil {
		t.Fatalf("ScanBytes failed: %v", err)
	}
	decls := map[string]VarDecl{}
	for _, d := range facts.VarDecls {
		decls[d.Name] = d
	}
	for name, wantType := range map[string]string{
		"ls_acc": "varchar", "l1": "long", "l2": "long", "p": "char",
		"c": "char", "ul": "unsigned long", "c_from_date": "varchar", "arr": "int",
	} {
		d, ok := decls[name]
		if !ok {
			t.Errorf("missing decl for %s (got %d decls: %v)", name, len(facts.VarDecls), facts.VarDecls)
			continue
		}
		if d.Type != wantType {
			t.Errorf("decl %s type = %q, want %q", name, d.Type, wantType)
		}
	}
	if d := decls["ls_acc"]; !d.Array {
		t.Errorf("ls_acc should be an array decl")
	}
	if d := decls["arr"]; !d.Array {
		t.Errorf("arr should be an array decl")
	}
}

func TestScanStringParensDoNotConfuseBalance(t *testing.T) {
	src := `void F(void)
{
    if(logmsg("a)b", 1) == -1)
    {
        EXEC SQL SELECT 1 INTO :x FROM DUAL;
    }
    if(ch == '(')
    {
        EXEC SQL SELECT 2 INTO :y FROM DUAL;
    }
}
`
	facts, err := ScanBytes([]byte(src), "lit.pc")
	if err != nil {
		t.Fatalf("ScanBytes failed: %v", err)
	}
	if len(facts.Branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(facts.Branches))
	}
	if !strings.Contains(facts.Branches[0].Cond, `"a)b"`) {
		t.Errorf("string literal inside condition mangled: %q", facts.Branches[0].Cond)
	}
	if !strings.Contains(facts.Branches[1].Cond, `'('`) {
		t.Errorf("char literal inside condition mangled: %q", facts.Branches[1].Cond)
	}
	if len(facts.Queries) != 2 {
		t.Errorf("expected 2 queries, got %d", len(facts.Queries))
	}
}
