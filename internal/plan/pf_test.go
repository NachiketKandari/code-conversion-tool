package plan

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
)

// tpMapping maps both conditions of the synthetic tpcall fixture.
func tpMapping() *Mapping {
	return &Mapping{
		Service:    "tpdemo",
		Module:     "mutual-fund-be/pkg/services/tpdemo",
		ReadDBs:    []string{"MF"},
		RouteGroup: "/tpdemo",
		Endpoints: []Endpoint{
			{Condition: 1, Name: "TpDetail", Route: "/tpdetail"},
			{Condition: 2, Name: "TpDefault", Route: "/tpdefault"},
		},
	}
}

func tpOptions(t *testing.T) Options {
	t.Helper()
	main, err := ir.ExtractFile("../../testdata/pf/SVC_TP_DEMO.pc")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(main.Path)
	if err != nil {
		t.Fatal(err)
	}
	return Options{Main: main, Source: string(src), Mapping: tpMapping(), Budget: budget.New(12000, 4000, 4)}
}

// TestTPCallPlanUnits pins PF-4.4: every mapped endpoint's tpcall site
// becomes exactly one KindTPCall unit carrying its full TPCall contract,
// named after the outbound service.
func TestTPCallPlanUnits(t *testing.T) {
	p, err := Build(tpOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	var tpUnits []Unit
	for _, u := range p.Units {
		if u.Kind == KindTPCall {
			tpUnits = append(tpUnits, u)
		}
	}
	if len(tpUnits) != 1 {
		t.Fatalf("tpcall units = %d, want 1: %+v", len(tpUnits), tpUnits)
	}
	u := tpUnits[0]
	if u.Name != "TPCallSvcDemoDetail" {
		t.Errorf("unit name = %q, want TPCallSvcDemoDetail", u.Name)
	}
	if u.TP == nil || u.TP.Service != "SVC_DEMO_DETAIL" || u.TP.Ambiguous {
		t.Errorf("unit contract = %+v", u.TP)
	}
	if u.LLM {
		t.Error("tpcall units render deterministically — llm must be false")
	}
	if !strings.HasSuffix(u.TargetPath, "controller/tpcall_placeholders.go") {
		t.Errorf("target = %q", u.TargetPath)
	}
	if !strings.Contains(strings.Join(u.Deps, ","), "u") {
		t.Errorf("deps = %v, want the controller interface", u.Deps)
	}
	if u.SourceLines != "35-40" {
		t.Errorf("source lines = %q, want 35-40", u.SourceLines)
	}

	// Determinism: identical inputs → byte-identical JSON.
	var b1, b2 bytes.Buffer
	if err := WriteJSON(&b1, p); err != nil {
		t.Fatal(err)
	}
	p2, err := Build(tpOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&b2, p2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Error("tpcall plan is not deterministic")
	}
}

// TestTPCallUnmappedIsRecordedSkip: a tpcall outside every mapped condition
// is a recorded skip — never silently dropped (§4.2.8).
func TestTPCallUnmappedIsRecordedSkip(t *testing.T) {
	opts := tpOptions(t)
	m := tpMapping()
	// Map only condition 2 (the else branch) — condition 1's tpcall skips.
	m.Endpoints = m.Endpoints[1:]
	opts.Mapping = m
	p, err := Build(opts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range p.Skipped {
		if s.QueryID == "tpcall:SVC_DEMO_DETAIL" {
			found = true
		}
	}
	if !found {
		t.Errorf("skips = %v, want the tpcall recorded", p.Skipped)
	}
	for _, u := range p.Units {
		if u.Kind == KindTPCall {
			t.Errorf("unmapped tpcall produced a unit: %+v", u)
		}
	}
}

// TestFragmentPlanGate pins PF-3.4 on the fragment fixture: the standard
// mapping applies (condition numbers reference the fragment's own chain),
// the taxonomy matches a full file, and the minimal two-endpoint mapping
// builds without special cases.
func TestFragmentPlanGate(t *testing.T) {
	m := &Mapping{
		Service:    "navslice",
		Module:     "mutual-fund-be/pkg/services/navslice",
		ReadDBs:    []string{"MF"},
		RouteGroup: "/navslice",
		Endpoints: []Endpoint{
			{Condition: 1, Name: "NavHistory", Route: "/mfnavhistory"},
			{Condition: 2, Name: "NavDefault", Route: "/mfnavdefault"},
		},
		DBMethods: map[string]MethodPin{
			"cur_demo": {Name: "GetNavSlice", Row: "NavSliceRow"},
		},
	}
	main, err := ir.ExtractFile("../../testdata/pf/fragment_nav_slice.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !main.Fragment || main.Entry != "__fragment" {
		t.Fatalf("fragment IR = %v entry %q", main.Fragment, main.Entry)
	}
	src, err := os.ReadFile(main.Path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(Options{Main: main, Source: string(src), Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}
	var db, ctrl, handler int
	for _, u := range p.Units {
		switch u.Kind {
		case KindDBMethod:
			db++
		case KindControllerMethod:
			ctrl++
		case KindHandlerMethod:
			handler++
		}
	}
	if db != 1 || ctrl != 2 || handler != 2 {
		t.Errorf("units = %d db/%d ctrl/%d handler, want 1/2/2", db, ctrl, handler)
	}
	if len(p.Blockers) != 0 || len(p.Skipped) != 0 || len(p.Orphans) != 0 {
		t.Errorf("plan noise = blockers %v skipped %v orphans %v", p.Blockers, p.Skipped, p.Orphans)
	}
	// The db unit slices the fragment file's own lines.
	var dbUnit *Unit
	for i := range p.Units {
		if p.Units[i].Kind == KindDBMethod {
			dbUnit = &p.Units[i]
		}
	}
	if dbUnit == nil || !strings.HasSuffix(dbUnit.SourceFile, "fragment_nav_slice.txt") || dbUnit.SourceLines != "15-24" {
		t.Errorf("db unit = %+v", dbUnit)
	}
}
