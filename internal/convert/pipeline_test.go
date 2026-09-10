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

// contractClient wraps the scripted fake so canned bodies satisfy the
// required-call contract (BP hardening parity with batchpy): any store call
// the prompt's branch view shows but the canned body omits is appended as a
// checked no-op assignment — Tier A only parses, so the shape stays valid.
type contractClient struct{ inner llm.Client }

func (c contractClient) Chat(ctx context.Context, req llm.ChatRequest) (llm.Response, error) {
	resp, err := c.inner.Chat(ctx, req)
	if err != nil {
		return resp, err
	}
	prompt := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			prompt += "\n" + m.Content
		}
	}
	extra := ""
	for _, call := range requiredCalls(prompt, "s.store.") {
		if !strings.Contains(resp.Content, call+"(") {
			extra += "\n\tif _, cerr := " + call + "(c); cerr != nil {\n\t\treturn nil, cerr\n\t}"
		}
	}
	resp.Content += extra
	return resp, nil
}

func (c contractClient) Stream(ctx context.Context, req llm.ChatRequest, onDelta func(string) error) (llm.Response, error) {
	return c.inner.Stream(ctx, req, onDelta)
}

func convertFixture(t *testing.T) (Options, *llm.FakeServer) {
	t.Helper()
	files, err := ir.ExtractDir("../../testdata/nav")
	if err != nil {
		t.Fatal(err)
	}
	var main *ir.File
	var fns []*ir.File
	for _, f := range files {
		if strings.HasSuffix(f.Path, "SVC_DEMO_LIST.pc") {
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
			"q1":                   {Name: "GetDateDetails", Row: "DateInfo"},
			"cur_demo_hist":        {Name: "GetNavHistory", Row: "NavHistoryDetail", Params: []string{"compCd:string", "schCd:string", "fromDate:time.Time", "toDate:time.Time"}},
			"q3":                   {Name: "GetCount", Params: []string{"matchAccount:string"}},
			"cur_demo_featured":    {Name: "GetSipFreedem", Row: "SipFreedemDetail"},
			"cur_demo_insured":     {Name: "GetSipInsurance", Row: "SipInsuranceDetail"},
			"cur_demo_list":        {Name: "GetNavDetails", Params: []string{"compCd:string"}},
			"fn_is_demo_active:q1": {Name: "IsDemoActive", Row: "DemoActive"},
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
		Client: contractClient{llm.New(llm.Endpoint{ProfileName: "fake", Model: "fake", APIBase: fake.URL, Temperature: 0.1})},
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
		"pkg/services/nav/controller/fnstubs.go",
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

	// LLM: all four controller bodies (fn_long_to_int rides the stub), and
	// no raw SQL ever reached the prompts.
	if fake.RequestCount() != 4 {
		t.Errorf("llm calls = %d, want 4 (stub-and-carry-on)", fake.RequestCount())
	}
	for i, req := range fake.Requests {
		prompt := promptOf(t, req)
		// The branch view legitimately keeps non-query EXEC constructs
		// (COMMIT/ROLLBACK tx markers) and dead SQL inside C comments
		// (the demo commented block targets :i_cnt_demos — never extracted); the
		// contract is that no LIVE query SQL leaks. Fragments are bind-
		// specific raw lines from the extracted regions.
		for _, frag := range []string{
			"INTO   :cnt_demo",                 // q3/q5 site
			"FROM   DEMO_ACCOUNT_MAP",          // q3/q5 site
			"DECLARE cur_demo_hist CURSOR",     // cursor q2
			"FROM   DEMO_PRICE_HIST",           // cursor q2
			"into :c_from_date",                // q1 dual select
			"DECLARE cur_demo_list CURSOR",     // cursor q7
			"DECLARE cur_demo_featured CURSOR", // cursor q4
			"DECLARE cur_demo_insured CURSOR",  // cursor q6
		} {
			if strings.Contains(prompt, frag) {
				t.Errorf("prompt %d leaked raw SQL (%q)", i, frag)
			}
		}
		if !strings.Contains(prompt, "s.store.Get") {
			t.Errorf("prompt %d missing the store contract", i)
		}
	}

	// Ledger: everything appended — no blocked units under the stub
	// policy, the unresolved fn carries a panicking stub instead.
	appended, failed, blocked, _, placeholders, deviated := opts.Ledger.Counts()
	if appended < 10 || failed != 0 || blocked != 0 || placeholders != 0 || deviated != 0 {
		t.Errorf("ledger = appended %d, failed %d, blocked %d, placeholders %d, deviated %d", appended, failed, blocked, placeholders, deviated)
	}
	if len(res.Stubs) != 1 || !strings.HasPrefix(res.Stubs[0], "fn_long_to_int") || !strings.Contains(res.Stubs[0], "NavList") {
		t.Errorf("stubs = %v, want fn_long_to_int → NavList", res.Stubs)
	}

	// The controller file holds the four converted methods, and the stub
	// file pins the panicking placeholder for the unresolved fn.
	ctrl, _ := os.ReadFile(filepath.Join(base, "pkg/services/nav/controller/nav.go"))
	for _, m := range []string{"NavHistory", "SipFreedem", "SipInsurance", "NavList"} {
		if !strings.Contains(string(ctrl), "func (s *navController) "+m+"(") {
			t.Errorf("controller file missing method %s", m)
		}
	}
	stubSrc, _ := os.ReadFile(filepath.Join(base, "pkg/services/nav/controller/fnstubs.go"))
	for _, want := range []string{"func fnLongToInt(args ...any) int", `panic("tuxgo: fn_long_to_int is undefined in the source corpus`} {
		if !strings.Contains(string(stubSrc), want) {
			t.Errorf("fnstubs.go missing %q\n---\n%s", want, stubSrc)
		}
	}
	// The NavList prompt must tell the model about the stub it can call.
	var navListPrompt string
	for _, req := range fake.Requests {
		if p := promptOf(t, req); strings.Contains(p, "Endpoint: NavList") {
			navListPrompt = p
			break
		}
	}
	if navListPrompt == "" {
		t.Fatal("no prompt requested the NavList body")
	}
	if !strings.Contains(navListPrompt, "Stubbed helpers") || !strings.Contains(navListPrompt, "fnLongToInt(args ...any) int") {
		t.Errorf("NavList prompt missing the stubbed-helper contract\n---\n%s", navListPrompt)
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
	if fake.RequestCount() != 5 { // 4 endpoints, one needed a retry
		t.Errorf("llm calls = %d, want 5 (4 + 1 retry)", fake.RequestCount())
	}
	if len(res.Failed) != 0 {
		t.Errorf("failed = %v, want none", res.Failed)
	}
}

// TestConvertConcurrentDBUnitsByteIdentical: workers>1 must produce the same
// bytes as workers=1 — the pool renders in parallel, the merge stays in unit
// order. Run under `go test -race` for the data-race check.
func TestConvertConcurrentDBUnitsByteIdentical(t *testing.T) {
	solo, _ := convertFixture(t)
	if _, err := Run(context.Background(), solo); err != nil {
		t.Fatal(err)
	}
	pool, _ := convertFixture(t)
	pool.Workers = 5
	if _, err := Run(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"pkg/services/nav/db/nav.go",
		"pkg/services/nav/db/interface.go",
		"pkg/services/nav/models/nav.go",
		"pkg/services/nav/controller/nav.go",
	} {
		a, err := os.ReadFile(filepath.Join(solo.BaseDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(pool.BaseDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Errorf("%s differs between workers=1 and workers=5", rel)
		}
	}
}

// TestConvertSkipLLM: the deterministic-only mode (run.llm: false) generates
// every deterministic artifact with zero LLM calls, marks pending controller
// units skipped — never failed — and a later LLM-enabled resume converts
// exactly those.
func TestConvertSkipLLM(t *testing.T) {
	opts, _ := convertFixture(t)
	opts.SkipLLM = true
	opts.Client = nil

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.LLMCalls != 0 {
		t.Errorf("skip-llm run made %d llm calls, want 0", res.LLMCalls)
	}
	for _, rel := range []string{
		"pkg/services/nav/models/nav.go",
		"pkg/services/nav/db/nav.go",
		"pkg/services/nav/db/interface.go",
		"pkg/services/nav/controller/interface.go",
		"pkg/services/nav/handler/router_snippet.txt",
	} {
		if _, err := os.Stat(filepath.Join(opts.BaseDir, rel)); err != nil {
			t.Errorf("missing artifact %s", rel)
		}
	}
	if len(res.Skipped) != 4 {
		t.Errorf("skipped = %v, want the 4 mapped endpoints", res.Skipped)
	}
	if len(res.Failed) != 0 {
		t.Errorf("failed = %v, want none in skip-llm mode", res.Failed)
	}
	appended, failed, _, skipped, _, _ := opts.Ledger.Counts()
	if skipped != 4 || failed != 0 || appended < 10 {
		t.Errorf("ledger = appended %d, failed %d, skipped %d", appended, failed, skipped)
	}

	// Resume with the LLM enabled: the skipped units convert, nothing re-runs.
	opts2, _ := convertFixture(t)
	// Share the first run's ledger by pointing opts2 at the same one.
	opts2.Ledger = opts.Ledger
	opts2.BaseDir = opts.BaseDir
	res2, err := Run(context.Background(), opts2)
	if err != nil {
		t.Fatal(err)
	}
	if res2.LLMCalls != 4 {
		t.Errorf("resume made %d llm calls, want 4 (only the skipped units)", res2.LLMCalls)
	}
	if len(res2.Skipped) != 0 || len(res2.Failed) != 0 {
		t.Errorf("resume skipped %v, failed %v, want none", res2.Skipped, res2.Failed)
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

// TestBuildPromptFlowDraft pins the FLW-D7 seam: the deterministic draft is
// an additive prompt section, present only when rendered — the legacy view,
// DB contract, and REQUIRED CALLS sections are unchanged either way.
func TestBuildPromptFlowDraft(t *testing.T) {
	view := budget.View{Source: "legacy C branch"}
	prompt := buildPrompt(view, "db contract", "struct contract", "NavHistory", "", nil)
	if strings.Contains(prompt, "Deterministic flow draft") {
		t.Error("empty draft must not add the flow section")
	}
	if !strings.Contains(prompt, "legacy C branch") {
		t.Error("legacy view missing from the prompt")
	}
	withDraft := buildPrompt(view, "db contract", "struct contract", "NavHistory", "\trows, err := s.store.GetNavHistory(c)", nil)
	if !strings.Contains(withDraft, "Deterministic flow draft") ||
		!strings.Contains(withDraft, "s.store.GetNavHistory(c)") {
		t.Errorf("draft section missing:\n%s", withDraft)
	}
	if !strings.Contains(withDraft, "keep the flow and every store call") {
		t.Error("draft instruction line missing")
	}
}
