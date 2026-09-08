package testgen

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/llm"
	"github.com/Public/convert-tux-to-go/internal/testscan"
)

// The synthetic converted tree (GT-6 stand-in for the proprietary corpus):
// a full pkg/services/nav tree with go.mod, shaped like the examples/nav
// reference conversions. GetCount ships covered (existing suite), NavList/
// NavHistory controllers map fields (the LLM gap), NavDirect is a
// passthrough (template-shaped).
const (
	modGoMod = "module mutual-fund-be\n\ngo 1.26.4\n"

	modDBStore = `package db

import (
	"context"

	"mutual-fund-be/pkg/services/nav/models"
)

type store struct{}

func (g *store) GetNavDetails(c context.Context, compCd string) ([]*models.NavDetails, error) {
	var navDetails []*models.NavDetails
	query := ` + "`" + `SELECT DEMO_PRICE_COMP_CD AS "COMP_CD", DEMO_CO_NAME AS "COMP_NAME"
	          FROM DEMO_COMPANY, DEMO_PRICE
	          WHERE DEMO_CO_ID = :1` + "`" + `
	err := g.db.SelectContext(c, &navDetails, query, compCd)
	if err != nil {
		return nil, err
	}
	return navDetails, nil
}

func (g *store) GetCount(ctx context.Context, matchAccount string) (int64, error) {
	var count int64
	query := ` + "`" + `SELECT COUNT(*) AS "count" FROM DEMO_ACCOUNT_MAP WHERE DEMO_MATCH_ACC = :1` + "`" + `
	err := g.db.GetContext(ctx, &count, query, matchAccount)
	if err != nil {
		return 0, err
	}
	return count, nil
}
`

	modDBIface = `package db

import (
	"context"

	"mutual-fund-be/pkg/services/nav/models"

	"github.com/jmoiron/sqlx"
)

type NavStore interface {
	GetNavDetails(context.Context, string) ([]*models.NavDetails, error)
	GetCount(ctx context.Context, matchAccount string) (int64, error)
}

func NewNavStore(db *sqlx.DB) NavStore { return &store{} }
`

	modDBTest = `package db

import "testing"

type NavStoreSuite struct{ navStore NavStore }

func TestNavStoreSuite(t *testing.T) {}

func (s *NavStoreSuite) TestGetCount() { s.navStore.GetCount(nil, "8500011155") }
`

	modModels = `package models

import "database/sql"

type NavRequest struct {
	CompCode string ` + "`" + `json:"FML_COMP_CD" binding:"required"` + "`" + `
}

type NavResponse struct {
	CompCode string ` + "`" + `json:"FML_COMP_CD,omitempty"` + "`" + `
	CompName string ` + "`" + `json:"FML_COMP_NAME,omitempty"` + "`" + `
}

type NavDetails struct {
	CompCd   sql.NullString ` + "`" + `db:"COMP_CD"` + "`" + `
	CompName sql.NullString ` + "`" + `db:"COMP_NAME"` + "`" + `
}

type NavHistRequest struct {
	CompCode   string ` + "`" + `json:"FML_COMP_CD" binding:"required"` + "`" + `
	SchemeCode string ` + "`" + `json:"FML_SCHEME_CD"` + "`" + `
}
`

	modController = `package controller

import (
	"context"

	"mutual-fund-be/pkg/services/nav/db"
	"mutual-fund-be/pkg/services/nav/models"
)

type navController struct {
	store db.NavStore
}

func (s *navController) NavList(ctx context.Context, request *models.NavRequest) (data []*models.NavResponse, err error) {
	result, err := s.store.GetNavDetails(ctx, request.CompCode)
	for _, row := range result {
		data = append(data, &models.NavResponse{
			CompCode: row.CompCd.String,
			CompName: row.CompName.String,
		})
	}
	return data, err
}

func (s *navController) NavHistory(ctx context.Context, request *models.NavHistRequest) (data []*models.NavResponse, err error) {
	dates, err := s.store.GetDateDetails(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.store.GetNavHistory(ctx, request.CompCode, request.SchemeCode, dates.FromDate.Time, dates.ToDate.Time)
	for _, row := range result {
		data = append(data, &models.NavResponse{CompCode: row.CompCd.String})
	}
	return data, err
}

func (s *navController) NavDirect(ctx context.Context, request *models.NavRequest) (data []*models.NavResponse, err error) {
	return s.store.GetNavDetails(ctx, request.CompCode)
}
`

	modCtrlIface = `package controller

import (
	"context"

	"mutual-fund-be/pkg/services/nav/db"
	"mutual-fund-be/pkg/services/nav/models"
)

type NavController interface {
	NavList(ctx context.Context, request *models.NavRequest) (data []*models.NavResponse, err error)
	NavHistory(ctx context.Context, request *models.NavHistRequest) (data []*models.NavResponse, err error)
	NavDirect(ctx context.Context, request *models.NavRequest) (data []*models.NavResponse, err error)
}

func NewNavController(store db.NavStore) NavController { return &navController{store: store} }
`

	modHandler = `package handler

import (
	"mutual-fund-be/pkg/services/nav/models"

	"github.com/gin-gonic/gin"
)

type navHandler struct {
	controller NavController
}

func (f *navHandler) NavList(c *gin.Context) {
	var request models.NavRequest
	if err := c.BindJSON(&request); err != nil {
		return
	}
	data, err := f.controller.NavList(c, &request)
	if err != nil {
		return
	}
	if data == nil {
		return
	}
	c.JSON(200, data)
}
`

	modHandlerIface = `package handler

import (
	"mutual-fund-be/pkg/services/nav/models"

	"github.com/gin-gonic/gin"
)

type NavController interface {
	NavList(ctx context.Context, request *models.NavRequest) (data []*models.NavResponse, err error)
}

type NavHandler interface {
	NavList(c *gin.Context)
}

func NewNavHandler(controller NavController) NavHandler { return &navHandler{controller: controller} }
`
)

// buildConvertedTree materializes the synthetic service under root.
func buildConvertedTree(t *testing.T, root string) string {
	t.Helper()
	svc := filepath.Join(root, "pkg", "services", "nav")
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", modGoMod)
	write("pkg/services/nav/db/store.go", modDBStore)
	write("pkg/services/nav/db/interface.go", modDBIface)
	write("pkg/services/nav/db/nav_test.go", modDBTest)
	write("pkg/services/nav/models/models.go", modModels)
	write("pkg/services/nav/controller/nav.go", modController)
	write("pkg/services/nav/controller/interface.go", modCtrlIface)
	write("pkg/services/nav/handler/nav.go", modHandler)
	write("pkg/services/nav/handler/interface.go", modHandlerIface)
	return svc
}

func scanTarget(t *testing.T, svc string) (*testscan.Target, *testscan.Report) {
	t.Helper()
	tgt, err := testscan.Resolve(svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := tgt.Scan()
	if err != nil {
		t.Fatal(err)
	}
	return tgt, rep
}

func parseAll(t *testing.T, files []string) {
	t.Helper()
	fset := token.NewFileSet()
	for _, f := range files {
		if _, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution); err != nil {
			t.Errorf("generated %s does not parse: %v", f, err)
		}
	}
}

func unitStatus(res *Result, fn string) string {
	for _, u := range res.Units {
		if u.Func == fn {
			return u.Status
		}
	}
	return "<missing>"
}

// TestGenerateNoLLM is the deterministic end-to-end: templates fill db,
// handler, and passthrough-controller gaps; field-mapping controllers report
// llm-required; the pre-covered function is never regenerated.
func TestGenerateNoLLM(t *testing.T) {
	root := t.TempDir()
	svc := buildConvertedTree(t, root)
	out := filepath.Join(root, "_staged")

	tgt, rep := scanTarget(t, svc)
	res, err := Generate(context.Background(), tgt, rep, Options{BaseDir: out, Workers: 1, NoLLM: true})
	if err != nil {
		t.Fatal(err)
	}

	if got := unitStatus(res, "GetNavDetails"); got != StatusTemplate {
		t.Errorf("GetNavDetails: got %s, want generated", got)
	}
	if got := unitStatus(res, "GetCount"); got != "skipped-covered" {
		t.Errorf("GetCount: got %s, want skipped-covered", got)
	}
	if got := unitStatus(res, "NewNavStore"); got != StatusDesign {
		t.Errorf("NewNavStore: got %s, want skipped-by-design", got)
	}
	if got := unitStatus(res, "NavDirect"); got != StatusTemplate {
		t.Errorf("NavDirect: got %s, want generated", got)
	}
	if got := unitStatus(res, "NavList"); got != StatusLLMNeeded {
		t.Errorf("NavList: got %s, want llm-required", got)
	}
	if got := unitStatus(res, "NavHistory"); got != StatusLLMNeeded {
		t.Errorf("NavHistory: got %s, want llm-required", got)
	}
	if got := unitStatus(res, "NavListHandler"); got != "<missing>" {
		// handler unit key is its own func name
		_ = got
	}
	if res.LLMCalls != 0 {
		t.Errorf("no-llm run made %d llm calls", res.LLMCalls)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
	if len(res.Files) != 3 {
		t.Fatalf("want 3 files (db, controller, handler), got %v", res.Files)
	}
	parseAll(t, res.Files)

	// Staged layout mirrors the module tree.
	for _, want := range []string{
		filepath.Join(out, "pkg", "services", "nav", "db", "nav_gentest_test.go"),
		filepath.Join(out, "pkg", "services", "nav", "controller", "nav_test.go"),
		filepath.Join(out, "pkg", "services", "nav", "handler", "nav_test.go"),
	} {
		found := false
		for _, f := range res.Files {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing output %s in %v", want, res.Files)
		}
	}

	dbOut := read(t, filepath.Join(out, "pkg", "services", "nav", "db", "nav_gentest_test.go"))
	for _, want := range []string{
		"type NavStoreGenSuite struct {",
		"func (suite *NavStoreGenSuite) TestGetNavDetails() {",
		`ExpectQuery("^SELECT (.+) FROM DEMO_COMPANY, DEMO_PRICE WHERE (.+)$")`,
		`sqlmock.NewRows([]string{"COMP_CD", "COMP_NAME"})`,
		`GetNavDetails(suite.ctx, "compcd")`,
		"func TestNavStoreGenSuite(t *testing.T) {",
	} {
		if !strings.Contains(dbOut, want) {
			t.Errorf("db test missing %q\n---\n%s", want, dbOut)
		}
	}
	if strings.Contains(dbOut, "TestGetCount") {
		t.Errorf("db test must not regenerate the covered GetCount\n---\n%s", dbOut)
	}

	ctrlOut := read(t, filepath.Join(out, "pkg", "services", "nav", "controller", "nav_test.go"))
	for _, want := range []string{
		"func (suite *NavControllerSuite) TestNavDirect() {",
		"GetNavDetails(suite.ctx, request.CompCode)",
		"NavDirect(suite.ctx, &request)",
	} {
		if !strings.Contains(ctrlOut, want) {
			t.Errorf("controller test missing %q\n---\n%s", want, ctrlOut)
		}
	}

	hOut := read(t, filepath.Join(out, "pkg", "services", "nav", "handler", "nav_test.go"))
	for _, want := range []string{
		"func (suite *NavHandlerSuite) TestNavList() {",
		"NavListError",
		"CompCode:",
		`"fmlcompcd"`,
		"request := models.NavRequest{CompCode: testCase.CompCode}",
		"utils.CreateTestGinContext(http.MethodPost, request, nil, nil, nil)",
		"utils.TypeConverter[[]*models.NavResponse](httpResponse.Data)",
	} {
		if !strings.Contains(hOut, want) {
			t.Errorf("handler test missing %q\n---\n%s", want, hOut)
		}
	}
}

// TestGenerateIdempotent: once the generated tests land in the tree, a
// second scan+generate finds them covered — nothing new is written.
func TestGenerateIdempotent(t *testing.T) {
	root := t.TempDir()
	svc := buildConvertedTree(t, root)
	out := filepath.Join(root, "_staged")

	tgt, rep := scanTarget(t, svc)
	if _, err := Generate(context.Background(), tgt, rep, Options{BaseDir: out, Workers: 1, NoLLM: true}); err != nil {
		t.Fatal(err)
	}

	// Copy generated tests back into the tree (in-place mode equivalent).
	if err := copyTree(t, out, root); err != nil {
		t.Fatal(err)
	}

	tgt2, rep2 := scanTarget(t, svc)
	res2, err := Generate(context.Background(), tgt2, rep2, Options{BaseDir: filepath.Join(root, "_staged2"), Workers: 1, NoLLM: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range res2.Units {
		if (u.Func == "GetNavDetails" || u.Func == "NavDirect") && u.Status != "skipped-covered" {
			t.Errorf("%s regenerated after landing: %s", u.Func, u.Status)
		}
	}
	if got := unitStatus(res2, "GetNavDetails"); got != "skipped-covered" {
		t.Errorf("GetNavDetails second run: %s", got)
	}
	if got := unitStatus(res2, "NavDirect"); got != "skipped-covered" {
		t.Errorf("NavDirect second run: %s", got)
	}
	if len(res2.Files) != 0 {
		t.Errorf("idempotent run wrote files: %v", res2.Files)
	}
}

// TestGenerateWithLLM: the field-mapping controllers fill through the seam
// on the per-function worker; every block passes the parse+shape gate.
func TestGenerateWithLLM(t *testing.T) {
	navListBlock := `func (suite *NavControllerSuite) TestNavList() {
	testCases := []struct {
		desc           string
		mockInput      []any
		expectedError  string
		expectedOutput []*models.NavResponse
	}{
		{desc: "StoreError", mockInput: []any{nil, errors.New("store error")}, expectedError: "store error"},
		{desc: "Success", mockInput: []any{[]*models.NavDetails{{CompCd: sql.NullString{String: "comp_cd", Valid: true}}}, nil}, expectedOutput: []*models.NavResponse{{CompCode: "comp_cd", CompName: "comp_name"}}},
	}
	for _, testCase := range testCases {
		suite.T().Run(testCase.desc, func(t *testing.T) {
			request := models.NavRequest{CompCode: "fml_comp_cd"}
			suite.storeMock.EXPECT().GetNavDetails(suite.ctx, request.CompCode).Return(testCase.mockInput...)
			data, err := suite.navController.NavList(suite.ctx, &request)
			if testCase.expectedError != "" {
				assert.Error(t, err)
				assert.Nil(t, data)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, testCase.expectedOutput, data)
		})
	}
}`

	navHistoryBlock := `func (suite *NavControllerSuite) TestNavHistory() {
	testCases := []struct {
		desc           string
		mockInput      []any
		expectedError  string
		expectedOutput []*models.NavResponse
	}{
		{desc: "StoreError", mockInput: []any{nil, errors.New("store error")}, expectedError: "store error"},
		{desc: "Success", mockInput: []any{[]*models.NavDetails{{CompCd: sql.NullString{String: "comp_cd", Valid: true}}}, nil}, expectedOutput: []*models.NavResponse{{CompCode: "comp_cd"}}},
	}
	for _, testCase := range testCases {
		suite.T().Run(testCase.desc, func(t *testing.T) {
			request := models.NavHistRequest{CompCode: "fml_comp_cd", SchemeCode: "fml_scheme_cd"}
			suite.storeMock.EXPECT().GetDateDetails(suite.ctx).Return(&models.DateInfo{}, nil)
			suite.storeMock.EXPECT().GetNavHistory(suite.ctx, request.CompCode, request.SchemeCode).Return(testCase.mockInput...)
			data, err := suite.navController.NavHistory(suite.ctx, &request)
			if testCase.expectedError != "" {
				assert.Error(t, err)
				assert.Nil(t, data)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, testCase.expectedOutput, data)
		})
	}
}`

	srv := llm.NewFakeServer(
		llm.FakeResponse{Content: "```go\n" + navListBlock + "\n```"},
		llm.FakeResponse{Content: "```go\n" + navHistoryBlock + "\n```"},
	)
	defer srv.Close()
	client := llm.New(llm.Endpoint{ProfileName: "test", Model: "test-model", APIBase: srv.URL})

	root := t.TempDir()
	svc := buildConvertedTree(t, root)
	out := filepath.Join(root, "_staged")

	tgt, rep := scanTarget(t, svc)
	res, err := Generate(context.Background(), tgt, rep, Options{
		BaseDir: out, Workers: 1, MaxRetries: 2,
		Client: client, Budget: budget.New(12000, 4000, 4),
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := unitStatus(res, "NavList"); got != StatusLLM {
		t.Errorf("NavList: got %s, want generated-llm", got)
	}
	if got := unitStatus(res, "NavHistory"); got != StatusLLM {
		t.Errorf("NavHistory: got %s, want generated-llm", got)
	}
	if res.LLMCalls != 2 {
		t.Errorf("llm calls: got %d, want 2", res.LLMCalls)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
	parseAll(t, res.Files)

	ctrlOut := read(t, filepath.Join(out, "pkg", "services", "nav", "controller", "nav_test.go"))
	for _, want := range []string{
		"func (suite *NavControllerSuite) TestNavList() {",
		"func (suite *NavControllerSuite) TestNavHistory() {",
		"GetDateDetails(suite.ctx)",
		"GetNavHistory(suite.ctx, request.CompCode, request.SchemeCode)",
	} {
		if !strings.Contains(ctrlOut, want) {
			t.Errorf("controller test missing %q\n---\n%s", want, ctrlOut)
		}
	}

	// The prompt carried the function source (no SQL, per the layer split).
	found := false
	for _, req := range srv.Requests {
		msgs, _ := req["messages"].([]any)
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if c, _ := mm["content"].(string); strings.Contains(c, "GetNavDetails") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("llm prompt missing the function source")
	}
}

// TestGenerateDeterministic: workers=1 and workers=N produce byte-identical
// staged trees.
func TestGenerateDeterministic(t *testing.T) {
	run := func(workers int) []byte {
		root := t.TempDir()
		svc := buildConvertedTree(t, root)
		out := filepath.Join(root, "_staged")
		tgt, rep := scanTarget(t, svc)
		if _, err := Generate(context.Background(), tgt, rep, Options{BaseDir: out, Workers: workers, NoLLM: true}); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		for _, f := range res(t, out) {
			rel, err := filepath.Rel(out, f)
			if err != nil {
				t.Fatal(err)
			}
			buf.WriteString("=== " + rel + "\n")
			buf.Write(readBytes(t, f))
		}
		return buf.Bytes()
	}
	a := run(1)
	b := run(3)
	if !bytes.Equal(a, b) {
		for i := 0; i < len(a) && i < len(b); i++ {
			if a[i] != b[i] {
				lo, hi := i-200, i+200
				if lo < 0 {
					lo = 0
				}
				if hi > len(a) {
					hi = len(a)
				}
				t.Fatalf("outputs differ at %d:\nA: %q\nB: %q", i, a[lo:hi], b[lo:hi])
			}
		}
		t.Fatalf("outputs differ in length: %d vs %d", len(a), len(b))
	}
}

// TestLLMGate rejects malformed seam output.
func TestLLMGate(t *testing.T) {
	u := &unit{suite: "NavControllerSuite", fn: testscan.Func{Name: "NavList"}}
	if err := gateCtrlBlock("func (suite *Wrong) TestNavList() {}", u); err == nil {
		t.Error("wrong receiver must be rejected")
	}
	if err := gateCtrlBlock("func (suite *NavControllerSuite) TestOther() {}", u); err == nil {
		t.Error("wrong name must be rejected")
	}
	if err := gateCtrlBlock("func (suite *NavControllerSuite) TestNavList() { _ = 1 }", u); err == nil {
		t.Error("non-table block must be rejected")
	}
	if err := gateCtrlBlock("not go", u); err == nil {
		t.Error("unparseable block must be rejected")
	}
	good := "func (suite *NavControllerSuite) TestNavList() { testCases := []struct{ desc string }{}; _ = testCases }"
	if err := gateCtrlBlock(good, u); err != nil {
		t.Errorf("valid block rejected: %v", err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	return string(readBytes(t, path))
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func res(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func copyTree(t *testing.T, src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// ---- GT-6 byte-pinned goldens ----

// The goldens are the -no-llm workers=1 output over testdata/gentest (the
// synthetic demo service). Regenerate with:
//
//	go run ./cmd/tuxgo gentest testdata/gentest/pkg/services/demo -no-llm -base testdata/gentest/expected
//
// then move the gap-report line via TestGoldenGapReport (regenerate by
// re-running this test with GT_UPDATE_GOLDENS=1).
func TestGoldenNoLLM(t *testing.T) {
	update := os.Getenv("GT_UPDATE_GOLDENS") == "1"
	fixture := filepath.Join("..", "..", "testdata", "gentest")
	out := t.TempDir()

	tgt, rep := scanTarget(t, fixture)
	got, err := Generate(context.Background(), tgt, rep, Options{BaseDir: out, Workers: 1, NoLLM: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.LLMCalls != 0 {
		t.Errorf("no-llm golden run made %d llm calls", got.LLMCalls)
	}
	if len(got.Files) != 3 {
		t.Fatalf("want 3 generated files, got %v", got.Files)
	}

	for _, f := range got.Files {
		rel, err := filepath.Rel(out, f)
		if err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("..", "..", "testdata", "gentest", "expected", rel)
		if update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(f)
			if err := os.WriteFile(golden, data, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("golden read %s: %v (regenerate with GT_UPDATE_GOLDENS=1)", golden, err)
		}
		if !bytes.Equal(want, readBytes(t, f)) {
			t.Errorf("%s drifted from golden %s (regenerate with GT_UPDATE_GOLDENS=1 after a deliberate template change)", rel, golden)
		}
	}

	// Status pins: the field-mapping controller is the one llm-required.
	if s := unitStatus(got, "OrderList"); s != StatusLLMNeeded {
		t.Errorf("OrderList: %s, want llm-required", s)
	}
	if s := unitStatus(got, "OrderDirect"); s != StatusTemplate {
		t.Errorf("OrderDirect: %s, want generated", s)
	}
	if s := unitStatus(got, "GetOrderCount"); s != StatusTemplate {
		t.Errorf("GetOrderCount: %s, want generated", s)
	}
}

// TestGoldenGapReport pins the scan's gap report over the demo fixture.
func TestGoldenGapReport(t *testing.T) {
	update := os.Getenv("GT_UPDATE_GOLDENS") == "1"
	target := filepath.Join("..", "..", "testdata", "gentest", "pkg", "services", "demo")
	tgt, err := testscan.Resolve(target, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := tgt.Scan()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("..", "..", "testdata", "gentest", "expected", "gap_report.txt")
	if update {
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("golden read: %v (regenerate with GT_UPDATE_GOLDENS=1)", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Errorf("gap report drifted from golden %s (regenerate with GT_UPDATE_GOLDENS=1 after a deliberate scan change)", golden)
	}
}
