package ir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/templates"
)

func navFile(t *testing.T) *File {
	t.Helper()
	f, err := ExtractFile(filepath.Join("..", "..", "..", "testdata", "nav", "SVC_DEMO_LIST.pc"))
	if err != nil {
		t.Fatalf("ExtractFile failed: %v", err)
	}
	return f
}

func TestExtractNavGolden(t *testing.T) {
	f := navFile(t)

	if f.Entry != "SVC_DEMO_LIST" {
		t.Errorf("entry = %q", f.Entry)
	}
	if len(f.Functions) != 1 || f.Functions[0] != "SVC_DEMO_LIST" {
		t.Errorf("functions = %v", f.Functions)
	}

	// Branching factor (doubling rule): 75 if/else-if headers, Σ 2^nest = 142.
	if f.BranchCount != 75 {
		t.Errorf("branch_count = %d, want 75", f.BranchCount)
	}
	if f.BranchingFactor != 142 {
		t.Errorf("branching_factor = %d, want 142", f.BranchingFactor)
	}

	// --- Condition inventory (§4.2.8): H / F / I / default, in order. ---
	wantConds := []struct {
		kind   string
		expr   string
		deflt  bool
		rng    [2]int
		quests []string
	}{
		{"if", "c_flag == 'H'", false, [2]int{180, 351}, []string{"q1", "cur_demo_hist"}},
		{"elseif", "c_flag == 'F'", false, [2]int{353, 578}, []string{"q3", "cur_demo_featured"}},
		{"elseif", "c_flag == 'I'", false, [2]int{582, 808}, []string{"q5", "cur_demo_insured"}},
		{"else", "", true, [2]int{811, 974}, []string{"cur_demo_list"}},
	}
	if len(f.Conditions) != len(wantConds) {
		t.Fatalf("got %d conditions, want %d", len(f.Conditions), len(wantConds))
	}
	for i, w := range wantConds {
		c := f.Conditions[i]
		if c.Index != i+1 || c.Kind != w.kind || c.Expr != w.expr || c.IsDefault != w.deflt {
			t.Errorf("condition %d = {kind %s expr %q default %v}, want {kind %s expr %q default %v}",
				i, c.Kind, c.Expr, c.IsDefault, w.kind, w.expr, w.deflt)
		}
		if c.StartLine != w.rng[0] || c.EndLine != w.rng[1] {
			t.Errorf("condition %d range = [%d,%d], want %v", i, c.StartLine, c.EndLine, w.rng)
		}
		if !reflect.DeepEqual(c.QueryIDs, w.quests) {
			t.Errorf("condition %d query ids = %v, want %v", i, c.QueryIDs, w.quests)
		}
		if c.Expr != "" && (len(c.FlagVars) == 0 || c.FlagVars[0] != "c_flag") {
			t.Errorf("condition %d flag vars = %v, want [c_flag]", i, c.FlagVars)
		}
	}

	// Fget32 = inputs / Fadd32 = outputs; FNOTPRES ⇒ optional (§4.8.3);
	// session/error plumbing is dropped (§4.8.4).
	opsByField := func(ops []FmlOp, kind FmlOpKind, field string) (found []FmlOp) {
		for _, op := range ops {
			if op.Kind == kind && op.Field == field {
				found = append(found, op)
			}
		}
		return found
	}
	// H branch: COMP_CD and SCH_CD required.
	for _, field := range []string{"FML_COMP_CD", "FML_SCHEME_CD"} {
		ops := opsByField(f.Conditions[0].FmlOps, FmlGet, field)
		if len(ops) != 1 || ops[0].Optional {
			t.Errorf("H %s get = %+v, want one required op", field, ops)
		}
	}
	// F and I branches: COMP_CD optional (FNOTPRES default), MATCH_ACCNT required.
	for _, ci := range []int{1, 2} {
		if ops := opsByField(f.Conditions[ci].FmlOps, FmlGet, "FML_COMP_CD"); len(ops) != 1 || !ops[0].Optional {
			t.Errorf("branch %d FML_COMP_CD = %+v, want optional", ci, ops)
		}
		if ops := opsByField(f.Conditions[ci].FmlOps, FmlGet, "FML_ACCOUNT"); len(ops) != 1 || ops[0].Optional {
			t.Errorf("branch %d FML_ACCOUNT = %+v, want required", ci, ops)
		}
	}
	// F branch response set includes the freedom-specific fields.
	for _, field := range []string{"FML_RATING", "FML_LABEL"} {
		if ops := opsByField(f.Conditions[1].FmlOps, FmlAdd, field); len(ops) != 1 {
			t.Errorf("F branch missing Fadd32 %s", field)
		}
	}
	// Default branch: COMP_CD required (no FNOTPRES there).
	if ops := opsByField(f.Conditions[3].FmlOps, FmlGet, "FML_COMP_CD"); len(ops) != 1 || ops[0].Optional {
		t.Errorf("default FML_COMP_CD = %+v, want required", ops)
	}
	// Entry preamble: session reads dropped, flag read optional.
	if ops := opsByField(f.FmlOps, FmlGet, "FML_USER_ID"); len(ops) != 1 || !ops[0].Dropped {
		t.Errorf("preamble FML_USER_ID = %+v, want dropped", ops)
	}
	if ops := opsByField(f.FmlOps, FmlGet, "FML_SESSION_ID"); len(ops) != 1 || !ops[0].Dropped {
		t.Errorf("preamble FML_SESSION_ID = %+v, want dropped", ops)
	}
	if ops := opsByField(f.FmlOps, FmlGet, "FML_MODE_FLG"); len(ops) != 1 || ops[0].Target != "c_flag" || !ops[0].Optional {
		t.Errorf("preamble flag read = %+v, want optional target c_flag", ops)
	}

	// --- Query units: 7 live queries (the ver-2.2 commented COUNTs never appear). ---
	if len(f.Queries) != 7 {
		t.Fatalf("got %d query units, want 7: %v", len(f.Queries), queryIDs(f))
	}
	byID := queryByID(f)

	// q1 — dual select, single-value, no input binds.
	q1 := byID["q1"]
	if q1 == nil || q1.Type != QuerySelectSingle || q1.TemplateID != TemplateSelectSingle {
		t.Fatalf("q1 = %+v", q1)
	}
	if !reflect.DeepEqual(q1.RowShape, []string{"c_from_date", "c_to_date"}) {
		t.Errorf("q1 row shape = %v", q1.RowShape)
	}
	if len(q1.Binds) != 0 || q1.BindArity != 0 {
		t.Errorf("q1 binds = %v (arity %d), want none — INTO targets are outputs", q1.Binds, q1.BindArity)
	}
	if !reflect.DeepEqual(q1.Tables, []string{"dual"}) {
		t.Errorf("q1 tables = %v", q1.Tables)
	}

	// cur_demo_hist — flattened cursor, 4 binds, 6-col row shape.
	q2 := byID["cur_demo_hist"]
	if q2 == nil || !q2.CursorFlattened || q2.Type != QuerySelectMulti || q2.TemplateID != TemplateSelectMulti {
		t.Fatalf("cur_demo_hist = %+v", q2)
	}
	if want := []string{"sql_demo_comp_cd", "sql_demo_scheme_cd", "c_from_date", "c_to_date"}; !reflect.DeepEqual(q2.Binds, want) {
		t.Errorf("cur_demo_hist binds = %v", q2.Binds)
	}
	if q2.BindArity != 4 || q2.StartLine != 232 || q2.EndLine != 343 {
		t.Errorf("cur_demo_hist arity/extent = %d [%d,%d]", q2.BindArity, q2.StartLine, q2.EndLine)
	}
	if q2.OrderBy != "DEMO_HIST_DATE desc" {
		t.Errorf("cur_demo_hist order by = %q", q2.OrderBy)
	}
	if !reflect.DeepEqual(q2.Tables, []string{"DEMO_COMPANY", "DEMO_SCHEME", "DEMO_PRICE_HIST"}) {
		t.Errorf("cur_demo_hist tables = %v", q2.Tables)
	}

	// q3 / q5 — the identical F/I COUNT dedup pair.
	q3, q5 := byID["q3"], byID["q5"]
	if q3 == nil || q5 == nil {
		t.Fatal("missing q3/q5")
	}
	if q3.DedupKey == "" || q3.DedupKey != q5.DedupKey {
		t.Errorf("dedup keys not shared: %q vs %q", q3.DedupKey, q5.DedupKey)
	}
	if q5.DuplicateOf != q3.ID || q3.DuplicateOf != "" {
		t.Errorf("duplicate link = q5→%q q3→%q", q5.DuplicateOf, q3.DuplicateOf)
	}
	if len(q3.Binds) != 1 || q3.Binds[0] != "ls_demo_acc" {
		t.Errorf("q3 binds = %v, want [ls_demo_acc] (cnt_demo is the INTO output)", q3.Binds)
	}
	if len(f.UniqueQueries()) != 6 {
		t.Errorf("unique queries = %d, want 6", len(f.UniqueQueries()))
	}

	// cur_demo_list — the default-branch cursor, 1 bind, no ORDER BY.
	q7 := byID["cur_demo_list"]
	if q7 == nil || len(q7.Binds) != 1 || q7.Binds[0] != "li_demo_comp" || q7.OrderBy != "" {
		t.Errorf("cur_demo_list = binds %v orderby %q", q7.Binds, q7.OrderBy)
	}
	if len(q7.RowShape) != 6 {
		t.Errorf("cur_demo_list row shape = %v", q7.RowShape)
	}

	// --- Host variables: local decls typed, header vars flagged. ---
	hv := hostVarByName(f)
	if v := hv["c_from_date"]; v == nil || v.CType != "varchar" || !v.InDeclareSection || !v.Nullable {
		t.Errorf("c_from_date = %+v", v)
	}
	if v := hv["cnt_demo"]; v == nil || v.CType != "int" || v.GoHint != "int" {
		t.Errorf("cnt_demo = %+v", v)
	}
	if v := hv["li_demo_comp"]; v == nil || v.CType != "long" || v.GoHint != "int64" {
		t.Errorf("li_demo_comp = %+v, want long/int64 (a cast must not retype it)", v)
	}
	if v := hv["sql_demo_comp_cd"]; v == nil || !v.FromHeader || v.CType != "" {
		t.Errorf("sql_demo_comp_cd = %+v, want untyped header var", v)
	}

	// --- External fns (file mode: nothing to resolve against). ---
	ext := externalByName(f)
	for _, name := range []string{"chk_session", "fn_is_demo_active", "fn_long_to_int"} {
		if ext[name] == nil {
			t.Fatalf("external fn %s missing", name)
		}
		if ext[name].Resolved {
			t.Errorf("%s must be unresolved in single-file mode", name)
		}
	}
	if calls := ext["fn_is_demo_active"].Callsites; !reflect.DeepEqual(calls, []int{419, 654}) {
		t.Errorf("fn_is_demo_active callsites = %v", calls)
	}
}

func TestExtractDirResolvesExternalFns(t *testing.T) {
	files, err := ExtractDir(filepath.Join("..", "..", "..", "testdata", "nav"))
	if err != nil {
		t.Fatalf("ExtractDir failed: %v", err)
	}
	var nav, fnFile *File
	for _, f := range files {
		if strings.HasSuffix(f.Path, "SVC_DEMO_LIST.pc") {
			nav = f
		}
		if strings.HasSuffix(f.Path, "fn_demo_lib.pc") {
			fnFile = f
		}
	}
	if nav == nil || fnFile == nil {
		t.Fatalf("fixture files missing: %v", files)
	}

	ext := externalByName(nav)
	fn := ext["fn_is_demo_active"]
	if fn == nil || !fn.Resolved || !strings.HasSuffix(fn.DefinedIn, "fn_demo_lib.pc") || !fn.HasSQL {
		t.Fatalf("fn_is_demo_active resolution = %+v", fn)
	}
	// The defining file's query unit is referenced by ID (§4.2.9).
	if len(fn.QueryIDs) != 1 || fnFile.Queries[0].ID != fn.QueryIDs[0] {
		t.Errorf("query ids = %v, want the defining file's %q", fn.QueryIDs, fnFile.Queries[0].ID)
	}
	if fnFile.Queries[0].OwningFunction != "fn_is_demo_active" {
		t.Errorf("defining unit owner = %q", fnFile.Queries[0].OwningFunction)
	}
	// Unresolved externals stay flagged, never stubbed (§4.2.9.4).
	for _, name := range []string{"chk_session", "fn_long_to_int"} {
		if ext[name] == nil || ext[name].Resolved {
			t.Errorf("%s must remain unresolved: %+v", name, ext[name])
		}
	}
}

func TestExtractDMLMarking(t *testing.T) {
	src := `void SVC_DML_DEMO(void)
{
    EXEC SQL INSERT INTO MF_LOG (ID, NOTE) VALUES (:seq, :note);
    EXEC SQL UPDATE DEMO_PRICE SET NAV = :nav WHERE COMP_CD = :comp;
    EXEC SQL DELETE FROM MF_TEMP WHERE ID = :id;
}
`
	path := filepath.Join(t.TempDir(), "dml.pc")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := ExtractFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Queries) != 3 {
		t.Fatalf("got %d queries, want 3", len(f.Queries))
	}
	wantTypes := []QueryType{QueryInsert, QueryUpdate, QueryDelete}
	wantTemplates := []string{TemplateInsertTx, TemplateUpdateTx, TemplateDeleteTx}
	wantTables := [][]string{{"MF_LOG"}, {"DEMO_PRICE"}, {"MF_TEMP"}}
	for i, q := range f.Queries {
		if q.Type != wantTypes[i] || q.TemplateID != wantTemplates[i] {
			t.Errorf("query %d type/template = %s/%s, want %s/%s", i, q.Type, q.TemplateID, wantTypes[i], wantTemplates[i])
		}
		if !reflect.DeepEqual(q.Tables, wantTables[i]) {
			t.Errorf("query %d tables = %v, want %v", i, q.Tables, wantTables[i])
		}
	}
}

// TestTemplateMirror pins the ir template-string mirror onto the real
// templates.ID constants so the two packages cannot drift.
func TestTemplateMirror(t *testing.T) {
	if TemplateSelectSingle != string(templates.DBMethodSelectSingle) ||
		TemplateSelectMulti != string(templates.DBMethodSelectMulti) ||
		TemplateInsertTx != string(templates.DBMethodInsertTx) ||
		TemplateUpdateTx != string(templates.DBMethodUpdateTx) ||
		TemplateDeleteTx != string(templates.DBMethodDeleteTx) ||
		TemplateMerge != string(templates.DBMethodMerge) {
		t.Fatal("ir template ids drifted from templates.ID constants")
	}
}

// TestExtractMergeGolden pins the MERGE rubric on the merge fixture: one
// MERGE query unit with the DML template, target table after MERGE INTO,
// named binds — plus the nesting math over its if/else chain (else bodies
// never nest; the nested sqlcode checks double once).
func TestExtractMergeGolden(t *testing.T) {
	f, err := ExtractFile(filepath.Join("..", "..", "..", "testdata", "merge", "SVC_DEMO_MERGE.pc"))
	if err != nil {
		t.Fatalf("ExtractFile failed: %v", err)
	}

	if f.Entry != "SVC_DEMO_MERGE" {
		t.Errorf("entry = %q", f.Entry)
	}
	if f.BranchCount != 3 || f.BranchingFactor != 4 {
		t.Errorf("branch count/factor = %d/%d, want 3/4 (1 + 1 + 2)", f.BranchCount, f.BranchingFactor)
	}

	if len(f.Queries) != 1 {
		t.Fatalf("queries = %d, want 1 (%v)", len(f.Queries), queryIDs(f))
	}
	q := f.Queries[0]
	if q.ID != "q1" || q.Type != QueryMerge || q.TemplateID != TemplateMerge {
		t.Errorf("query = %s/%s/%s, want q1/MERGE/%s", q.ID, q.Type, q.TemplateID, TemplateMerge)
	}
	if !strings.Contains(q.SQL, "MERGE INTO DEMO_ACCOUNTS") {
		t.Errorf("merge SQL body missing: %q", q.SQL)
	}
	if !reflect.DeepEqual(q.Tables, []string{"DEMO_ACCOUNTS"}) {
		t.Errorf("tables = %v, want [DEMO_ACCOUNTS]", q.Tables)
	}
	if !reflect.DeepEqual(q.Binds, []string{"account_id", "balance"}) || q.BindArity != 2 {
		t.Errorf("binds/arity = %v/%d, want [account_id balance]/2", q.Binds, q.BindArity)
	}

	// Condition inventory: the top-level if/else chain — the MERGE lives in
	// the default branch, the FML read in the preamble outside it.
	if len(f.Conditions) != 2 {
		t.Fatalf("conditions = %d, want 2", len(f.Conditions))
	}
	if f.Conditions[0].Kind != "if" || f.Conditions[0].IsDefault ||
		f.Conditions[0].FlagVars == nil || len(f.Conditions[0].FlagVars) != 1 {
		t.Errorf("condition 1 = %+v", f.Conditions[0])
	}
	c2 := f.Conditions[1]
	if c2.Kind != "else" || !c2.IsDefault || !reflect.DeepEqual(c2.QueryIDs, []string{"q1"}) {
		t.Errorf("condition 2 = %+v (want else, default, q1)", c2)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	f := navFile(t)
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back File
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	b2, err := json.Marshal(&back)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(b2) {
		t.Error("IR JSON round trip not stable")
	}
}

func queryByID(f *File) map[string]*Query {
	m := make(map[string]*Query, len(f.Queries))
	for _, q := range f.Queries {
		m[q.ID] = q
	}
	return m
}

func queryIDs(f *File) []string {
	out := make([]string, 0, len(f.Queries))
	for _, q := range f.Queries {
		out = append(out, q.ID)
	}
	return out
}

func externalByName(f *File) map[string]*ExternalFn {
	m := make(map[string]*ExternalFn, len(f.ExternalFns))
	for i := range f.ExternalFns {
		m[f.ExternalFns[i].Name] = &f.ExternalFns[i]
	}
	return m
}

func hostVarByName(f *File) map[string]*HostVar {
	m := make(map[string]*HostVar, len(f.HostVars))
	for i := range f.HostVars {
		m[f.HostVars[i].Name] = &f.HostVars[i]
	}
	return m
}
