package templates

import (
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
				{Name: "MatchAccount", Type: "string", JSONTag: "FML_MATCH_ACCNT", Binding: "required,matchaccount", ErrMsg: "Provide valid Match account"},
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
		"MatchAccount string `json:\"FML_MATCH_ACCNT\" binding:\"required,matchaccount\" error:\"Provide valid Match account\"`",
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
		Query:   "SELECT MF_NAV_COMP_CD AS \"COMP_CD\" FROM MF_NAVS WHERE MF_COMP_CD = :1",
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
		Query:   "SELECT COUNT(*) AS \"count\" FROM DMM_D2U_MATCH_MPPNG_MSTR WHERE DMM_MATCH_ACC = :1",
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
