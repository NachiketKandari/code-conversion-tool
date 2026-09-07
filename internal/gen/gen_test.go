package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/goast"
	"github.com/Public/convert-tux-to-go/internal/plan"
)

// genNavFixture builds the plan (the user mapping pins reference-quality
// names) and the derived Service. The fn files are returned so variants can
// rebuild a Service (e.g. WithGorm) without re-extracting.
func genNavFixture(t *testing.T) (*Service, *plan.Plan, []*ir.File) {
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
	if main == nil {
		t.Fatal("nav IR missing")
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
	s, err := NewService(Options{Plan: p, Main: main, FnFiles: fns})
	if err != nil {
		t.Fatal(err)
	}
	return s, p, fns
}

func TestGenModels(t *testing.T) {
	s, p, _ := genNavFixture(t)
	models, err := s.ModelFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type NavHistoryRequest struct",
		"CompCd string `json:\"FML_COMP_CD\" binding:\"required\"`",
		"type NavListResponse struct",
		"PriceDate", "json:\"FML_PRICE_DATE,omitempty\"",
		"type NavDetails struct",
		// gofmt column-aligns struct tags, so name and tag are asserted apart.
		"DemoCompCd", "DemoSchemeDesc", "db:\"DEMO_COMP_CD\"", "db:\"DEMO_SCHEME_DESC\"",
		"type NavHistoryDetail struct",
		"CFromDate", "CToDate", "db:\"C_FROM_DATE\"", "db:\"C_TO_DATE\"",
		"type DemoActive struct",
		"CActiveFlag", "db:\"C_ACTIVE_FLAG\"",
		"import \"database/sql\"",
	} {
		if !strings.Contains(models, want) {
			t.Errorf("models file missing %q\n%s", want, models)
		}
	}
	again, err := s.ModelFile(p)
	if err != nil || models != again {
		t.Error("models derivation is not deterministic")
	}
}

func TestGenDBMethodsAndInterface(t *testing.T) {
	s, p, fns := genNavFixture(t)
	file, err := s.DBMethodsFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func (g *store) GetNavDetails(c context.Context, compCd string) ([]*models.NavDetails, error)",
		"SelectContext(c, &navDetails, query, compCd)",
		"func (g *store) GetNavHistory(c context.Context, compCd string, schCd string, fromDate time.Time, toDate time.Time) ([]*models.NavHistoryDetail, error)",
		"func (g *store) GetCount(c context.Context, matchAccount string) (int64, error)",
		"GetContext(c, &count, query, matchAccount)",
		"func (g *store) IsDemoActive(c context.Context, cMtchAccnt string) (*models.DemoActive, error)",
		"\"database/sql\"",
		"\"time\"",
		"mutual-fund-be/pkg/logger",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("db methods file missing %q", want)
		}
	}
	if strings.Count(file, "func (g *store)") != 7 {
		t.Errorf("db methods = %d, want 7", strings.Count(file, "func (g *store)"))
	}

	iface, err := s.DBInterface(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type NavStore interface",
		"GetNavDetails(c context.Context, compCd string) ([]*models.NavDetails, error)",
		"GetCount(c context.Context, matchAccount string) (int64, error)",
		"func NewNavStore(db *sqlx.DB) NavStore",
	} {
		if !strings.Contains(iface, want) {
			t.Errorf("db interface missing %q\n%s", want, iface)
		}
	}
	// Default shape is sqlx-only — the gorm handle is the opt-in variant.
	if strings.Contains(iface, "gorm") {
		t.Errorf("default db interface must be sqlx-only:\n%s", iface)
	}

	// The db.withGorm variant carries the legacy handle (nav-example shape).
	g, err := NewService(Options{Plan: p, Main: s.Main, FnFiles: fns, WithGorm: true})
	if err != nil {
		t.Fatal(err)
	}
	gormIface, err := g.DBInterface(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func NewNavStore(oracle *gorm.DB, db *sqlx.DB) NavStore",
		"oracle *gorm.DB",
	} {
		if !strings.Contains(gormIface, want) {
			t.Errorf("gorm variant missing %q\n%s", want, gormIface)
		}
	}
}

func TestGenAccumulateDBInterface(t *testing.T) {
	s, p, _ := genNavFixture(t)
	path := filepath.Join(t.TempDir(), "db", "interface.go")
	n := 0
	for _, u := range p.Units {
		if u.Kind != plan.KindDBMethod {
			continue
		}
		_, sig, _, err := s.DBMethod(u)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AccumulateDBInterface(path, sig); err != nil {
			t.Fatal(err)
		}
		n++
	}
	// Re-accumulate — idempotent, converged.
	for _, u := range p.Units {
		if u.Kind != plan.KindDBMethod {
			continue
		}
		_, sig, _, _ := s.DBMethod(u)
		if err := s.AccumulateDBInterface(path, sig); err != nil {
			t.Fatal(err)
		}
	}
	sigs, err := goastInspect(path, "NavStore")
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != n {
		t.Errorf("accumulated %d signatures, want %d", len(sigs), n)
	}
}

func TestGenControllerHandlerRouter(t *testing.T) {
	s, p, _ := genNavFixture(t)

	ctrl, err := s.ControllerInterface(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type NavController interface",
		"NavHistory(ctx context.Context, request *models.NavHistoryRequest) (data []*models.NavHistoryResponse, err error)",
		"func NewNavController(store db.NavStore) NavController",
		"store db.NavStore",
	} {
		if !strings.Contains(ctrl, want) {
			t.Errorf("controller interface missing %q\n%s", want, ctrl)
		}
	}

	hiface, err := s.HandlerInterface(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type NavHandler interface",
		"NavHistory(c *gin.Context)",
		"func NavController(repo repo.DataObject) controller.NavController",
		"db.NewNavStore(repo.Databases.ReadDatabase.EBATEST, repo.Databases.ReadDatabase.MF)",
	} {
		if !strings.Contains(hiface, want) {
			t.Errorf("handler interface missing %q\n%s", want, hiface)
		}
	}

	handlers, err := s.HandlerMethodsFile()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func (f *navHandler) NavHistory(c *gin.Context)",
		"var request models.NavHistoryRequest",
		"gCtx.BadRequestJSON(err, request)",
		"gCtx.SuccessJSON(data)",
	} {
		if !strings.Contains(handlers, want) {
			t.Errorf("handler methods missing %q", want)
		}
	}

	router, err := s.Router()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"nav := v1.Group(\"/nav\")",
		"nav.POST(\"/mfnavhistory\", obj.NavHistory)",
		"nav.POST(\"/mfnavschemelist\", obj.NavList)",
	} {
		if !strings.Contains(router, want) {
			t.Errorf("router snippet missing %q", want)
		}
	}
}

// TestGenGateGoldenFiles is the Phase 5 deterministic-generation gate: every
// artifact renders deterministically (byte-identical re-runs).
func TestGenGateGoldenFiles(t *testing.T) {
	s, p, _ := genNavFixture(t)
	type artifact struct {
		name string
		fn   func() (string, error)
	}
	arts := []artifact{
		{"models", func() (string, error) { return s.ModelFile(p) }},
		{"db-methods", func() (string, error) { return s.DBMethodsFile(p) }},
		{"db-interface", func() (string, error) { return s.DBInterface(p) }},
		{"controller-interface", func() (string, error) { return s.ControllerInterface(p) }},
		{"handler-interface", func() (string, error) { return s.HandlerInterface(p) }},
		{"handler-methods", func() (string, error) { return s.HandlerMethodsFile() }},
		{"router", func() (string, error) { return s.Router() }},
	}
	for _, a := range arts {
		first, err := a.fn()
		if err != nil {
			t.Fatalf("%s: %v", a.name, err)
		}
		second, err := a.fn()
		if err != nil {
			t.Fatalf("%s rerun: %v", a.name, err)
		}
		if first != second {
			t.Errorf("%s generation is not deterministic", a.name)
		}
		if strings.TrimSpace(first) == "" {
			t.Errorf("%s rendered empty", a.name)
		}
	}
}

func goastInspect(path, iface string) ([]string, error) {
	sigs, err := goast.InspectInterface(path, iface)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(sigs))
	for i, s := range sigs {
		out[i] = s.Text
	}
	return out, nil
}

// TestGenControllerPromptContext pins the fixed contract the controller
// prompt consumes: exact signature (named returns), verbatim request/
// response structs, and only the row structs the endpoint's store calls
// return.
func TestGenControllerPromptContext(t *testing.T) {
	s, p, _ := genNavFixture(t)
	ctx, err := s.ControllerPromptContext("SipFreedem", p, []string{"GetCount", "IsDemoActive", "GetSipFreedem"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func (s *navController) SipFreedem(c context.Context, request *models.SipFreedemRequest) (data []*models.SipFreedemResponse, err error)",
		"type SipFreedemRequest struct {",
		"type SipFreedemResponse struct {",
		"type SipFreedemDetail struct {",
		"type DemoActive struct {",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("prompt context missing %q:\n%s", want, ctx)
		}
	}
	// Only the endpoint's own rows: other units' structs stay out.
	for _, absent := range []string{"NavHistoryDetail", "NavListRequest", "GetCount"} {
		if strings.Contains(ctx, absent) {
			t.Errorf("prompt context leaks %q:\n%s", absent, ctx)
		}
	}
}
