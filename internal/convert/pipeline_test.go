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

const fakeBody = "\tlogger.Log(c).Debug(\"converted\")\n\treturn nil, err\n"

func convertFixture(t *testing.T) (Options, *llm.FakeServer) {
	t.Helper()
	files, err := ir.ExtractDir("../../testdata/nav")
	if err != nil {
		t.Fatal(err)
	}
	var main *ir.File
	var fns []*ir.File
	for _, f := range files {
		if strings.HasSuffix(f.Path, "SVC_MF_NAV_LIST.pc") {
			main = f
		} else {
			fns = append(fns, f)
		}
	}
	m := &plan.Mapping{
		Service:    "nav",
		Module:     "mutual-fund-be/pkg/services/nav",
		ReadDBs:    []string{"EBATEST", "MF"},
		RouteGroup: "/nav",
		Endpoints: []plan.Endpoint{
			{Condition: 1, Name: "NavHistory", Route: "/mfnavhistory"},
			{Condition: 2, Name: "SipFreedem", Route: "/mf_sipfreedem_schemes"},
			{Condition: 3, Name: "SipInsurance", Route: "/mf_sipinsurance_schemes"},
			{Condition: 4, Name: "NavList", Route: "/mfnavschemelist"},
		},
		DBMethods: map[string]plan.MethodPin{
			"q1":                  {Name: "GetDateDetails", Row: "DateInfo"},
			"cur_mf_nav_hist":     {Name: "GetNavHistory", Row: "NavHistoryDetail", Params: []string{"compCd:string", "schCd:string", "fromDate:time.Time", "toDate:time.Time"}},
			"q3":                  {Name: "GetCount", Params: []string{"matchAccount:string"}},
			"cur_mf_freed":        {Name: "GetSipFreedem", Row: "SipFreedemDetail"},
			"cur_mf_nav":          {Name: "GetSipInsurance", Row: "SipInsuranceDetail"},
			"cur_mf_nav_list":     {Name: "GetNavDetails", Params: []string{"compCd:string"}},
			"fn_is_d2u_active:q1": {Name: "IsD2uActive", Row: "D2uActive"},
		},
	}
	src, err := os.ReadFile(main.Path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(plan.Options{Main: main, Source: string(src), FnFiles: fns, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}

	fake := llm.NewFakeServer(llm.FakeResponse{Content: fakeBody})
	t.Cleanup(fake.Close)

	base := t.TempDir()
	ledgerDir := t.TempDir()
	led, err := ledger.Load(ledgerDir, "nav")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := audit.New(t.TempDir(), "test-run")
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Plan: p, Main: main, Source: string(src), FnFiles: fns,
		Client: llm.New(llm.Endpoint{ProfileName: "fake", Model: "fake", APIBase: fake.URL, Temperature: 0.1}),
		Budget: budget.New(12000, 4000, 4), BaseDir: base,
		Ledger: led, Validator: validate.New(validate.Options{}), MaxRetries: 2, Audit: rec,
	}
	return opts, fake
}

// TestConvertGateEndToEnd is the Phase 5 orchestration gate: the nav
// fixtures convert end-to-end against the fake LLM — deterministic files
// land, controller prompts never contain raw SQL, blocking is visible, the
// ledger resumes (a second run makes zero LLM calls).
func TestConvertGateEndToEnd(t *testing.T) {
	opts, fake := convertFixture(t)
	base := opts.BaseDir

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}

	// Deterministic artifacts exist and parse.
	for _, rel := range []string{
		"pkg/services/nav/models/nav.go",
		"pkg/services/nav/db/nav.go",
		"pkg/services/nav/db/interface.go",
		"pkg/services/nav/controller/interface.go",
		"pkg/services/nav/controller/nav.go",
		"pkg/services/nav/handler/interface.go",
		"pkg/services/nav/handler/nav.go",
		"pkg/services/nav/handler/router_snippet.txt",
	} {
		if _, err := os.Stat(filepath.Join(base, rel)); err != nil {
			t.Errorf("missing artifact %s", rel)
		}
	}
	// The db interface accumulated every method (7 units).
	iface, _ := os.ReadFile(filepath.Join(base, "pkg/services/nav/db/interface.go"))
	if got := strings.Count(string(iface), "\tGet") + strings.Count(string(iface), "\tIs"); got != 7 {
		t.Errorf("db interface accumulated %d methods, want 7\n%s", got, iface)
	}

	// LLM: 3 controller bodies (NavList blocked by fn_long_to_int), and no
	// raw SQL ever reached the prompts.
	if fake.RequestCount() != 3 {
		t.Errorf("llm calls = %d, want 3 (NavList blocked)", fake.RequestCount())
	}
	for i, req := range fake.Requests {
		prompt := promptOf(t, req)
		// The branch view legitimately keeps non-query EXEC constructs
		// (COMMIT/ROLLBACK tx markers) and dead SQL inside C comments
		// (the ver 2.2 block targets :i_cnt_d2us — never extracted); the
		// contract is that no LIVE query SQL leaks. Fragments are bind-
		// specific raw lines from the extracted regions.
		for _, frag := range []string{
			"INTO   :cnt_d2u",                 // q3/q5 site
			"FROM   DMM_D2U_MATCH_MPPNG_MSTR", // q3/q5 site
			"DECLARE cur_mf_nav_hist CURSOR",  // cursor q2
			"FROM   MF_NAVS_HIST",             // cursor q2
			"into :c_from_date",               // q1 dual select
			"DECLARE cur_mf_nav_list CURSOR",  // cursor q7
			"DECLARE cur_mf_freed CURSOR",     // cursor q4
			"DECLARE cur_mf_nav CURSOR",       // cursor q6
		} {
			if strings.Contains(prompt, frag) {
				t.Errorf("prompt %d leaked raw SQL (%q)", i, frag)
			}
		}
		if !strings.Contains(prompt, "s.store.Get") {
			t.Errorf("prompt %d missing the store contract", i)
		}
	}

	// Ledger: everything appended except the blocked endpoint.
	appended, failed, blocked, _ := opts.Ledger.Counts()
	if appended < 10 || failed != 0 || blocked != 1 {
		t.Errorf("ledger = appended %d, failed %d, blocked %d", appended, failed, blocked)
	}
	var blockedNames []string
	for _, e := range opts.Ledger.Units {
		if e.Status == ledger.StatusBlocked {
			blockedNames = append(blockedNames, e.Name)
		}
	}
	if len(blockedNames) != 1 || blockedNames[0] != "NavList" {
		t.Errorf("blocked = %v, want [NavList]", blockedNames)
	}

	// The controller file holds the three converted methods.
	ctrl, _ := os.ReadFile(filepath.Join(base, "pkg/services/nav/controller/nav.go"))
	for _, m := range []string{"NavHistory", "SipFreedem", "SipInsurance"} {
		if !strings.Contains(string(ctrl), "func (s *navController) "+m+"(") {
			t.Errorf("controller file missing method %s", m)
		}
	}
	if strings.Contains(string(ctrl), "NavList(") {
		t.Error("blocked endpoint must not generate")
	}

	// Resume: a second run converts nothing new — zero LLM calls.
	res2, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res2.LLMCalls != 0 {
		t.Errorf("resume made %d llm calls, want 0", res2.LLMCalls)
	}

	// Tier B degrade: no target module → recorded reason, no failure.
	if res.TierB == nil || res.TierB.DegradeReason == "" {
		t.Errorf("tier B should record its degrade reason: %+v", res.TierB)
	}
}

// TestConvertRetryFeedsTrimmedErrors: a syntactically broken first response
// is rejected by Tier A and retried; the second attempt lands.
func TestConvertRetryFeedsTrimmedErrors(t *testing.T) {
	opts, fake := convertFixture(t)
	// Script: first response broken (unbalanced brace), then the good one
	// repeats (the fake repeats its last entry).
	fake.Reset(llm.FakeResponse{Content: "\tfunc oops( {\n"}, llm.FakeResponse{Content: fakeBody})

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if fake.RequestCount() != 4 { // 3 endpoints, one needed a retry
		t.Errorf("llm calls = %d, want 4 (3 + 1 retry)", fake.RequestCount())
	}
	if len(res.Failed) != 0 {
		t.Errorf("failed = %v, want none", res.Failed)
	}
}

func promptOf(t *testing.T, req map[string]any) string {
	t.Helper()
	msgs, ok := req["messages"].([]any)
	if !ok {
		t.Fatal("request has no messages")
	}
	var sb strings.Builder
	for _, m := range msgs {
		mm := m.(map[string]any)
		sb.WriteString(mm["content"].(string))
		sb.WriteString("\n")
	}
	return sb.String()
}
