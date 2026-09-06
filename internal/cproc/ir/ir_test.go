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
	f, err := ExtractFile(filepath.Join("..", "..", "..", "testdata", "nav", "SVC_MF_NAV_LIST.pc"))
	if err != nil {
		t.Fatalf("ExtractFile failed: %v", err)
	}
	return f
}

func TestExtractNavGolden(t *testing.T) {
	f := navFile(t)

	if f.Entry != "SVC_MF_NAV_LIST" {
		t.Errorf("entry = %q", f.Entry)
	}
	if len(f.Functions) != 1 || f.Functions[0] != "SVC_MF_NAV_LIST" {
		t.Errorf("functions = %v", f.Functions)
	}

	// --- Condition inventory (§4.2.8): H / F / I / default, in order. ---
	wantConds := []struct {
		kind   string
		expr   string
		deflt  bool
		rng    [2]int
		quests []string
	}{
		{"if", "c_flag == 'H'", false, [2]int{180, 351}, []string{"q1", "cur_mf_nav_hist"}},
		{"elseif", "c_flag == 'F'", false, [2]int{353, 578}, []string{"q3", "cur_mf_freed"}},
		{"elseif", "c_flag == 'I'", false, [2]int{582, 808}, []string{"q5", "cur_mf_nav"}},
		{"else", "", true, [2]int{811, 974}, []string{"cur_mf_nav_list"}},
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
	for _, field := range []string{"FML_COMP_CD", "FML_MF_SCH_CD"} {
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
		if ops := opsByField(f.Conditions[ci].FmlOps, FmlGet, "FML_MATCH_ACCNT"); len(ops) != 1 || ops[0].Optional {
			t.Errorf("branch %d FML_MATCH_ACCNT = %+v, want required", ci, ops)
		}
	}
	// F branch response set includes the freedom-specific fields.
	for _, field := range []string{"FML_VLME", "FML_UPL_PRTFLO_NM"} {
		if ops := opsByField(f.Conditions[1].FmlOps, FmlAdd, field); len(ops) != 1 {
			t.Errorf("F branch missing Fadd32 %s", field)
		}
	}
	// Default branch: COMP_CD required (no FNOTPRES there).
	if ops := opsByField(f.Conditions[3].FmlOps, FmlGet, "FML_COMP_CD"); len(ops) != 1 || ops[0].Optional {
		t.Errorf("default FML_COMP_CD = %+v, want required", ops)
	}
	// Entry preamble: session reads dropped, flag read optional.
	if ops := opsByField(f.FmlOps, FmlGet, "FML_USR_ID"); len(ops) != 1 || !ops[0].Dropped {
		t.Errorf("preamble FML_USR_ID = %+v, want dropped", ops)
	}
	if ops := opsByField(f.FmlOps, FmlGet, "FML_SSSN_ID"); len(ops) != 1 || !ops[0].Dropped {
		t.Errorf("preamble FML_SSSN_ID = %+v, want dropped", ops)
	}
	if ops := opsByField(f.FmlOps, FmlGet, "FML_MF_GROWTH_FLG"); len(ops) != 1 || ops[0].Target != "c_flag" || !ops[0].Optional {
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

	// cur_mf_nav_hist — flattened cursor, 4 binds, 6-col row shape.
	q2 := byID["cur_mf_nav_hist"]
	if q2 == nil || !q2.CursorFlattened || q2.Type != QuerySelectMulti || q2.TemplateID != TemplateSelectMulti {
		t.Fatalf("cur_mf_nav_hist = %+v", q2)
	}
	if want := []string{"sql_mf_nav_comp_cd", "sql_mf_nav_sch_cd", "c_from_date", "c_to_date"}; !reflect.DeepEqual(q2.Binds, want) {
		t.Errorf("cur_mf_nav_hist binds = %v", q2.Binds)
	}
	if q2.BindArity != 4 || q2.StartLine != 232 || q2.EndLine != 343 {
		t.Errorf("cur_mf_nav_hist arity/extent = %d [%d,%d]", q2.BindArity, q2.StartLine, q2.EndLine)
	}
	if q2.OrderBy != "MF_NAV_HIST_DATE desc" {
		t.Errorf("cur_mf_nav_hist order by = %q", q2.OrderBy)
	}
	if !reflect.DeepEqual(q2.Tables, []string{"MF_COMPANIES", "MF_SCHEME_MASTER", "MF_NAVS_HIST"}) {
		t.Errorf("cur_mf_nav_hist tables = %v", q2.Tables)
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
	if len(q3.Binds) != 1 || q3.Binds[0] != "ls_match_acc" {
		t.Errorf("q3 binds = %v, want [ls_match_acc] (cnt_d2u is the INTO output)", q3.Binds)
	}
	if len(f.UniqueQueries()) != 6 {
		t.Errorf("unique queries = %d, want 6", len(f.UniqueQueries()))
	}

	// cur_mf_nav_list — the default-branch cursor, 1 bind, no ORDER BY.
	q7 := byID["cur_mf_nav_list"]
	if q7 == nil || len(q7.Binds) != 1 || q7.Binds[0] != "li_mf_comp_cd" || q7.OrderBy != "" {
		t.Errorf("cur_mf_nav_list = binds %v orderby %q", q7.Binds, q7.OrderBy)
	}
	if len(q7.RowShape) != 6 {
		t.Errorf("cur_mf_nav_list row shape = %v", q7.RowShape)
	}

	// --- Host variables: local decls typed, header vars flagged. ---
	hv := hostVarByName(f)
	if v := hv["c_from_date"]; v == nil || v.CType != "varchar" || !v.InDeclareSection || !v.Nullable {
		t.Errorf("c_from_date = %+v", v)
	}
	if v := hv["cnt_d2u"]; v == nil || v.CType != "int" || v.GoHint != "int" {
		t.Errorf("cnt_d2u = %+v", v)
	}
	if v := hv["li_mf_comp_cd"]; v == nil || v.CType != "long" || v.GoHint != "int64" {
		t.Errorf("li_mf_comp_cd = %+v, want long/int64 (a cast must not retype it)", v)
	}
	if v := hv["sql_mf_nav_comp_cd"]; v == nil || !v.FromHeader || v.CType != "" {
		t.Errorf("sql_mf_nav_comp_cd = %+v, want untyped header var", v)
	}

	// --- External fns (file mode: nothing to resolve against). ---
	ext := externalByName(f)
	for _, name := range []string{"chk_sssn", "fn_is_d2u_active", "fn_long_to_int"} {
		if ext[name] == nil {
			t.Fatalf("external fn %s missing", name)
		}
		if ext[name].Resolved {
			t.Errorf("%s must be unresolved in single-file mode", name)
		}
	}
	if calls := ext["fn_is_d2u_active"].Callsites; !reflect.DeepEqual(calls, []int{419, 654}) {
		t.Errorf("fn_is_d2u_active callsites = %v", calls)
	}
}

func TestExtractDirResolvesExternalFns(t *testing.T) {
	files, err := ExtractDir(filepath.Join("..", "..", "..", "testdata", "nav"))
	if err != nil {
		t.Fatalf("ExtractDir failed: %v", err)
	}
	var nav, fnFile *File
	for _, f := range files {
		if strings.HasSuffix(f.Path, "SVC_MF_NAV_LIST.pc") {
			nav = f
		}
		if strings.HasSuffix(f.Path, "fn_d2u_mf.pc") {
			fnFile = f
		}
	}
	if nav == nil || fnFile == nil {
		t.Fatalf("fixture files missing: %v", files)
	}

	ext := externalByName(nav)
	fn := ext["fn_is_d2u_active"]
	if fn == nil || !fn.Resolved || !strings.HasSuffix(fn.DefinedIn, "fn_d2u_mf.pc") || !fn.HasSQL {
		t.Fatalf("fn_is_d2u_active resolution = %+v", fn)
	}
	// The defining file's query unit is referenced by ID (§4.2.9).
	if len(fn.QueryIDs) != 1 || fnFile.Queries[0].ID != fn.QueryIDs[0] {
		t.Errorf("query ids = %v, want the defining file's %q", fn.QueryIDs, fnFile.Queries[0].ID)
	}
	if fnFile.Queries[0].OwningFunction != "fn_is_d2u_active" {
		t.Errorf("defining unit owner = %q", fnFile.Queries[0].OwningFunction)
	}
	// Unresolved externals stay flagged, never stubbed (§4.2.9.4).
	for _, name := range []string{"chk_sssn", "fn_long_to_int"} {
		if ext[name] == nil || ext[name].Resolved {
			t.Errorf("%s must remain unresolved: %+v", name, ext[name])
		}
	}
}

func TestExtractDMLMarking(t *testing.T) {
	src := `void SVC_DML_DEMO(void)
{
    EXEC SQL INSERT INTO MF_LOG (ID, NOTE) VALUES (:seq, :note);
    EXEC SQL UPDATE MF_NAVS SET NAV = :nav WHERE COMP_CD = :comp;
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
	wantTables := [][]string{{"MF_LOG"}, {"MF_NAVS"}, {"MF_TEMP"}}
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
		TemplateDeleteTx != string(templates.DBMethodDeleteTx) {
		t.Fatal("ir template ids drifted from templates.ID constants")
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
