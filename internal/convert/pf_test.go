package convert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/ledger"
	"github.com/Public/convert-tux-to-go/internal/llm"
	"github.com/Public/convert-tux-to-go/internal/plan"
	"github.com/Public/convert-tux-to-go/internal/validate"
)

// pfFixture builds convert options over a single-file IR with a mapping —
// the shared shape of the PF-3/PF-4 end-to-end gates.
func pfFixture(t *testing.T, pcPath, service string, m *plan.Mapping) (Options, *llm.FakeServer) {
	t.Helper()
	main, err := ir.ExtractFile(pcPath)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(main.Path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(plan.Options{Main: main, Source: string(src), Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}
	fake := llm.NewFakeServer(llm.FakeResponse{Content: fakeBody})
	t.Cleanup(fake.Close)

	led, err := ledger.Load(t.TempDir(), service)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := audit.New(t.TempDir(), "pf-run")
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Plan: p, Main: main, Source: string(src),
		Client: llm.New(llm.Endpoint{ProfileName: "fake", Model: "fake", APIBase: fake.URL, Temperature: 0.1}),
		Budget: budget.New(12000, 4000, 4), BaseDir: t.TempDir(),
		Ledger: led, Validator: validate.New(validate.Options{}), MaxRetries: 2, Audit: rec,
	}
	return opts, fake
}

// TestConvertFragmentGate is the PF-3.5/3.6 gate: a lone if/else block with
// one embedded cursor + FML calls converts end-to-end under the standard
// gates — Tier A clean, SQL-free controller prompts, same artifacts and
// ledger lifecycle as a full file, provenance marked by the fragment file.
func TestConvertFragmentGate(t *testing.T) {
	m := &plan.Mapping{
		Service:    "navslice",
		Module:     "mutual-fund-be/pkg/services/navslice",
		ReadDBs:    []string{"MF"},
		RouteGroup: "/navslice",
		Endpoints: []plan.Endpoint{
			{Condition: 1, Name: "NavHistory", Route: "/mfnavhistory"},
			{Condition: 2, Name: "NavDefault", Route: "/mfnavdefault"},
		},
		DBMethods: map[string]plan.MethodPin{
			"cur_demo": {Name: "GetNavSlice", Row: "NavSliceRow", Params: []string{"compCd:string"}},
		},
	}
	opts, fake := pfFixture(t, "../../testdata/pf/fragment_nav_slice.txt", "navslice", m)

	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("fragment conversion failed: %v", err)
	}
	if fake.RequestCount() != 2 {
		t.Errorf("llm calls = %d, want 2 (one per branch)", fake.RequestCount())
	}

	// Same artifact taxonomy as a full file; every .go artifact is Tier A
	// clean (Run validates on write — reaching here means all parse).
	base := opts.BaseDir
	for _, rel := range []string{
		"pkg/services/navslice/models/navslice.go",
		"pkg/services/navslice/db/navslice.go",
		"pkg/services/navslice/db/interface.go",
		"pkg/services/navslice/controller/navslice.go",
		"pkg/services/navslice/handler/navslice.go",
		"pkg/services/navslice/handler/router_snippet.txt",
	} {
		if _, err := os.Stat(filepath.Join(base, rel)); err != nil {
			t.Errorf("missing artifact %s", rel)
		}
	}

	// SQL-free prompts (the query-replaced view), store contract included.
	for i, req := range fake.Requests {
		prompt := promptOf(t, req)
		for _, frag := range []string{"DECLARE cur_demo", "FROM DEMO_PRICE_HIST", "INTO :sql_nav_date"} {
			if strings.Contains(prompt, frag) {
				t.Errorf("fragment prompt %d leaked raw SQL (%q)", i, frag)
			}
		}
		if !strings.Contains(prompt, "s.store.GetNavSlice") {
			t.Errorf("fragment prompt %d missing the store contract", i)
		}
	}

	// Ledger lifecycle identical to a full file; no placeholders.
	appended, failed, blocked, _, placeholders := opts.Ledger.Counts()
	if appended < 6 || failed != 0 || blocked != 0 || placeholders != 0 {
		t.Errorf("ledger = appended %d failed %d blocked %d placeholders %d", appended, failed, blocked, placeholders)
	}
}

// TestConvertTPCallPlaceholderGate is the PF-4 gate: a synthetic tpcall
// fixture (send block → tpcall → recv block) yields a placeholder that
// compiles, carries the complete send/recv contract in its marker, and
// reports exactly 1 placeholder in the ledger.
func TestConvertTPCallPlaceholderGate(t *testing.T) {
	m := &plan.Mapping{
		Service:    "tpdemo",
		Module:     "mutual-fund-be/pkg/services/tpdemo",
		ReadDBs:    []string{"MF"},
		RouteGroup: "/tpdemo",
		Endpoints: []plan.Endpoint{
			{Condition: 1, Name: "TpDetail", Route: "/tpdetail"},
			{Condition: 2, Name: "TpDefault", Route: "/tpdefault"},
		},
	}
	opts, _ := pfFixture(t, "../../testdata/pf/SVC_TP_DEMO.pc", "tpdemo", m)

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("tpcall conversion failed: %v", err)
	}

	// The run reports placeholders as a first-class count.
	if len(res.Placeholders) != 1 || res.Placeholders[0] != "TPCallSvcDemoDetail" {
		t.Errorf("placeholders = %v, want [TPCallSvcDemoDetail]", res.Placeholders)
	}
	_, _, _, _, placeholders := opts.Ledger.Counts()
	if placeholders != 1 {
		t.Errorf("ledger placeholders = %d, want 1", placeholders)
	}

	// The stub file parses (Tier A passed on write), is greppable via
	// tuxgo:TODO, and carries the complete contract.
	ph, err := os.ReadFile(filepath.Join(opts.BaseDir, "pkg/services/tpdemo/controller/tpcall_placeholders.go"))
	if err != nil {
		t.Fatal(err)
	}
	phStr := string(ph)
	for _, want := range []string{
		"tuxgo:TODO tp:SVC_DEMO_DETAIL",
		"// send: FML_COMP_CD, FML_SCHEME_CD",
		"// recv: FML_NAV_DATE, FML_NAV_NAV",
		"func TPCallSvcDemoDetail(send map[string]string) (recv map[string]string, err error)",
		"return nil, errPlaceholder",
	} {
		if !strings.Contains(phStr, want) {
			t.Errorf("placeholder file missing %q", want)
		}
	}

	// Resume: the second run renders nothing new — placeholders stay.
	res2, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Placeholders) != 0 {
		t.Errorf("resume re-rendered placeholders: %v", res2.Placeholders)
	}
	_, _, _, _, ph2 := opts.Ledger.Counts()
	if ph2 != 1 {
		t.Errorf("resume placeholders = %d, want 1", ph2)
	}
}

// TestConvertTPCallPromptContract: the endpoint whose branch contains the
// tpcall sees the placeholder signature in its prompt contract; endpoints
// without tpcalls keep prompts free of the section.
func TestConvertTPCallPromptContract(t *testing.T) {
	m := &plan.Mapping{
		Service:    "tpdemo",
		Module:     "mutual-fund-be/pkg/services/tpdemo",
		ReadDBs:    []string{"MF"},
		RouteGroup: "/tpdemo",
		Endpoints: []plan.Endpoint{
			{Condition: 1, Name: "TpDetail", Route: "/tpdetail"},
			{Condition: 2, Name: "TpDefault", Route: "/tpdefault"},
		},
	}
	opts, fake := pfFixture(t, "../../testdata/pf/SVC_TP_DEMO.pc", "tpdemo", m)

	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	sawSection := 0
	for _, req := range fake.Requests {
		prompt := promptOf(t, req)
		has := strings.Contains(prompt, "TPCallSvcDemoDetail(send map[string]string) (recv map[string]string, error)")
		if strings.Contains(prompt, "TpDetail") && has {
			sawSection++
		}
		if strings.Contains(prompt, "TpDefault") && has {
			t.Error("endpoint without tpcalls saw the placeholder section")
		}
	}
	if sawSection != 1 {
		t.Errorf("placeholder section seen %d times, want exactly the TpDetail endpoint", sawSection)
	}
}
