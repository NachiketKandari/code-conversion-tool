package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/config"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/plan"
)

// svcEntrySrc is the minimal .pc text the scanner recognizes as a Tuxedo
// entry function (any SVC_* function definition marks the file an entry).
const svcEntrySrc = `void %s(TPSVCINFO* rqst)
{
    userlog("%s");
}
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestExtractPlanIRPartitioning pins the dir-mode partitioning contract:
// every SVC_* file is an entry (one service each), helpers ride along, and
// file mode stays single-service.
func TestExtractPlanIRPartitioning(t *testing.T) {
	cfg := config.Default()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "SVC_ALPHA.pc"), fmt.Sprintf(svcEntrySrc, "SVC_ALPHA", "alpha"))
	writeFile(t, filepath.Join(dir, "SVC_BETA.pc"), fmt.Sprintf(svcEntrySrc, "SVC_BETA", "beta"))
	writeFile(t, filepath.Join(dir, "fn_helper.pc"), "long fn_helper(void)\n{\n    return 1;\n}\n")

	files, mains, _, err := extractPlanIR(context.Background(), dir, cfg, false)
	if err != nil {
		t.Fatalf("extractPlanIR(dir): %v", err)
	}
	if len(mains) != 2 {
		t.Fatalf("mains = %d, want 2", len(mains))
	}
	if len(files) != 3 {
		t.Errorf("files = %d, want 3 (entries + helper)", len(files))
	}
	if mains[0].Entry != "SVC_ALPHA" || mains[1].Entry != "SVC_BETA" {
		t.Errorf("entries = %s, %s; want SVC_ALPHA, SVC_BETA", mains[0].Entry, mains[1].Entry)
	}

	// No-entry dir stays a hard error.
	empty := t.TempDir()
	writeFile(t, filepath.Join(empty, "fn_only.pc"), "long fn_only(void)\n{\n    return 1;\n}\n")
	if _, _, _, err := extractPlanIR(context.Background(), empty, cfg, false); err == nil {
		t.Error("expected no-entry dir to error")
	}

	// File mode: exactly one service.
	file := filepath.Join(dir, "SVC_ALPHA.pc")
	files, mains, _, err = extractPlanIR(context.Background(), file, cfg, false)
	if err != nil {
		t.Fatalf("extractPlanIR(file): %v", err)
	}
	if len(mains) != 1 || mains[0].Entry != "SVC_ALPHA" || len(files) != 1 {
		t.Errorf("file mode: mains=%d files=%d entry=%s; want 1/1/SVC_ALPHA", len(mains), len(files), mains[0].Entry)
	}
}

func navEndpoints() []plan.Endpoint {
	return []plan.Endpoint{
		{Condition: 1, Name: "NavHistory", Route: "/mfnavhistory"},
		{Condition: 2, Name: "SipFreedem", Route: "/mf_sipfreedem_schemes"},
		{Condition: 3, Name: "SipInsurance", Route: "/mf_sipinsurance_schemes"},
		{Condition: 4, Name: "NavList", Route: "/mfnavschemelist"},
	}
}

func navDBMethods() map[string]plan.MethodPin {
	return map[string]plan.MethodPin{
		"q1":                   {Name: "GetDateDetails", Row: "DateInfo"},
		"cur_demo_hist":        {Name: "GetNavHistory", Row: "NavHistoryDetail", Params: []string{"compCd:string", "schCd:string", "fromDate:time.Time", "toDate:time.Time"}},
		"q3":                   {Name: "GetCount", Params: []string{"matchAccount:string"}},
		"cur_demo_featured":    {Name: "GetSipFreedem", Row: "SipFreedemDetail"},
		"cur_demo_insured":     {Name: "GetSipInsurance", Row: "SipInsuranceDetail"},
		"cur_demo_list":        {Name: "GetNavDetails", Params: []string{"compCd:string"}},
		"fn_is_demo_active:q1": {Name: "IsDemoActive", Row: "DemoActive"},
	}
}

// mappingYAML renders one service's mapping file for the fan-out corpus.
func mappingYAML(source, service string) string {
	return fmt.Sprintf(`source: %s
service: %s
module: mutual-fund-be/pkg/services/%s
readDBs: [EBATEST, MF]
routeGroup: /%s
endpoints:
  - condition: 1
    name: NavHistory
    route: /mfnavhistory
  - condition: 2
    name: SipFreedem
    route: /mf_sipfreedem_schemes
  - condition: 3
    name: SipInsurance
    route: /mf_sipinsurance_schemes
  - condition: 4
    name: NavList
    route: /mfnavschemelist
dbMethods:
  q1: {name: GetDateDetails, row: DateInfo}
  cur_demo_hist: {name: GetNavHistory, row: NavHistoryDetail, params: [compCd:string, schCd:string, fromDate:time.Time, toDate:time.Time]}
  q3: {name: GetCount, params: [matchAccount:string]}
  cur_demo_featured: {name: GetSipFreedem, row: SipFreedemDetail}
  cur_demo_insured: {name: GetSipInsurance, row: SipInsuranceDetail}
  cur_demo_list: {name: GetNavDetails, params: [compCd:string]}
  fn_is_demo_active:q1: {name: IsDemoActive, row: DemoActive}
`, source, service, service, service)
}

func TestLoadMappingDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "alpha.yaml"), mappingYAML("SVC_DEMO_LIST.pc", "alpha"))
	writeFile(t, filepath.Join(dir, "beta.yaml"), mappingYAML("SVC_DEMO_TWO.pc", "beta"))

	bySource, err := loadMappingDir(dir, false)
	if err != nil {
		t.Fatalf("loadMappingDir: %v", err)
	}
	if len(bySource) != 2 {
		t.Fatalf("loaded %d mappings, want 2", len(bySource))
	}
	if m := bySource["svc_demo_list.pc"].mapping; m == nil || m.Service != "alpha" {
		t.Errorf("svc_demo_list.pc mapping = %+v, want service alpha", bySource["svc_demo_list.pc"])
	}

	// Missing source field is an error in dir mode.
	bad := t.TempDir()
	writeFile(t, filepath.Join(bad, "nosource.yaml"), "service: x\nmodule: m/pkg/services/x\nendpoints:\n  - condition: 1\n    name: A\n    route: /a\n")
	if _, err := loadMappingDir(bad, false); err == nil || !strings.Contains(err.Error(), "missing source") {
		t.Errorf("missing source: err = %v, want 'missing source'", err)
	}

	// Two mappings claiming one entry is an error.
	dup := t.TempDir()
	writeFile(t, filepath.Join(dup, "one.yaml"), mappingYAML("SVC_DEMO_LIST.pc", "alpha"))
	writeFile(t, filepath.Join(dup, "two.yaml"), mappingYAML("svc_demo_list.pc", "beta"))
	if _, err := loadMappingDir(dup, false); err == nil || !strings.Contains(err.Error(), "both declare source") {
		t.Errorf("duplicate source: err = %v, want 'both declare source'", err)
	}

	// Empty mapping dir is an error.
	empty := t.TempDir()
	if _, err := loadMappingDir(empty, false); err == nil {
		t.Error("expected empty mapping dir to error")
	}
}

func TestMatchMappingsStrict(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "alpha.yaml"), mappingYAML("SVC_DEMO_LIST.pc", "alpha"))
	writeFile(t, filepath.Join(dir, "beta.yaml"), mappingYAML("SVC_DEMO_TWO.pc", "beta"))
	bySource, err := loadMappingDir(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	mains := []*ir.File{
		{Path: filepath.Join("corpus", "SVC_DEMO_LIST.pc"), Entry: "SVC_DEMO_LIST"},
		{Path: filepath.Join("corpus", "SVC_DEMO_TWO.pc"), Entry: "SVC_DEMO_TWO"},
	}

	matched, err := matchMappings(context.Background(), "corpus", mains, bySource, nil, false)
	if err != nil {
		t.Fatalf("matchMappings: %v", err)
	}
	if len(matched) != 2 || matched[0].Service != "alpha" || matched[1].Service != "beta" {
		t.Errorf("matched = %v, %v; want alpha, beta in input order", matched[0].Service, matched[1].Service)
	}

	// A mapping whose source names no entry is drift — surfaced, not skipped.
	_, err = matchMappings(context.Background(), "corpus", mains[:1], bySource, nil, false)
	if err == nil || !strings.Contains(err.Error(), "no entry file") {
		t.Errorf("orphan mappings: err = %v, want 'no entry file'", err)
	}

	// ...unless the source was excluded by convert.fileFilter — a deliberate
	// skip, never orphan drift.
	_, err = matchMappings(context.Background(), "corpus", mains[:1], bySource, []string{"svc_demo_two.pc"}, false)
	if err != nil {
		t.Errorf("filter-excluded mapping: err = %v, want a skip", err)
	}

	// An entry without a mapping is a hard error (endpoints are user data).
	delete(bySource, "svc_demo_two.pc")
	_, err = matchMappings(context.Background(), "corpus", mains, bySource, nil, false)
	if err == nil || !strings.Contains(err.Error(), "no mapping for entry") {
		t.Errorf("unmapped entry: err = %v, want 'no mapping for entry'", err)
	}
}

// TestMappingDirLenient pins the convention-dir posture: foreign drafts
// (no source, older formats) are skipped, unmatched sources are normal
// local state, and validation of a matched winner is deferred to
// matchMappings — an untagged draft surfaces there, named.
func TestMappingDirLenient(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "foreign.mapping.yaml"),
		"service: svc_demo_list\nendpoints:\n  - condition: 1\n    name: \"\"\n    route: \"\"\n") // stale unfilled draft, no source
	writeFile(t, filepath.Join(dir, "alpha.yaml"), mappingYAML("SVC_DEMO_LIST.pc", "alpha"))

	bySource, err := loadMappingDir(dir, true)
	if err != nil {
		t.Fatalf("loadMappingDir lenient: %v", err)
	}
	if len(bySource) != 1 || bySource["svc_demo_list.pc"].mapping != nil {
		t.Fatalf("lenient load = %+v, want one deferred (unvalidated) source", bySource)
	}

	mains := []*ir.File{{Path: filepath.Join("corpus", "SVC_DEMO_LIST.pc"), Entry: "SVC_DEMO_LIST"}}
	matched, err := matchMappings(context.Background(), "corpus", mains, bySource, nil, true)
	if err != nil {
		t.Fatalf("matchMappings lenient: %v", err)
	}
	if len(matched) != 1 || matched[0].Service != "alpha" {
		t.Errorf("matched = %v, want alpha", matched)
	}

	// A matching but untagged draft surfaces at match time, named.
	untagged := t.TempDir()
	writeFile(t, filepath.Join(untagged, "SVC_DEMO_LIST.mapping.yaml"),
		"source: SVC_DEMO_LIST.pc\nservice: svc_demo_list\nendpoints:\n  - condition: 1\n    name: \"\"\n    route: \"\"\n")
	bad, err := loadMappingDir(untagged, true)
	if err != nil {
		t.Fatal(err)
	}
	ms := bad["svc_demo_list.pc"]
	ms.mapping = nil
	bad["svc_demo_list.pc"] = ms
	if _, err := matchMappings(context.Background(), "corpus", mains, bad, nil, true); err == nil || !strings.Contains(err.Error(), "SVC_DEMO_LIST.mapping.yaml") {
		t.Errorf("untagged match: err = %v, want the draft file named", err)
	}
}

// convertCorpus mirrors the nav demo: two entry services (the golden demo
// service copied under two names) plus the shared helper library.
func convertCorpus(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	list, err := os.ReadFile(filepath.Join("..", "..", "testdata", "nav", "SVC_DEMO_LIST.pc"))
	if err != nil {
		t.Fatalf("read testdata/nav/SVC_DEMO_LIST.pc: %v", err)
	}
	lib, err := os.ReadFile(filepath.Join("..", "..", "testdata", "nav", "fn_demo_lib.pc"))
	if err != nil {
		t.Fatalf("read testdata/nav/fn_demo_lib.pc: %v", err)
	}
	writeFile(t, filepath.Join(dir, "SVC_DEMO_LIST.pc"), string(list))
	writeFile(t, filepath.Join(dir, "SVC_DEMO_TWO.pc"), string(list))
	writeFile(t, filepath.Join(dir, "fn_demo_lib.pc"), string(lib))
	return dir
}

func writeFanoutConfig(t *testing.T, workers int, staged, ledger string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".tuxgo.yaml")
	writeFile(t, path, fmt.Sprintf(`run:
  llm: false
concurrency:
  workers: %d
validate:
  compile: never
paths:
  mainGo: ""
  ledger: %s
  state: %s
  staged: %s
`, workers, ledger, filepath.Join(t.TempDir(), "state"), staged))
	return path
}

// runFanout executes runConvert the way the CLI would.
func runFanout(t *testing.T, cfgPath, mappingDir, corpus string) error {
	t.Helper()
	args := []string{"-config", cfgPath, "-no-llm", "-mapping", mappingDir, corpus}
	return runConvert(context.Background(), args)
}

// treeSnapshot collects every file under root as relpath → content.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestConvertDirFanoutEndToEnd converts two services from one directory:
// workers=2 fans out one goroutine per service, each landing in its own
// output subtree with its own ledger — and the whole run is byte-identical
// to a workers=1 run (correctness never depends on concurrency).
func TestConvertDirFanoutEndToEnd(t *testing.T) {
	corpus := convertCorpus(t)
	mappingDir := t.TempDir()
	writeFile(t, filepath.Join(mappingDir, "alpha.yaml"), mappingYAML("SVC_DEMO_LIST.pc", "alpha"))
	writeFile(t, filepath.Join(mappingDir, "beta.yaml"), mappingYAML("SVC_DEMO_TWO.pc", "beta"))

	stagedA, ledgerA := filepath.Join(t.TempDir(), "staged"), filepath.Join(t.TempDir(), "ledger")
	if err := runFanout(t, writeFanoutConfig(t, 2, stagedA, ledgerA), mappingDir, corpus); err != nil {
		t.Fatalf("workers=2 run: %v", err)
	}

	// Both services landed in their own subtrees with their own ledgers.
	// (-no-llm leaves controller bodies pending, so only the controller
	// interface is deterministic output; the bodies arrive on an LLM run.)
	for _, svc := range []string{"alpha", "beta"} {
		for _, rel := range []string{
			fmt.Sprintf("%s/pkg/services/%s/models/%s.go", svc, svc, svc),
			fmt.Sprintf("%s/pkg/services/%s/db/%s.go", svc, svc, svc),
			fmt.Sprintf("%s/pkg/services/%s/db/interface.go", svc, svc),
			fmt.Sprintf("%s/pkg/services/%s/controller/interface.go", svc, svc),
			fmt.Sprintf("%s/pkg/services/%s/handler/interface.go", svc, svc),
			fmt.Sprintf("%s/pkg/services/%s/handler/%s.go", svc, svc, svc),
		} {
			if _, err := os.Stat(filepath.Join(stagedA, rel)); err != nil {
				t.Errorf("missing artifact %s: %v", rel, err)
			}
		}
		if _, err := os.Stat(filepath.Join(ledgerA, svc+".ledger.json")); err != nil {
			t.Errorf("missing ledger for %s: %v", svc, err)
		}
	}

	// workers=1 into a fresh tree: byte-identical output.
	stagedB, ledgerB := filepath.Join(t.TempDir(), "staged"), filepath.Join(t.TempDir(), "ledger")
	if err := runFanout(t, writeFanoutConfig(t, 1, stagedB, ledgerB), mappingDir, corpus); err != nil {
		t.Fatalf("workers=1 run: %v", err)
	}
	gotA, gotB := treeSnapshot(t, stagedA), treeSnapshot(t, stagedB)
	if len(gotA) == 0 {
		t.Fatal("workers=2 staged tree is empty")
	}
	for rel, content := range gotA {
		if gotB[rel] != content {
			t.Errorf("workers=1 vs workers=2 differ for %s", rel)
		}
	}
	for rel := range gotB {
		if _, ok := gotA[rel]; !ok {
			t.Errorf("workers=1 tree has extra file %s", rel)
		}
	}
}

// TestConvertFanoutCollisionGuardScopesPerService: a stale staged subtree
// left by a previous run hard-errors that service only — the sibling
// service (its own subtree, its own ledger) still converts.
func TestConvertFanoutCollisionGuardScopesPerService(t *testing.T) {
	corpus := convertCorpus(t)
	mappingDir := t.TempDir()
	writeFile(t, filepath.Join(mappingDir, "alpha.yaml"), mappingYAML("SVC_DEMO_LIST.pc", "alpha"))
	writeFile(t, filepath.Join(mappingDir, "beta.yaml"), mappingYAML("SVC_DEMO_TWO.pc", "beta"))

	staged := filepath.Join(t.TempDir(), "staged")
	probe := filepath.Join(staged, "alpha", "pkg", "services", "alpha", "models", "alpha.go")
	writeFile(t, probe, "package models\n")

	err := runFanout(t, writeFanoutConfig(t, 2, staged, filepath.Join(t.TempDir(), "ledger")), mappingDir, corpus)
	if err == nil || !strings.Contains(err.Error(), "ledger is fresh") {
		t.Fatalf("err = %v, want the staged-collision guard error", err)
	}

	// The guarded service's stale file is untouched; the sibling converted.
	data, rerr := os.ReadFile(probe)
	if rerr != nil || string(data) != "package models\n" {
		t.Errorf("probe file modified: %v %q", rerr, data)
	}
	if _, serr := os.Stat(filepath.Join(staged, "beta", "pkg", "services", "beta", "models", "beta.go")); serr != nil {
		t.Errorf("sibling service did not convert: %v", serr)
	}
}

// TestConvertFanoutRequiresMappingDir: multiple entries with a single
// mapping file is a hard error naming the fix.
func TestConvertFanoutRequiresMappingDir(t *testing.T) {
	corpus := convertCorpus(t)
	single := filepath.Join(t.TempDir(), "alpha.mapping.yaml")
	writeFile(t, single, mappingYAML("SVC_DEMO_LIST.pc", "alpha"))

	err := runFanout(t, writeFanoutConfig(t, 2, filepath.Join(t.TempDir(), "staged"), filepath.Join(t.TempDir(), "ledger")), single, corpus)
	if err == nil || !strings.Contains(err.Error(), "pass a mapping directory") {
		t.Fatalf("err = %v, want mapping-directory guidance", err)
	}
}

// TestConvertServiceSummariesConcurrent hammers the shared audit recorder
// path the fan-out uses (contract parity with the audit package's own race
// test) — run under -race in CI.
func TestConvertServiceSummariesConcurrent(t *testing.T) {
	rec, err := audit.New(t.TempDir(), "test-run")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := rec.Write(fmt.Sprintf("svc-%d.ledger.json", i), func(w io.Writer) error {
				_, werr := fmt.Fprintf(w, "{\"service\":%d}", i)
				return werr
			})
			if err != nil {
				t.Errorf("write %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}
