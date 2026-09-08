package templates

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

const navModule = "mutual-fund-be"

func TestAllTemplatesParse(t *testing.T) {
	for _, id := range AllIDs {
		if _, err := load(id); err != nil {
			t.Errorf("template %s failed to parse: %v", id, err)
		}
	}
}

func render(t *testing.T, id ID, data any) string {
	t.Helper()
	out, err := NewEmbeddedProvider().Render(id, data)
	if err != nil {
		t.Fatalf("Render(%s) failed: %v", id, err)
	}
	return out
}

func TestRenderModelFile(t *testing.T) {
	out := render(t, ModelFile, ModelFileData{
		Package: "models",
		Structs: []StructSpec{
			{Name: "NavRequest", Fields: []FieldSpec{
				{Name: "CompCode", Type: "string", JSONTag: "FML_COMP_CD", Binding: "required"},
			}},
			{Name: "SipFreedemNavRequest", Fields: []FieldSpec{
				{Name: "CompCode", Type: "string", JSONTag: "FML_COMP_CD", Binding: "required"},
				{Name: "MatchAccount", Type: "string", JSONTag: "FML_ACCOUNT", Binding: "required,matchaccount", ErrMsg: "Provide valid Match account"},
			}},
			{Name: "NavResponse", Fields: []FieldSpec{
				{Name: "CompCode", Type: "string", JSONTag: "FML_COMP_CD", OmitEmpty: true},
			}},
			{Name: "NavDetails", Fields: []FieldSpec{
				{Name: "CompCd", Type: "sql.NullString", DBTag: "COMP_CD"},
			}},
			{Name: "DateInfo", Fields: []FieldSpec{
				{Name: "FromDate", Type: "sql.NullTime", DBTag: "Date1"},
			}},
		},
	})

	for _, want := range []string{
		`import "database/sql"`,
		"CompCode string `json:\"FML_COMP_CD\" binding:\"required\"`",
		"MatchAccount string `json:\"FML_ACCOUNT\" binding:\"required,matchaccount\" error:\"Provide valid Match account\"`",
		"CompCode string `json:\"FML_COMP_CD,omitempty\"`",
		"CompCd sql.NullString `db:\"COMP_CD\"`",
		"FromDate sql.NullTime `db:\"Date1\"`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("model file missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDBInterfaceFile(t *testing.T) {
	out := render(t, DBInterfaceFile, DBInterfaceData{
		Package:   "db",
		StoreType: "store",
		IfaceName: "NavStore",
		CtorName:  "NewNavStore",
		WithGorm:  true,
		ModelsPkg: navModule + "/pkg/services/nav/models",
		ExtraImports: []string{
			`"time"`,
		},
		Methods: []string{
			"GetNavDetails(context.Context, string) ([]*models.NavDetails, error)",
			"GetCount(ctx context.Context, matchAccount string) (int64, error)",
		},
	})

	for _, want := range []string{
		"oracle *gorm.DB",
		"db *sqlx.DB",
		"type NavStore interface {",
		"GetNavDetails(context.Context, string) ([]*models.NavDetails, error)",
		"func NewNavStore(oracle *gorm.DB, db *sqlx.DB) NavStore {",
		`"github.com/jmoiron/sqlx"`,
		`"gorm.io/gorm"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("db interface file missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDBMethodSelectMulti(t *testing.T) {
	out := render(t, DBMethodSelectMulti, DBMethodData{
		Receiver: "g", StoreType: "store", Name: "GetNavDetails", CtxName: "c",
		Params:  []ParamSpec{{Name: "compCd", Type: "string"}},
		Query:   "SELECT DEMO_PRICE_COMP_CD AS \"COMP_CD\" FROM DEMO_PRICE WHERE MF_COMP_CD = :1",
		VarName: "navDetails", RowType: "models.NavDetails", Multi: true,
	})

	for _, want := range []string{
		"func (g *store) GetNavDetails(c context.Context, compCd string) ([]*models.NavDetails, error) {",
		"var navDetails []*models.NavDetails",
		"g.db.SelectContext(c, &navDetails, query, compCd)",
		"return navDetails, nil",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("select-multi method missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDBMethodSelectSingle(t *testing.T) {
	// Struct result (DateInfo)
	out := render(t, DBMethodSelectSingle, DBMethodData{
		Receiver: "g", StoreType: "store", Name: "GetDateDetails", CtxName: "c",
		Query:   "SELECT date(...) AS \"Date1\", sysdate AS \"Date2\" FROM dual",
		VarName: "dateinfo", RowType: "models.DateInfo",
	})
	for _, want := range []string{
		"func (g *store) GetDateDetails(c context.Context) (*models.DateInfo, error) {",
		"g.db.GetContext(c, &dateinfo, query)",
		"errors.Is(err, sql.ErrNoRows)",
		"return &dateinfo, nil",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("select-single (struct) missing %q\n---\n%s", want, out)
		}
	}

	// Scalar result (GetCount)
	out = render(t, DBMethodSelectSingle, DBMethodData{
		Receiver: "g", StoreType: "store", Name: "GetCount", CtxName: "ctx",
		Params:  []ParamSpec{{Name: "matchAccount", Type: "string"}},
		Query:   "SELECT COUNT(*) AS \"count\" FROM DEMO_ACCOUNT_MAP WHERE DEMO_MATCH_ACC = :1",
		VarName: "count", Scalar: "int64",
	})
	for _, want := range []string{
		"func (g *store) GetCount(ctx context.Context, matchAccount string) (int64, error) {",
		"g.db.GetContext(ctx, &count, query, matchAccount)",
		"return count, nil",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("select-single (scalar) missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDBMethodInsertTx(t *testing.T) {
	// InsertRiskProfile shape (examples/dbTransactionEx.txt): named binds in
	// SQL, positional args in Go, RowsAffected()==0 → logged sql.ErrNoRows,
	// success debug line, nil.
	out := render(t, DBMethodInsertTx, DBMethodData{
		Receiver: "g", StoreType: "store", Name: "InsertRiskProfile", CtxName: "ctx",
		Params: []ParamSpec{
			{Name: "userId", Type: "string"}, {Name: "riskProfile", Type: "string"}, {Name: "uniqueNumber", Type: "string"},
		},
		Query:      "INSERT INTO URF_USR_RISK_PROF(URF_USR_ID, URF_RISK_PROF, URF_URA_UNIQ_NMBR) VALUES(:userId, :riskProfile, :uniqueNumber)",
		SuccessMsg: "Risk Profile Inserted Successfully",
	})
	for _, want := range []string{
		"func (g *store) InsertRiskProfile(ctx context.Context, tx *sqlx.Tx, userId string, riskProfile string, uniqueNumber string) error {",
		"tx.ExecContext(ctx, query, userId, riskProfile, uniqueNumber)",
		"count, _ := result.RowsAffected()",
		"logger.Log(ctx).Error(sql.ErrNoRows.Error())",
		"return sql.ErrNoRows",
		"logger.Log(ctx).Debug(\"Risk Profile Inserted Successfully\")",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("insert-tx method missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDBMethodUpdateTx(t *testing.T) {
	// UpdateIBFStatus shape: tx-variant UPDATE, zero rows → sql.ErrNoRows.
	out := render(t, DBMethodUpdateTx, DBMethodData{
		Receiver: "g", StoreType: "store", Name: "UpdateIBFStatus", CtxName: "ctx",
		Params:     []ParamSpec{{Name: "userId", Type: "string"}},
		Query:      "UPDATE IBF_INFO_BOOKMARK_FORMS SET IBF_STATUS = 'C' WHERE IBF_USER_ID = :userId",
		SuccessMsg: "IBF Status Updated Successfully",
	})
	for _, want := range []string{
		"func (g *store) UpdateIBFStatus(ctx context.Context, tx *sqlx.Tx, userId string) error {",
		"result, err := tx.ExecContext(ctx, query, userId)",
		"return sql.ErrNoRows",
		"logger.Log(ctx).Debug(\"IBF Status Updated Successfully\")",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("update-tx method missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDBMethodDeleteTx(t *testing.T) {
	// DeleteQnA shape: DELETE tolerates zero rows — result discarded, no
	// RowsAffected check at all.
	out := render(t, DBMethodDeleteTx, DBMethodData{
		Receiver: "g", StoreType: "store", Name: "DeleteQnA", CtxName: "ctx",
		Params: []ParamSpec{{Name: "userId", Type: "string"}, {Name: "customerType", Type: "string"}},
		Query:  "DELETE FROM RPQA_RP_QUESTION_ANS WHERE RPQA_USR_ID = :1 AND RPQA_CUST_TYPE = :2",
	})
	if strings.Contains(out, "RowsAffected") {
		t.Errorf("delete must not check RowsAffected (DeleteQnA convention):\n%s", out)
	}
	for _, want := range []string{
		"func (g *store) DeleteQnA(ctx context.Context, tx *sqlx.Tx, userId string, customerType string) error {",
		"_, err := tx.ExecContext(ctx, query, userId, customerType)",
		"logger.Log(ctx).Error(\"unable to delete\")",
		"return nil",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("delete-tx method missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDBMethodSelectSingleTx(t *testing.T) {
	// GetMarks shape: tx-variant mid-flow read — runs on tx, propagates every
	// error (no ErrNoRows tolerance), returns the scalar.
	out := render(t, DBMethodSelectSingleTx, DBSelectTxData{
		Receiver: "g", StoreType: "store", Name: "GetMarks", CtxName: "ctx",
		Params:  []ParamSpec{{Name: "questionId", Type: "string"}, {Name: "answerId", Type: "string"}},
		Query:   "SELECT RPAM_MARKS FROM RPAM_RP_ANSWER_MASTER WHERE RPAM_QSTN_ID = :1 AND RPAM_ANSWER_ID = :2",
		VarName: "marks", ScanType: "sql.NullString", Extract: "marks.String", Zero: `""`, Return: "string",
	})
	for _, want := range []string{
		"func (g *store) GetMarks(ctx context.Context, tx *sqlx.Tx, questionId string, answerId string) (string, error) {",
		"var marks sql.NullString",
		"err := tx.GetContext(ctx, &marks, query, questionId, answerId)",
		`return "", err`,
		"return marks.String, nil",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("select-single-tx method missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderControllerMethodTx(t *testing.T) {
	// AssessQnA shape: pre-flow read outside the tx, every call inside the
	// ExecTransaction closure takes tx (incl. the mid-flow SELECT), post-commit
	// reads assemble the response.
	pre := strings.Join([]string{
		"customerType, err := c.userStore.GetHNICustomerType(ctx, request.UserID)",
		"if err != nil {",
		"\treturn nil, err",
		"}",
	}, "\n")
	txBody := strings.Join([]string{
		"\terr := c.store.DeleteQnA(ctx, tx, request.UserID, customerType)",
		"\tif err != nil {",
		"\t\treturn err",
		"\t}",
		"",
		"\tmarks, err := c.store.GetMarks(ctx, tx, request.QuestionID, request.AnswerID)",
		"\tif err != nil {",
		"\t\treturn err",
		"\t}",
	}, "\n")
	post := strings.Join([]string{
		"marksScored, err := c.store.GetMarksScored(ctx, request.UserID, customerType)",
		"if err != nil {",
		"\treturn nil, err",
		"}",
		"",
		"response := &models.AssessQnAResponse{RiskProfile: marksScored}",
		"return response, nil",
	}, "\n")
	out := render(t, ControllerMethodTx, ControllerTxMethodData{
		Receiver: "c", StructName: "controller", Name: "AssessQnA", CtxName: "ctx",
		RequestType: "models.AssessQnARequest", ResponseType: "models.AssessQnAResponse",
		PreFlow: pre, TxBody: txBody, PostFlow: post,
	})
	for _, want := range []string{
		"func (c *controller) AssessQnA(ctx context.Context, request *models.AssessQnARequest) (*models.AssessQnAResponse, error) {",
		"logger.Log(ctx).Debug(\"START\")",
		"defer logger.Log(ctx).Debug(\"END\")",
		"err = utils.ExecTransaction(ctx, c.store.GetDB(), func(tx *sqlx.Tx) error {",
		"c.store.DeleteQnA(ctx, tx, request.UserID, customerType)",
		"c.store.GetMarks(ctx, tx, request.QuestionID, request.AnswerID)",
		"return nil, err",
		"return response, nil",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("controller tx method missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDBMethodDMLPlain(t *testing.T) {
	out := render(t, DBMethodDMLPlain, DBMethodData{
		Receiver: "g", StoreType: "store", Name: "InsertStatus", CtxName: "ctx",
		Params: []ParamSpec{{Name: "userId", Type: "string"}},
		Query:  "INSERT INTO T(USR_ID) VALUES (:1)",
	})
	if strings.Contains(out, "tx *sqlx.Tx") {
		t.Errorf("plain DML must not take a tx param (decision 27):\n%s", out)
	}
	for _, want := range []string{
		"func (g *store) InsertStatus(ctx context.Context, userId string) error {",
		"g.db.ExecContext(ctx, query, userId)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dml-plain method missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDBMethodMerge(t *testing.T) {
	out := render(t, DBMethodMerge, DBMethodData{
		Receiver: "g", StoreType: "store", Name: "MergeAccounts", CtxName: "ctx",
		Params: []ParamSpec{{Name: "accountId", Type: "string"}, {Name: "balance", Type: "string"}},
		Query:  "MERGE INTO DEMO_ACCOUNTS a USING (SELECT :1 AS ID FROM DUAL) s ON (a.ACCOUNT_ID = s.ID) WHEN MATCHED THEN UPDATE SET a.BALANCE = :2 WHEN NOT MATCHED THEN INSERT (ACCOUNT_ID, BALANCE) VALUES (:1, :2)",
	})
	for _, want := range []string{
		"func (g *store) MergeAccounts(ctx context.Context, accountId string, balance string) error {",
		"g.db.ExecContext(ctx, query, accountId, balance)",
		"return sql.ErrNoRows",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("merge method missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "GetContext") || strings.Contains(out, "SelectContext") {
		t.Errorf("merge must render through the DML contract (no row reads):\n%s", out)
	}
}

func TestRenderControllerInterfaceFile(t *testing.T) {
	out := render(t, ControllerInterfaceFile, ControllerInterfaceData{
		Package: "controller", StructName: "navController", IfaceName: "NavController",
		CtorName:   "NewNavController",
		DBPkg:      navModule + "/pkg/services/nav/db",
		ModelsPkg:  navModule + "/pkg/services/nav/models",
		StoreIface: "db.NavStore",
		Methods: []string{
			"NavList(ctx context.Context, request *models.NavRequest) (data []*models.NavResponse, err error)",
		},
	})
	for _, want := range []string{
		"store db.NavStore",
		"type NavController interface {",
		"NavList(ctx context.Context, request *models.NavRequest) (data []*models.NavResponse, err error)",
		"func NewNavController(store db.NavStore) NavController {",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("controller interface missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderControllerMethod(t *testing.T) {
	body := strings.Join([]string{
		"result, err := s.store.GetNavDetails(ctx, request.CompCode)",
		"",
		"for _, datadetails := range result {",
		"\tdata = append(data, &models.NavResponse{CompCode: datadetails.CompCd.String})",
		"}",
		"",
		"return data, err",
	}, "\n")
	out := render(t, ControllerMethod, ControllerMethodData{
		StructName: "navController", Name: "NavList", CtxName: "ctx",
		RequestType: "models.NavRequest", ResponseType: "models.NavResponse",
		Body: body,
	})
	for _, want := range []string{
		"func (s *navController) NavList(ctx context.Context, request *models.NavRequest) (data []*models.NavResponse, err error) {",
		"logger.Log(ctx).Debug(\"START\")",
		"defer logger.Log(ctx).Debug(\"END\")",
		"s.store.GetNavDetails(ctx, request.CompCode)",
		"return data, err",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("controller method missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderHandlerInterfaceFile(t *testing.T) {
	out := render(t, HandlerInterfaceFile, HandlerInterfaceData{
		Package: "handler", Module: navModule, Service: "nav",
		StructName: "navHandler", IfaceName: "NavHandler", CtorName: "NewNavHandler",
		WiringFnName:    "NavController",
		ControllerIface: "NavController", ControllerCtor: "NewNavController",
		StoreCtor: "NewNavStore", ReadDBs: []string{"EBATEST", "MF"},
		Methods: []string{"NavList", "NavHistory", "SipFreedem"},
	})
	for _, want := range []string{
		"type navHandler struct {",
		"NavList(c *gin.Context)",
		"func NewNavHandler(controller controller.NavController) NavHandler {",
		"func NavController(repo repo.DataObject) controller.NavController {",
		"repo.Databases.ReadDatabase.EBATEST, repo.Databases.ReadDatabase.MF",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("handler interface missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderHandlerMethod(t *testing.T) {
	out := render(t, HandlerMethod, HandlerMethodData{
		StructName: "navHandler", Name: "NavList", RequestType: "models.NavRequest",
	})
	for _, want := range []string{
		"func (f *navHandler) NavList(c *gin.Context) {",
		"logger.Log(c).Debug(\"SERVICE-START\")",
		"defer logger.Log(c).Debug(\"SERVICE-END\")",
		"gCtx := &network.GinContext{Context: c}",
		"gCtx.BadRequestJSON(err, request)",
		"gCtx.FailureJSON(err)",
		"gCtx.NoContentJSON()",
		"gCtx.SuccessJSON(data)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("handler method missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderRouterSnippet(t *testing.T) {
	out := render(t, RouterSnippet, RouterData{
		Service: "nav",
		Routes: []RouteSpec{
			{Path: "/mfnavhistory", Handler: "NavHistory"},
			{Path: "/mfnavschemelist", Handler: "NavList"},
			{Path: "/mf_sipfreedem_schemes", Handler: "SipFreedem"},
		},
	})
	for _, want := range []string{
		"nav := v1.Group(\"/nav\")",
		"nav.POST(\"/mfnavhistory\", obj.NavHistory)",
		"nav.POST(\"/mfnavschemelist\", obj.NavList)",
		"nav.POST(\"/mf_sipfreedem_schemes\", obj.SipFreedem)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("router snippet missing %q\n---\n%s", want, out)
		}
	}
}

// ---- Test templates (PRD-2026-09-09 GT-2) ----

const (
	testLoggerPkg   = navModule + "/pkg/logger"
	testModelsPkg   = navModule + "/pkg/services/nav/models"
	testUtilsPkg    = navModule + "/pkg/utils"
	testNetworkPkg  = navModule + "/pkg/network"
	testDBPkg       = navModule + "/pkg/services/nav/db"
	testCtrlPkg     = navModule + "/pkg/services/nav/controller"
	testMockGenCmd  = "mockgen -source=pkg/services/nav/db/interface.go -destination=pkg/services/nav/db/mock_store.go -package=db"
	testCoverageCmd = "go test pkg/services/nav/db/nav_test.go pkg/services/nav/db/nav.go pkg/services/nav/db/interface.go -v -coverprofile=coverage.txt -covermode count && go tool cover -html=coverage.txt"
)

func parseTestFile(t *testing.T, name, src string) {
	t.Helper()
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution); err != nil {
		t.Fatalf("composed test file %s does not parse: %v\n---\n%s", name, err, src)
	}
}

func TestRenderTestDBFile(t *testing.T) {
	navDetailsMethod := render(t, TestDBMethod, TestDBMethodData{
		SuiteName: "NavStoreSuite", StoreVar: "navStore", Name: "GetNavDetails",
		Regex:      `^SELECT (.+) FROM DEMO_COMPANY, DEMO_SCHEME, DEMO_PRICE WHERE (.+)$`,
		Cols:       []string{"COMP_CD", "SCH_CD", "NAV"},
		Row:        []string{"852", "123", "50"},
		ExpectType: "[]*models.NavDetails",
		ExpectExpr: `[]*models.NavDetails{{CompCd: sql.NullString{String: "852", Valid: true}}}`,
		CallArgs:   []string{`"852"`},
	})
	getCountMethod := render(t, TestDBMethod, TestDBMethodData{
		SuiteName: "NavStoreSuite", StoreVar: "navStore", Name: "GetCount",
		Regex:      `^SELECT (.+) FROM DEMO_ACCOUNT_MAP WHERE (.+)$`,
		Cols:       []string{"count"},
		Row:        []string{"0"},
		NoRows:     true,
		NoRowsExpr: "0",
		ExpectType: "int64",
		ExpectExpr: "0",
		CallArgs:   []string{`"8500011155"`},
	})
	out := render(t, TestDBFile, TestDBFileData{
		Package: "db", LoggerPkg: testLoggerPkg, ModelsPkg: testModelsPkg, UtilsPkg: testUtilsPkg,
		SuiteName: "NavStoreSuite", StoreVar: "navStore", IfaceName: "NavStore",
		CtorCall: "NewNavStore(nil, suite.sqlDB)", NeedsSQL: true, NeedsModels: true,
		Methods: []string{navDetailsMethod, getCountMethod},
	})
	parseTestFile(t, "nav_test.go", out)

	for _, want := range []string{
		"type NavStoreSuite struct {",
		"func TestNavStoreSuite(t *testing.T) {",
		"logger.LoggerInit(\"\", -1)",
		"utils.NewSqlxMockDB()",
		"NewNavStore(nil, suite.sqlDB)",
		"func (suite *NavStoreSuite) TestGetNavDetails() {",
		`desc:          "SQLError",`,
		`sqlmock.NewRows([]string{ "COMP_CD", "SCH_CD", "NAV", }).AddRow("852", "123", "50", ),`,
		`ExpectQuery("^SELECT (.+) FROM DEMO_COMPANY, DEMO_SCHEME, DEMO_PRICE WHERE (.+)$")`,
		`suite.navStore.GetNavDetails(suite.ctx, "852")`,
		"func (suite *NavStoreSuite) TestGetCount() {",
		"Success-NoRows",
		`suite.navStore.GetCount(suite.ctx, "8500011155")`,
		"assert.ErrorContains(t, err, testCase.expectedError)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("db test file missing %q\n---\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\"database/sql\"") || !strings.Contains(out, testModelsPkg) {
		t.Errorf("db test file needs sql/models imports\n---\n%s", out)
	}
}

func TestRenderTestControllerFile(t *testing.T) {
	navListMethod := render(t, TestControllerMethod, TestControllerMethodData{
		SuiteName: "NavControllerSuiteController", StoreVar: "navStore", CtrlVar: "navController",
		Name: "NavList", StoreCall: "GetNavDetails", StoreArgs: []string{`"FML_COMP_CD"`},
		ReqFields:  []ReqField{{Name: "CompCode", Value: "FML_COMP_CD"}},
		ReqExpr:    "models.NavRequest{CompCode: testCase.CompCode}",
		MockReturn: `[]any{[]*models.NavDetails{{CompCd: sql.NullString{String: "FML_COMP_CD", Valid: true}}}, nil}`,
		ExpectType: "[]*models.NavResponse",
		ExpectExpr: `[]*models.NavResponse{{CompCode: "FML_COMP_CD"}}`,
	})
	out := render(t, TestControllerFile, TestControllerFileData{
		Package: "controller", DBPkg: testDBPkg, LoggerPkg: testLoggerPkg, ModelsPkg: testModelsPkg,
		SuiteName: "NavControllerSuiteController", StoreVar: "navStore", CtrlVar: "navController", CtrlIface: "NavController",
		MockType: "db.MockNavStore",
		MockCtor: "db.NewMockNavStore(suite.mockController)",
		CtorCall: "NewNavController(suite.navStore)",
		NeedsSQL: true, NeedsTime: true,
		Methods: []string{navListMethod},
	})
	parseTestFile(t, "nav_test.go", out)

	for _, want := range []string{
		"mockController *gomock.Controller",
		"navStore *db.MockNavStore",
		"db.NewMockNavStore(suite.mockController)",
		"NewNavController(suite.navStore)",
		"suite.mockController.Finish()",
		"func (suite *NavControllerSuiteController) TestNavList() {",
		"CompCode:     \"FML_COMP_CD\",",
		"request := &models.NavRequest{CompCode: testCase.CompCode}",
		"GetNavDetails(gomock.Any(), \"FML_COMP_CD\")",
		"Return(testCase.mockInput...)",
		"suite.navController.NavList(suite.ctx, request)",
		"assert.ErrorContains(t, err, testCase.expectedError)",
		"assert.Equal(t, actualOutput, testCase.expectedOutput)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("controller test file missing %q\n---\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\"database/sql\"") || !strings.Contains(out, "\"time\"") {
		t.Errorf("controller test file needs conditional sql/time imports\n---\n%s", out)
	}
}

func TestRenderTestHandlerFile(t *testing.T) {
	navListMethod := render(t, TestHandlerMethod, TestHandlerMethodData{
		SuiteName: "NavHandlerSuite", CtrlMockVar: "navController", HandlerVar: "navHandler",
		Name:         "NavList",
		ReqFields:    []ReqField{{Name: "CompCode", Value: "FML_COMP_CD"}},
		ReqInit:      "models.NavRequest{CompCode: testCase.CompCode}",
		SuccessInput: `[]any{[]*models.NavResponse{{CompCode: "FML_COMP_CD"}}, nil}`,
		RespType:     "[]*models.NavResponse",
	})
	out := render(t, TestHandlerFile, TestHandlerFileData{
		Package: "handler", ControllerPkg: testCtrlPkg, LoggerPkg: testLoggerPkg,
		ModelsPkg: testModelsPkg, NetworkPkg: testNetworkPkg, UtilsPkg: testUtilsPkg,
		SuiteName: "NavHandlerSuite", CtrlMockVar: "navController", CtrlMockType: "controller.MockNavController",
		MockCtor:   "controller.NewMockNavController(gomock.NewController(suite.T()))",
		HandlerVar: "navHandler", IfaceName: "NavHandler", CtorCall: "NewNavHandler(suite.navController)",
		Methods: []string{navListMethod},
	})
	parseTestFile(t, "nav_test.go", out)

	for _, want := range []string{
		"gin.SetMode(gin.TestMode)",
		"utils.RegisterValidations(v)",
		"controller.NewMockNavController(gomock.NewController(suite.T()))",
		"NewNavHandler(suite.navController)",
		"func (suite *NavHandlerSuite) TestNavList() {",
		"desc: \"NavListError\",",
		"CompCode: \"FML_COMP_CD\",",
		"expectedErrorHttpCode: http.StatusInternalServerError,",
		"request := models.NavRequest{CompCode: testCase.CompCode}",
		"utils.CreateTestGinContext(http.MethodPost, request, nil, nil, nil)",
		"suite.navHandler.NavList(ctx)",
		"var httpResponse network.HttpResponse",
		"utils.TypeConverter[[]*models.NavResponse](httpResponse.Data)",
		"desc: \"Failure\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("handler test file missing %q\n---\n%s", want, out)
		}
	}
}
