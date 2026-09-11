package ir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// defineSource is the synthetic DEF-1 corpus: file-scope defines before the
// entry, an in-body define shadowing one, a #undef tombstone, a
// function-like macro, a redefinition, and a malformed arg.
const defineSource = `#define BUF_LEN 6144
#define DEF_USR 9999
#define OUT_FML 6
void SVC_DEMO(TPSVCINFO* rqst)
{
#define OUT_FML 8
    int a[OUT_FML];
#undef DEF_USR
    if (a[0] == BUF_LEN) {}
}

void SVC_OTHER(TPSVCINFO* rqst)
{
#define OTHER(x) ((x) * 2)
    int b[OUT_FML];
}
#define LATE 1
`

func TestBuildDefines(t *testing.T) {
	f := ExtractTestFile(t, "defines_demo.pc", defineSource)

	type want struct {
		name, value, fn string
		macro, undef    bool
	}
	wants := []want{
		{"BUF_LEN", "6144", "", false, false},
		{"DEF_USR", "9999", "", false, false},
		{"OUT_FML", "6", "", false, false},
		{"OUT_FML", "8", "SVC_DEMO", false, false},
		{"DEF_USR", "", "SVC_DEMO", false, true},
		{"OTHER", "(x) ((x) * 2)", "SVC_OTHER", true, false},
		{"LATE", "1", "", false, false},
	}
	if len(f.Defines) != len(wants) {
		t.Fatalf("defines = %d entries, want %d: %+v", len(f.Defines), len(wants), f.Defines)
	}
	for i, w := range wants {
		d := f.Defines[i]
		if d.Name != w.name || d.Value != w.value || d.Function != w.fn || d.Macro != w.macro || d.Undef != w.undef {
			t.Errorf("defines[%d] = %+v, want %+v", i, d, w)
		}
	}
}

func TestBuildDefinesMalformedSkipped(t *testing.T) {
	// A macro with a glued paren, an unbalanced-paren macro, and a bare
	// #define with no argument all scan; only parseable names become
	// entries (the raw Directive stays the audit trail).
	src := "#define BAD_MACRO(x) ((x) * 2\n" +
		"#define UNBALANCED_PAREN (x\n" +
		"#define FLAG\n" +
		"void SVC_D(TPSVCINFO* rqst) { int i; }\n"
	f := ExtractTestFile(t, "defines_bad.pc", src)
	names := map[string]int{}
	for _, d := range f.Defines {
		names[d.Name]++
		if d.Name == "FLAG" && d.Value != "" {
			t.Errorf("bare define value = %q, want empty", d.Value)
		}
	}
	if names["BAD_MACRO"] != 1 || names["UNBALANCED_PAREN"] != 1 || names["FLAG"] != 1 {
		t.Errorf("malformed/bare defines missing: %+v", f.Defines)
	}
}

func TestDefineAtScoping(t *testing.T) {
	f := ExtractTestFile(t, "defines_scope.pc", defineSource)

	// File define visible inside the entry from anywhere.
	if d, ok := f.DefineAt("SVC_DEMO", 15, "BUF_LEN"); !ok || d.Value != "6144" {
		t.Errorf("BUF_LEN in SVC_DEMO = (%+v, %v)", d, ok)
	}
	// In-body define shadows the file scope from its line onward.
	if d, ok := f.DefineAt("SVC_DEMO", 5, "OUT_FML"); !ok || d.Value != "6" || d.Function != "" {
		t.Errorf("OUT_FML before body define = (%+v, %v)", d, ok)
	}
	if d, ok := f.DefineAt("SVC_DEMO", 8, "OUT_FML"); !ok || d.Value != "8" || d.Function != "SVC_DEMO" {
		t.Errorf("OUT_FML after body define = (%+v, %v)", d, ok)
	}
	// The shadow never leaks into the next function.
	if d, ok := f.DefineAt("SVC_OTHER", 20, "OUT_FML"); !ok || d.Value != "6" || d.Function != "" {
		t.Errorf("OUT_FML in SVC_OTHER = (%+v, %v), want the file-scope 6", d, ok)
	}
	// #undef clears the name from its line onward, in its function only.
	if _, ok := f.DefineAt("SVC_DEMO", 10, "DEF_USR"); ok {
		t.Error("DEF_USR after #undef must not resolve in SVC_DEMO")
	}
	if d, ok := f.DefineAt("SVC_OTHER", 10, "DEF_USR"); !ok || d.Value != "9999" {
		t.Errorf("DEF_USR in SVC_OTHER = (%+v, %v), want 9999 (undef is fn-scoped)", d, ok)
	}
	// Function-like macros never resolve (audit-only facts).
	if _, ok := f.DefineAt("SVC_OTHER", 20, "OTHER"); ok {
		t.Error("macro OTHER must not resolve via DefineAt")
	}
	// Nothing resolves before the define's line or for unknown names.
	if _, ok := f.DefineAt("SVC_DEMO", 1, "OUT_FML"); ok {
		t.Error("OUT_FML (line 3) must not resolve at line 1")
	}
	if _, ok := f.DefineAt("SVC_DEMO", 16, "LATE"); ok {
		t.Error("LATE (line 17) must not resolve at line 16")
	}
	if _, ok := f.DefineAt("SVC_DEMO", 15, "NOPE"); ok {
		t.Error("unknown name resolved")
	}
	// File-level consumers (fn == "") see only file-scope defines — the
	// file-scope OUT_FML (6), never the SVC_DEMO-scoped one (8).
	if d, ok := f.DefineAt("", 15, "BUF_LEN"); !ok || d.Value != "6144" {
		t.Errorf("file-level BUF_LEN = (%+v, %v)", d, ok)
	}
	if d, ok := f.DefineAt("", 20, "OUT_FML"); !ok || d.Value != "6" || d.Function != "" {
		t.Errorf("file-level OUT_FML = (%+v, %v), want the file-scope 6", d, ok)
	}
}

// errCodeSource is the DEF-3 corpus: the two-step errlog→add convention,
// strcpy/sprintf writers, a direct-literal add, a define-named code
// (unresolvable), a get (never coded), and nearest-writer ordering.
const errCodeSource = `void SVC_ERR(TPSVCINFO *rqst) {
	char c_errmsg[256];
	char c_other[64];
	if (x == 1) {
		errlog(c_ServiceName,"S31005",FMLMSG,DEF_USR,DEF_SSSN,c_errmsg) ;
		Fadd32(ptr_fml_Ibuffer,FML_ERR_MSG,c_errmsg,0) ;
		tpreturn(TPFAIL,0L,(char *)ptr_fml_Ibuffer,0L,0) ;
	}
	strcpy(c_errmsg, "S31010");
	Fadd32(ptr_fml_Ibuffer,FML_ERR_MSG,c_errmsg,0) ;
	sprintf(c_other,"S77777");
	Fadd32(ptr_fml_Obuffer,FML_OTHER,c_other,0) ;
	Fadd32(ptr_fml_Ibuffer,FML_ERR_MSG,"S21034",0) ;
	errlog(c_ServiceName,ERR_DEFINE,FMLMSG,DEF_USR,DEF_SSSN,c_other) ;
	Fadd32(ptr_fml_Obuffer,FML_OTHER,c_other,0) ;
	if (Fget32(ptr_fml_Ibuffer,FML_USER_ID,0,c_user_id,0) == -1) {
		tpreturn(TPFAIL,0L,(char *)ptr_fml_Ibuffer,0L,0) ;
	}
}
`

func TestFmlOpErrorCodeCapture(t *testing.T) {
	f := ExtractTestFile(t, "errcode_demo.pc", errCodeSource)

	type want struct {
		field, target, code string
	}
	// All adds in source order.
	var got []want
	for _, op := range f.FmlOps {
		if op.Kind != FmlAdd {
			continue
		}
		got = append(got, want{op.Field, op.Target, op.Code})
	}
	wants := []want{
		{"FML_ERR_MSG", "c_errmsg", "S31005"}, // errlog correlation
		{"FML_ERR_MSG", "c_errmsg", "S31010"}, // strcpy correlation
		{"FML_OTHER", "c_other", "S77777"},    // sprintf correlation
		{"FML_ERR_MSG", "", "S21034"},         // direct-literal add
		{"FML_OTHER", "c_other", ""},          // define-named code: unresolvable
	}
	if len(got) != len(wants) {
		t.Fatalf("adds = %+v, want %d", got, len(wants))
	}
	for i := range wants {
		if got[i] != wants[i] {
			t.Errorf("add[%d] = %+v, want %+v", i, got[i], wants[i])
		}
	}
	// Gets never carry codes.
	for _, op := range f.FmlOps {
		if op.Kind == FmlGet && op.Code != "" {
			t.Errorf("get op %s carries code %q", op.Field, op.Code)
		}
	}
}

func TestErrCodeNearestWriterWins(t *testing.T) {
	src := `void SVC_N(TPSVCINFO *rqst) {
	char c_errmsg[64];
	errlog(s,"S1",A,B,c_errmsg) ;
	errlog(s,"S2",A,B,c_errmsg) ;
	Fadd32(ib,FML_ERR_MSG,c_errmsg,0) ;
}
`
	f := ExtractTestFile(t, "errcode_nearest.pc", src)
	for _, op := range f.FmlOps {
		if op.Kind == FmlAdd {
			if op.Code != "S2" {
				t.Errorf("code = %q, want the nearest writer S2", op.Code)
			}
			return
		}
	}
	t.Fatal("no add found")
}

func TestDefineAtRedefinitionLastWins(t *testing.T) {
	src := "#define A 1\nvoid SVC_X(TPSVCINFO* rqst) {\n#define A 2\n int i;\n}\n"
	f := ExtractTestFile(t, "defines_redef.pc", src)
	if d, ok := f.DefineAt("SVC_X", 4, "A"); !ok || d.Value != "2" {
		t.Errorf("redefined A = (%+v, %v), want 2", d, ok)
	}
}

// ExtractTestFile writes src to a temp .pc file and extracts it — the
// fresh-clone-safe way to pin extraction behavior on synthetic sources.
func ExtractTestFile(t *testing.T, name, src string) *File {
	t.Helper()
	if !strings.HasSuffix(name, ".pc") {
		name += ".pc"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	f, err := ExtractFile(path)
	if err != nil {
		t.Fatalf("extract %s: %v", name, err)
	}
	return f
}

// tpcallCallerSource / tpcallTargetSource are the DEF tpcall-resolution
// corpus: the caller's tpcall("SVC_TARGET", …) must resolve to the corpus
// file whose entry (or base name) carries that service.
const tpcallCallerSource = `void SVC_CALLER(TPSVCINFO *rqst) {
	char c_errmsg[64];
	if (x == 1) {
		tpcall("SVC_TARGET", sbuffer, 0, rbuffer, 0, 0) ;
	}
}
`

const tpcallTargetSource = `void SVC_TARGET(TPSVCINFO *rqst) {
	char c_flag;
	if (c_flag == 'N') {
		work();
	}
}
`

func TestTPCallServiceFileResolution(t *testing.T) {
	dir := t.TempDir()
	caller := filepath.Join(dir, "SVC_CALLER.pc")
	target := filepath.Join(dir, "SVC_TARGET.pc")
	if err := os.WriteFile(caller, []byte(tpcallCallerSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(tpcallTargetSource), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := ExtractDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		for _, tp := range f.TPCalls {
			got = append(got, tp.Service+" → "+tp.ServiceFile)
		}
	}
	if len(got) != 1 || got[0] != "SVC_TARGET → "+target {
		t.Errorf("service resolution = %v, want [SVC_TARGET → %s]", got, target)
	}

	// Single-file extraction has no corpus — the fact stays empty.
	single, err := ExtractFile(caller)
	if err != nil {
		t.Fatal(err)
	}
	if len(single.TPCalls) != 1 || single.TPCalls[0].ServiceFile != "" {
		t.Errorf("single-file service_file must stay empty, got %+v", single.TPCalls)
	}
}

func TestTPCallServiceFileUnresolvable(t *testing.T) {
	// A service outside the corpus keeps the field empty — visible, never a guess.
	dir := t.TempDir()
	caller := filepath.Join(dir, "SVC_CALLER.pc")
	if err := os.WriteFile(caller, []byte(tpcallCallerSource), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := ExtractDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files[0].TPCalls) != 1 || files[0].TPCalls[0].ServiceFile != "" {
		t.Errorf("unresolvable service_file must stay empty, got %+v", files[0].TPCalls)
	}
}
