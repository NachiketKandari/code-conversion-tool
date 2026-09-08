// Per-method block renderers: each turns one unit's extracted facts plus
// fixture values into a single table-driven suite-method block via the
// test templates (PRD-2026-09-09 GT-3).
package testgen

import (
	"fmt"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/templates"
)

// dbCols returns the mock-row columns for a db fact: the row struct's
// db-tagged fields in declaration order, or the scalar heuristic column.
func dbCols(sc *serviceCtx, f *dbFact) []string {
	if f.RowType == "" {
		if strings.Contains(strings.ToLower(f.Name), "count") {
			return []string{"count"}
		}
		return []string{"value"}
	}
	base := structBase(f.RowType)
	var cols []string
	for _, fl := range sc.models.Structs[base] {
		if fl.DB != "" {
			cols = append(cols, fl.DB)
		}
	}
	return cols
}

// dbColFields pairs each column with its field name + Go type (expected
// struct literals).
func dbColFields(sc *serviceCtx, f *dbFact) []fieldInfo {
	if f.RowType == "" {
		return nil
	}
	base := structBase(f.RowType)
	var out []fieldInfo
	for _, fl := range sc.models.Structs[base] {
		if fl.DB != "" {
			out = append(out, fl)
		}
	}
	return out
}

// structBase strips slice/pointer wrappers and the models qualifier,
// leaving the bare struct name for the models inventory lookup.
func structBase(t string) string {
	t = strings.TrimPrefix(t, "[]*")
	t = strings.TrimPrefix(t, "[]")
	t = strings.TrimPrefix(t, "*")
	return strings.TrimPrefix(t, "models.")
}

// dbRegex builds the ExpectQuery regex over the FROM table list.
func dbRegex(f *dbFact) string {
	return "^SELECT (.+) FROM " + strings.Join(f.Tables, ", ") + " WHERE (.+)$"
}

// dbExpectType renders the expectedOutput Go type.
func dbExpectType(f *dbFact) string {
	switch f.Shape {
	case "multi":
		return "[]*" + f.RowType
	case "single":
		return "*" + f.RowType
	default:
		return f.Scalar
	}
}

// rowLiteral renders a row-struct composite literal with one element, from
// the fixture source.
func rowLiteral(sc *serviceCtx, rowType string) string {
	base := structBase(rowType)
	var fields []string
	for _, fl := range sc.models.Structs[base] {
		if fl.DB == "" {
			continue
		}
		fields = append(fields, fl.Name+": "+sc.fixtures.ColExpr(fl.DB, fl.Type))
	}
	joined := strings.Join(fields, ", ")
	switch {
	case strings.HasPrefix(rowType, "[]*"):
		if joined == "" {
			return rowType + "{{}}"
		}
		return rowType + "{{" + joined + "}}"
	case strings.HasPrefix(rowType, "*"):
		return "&" + rowType[1:] + "{" + joined + "}"
	default:
		return rowType + "{" + joined + "}"
	}
}

// dbExpectExpr renders the Success expectedOutput literal.
func dbExpectExpr(sc *serviceCtx, f *dbFact) string {
	switch f.Shape {
	case "multi":
		return rowLiteral(sc, "[]*"+f.RowType)
	case "single":
		return rowLiteral(sc, "*"+f.RowType)
	default:
		return sc.fixtures.ZeroExpr(f.Scalar)
	}
}

// dbCallArgs renders the bind literals after ctx, by param type.
func dbCallArgs(sc *serviceCtx, f *dbFact) []string {
	var out []string
	for _, p := range f.Params {
		if p.Type == "context.Context" {
			continue
		}
		out = append(out, sc.fixtures.ArgValue(p.Name, p.Type))
	}
	return out
}

// renderDBMethod renders one db suite method block.
func renderDBMethod(u *unit) (string, error) {
	sc := u.sc
	f := u.db
	cols := dbCols(sc, f)
	prov := templates.NewEmbeddedProvider()
	return prov.Render(templates.TestDBMethod, templates.TestDBMethodData{
		SuiteName:  u.suite,
		StoreVar:   strings.ToLower(sc.name) + "Store",
		Name:       f.Name,
		Regex:      dbRegex(f),
		Cols:       cols,
		Row:        sc.fixtures.RowValues(cols),
		NoRows:     f.Shape == "scalar",
		NoRowsExpr: sc.fixtures.ZeroExpr(f.Scalar),
		ExpectType: dbExpectType(f),
		ExpectExpr: dbExpectExpr(sc, f),
		CallArgs:   dbCallArgs(sc, f),
	})
}

// storeMethodArgs renders EXPECT args for a store call: request-field
// selectors and literals pass through verbatim, anything else degrades to a
// fixture placeholder.
func storeMethodArgs(sc *serviceCtx, args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "request.") || isGoLiteral(a) {
			out = append(out, a)
			continue
		}
		out = append(out, sc.fixtures.ArgValue(strings.TrimPrefix(a, "&"), "string"))
	}
	return out
}

// isGoLiteral reports whether the rendered arg is a basic literal.
func isGoLiteral(a string) bool {
	if a == "" {
		return false
	}
	switch a[0] {
	case '"', '`', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '-':
		return true
	}
	return a == "true" || a == "false" || a == "nil" || strings.HasPrefix(a, "time.")
}

// requestLiteral renders a request-struct composite literal from fixture
// values: models.NavRequest{CompCode: "fml_comp_cd"}.
func requestLiteral(sc *serviceCtx, requestType string) string {
	base := structBase(requestType)
	var fields []string
	for _, fv := range sc.fixtures.FieldValues(base) {
		fields = append(fields, fv[0]+": "+fmt.Sprintf("%q", fv[1]))
	}
	if len(fields) == 0 {
		return "models." + base + "{}"
	}
	return "models." + base + "{" + strings.Join(fields, ", ") + "}"
}

// responseLiteral renders a response-struct literal (one element) from
// fixture values.
func responseLiteral(sc *serviceCtx, responseType string) string {
	base := structBase(responseType)
	var fields []string
	for _, fv := range sc.fixtures.FieldValues(base) {
		fields = append(fields, fv[0]+": "+fmt.Sprintf("%q", fv[1]))
	}
	switch {
	case strings.HasPrefix(responseType, "[]*"):
		inner := "{" + strings.Join(fields, ", ") + "}"
		return responseType + "{" + inner + "}"
	case strings.HasPrefix(responseType, "*"):
		return "&" + responseType[1:] + "{" + strings.Join(fields, ", ") + "}"
	default:
		return responseType + "{" + strings.Join(fields, ", ") + "}"
	}
}

// mockReturnLiteral renders the gomock Return payload for a controller's
// store call: the store's own row shape from its db fact (nil when unknown).
func mockReturnLiteral(sc *serviceCtx, method string) string {
	df := sc.dbFacts.DB[method]
	if df == nil {
		return "nil"
	}
	switch df.Shape {
	case "multi":
		return "[]any{" + rowLiteral(sc, "[]*"+df.RowType) + ", nil}"
	case "single":
		return "[]any{" + rowLiteral(sc, "*"+df.RowType) + ", nil}"
	case "scalar":
		return "[]any{" + sc.fixtures.ZeroExpr(df.Scalar) + ", nil}"
	default:
		return "[]any{nil, nil}"
	}
}

// renderCtrlMethod renders one passthrough controller suite method block.
func renderCtrlMethod(u *unit) (string, error) {
	sc := u.sc
	f := u.ctrl
	call := f.StoreCalls[0]
	prov := templates.NewEmbeddedProvider()
	return prov.Render(templates.TestControllerMethod, templates.TestControllerMethodData{
		SuiteName:  u.suite,
		CtrlVar:    strings.ToLower(sc.name) + "Controller",
		MockVar:    "storeMock",
		Name:       f.Name,
		StoreCall:  call.Method,
		StoreArgs:  storeMethodArgs(sc, call.Args),
		ReqExpr:    requestLiteral(sc, f.RequestType),
		MockReturn: mockReturnLiteral(sc, call.Method),
		ExpectType: f.ResponseType,
		ExpectExpr: responseLiteral(sc, f.ResponseType),
	})
}

// renderHandlerMethod renders one handler suite method block.
func renderHandlerMethod(u *unit) (string, error) {
	sc := u.sc
	f := u.handler
	cf := sc.ctrlFacts.Ctrl[f.CtrlCall]
	reqBase := structBase(f.RequestType)
	var reqFields []templates.ReqField
	var caseRefs []string
	for _, fv := range sc.fixtures.FieldValues(reqBase) {
		reqFields = append(reqFields, templates.ReqField{Name: fv[0], Value: fv[1]})
		caseRefs = append(caseRefs, fv[0]+": testCase."+fv[0])
	}
	prov := templates.NewEmbeddedProvider()
	return prov.Render(templates.TestHandlerMethod, templates.TestHandlerMethodData{
		SuiteName:    u.suite,
		CtrlMockVar:  strings.ToLower(sc.name) + "Controller",
		HandlerVar:   strings.ToLower(sc.name) + "Handler",
		Name:         f.Name,
		ReqFields:    reqFields,
		ReqInit:      "models." + reqBase + "{" + strings.Join(caseRefs, ", ") + "}",
		SuccessInput: "[]any{" + responseLiteral(sc, cf.ResponseType) + ", nil}",
		RespType:     cf.ResponseType,
	})
}
