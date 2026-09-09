package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/cproc/flow"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
	"github.com/Public/convert-tux-to-go/internal/llm"
	"github.com/Public/convert-tux-to-go/internal/plan"
)

const navFixture = "../../testdata/nav/SVC_DEMO_LIST.pc"

func discoverDraft(t *testing.T) string {
	t.Helper()
	f, err := ir.ExtractFileOpts(navFixture, ir.Options{})
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(navFixture)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := scanner.ScanBytes(src, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	tree := flow.Build(src, facts, f.Entry, f)
	candidates := flow.Discover(tree, f.Conditions)
	if len(candidates) == 0 {
		t.Fatal("no candidates discovered for the nav fixture")
	}
	return renderDraft(f, candidates, false, nil)
}

// TestDiscoverDraftRoundTrip pins the scan-then-tag contract: the untagged
// draft fails mapping validation (the tag instruction), and after tagging
// names/routes the same bytes load through the strict loader unchanged.
func TestDiscoverDraftRoundTrip(t *testing.T) {
	draft := discoverDraft(t)
	if !strings.Contains(draft, `name: ""`) || !strings.Contains(draft, `route: ""`) {
		t.Errorf("draft missing tag placeholders:\n%s", draft)
	}
	if strings.Contains(draft, "service: \n") {
		t.Error("service must be prefilled from the entry name")
	}

	// Untagged: the strict loader rejects the empty identifiers.
	untagged := filepath.Join(t.TempDir(), "untagged.yaml")
	if err := os.WriteFile(untagged, []byte(draft), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.LoadMapping(untagged); err == nil {
		t.Error("untagged draft must fail LoadMapping")
	}

	// Tagged: same draft with the placeholders filled and module uncommented
	// — exactly the flow the draft header instructs — loads clean. Names are
	// unique per endpoint, as the validation requires.
	tagged := draft
	tagged = strings.Replace(tagged, "# module: your-app/", "module: your-app/", 1)
	names := []string{"NavHistory", "SipFreedem", "SipInsurance", "NavList", "Extra"}
	for _, n := range names {
		tagged = strings.Replace(tagged, `name: ""`, `name: `+n, 1)
	}
	tagged = strings.ReplaceAll(tagged, `route: ""`, `route: /route`)
	taggedPath := filepath.Join(t.TempDir(), "tagged.yaml")
	if err := os.WriteFile(taggedPath, []byte(tagged), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := plan.LoadMapping(taggedPath)
	if err != nil {
		t.Fatalf("tagged draft must load: %v", err)
	}
	if m.Service != "svc_demo_list" || len(m.Endpoints) == 0 {
		t.Errorf("loaded mapping = %+v", m)
	}
	for _, e := range m.Endpoints {
		if e.Name == "" || !strings.HasPrefix(e.Route, "/") {
			t.Errorf("endpoint not tagged: %+v", e)
		}
	}
}

// TestDiscoverDirMode runs the command over the real corpus directory: one
// draft per service entry, batch/fn-lib files skipped loudly.
func TestDiscoverDirMode(t *testing.T) {
	out := t.TempDir()
	if err := runDiscover(context.Background(), []string{"../../tuxExamples", "-out", out}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var drafts int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".mapping.yaml") {
			drafts++
		}
	}
	if drafts != 1 {
		t.Fatalf("drafts written = %d, want 1 (mainTux.pc)", drafts)
	}
}

// TestDiscoverDirModeHonestZero pins the rubric on the stripped corpus: its
// condition bodies only write (reads live in the entry preamble), so zero
// candidates is the CORRECT answer — the skip lines say so, never a guess.
func TestDiscoverDirModeHonestZero(t *testing.T) {
	out := t.TempDir()
	if err := runDiscover(context.Background(), []string{"../../testdata/stripped", "-out", out}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".mapping.yaml") {
			t.Errorf("unexpected draft %s (stripped corpus has no qualifying candidates)", e.Name())
		}
	}
}

// TestDiscoverOutDir pins the -out override vs the mappings/ convention.
func TestDiscoverOutDir(t *testing.T) {
	if got := discoverOutDir(""); got != "mappings" {
		t.Errorf("default out dir = %q, want mappings", got)
	}
	if got := discoverOutDir("/tmp/x"); got != "/tmp/x" {
		t.Errorf("override out dir = %q", got)
	}
}

// TestDiscoverConfigFallback pins the bare-command UX: with no positional
// target the tool falls back to convert.input from .tuxgo.yaml.
func TestDiscoverConfigFallback(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".tuxgo.yaml")
	if err := os.WriteFile(cfg, []byte("convert:\n  input: ../../testdata/nav/SVC_DEMO_LIST.pc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "drafts")
	if err := runDiscover(context.Background(), []string{"-config", cfg, "-out", out}); err != nil {
		t.Fatalf("bare discover with convert.input: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "SVC_DEMO_LIST.mapping.yaml")); err != nil {
		t.Errorf("draft not written from config target: %v", err)
	}
}

// TestDiscoverNoClobber pins that a second run never overwrites a draft —
// a tagged draft is user work, and the fresh draft lands alongside as a
// numbered sibling ("<base> (1).mapping.yaml").
func TestDiscoverNoClobber(t *testing.T) {
	out := t.TempDir()
	args := []string{"../../tuxExamples/mainTux.pc", "-out", out}
	if err := runDiscover(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(out, "mainTux.mapping.yaml")
	if err := os.WriteFile(path, []byte("# my tagged draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runDiscover(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# my tagged draft\n" {
		t.Error("second discover run overwrote the user's draft")
	}
	variant, err := os.ReadFile(filepath.Join(out, "mainTux (1).mapping.yaml"))
	if err != nil {
		t.Fatalf("fresh draft not written alongside: %v", err)
	}
	if !strings.Contains(string(variant), "endpoints:") {
		t.Error("variant file is not a draft")
	}
}

// TestDiscoverFragment pins the standalone-condition mode (user report,
// 2026-09-10): a lone `else if` block with no SVC entry is a fragment —
// flow/discover must wrap it like the extractor does and still find the
// candidate (a plain ScanBytes sees no function body and yields an empty
// tree).
func TestDiscoverFragment(t *testing.T) {
	dir := t.TempDir()
	frag := filepath.Join(dir, "single.pc")
	src := "/********2.0 added*********/\n" +
		"    else if (c_flag == 'F')\n" +
		"    {\n" +
		"        if(Fget32(ptr_fml_Ibuffer,FML_COMP_CD,0,(char*)&v,0) == -1)\n" +
		"        {\n" +
		"            Fadd32(ptr_fml_Ibuffer,FML_ERR_MSG,c_errmsg,0);\n" +
		"            tpreturn(TPFAIL,0L,(char *)ptr_fml_Ibuffer,0L,0);\n" +
		"        }\n" +
		"        EXEC SQL DECLARE cur_x CURSOR FOR SELECT A FROM T;\n" +
		"        EXEC SQL OPEN cur_x;\n" +
		"        while(1)\n" +
		"        {\n" +
		"            EXEC SQL FETCH cur_x INTO :a;\n" +
		"            if(SQLCODE == NO_DATA_FOUND) break;\n" +
		"            i_err_op[0] = Fadd32(ptr_fml_Obuffer,FML_A,(char*)&a,0);\n" +
		"            i_err_op[1] = Fadd32(ptr_fml_Obuffer,FML_B,(char*)&b,0);\n" +
		"        }\n" +
		"        EXEC SQL CLOSE cur_x;\n" +
		"    }\n"
	if err := os.WriteFile(frag, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "mappings")
	if err := runDiscover(context.Background(), []string{frag, "-out", out}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "single.mapping.yaml"))
	if err != nil {
		t.Fatalf("fragment draft not written: %v", err)
	}
	if !strings.Contains(string(data), "- condition: 1") {
		t.Errorf("fragment draft missing the condition-1 endpoint:\n%s", data)
	}
	if strings.Contains(string(data), "- conditionRef:") {
		t.Errorf("fragment guards must not become nested candidates:\n%s", data)
	}
}

// TestDiscoverAINaming pins the AI-defaults seam: with discover.ai (or -ai)
// and a reachable model, each candidate endpoint gets one naming call whose
// JSON proposal pre-fills name/route and the dbMethods pins — advisory
// defaults in an otherwise unchanged draft. Failure paths keep the
// deterministic placeholders.
func TestDiscoverAINaming(t *testing.T) {
	server := llm.NewFakeServer(llm.FakeResponse{Content: "```json\n" +
		`{"name":"GetNavHistory","route":"/mfnavhistory","dbMethods":[` +
		`{"id":"q1","name":"GetDateDetails","row":"DateInfo"},` +
		`{"id":"cur_x","name":"GetNavRows","row":"NavRow"}]}` + "\n```"})
	// q1 is deliberately bogus (the flattened cursor unit's id is cur_x) —
	// the id filter must drop it.
	defer server.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, ".tuxgo.yaml")
	conf := "run:\n  profile: fake\n  llm: true\nmodels:\n" +
		"  - name: fake\n    provider: openai-compatible\n    model: fake-model\n" +
		"    apiBase: " + server.URL + "\n    apiKey: test\n" +
		"discover:\n  mode: ai\n"
	if err := os.WriteFile(cfg, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	frag := filepath.Join(dir, "single.pc")
	src := "else if (c_flag == 'F')\n{\n" +
		"    if(Fget32(ptr_fml_Ibuffer,FML_COMP_CD,0,(char*)&v,0) == -1)\n" +
		"    {\n        Fadd32(ptr_fml_Ibuffer,FML_ERR_MSG,c_errmsg,0);\n" +
		"        tpreturn(TPFAIL,0L,(char *)ptr_fml_Ibuffer,0L,0);\n    }\n" +
		"    EXEC SQL DECLARE cur_x CURSOR FOR SELECT A FROM T;\n" +
		"    EXEC SQL OPEN cur_x;\n    while(1)\n    {\n" +
		"        EXEC SQL FETCH cur_x INTO :a;\n" +
		"        if(SQLCODE == NO_DATA_FOUND) break;\n" +
		"        i_err_op[0] = Fadd32(ptr_fml_Obuffer,FML_A,(char*)&a,0);\n" +
		"        i_err_op[1] = Fadd32(ptr_fml_Obuffer,FML_B,(char*)&b,0);\n    }\n" +
		"    EXEC SQL CLOSE cur_x;\n}\n"
	if err := os.WriteFile(frag, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "mappings")
	if err := runDiscover(context.Background(), []string{"-config", cfg, "-out", out, frag}); err != nil {
		t.Fatal(err)
	}
	if server.RequestCount() == 0 {
		t.Fatal("no AI call was made")
	}
	data, err := os.ReadFile(filepath.Join(out, "single.mapping.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	draft := string(data)
	if !strings.Contains(draft, "name: \"GetNavHistory\"") || !strings.Contains(draft, "ai-suggested") {
		t.Errorf("endpoint name not pre-filled from the AI proposal:\n%s", draft)
	}
	if !strings.Contains(draft, "route: \"/mfnavhistory\"") {
		t.Errorf("route not pre-filled:\n%s", draft)
	}
	if !strings.Contains(draft, "dbMethods:") || !strings.Contains(draft, "name: GetNavRows") ||
		!strings.Contains(draft, "row: NavRow") {
		t.Errorf("dbMethods pins not emitted from the AI proposal:\n%s", draft)
	}
	if strings.Contains(draft, "GetDateDetails") || strings.Contains(draft, "DateInfo") {
		t.Errorf("a pin for an unknown query id leaked into the draft:\n%s", draft)
	}
	// And the tagged draft still loads through the strict loader.
	tagged := strings.ReplaceAll(draft, "ai-suggested", "tagged")
	tagged = strings.Replace(tagged, "# module: your-app/", "module: your-app/", 1)
	taggedPath := filepath.Join(dir, "tagged.yaml")
	if err := os.WriteFile(taggedPath, []byte(tagged), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := plan.LoadMapping(taggedPath)
	if err != nil {
		t.Fatalf("AI-prefilled draft must load after (optional) edits: %v", err)
	}
	if m.Endpoints[0].Name != "GetNavHistory" {
		t.Errorf("endpoint name = %q", m.Endpoints[0].Name)
	}
}

// TestDiscoverDeterministicNames pins the deterministic mode: drafts come
// pre-filled with Go-legal names/routes derived from the strongest semantic
// token source (cursor name → response fields → condition index), and the
// draft loads through the strict loader once module/readDBs are filled —
// names never have to be typed unless the user wants different ones.
func TestDiscoverDeterministicNames(t *testing.T) {
	f, err := ir.ExtractFileOpts(navFixture, ir.Options{})
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(navFixture)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := scanner.ScanBytes(src, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	tree := flow.Build(src, facts, f.Entry, f)
	candidates := flow.Discover(tree, f.Conditions)
	suggestions := map[string]aiSuggestion{}
	for _, c := range candidates {
		suggestions[c.Key] = deterministicSuggestion(c)
	}
	draft := renderDraft(f, candidates, false, suggestions)
	if !strings.Contains(draft, "# deterministic — edit freely") {
		t.Errorf("deterministic origin comment missing:\n%s", draft)
	}
	if strings.Contains(draft, `name: ""`) {
		t.Errorf("deterministic mode must not leave empty names:\n%s", draft)
	}
	// Names must be unique, exported Go identifiers (loader-legal).
	loaded := strings.Replace(draft, "# module: your-app/", "module: your-app/", 1)
	path := filepath.Join(t.TempDir(), "det.yaml")
	if err := os.WriteFile(path, []byte(loaded), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := plan.LoadMapping(path)
	if err != nil {
		t.Fatalf("deterministic draft must load with only module filled: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range m.Endpoints {
		if seen[e.Name] {
			t.Errorf("duplicate deterministic name %q", e.Name)
		}
		seen[e.Name] = true
	}
}
