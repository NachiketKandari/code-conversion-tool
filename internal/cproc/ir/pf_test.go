package ir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

// navFacts returns the nav fixture's raw scanner facts.
func navFacts(t *testing.T) *scanner.SourceFacts {
	t.Helper()
	facts, err := scanner.ScanFile(filepath.Join("..", "..", "..", "testdata", "nav", "SVC_DEMO_LIST.pc"))
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

// writePC writes a .pc fixture into a temp dir.
func writePC(t *testing.T, path, src string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// bufferRoleByName indexes the file's buffer facts by variable name.
func bufferRoleByName(f *File) map[string]FmlBufferRole {
	m := map[string]FmlBufferRole{}
	for _, b := range f.Buffers {
		m[b.Name] = b.Role
	}
	return m
}

// TestNavFactsOutOfComments pins PF-1.5 on the nav fixture: with the comment
// inventory in place, the exclusion of comment-located facts is a testable
// overlap query — zero recorded call or SQL position sits inside a comment
// span, and the banner 'added here … comment ends' live region keeps
// fn_long_to_int recorded.
func TestNavFactsOutOfComments(t *testing.T) {
	facts := navFacts(t)

	if len(facts.Comments) == 0 {
		t.Fatal("comment inventory empty on the nav fixture")
	}
	for i := range facts.Calls {
		call := &facts.Calls[i]
		if facts.InComment(call.Line, call.Col) {
			t.Errorf("call %s at %d:%d sits inside a comment span", call.Name, call.Line, call.Col)
		}
	}
	for i := range facts.Queries {
		q := &facts.Queries[i]
		if facts.InComment(q.StartLine, q.StartCol) {
			t.Errorf("query at %d sits inside a comment span", q.StartLine)
		}
	}
	// Live code between banners is recorded (the fn_long_to_int region).
	var foundLongToInt bool
	for _, call := range facts.Calls {
		if call.Name == "fn_long_to_int" {
			foundLongToInt = true
		}
	}
	if !foundLongToInt {
		t.Error("fn_long_to_int (live code between banner markers) missing from calls")
	}
	// Banner markers are recorded as banner facts, Live regions.
	var banners int
	for _, c := range facts.Comments {
		if c.Kind == "banner" {
			banners++
		}
	}
	if banners == 0 {
		t.Error("nav fixture records no banner comments")
	}
	if len(facts.Unbalanced) != 0 {
		t.Errorf("nav fixture unbalanced = %v, want none", facts.Unbalanced)
	}
}

// TestNavPredicatesAndFlagVars pins PF-2 on the nav fixture: every chain
// condition parses to a Cmp tree over c_flag, the serialized IR carries the
// predicate, and the flag-var results are byte-identical to the pre-tree
// goldens (the rewrite is a precision upgrade, not a behavior change).
func TestNavPredicatesAndFlagVars(t *testing.T) {
	f := navFile(t)
	wantExprs := []string{"c_flag == 'H'", "c_flag == 'F'", "c_flag == 'I'"}
	for i, want := range wantExprs {
		c := f.Conditions[i]
		if c.Predicate == nil || c.Predicate.Kind != "cmp" || c.Predicate.String() != want {
			t.Errorf("condition %d predicate = %v, want cmp %q", c.Index, c.Predicate, want)
		}
		if !reflect.DeepEqual(c.FlagVars, []string{"c_flag"}) {
			t.Errorf("condition %d flag vars = %v, want [c_flag]", c.Index, c.FlagVars)
		}
	}
	if f.Conditions[3].Predicate != nil {
		t.Errorf("default condition carries a predicate: %v", f.Conditions[3].Predicate)
	}

	// PF-2.3: the predicate serializes into the IR JSON alongside the raw
	// audit-trail text.
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"predicate":{`) {
		t.Error("IR JSON missing the predicate field")
	}

	// JSON round trip keeps the tree (concrete struct, no custom codec).
	var back File
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Conditions[0].Predicate == nil || back.Conditions[0].Predicate.String() != wantExprs[0] {
		t.Errorf("round-tripped predicate = %v", back.Conditions[0].Predicate)
	}
}

// TestBufferRolesNav pins PF-4.1/4.2 on the nav fixture: Fget32/Fadd32 carry
// their buffer, the buffer facts resolve the convention roles
// (ptr_fml_Ibuffer → input, ptr_fml_Obuffer → output), and an unknown buffer
// name degrades to the visible unknown-role fact. Nav has zero tpcalls —
// the TPCalls list stays empty (analyzer pins hold).
func TestBufferRolesNav(t *testing.T) {
	f := navFile(t)

	roles := bufferRoleByName(f)
	if roles["ptr_fml_Ibuffer"] != BufferInput || roles["ptr_fml_Obuffer"] != BufferOutput {
		t.Errorf("nav buffer roles = %v", roles)
	}

	var gotOps int
	for i := range f.FmlOps {
		if f.FmlOps[i].Buffer != "" {
			gotOps++
		}
	}
	if gotOps == 0 {
		t.Error("entry FmlOps carry no buffer")
	}

	// The nav fixture has no unknown buffer vars — everything follows the
	// convention (a healthy corpus property, pinned here).
	for _, b := range f.Buffers {
		if b.Role == BufferUnknown {
			t.Errorf("nav buffer %q degraded to unknown-role", b.Name)
		}
	}
	if len(f.TPCalls) != 0 {
		t.Errorf("nav tpcalls = %v, want none", f.TPCalls)
	}
}

// TestTPCallCorrelation pins PF-4.3 on the synthetic fixture: the site
// resolves the service name and correlates the Fadd32 send contract (before
// the call) with the Fget32 recv contract (after it) inside the enclosing
// block, with the extent spanning call → last recv.
func TestTPCallCorrelation(t *testing.T) {
	f, err := ExtractFile(filepath.Join("..", "..", "..", "testdata", "pf", "SVC_TP_DEMO.pc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.TPCalls) != 1 {
		t.Fatalf("tpcalls = %v, want exactly one correlated site", f.TPCalls)
	}
	tp := f.TPCalls[0]
	if tp.Service != "SVC_DEMO_DETAIL" || tp.Ambiguous {
		t.Errorf("tpcall = %+v", tp)
	}
	if tp.SendBuffer != "sbuffer" || tp.RecvBuffer != "rbuffer" {
		t.Errorf("buffer vars = %q/%q", tp.SendBuffer, tp.RecvBuffer)
	}
	if want := []string{"FML_COMP_CD", "FML_SCHEME_CD"}; !reflect.DeepEqual(fmlFieldsOf(tp.SendFML), want) {
		t.Errorf("send contract = %v, want %v", fmlFieldsOf(tp.SendFML), want)
	}
	if want := []string{"FML_NAV_DATE", "FML_NAV_NAV"}; !reflect.DeepEqual(fmlFieldsOf(tp.RecvFML), want) {
		t.Errorf("recv contract = %v, want %v", fmlFieldsOf(tp.RecvFML), want)
	}
	if tp.StartLine != 35 || tp.EndLine != 40 {
		t.Errorf("site extent = %d-%d, want 35-40", tp.StartLine, tp.EndLine)
	}

	roles := bufferRoleByName(f)
	want := map[string]FmlBufferRole{
		"ptr_fml_Ibuffer": BufferInput,
		"ptr_fml_Obuffer": BufferOutput,
		"sbuffer":         BufferSend,
		"rbuffer":         BufferRecv,
	}
	for name, role := range want {
		if roles[name] != role {
			t.Errorf("buffer %q role = %q, want %q", name, roles[name], role)
		}
	}
}

// TestTPCallAmbiguousDegrade: a tpcall whose buffer args are not plain
// variables degrades to a visible Ambiguous fact, never a guess (PF-4.3).
func TestTPCallAmbiguousDegrade(t *testing.T) {
	src := `void SVC_AMP(void)
{
    if(x == 1)
    {
        if(tpcall("SVC_X", build(), 0, 0, 0, 0) == -1)
        {
            userlog("failed");
        }
    }
}
`
	path := filepath.Join(t.TempDir(), "amp.pc")
	writePC(t, path, src)
	f, err := ExtractFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.TPCalls) != 1 || !f.TPCalls[0].Ambiguous {
		t.Fatalf("tpcalls = %+v, want one ambiguous site", f.TPCalls)
	}
	if f.TPCalls[0].Service != "SVC_X" {
		t.Errorf("service = %q, want SVC_X", f.TPCalls[0].Service)
	}
	if len(f.TPCalls[0].SendFML) != 0 || len(f.TPCalls[0].RecvFML) != 0 {
		t.Errorf("ambiguous site built contracts: %+v", f.TPCalls[0])
	}
}

// TestFragmentIR pins PF-3.3 on the fragment fixture: the __fragment entry,
// the top-level if/else chain building conditions with flag vars, the
// embedded cursor flattening identically to full files, FML ops from calls
// inside the fragment, and typed host vars from the fragment's decls.
func TestFragmentIR(t *testing.T) {
	f, err := ExtractFile(filepath.Join("..", "..", "..", "testdata", "pf", "fragment_nav_slice.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if !f.Fragment || f.Entry != "__fragment" {
		t.Errorf("fragment marking = %v entry %q", f.Fragment, f.Entry)
	}

	// Same buildConditions path: if + else chain, flag vars resolved from
	// the fragment's own decls.
	if len(f.Conditions) != 2 {
		t.Fatalf("conditions = %+v, want the 2-branch chain", f.Conditions)
	}
	if f.Conditions[0].Kind != "if" || f.Conditions[0].Expr != "c_flag == 'H'" ||
		!reflect.DeepEqual(f.Conditions[0].FlagVars, []string{"c_flag"}) {
		t.Errorf("condition 1 = %+v", f.Conditions[0])
	}
	if f.Conditions[0].Predicate == nil || f.Conditions[0].Predicate.String() != "c_flag == 'H'" {
		t.Errorf("condition 1 predicate = %v", f.Conditions[0].Predicate)
	}
	if !f.Conditions[1].IsDefault || f.Conditions[1].StartLine != 29 {
		t.Errorf("condition 2 = %+v", f.Conditions[1])
	}
	// Line numbers are the fragment file's own.
	if f.Conditions[0].StartLine != 8 || f.Conditions[0].EndLine != 28 {
		t.Errorf("condition 1 extent = %d-%d, want 8-28", f.Conditions[0].StartLine, f.Conditions[0].EndLine)
	}

	// Queries extract identically: the cursor flattens to one SELECT_MULTI
	// with the FETCH-INTO row shape and the WHERE bind.
	if len(f.Queries) != 1 {
		t.Fatalf("queries = %+v, want the flattened cur_demo", f.Queries)
	}
	q := f.Queries[0]
	if !q.CursorFlattened || q.Type != QuerySelectMulti || q.CursorName != "cur_demo" {
		t.Errorf("cursor unit = %+v", q)
	}
	if !reflect.DeepEqual(q.Binds, []string{"li_demo_comp"}) {
		t.Errorf("binds = %v", q.Binds)
	}
	if !reflect.DeepEqual(q.RowShape, []string{"sql_nav_date", "sql_nav_nav"}) {
		t.Errorf("row shape = %v", q.RowShape)
	}
	if want := []string{"DEMO_PRICE_HIST"}; !reflect.DeepEqual(q.Tables, want) {
		t.Errorf("tables = %v, want %v", q.Tables, want)
	}
	if q.OwningFunction != "__fragment" {
		t.Errorf("owning function = %q, want __fragment", q.OwningFunction)
	}
	if q.OrderBy != "NAV_DATE DESC" {
		t.Errorf("order by = %q", q.OrderBy)
	}

	// FML ops come from Fget32/Fadd32 inside the fragment; branch-internal
	// ops live on their condition (entry-preamble logic does not apply).
	if len(f.FmlOps) != 0 {
		t.Errorf("entry fml ops = %v, want none (everything is branch-internal)", f.FmlOps)
	}
	var gets, adds int
	for _, op := range f.Conditions[0].FmlOps {
		switch op.Kind {
		case FmlGet:
			gets++
		case FmlAdd:
			adds++
		}
	}
	if gets != 1 || adds != 2 {
		t.Errorf("condition 1 ops = %d gets/%d adds, want 1/2", gets, adds)
	}

	// Host vars typed from the fragment's declarations (no header vars).
	hv := hostVarByName(f)
	if v := hv["c_flag"]; v == nil || v.CType != "char" || v.FromHeader {
		t.Errorf("c_flag host var = %+v", v)
	}
	if v := hv["sql_nav_date"]; v == nil || v.CType != "varchar" {
		t.Errorf("sql_nav_date host var = %+v", v)
	}
}

// TestChainlessFragment pins design decision 3: a fragment without a
// top-level if-chain becomes ONE endpoint unit covering the whole block.
func TestChainlessFragment(t *testing.T) {
	src := `char c_flag;
EXEC SQL SELECT COUNT(*) INTO :cnt FROM DUAL;
Fadd32(Obuffer,FML_COUNT,(char*)&cnt,0);
`
	path := filepath.Join(t.TempDir(), "plain.txt")
	writePC(t, path, src)
	f, err := ExtractFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Fragment {
		t.Error("plain fragment not detected")
	}
	if len(f.Conditions) != 1 {
		t.Fatalf("conditions = %+v, want one synthetic condition", f.Conditions)
	}
	c := f.Conditions[0]
	if !c.IsDefault || c.Index != 1 || c.StartLine != 1 || c.EndLine != 3 {
		t.Errorf("synthetic condition = %+v", c)
	}
	if len(c.FmlOps) != 1 || c.FmlOps[0].Field != "FML_COUNT" {
		t.Errorf("synthetic condition ops = %+v, want the whole block's FML ops", c.FmlOps)
	}
	if len(c.QueryIDs) != 1 || c.QueryIDs[0] != "q1" {
		t.Errorf("synthetic condition queries = %v, want [q1]", c.QueryIDs)
	}
}

// TestHelperFileNotFragment pins the detection boundary: a full helper file
// with fn definitions (fn_demo_lib.pc) is never auto-converted to a fragment —
// its units keep their real owning function (the corpus-resolution shape).
func TestHelperFileNotFragment(t *testing.T) {
	f, err := ExtractFile(filepath.Join("..", "..", "..", "testdata", "nav", "fn_demo_lib.pc"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Fragment {
		t.Error("helper file auto-detected as fragment")
	}
	if f.Entry != "" {
		t.Errorf("helper entry = %q, want empty", f.Entry)
	}
	if len(f.Queries) != 1 || f.Queries[0].OwningFunction != "fn_is_demo_active" {
		t.Errorf("helper queries = %+v", f.Queries)
	}
}

// TestBufferRoleRegistryOverride pins the config-extensible registry: a
// renamed buffer convention resolves through Options.BufferRoles; unknown
// names degrade to unknown-role.
func TestBufferRoleRegistryOverride(t *testing.T) {
	src := `void SVC_REG(void)
{
    if(a == 1)
    {
        Fget32(myreq,FML_A,0,&v,0);
    }
}
`
	path := filepath.Join(t.TempDir(), "reg.pc")
	writePC(t, path, src)
	f, err := ExtractFileOpts(path, Options{BufferRoles: map[string]string{"myreq": "input"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := bufferRoleByName(f)["myreq"]; got != BufferInput {
		t.Errorf("registry override role = %q, want input", got)
	}
	// Default registry: myreq is outside the convention → unknown-role.
	f2, err := ExtractFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := bufferRoleByName(f2)["myreq"]; got != BufferUnknown {
		t.Errorf("default role = %q, want unknown-role", got)
	}
}

func fmlFieldsOf(ops []FmlOp) []string {
	var out []string
	for _, op := range ops {
		out = append(out, op.Field)
	}
	return out
}
