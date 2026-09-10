package templates

import "strings"

// FieldSpec is one struct field in a models struct (PRD §4.8.3: json tags =
// raw FML field names; db tags = SELECT aliases; gin binding validators).
type FieldSpec struct {
	Name      string // Go field name, e.g. CompCode
	Type      string // string, sql.NullString, sql.NullTime, int64, ...
	JSONTag   string // raw FML name, e.g. FML_COMP_CD (request/response fields)
	OmitEmpty bool   // response fields carry ,omitempty
	DBTag     string // SELECT alias, e.g. COMP_CD (row-struct fields)
	Binding   string // gin validator list, e.g. "required,matchaccount"
	ErrMsg    string // custom validator message rendered as error:"..."
}

// Tag renders the struct tag for the field.
func (f FieldSpec) Tag() string {
	var parts []string
	switch {
	case f.JSONTag != "":
		tag := f.JSONTag
		if f.OmitEmpty {
			tag += ",omitempty"
		}
		parts = append(parts, "json:\""+tag+"\"")
	case f.DBTag != "":
		parts = append(parts, "db:\""+f.DBTag+"\"")
	}
	if f.Binding != "" {
		parts = append(parts, "binding:\""+f.Binding+"\"")
	}
	if f.ErrMsg != "" {
		parts = append(parts, "error:\""+f.ErrMsg+"\"")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// HasNullType reports whether any field uses a database/sql Null* type.
func HasNullType(structs []StructSpec) bool {
	for _, s := range structs {
		for _, f := range s.Fields {
			if strings.HasPrefix(f.Type, "sql.Null") {
				return true
			}
		}
	}
	return false
}

// StructSpec is one struct in the models file.
type StructSpec struct {
	Name   string
	Fields []FieldSpec
}

// ModelFileData renders models.go.
type ModelFileData struct {
	Package string // "models"
	Structs []StructSpec
}

// HasNullTypes reports whether the file needs `import "database/sql"`.
func (d ModelFileData) HasNullTypes() bool { return HasNullType(d.Structs) }

// ParamSpec is one function parameter.
type ParamSpec struct{ Name, Type string }

// DBMethodData renders a store method (PRD §4.8.2 contract: ctx first arg,
// positional :1 binds, columns aliased AS "COL" matching db tags, GetContext /
// SelectContext / ExecContext).
type DBMethodData struct {
	Receiver   string // g
	StoreType  string // store
	Name       string // GetNavDetails
	Doc        string // optional doc comment (multi-line, already commented)
	CtxName    string // c (nav example) or ctx
	Params     []ParamSpec
	Query      string // backtick-free SQL body placed inside a raw string literal
	VarName    string // scan target variable, e.g. navDetails / dateinfo / count
	RowType    string // models.NavDetails ("" for scalar results)
	Scalar     string // int64 etc. when RowType == ""
	Multi      bool   // SelectContext into []*T
	TxParam    bool   // takes tx *sqlx.Tx as second parameter (DML / tx-variant read)
	SuccessMsg string // insert/update debug log after a successful exec
}

// ArgList renders the comma-separated bind argument names.
func (d DBMethodData) ArgList() string {
	names := make([]string, len(d.Params))
	for i, p := range d.Params {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

// ReturnType renders the Go return type pair.
func (d DBMethodData) ReturnType() string {
	switch {
	case d.Multi && d.RowType != "":
		return "[]*" + d.RowType
	case d.RowType != "":
		return "*" + d.RowType
	default:
		return d.Scalar
	}
}

// DBInterfaceData renders db/interface.go (store struct + <Name>Store
// interface + constructor; the interface accretes one line per generated
// method — PRD §4.2.3).
type DBInterfaceData struct {
	Package      string // "db"
	StoreType    string // "store"
	IfaceName    string // "NavStore"
	CtorName     string // "NewNavStore"
	WithGorm     bool   // store carries the legacy *gorm.DB handle alongside sqlx
	ModelsPkg    string // models package import path
	ExtraImports []string
	Methods      []string // rendered signatures, one line each
}

// ControllerInterfaceData renders controller/interface.go (PRD §4.8.6).
type ControllerInterfaceData struct {
	Package    string // "controller"
	StructName string // "navController"
	IfaceName  string // "NavController"
	CtorName   string // "NewNavController"
	DBPkg      string // db package import path
	ModelsPkg  string // models package import path
	StoreIface string // "db.NavStore"
	Methods    []string
}

// FnStub is one panicking placeholder for an unresolved external fn
// (stub-and-carry-on, 2026-09-10): variadic args, int return matching the
// corpus's -1 error convention.
type FnStub struct {
	Name     string // Go name, e.g. fnLongToInt
	Original string // corpus symbol, e.g. fn_long_to_int
}

// FnStubFileData renders controller/fnstubs.go.
type FnStubFileData struct {
	Package string // "controller"
	Stubs   []FnStub
}

// ControllerMethodData renders one controller endpoint method (pure business
// logic + store calls; the body slot is filled by the pipeline/LLM).
type ControllerMethodData struct {
	StructName   string // "navController"
	Name         string // "NavList"
	CtxName      string // "ctx"
	RequestType  string // "models.NavRequest"
	ResponseType string // "models.NavResponse"
	Body         string // logic including any return statements
}

// HandlerInterfaceData renders handler/interface.go: handler struct + interface
// + constructor + the wiring function building the store from the existing repo
// (PRD §4.8.6, OQ11).
type HandlerInterfaceData struct {
	Package         string   // "handler"
	Module          string   // "mutual-fund-be"
	Service         string   // "nav"
	StructName      string   // "navHandler"
	IfaceName       string   // "NavHandler"
	CtorName        string   // "NewNavHandler"
	WiringFnName    string   // "NavController" (repo.DataObject wiring function)
	ControllerIface string   // "NavController"
	ControllerCtor  string   // "NewNavController"
	StoreCtor       string   // "NewNavStore"
	ReadDBs         []string // "EBATEST", "MF"
	Methods         []string
}

// ReadDBArgList renders the repo.Databases.ReadDatabase.* constructor args.
func (h HandlerInterfaceData) ReadDBArgList() string {
	args := make([]string, len(h.ReadDBs))
	for i, db := range h.ReadDBs {
		args[i] = "repo.Databases.ReadDatabase." + db
	}
	return strings.Join(args, ", ")
}

// HandlerMethodData renders one gin handler method (PRD §4.8.6 handler shape).
type HandlerMethodData struct {
	StructName  string // "navHandler"
	Name        string // "NavList"
	RequestType string // "models.NavRequest"
}

// DBSelectTxData renders a tx-variant single-row read (GetMarks shape in
// examples/dbTransactionEx.txt): a mid-flow read inside a sequential-crux flow
// runs on tx, takes `tx *sqlx.Tx` as its second parameter (PRD §4.2.3 variant
// rule) and propagates every error — including sql.ErrNoRows — to the flow.
type DBSelectTxData struct {
	Receiver  string // g
	StoreType string // store
	Name      string // GetMarks
	CtxName   string // ctx
	Params    []ParamSpec
	Query     string
	VarName   string // "marks"
	ScanType  string // "sql.NullString"
	Extract   string // "marks.String"
	Zero      string // `""` — zero-value return on the error path
	Return    string // "string"
}

// ArgList renders the comma-separated bind argument names.
func (d DBSelectTxData) ArgList() string {
	names := make([]string, len(d.Params))
	for i, p := range d.Params {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

// ControllerTxMethodData renders a controller endpoint that wraps a
// sequential-crux flow in utils.ExecTransaction (AssessQnA shape in
// examples/controllerTxSignature.txt; decision 27): pre-flow reads run outside
// the tx, every call inside the closure takes tx, post-commit reads assemble
// the response.
type ControllerTxMethodData struct {
	Receiver     string // "c"
	StructName   string // "controller"
	Name         string // "AssessQnA"
	CtxName      string // "ctx"
	RequestType  string // "models.AssessQnARequest"
	ResponseType string // "models.AssessQnAResponse"
	PreFlow      string // code before the tx opens (errors → return nil, err)
	TxBody       string // crux sequence; ends with `return nil`
	PostFlow     string // code after the wrapper; ends with the return statement
}

// RouteSpec is one router entry.
type RouteSpec struct {
	Path    string // "/mfnavhistory"
	Handler string // "NavHistory"
}

// RouterData renders the router snippet (examples/router.txt shape).
type RouterData struct {
	Service string // "nav"
	Routes  []RouteSpec
}

// ---- Test templates (PRD-2026-09-09 GT-2, distilled from examples/nav) ----

// TestHeaderData renders the mockgen + coverage comment header carried by
// every generated test file (examples/nav/*_test.txt convention).
type TestHeaderData struct {
	MockGenCmd  string // full mockgen command line
	CoverageCmd string // full go test + cover command line
}

// TestDBFileData renders a db layer test file: suite struct + runner +
// SetupSuite + per-method test blocks (examples/nav/db/nav_test.txt shape).
type TestDBFileData struct {
	TestHeaderData
	Package     string // db
	LoggerPkg   string // <module>/pkg/logger
	ModelsPkg   string // models import path
	UtilsPkg    string // <module>/pkg/utils
	SuiteName   string // NavStoreSuite
	StoreVar    string // navStore
	IfaceName   string // NavStore
	CtorCall    string // NewNavStore(nil, suite.sqlDB) — pre-rendered
	NeedsSQL    bool   // database/sql import (sql.Null* expected exprs)
	NeedsModels bool   // models import (row-struct expected exprs)
	Methods     []string
}

// TestDBMethodData renders one suite method: table-driven sqlmock cases
// (SQLError / [Success-NoRows] / Success) over one store call. Braced
// literals arrive pre-rendered (ExpectExpr/NoRowsExpr) so templates never
// fight text/template's {{ parsing.
type TestDBMethodData struct {
	SuiteName  string   // NavStoreSuite
	StoreVar   string   // navStore
	Name       string   // GetNavDetails
	Regex      string   // ExpectQuery regex over the FROM table list
	Cols       []string // mock row columns (db tags)
	Row        []string // Success row values (assumed fixture source)
	NoRows     bool     // scalar GetContext: extra Success-NoRows case
	NoRowsExpr string   // expectedOutput literal for the NoRows case
	ExpectType string   // []*models.NavDetails / int64
	ExpectExpr string   // Success expectedOutput literal
	CallArgs   []string // rendered store-call args after ctx
}

// TestControllerFileData renders a controller test file
// (examples/nav/controller/nav_test.txt shape): gomock store mock built in
// SetupTest, torn down with Finish, suite fields ctx/mockController/store/
// controller.
type TestControllerFileData struct {
	TestHeaderData
	Package   string // controller
	DBPkg     string // db package import (Mock<StoreIface> lives there)
	LoggerPkg string
	ModelsPkg string
	SuiteName string // DemoControllerSuiteController
	StoreVar  string // demoStore (the db.Mock<StoreIface> field)
	CtrlVar   string // demoController (controller under test)
	CtrlIface string // DemoController
	MockType  string // db.MockDemoStore
	MockCtor  string // db.NewMockDemoStore(suite.mockController)
	CtorCall  string // NewDemoController(suite.demoStore)
	NeedsSQL  bool   // database/sql import (sql.Null* mock-row literals)
	NeedsTime bool   // time import (sql.NullTime / time.Now literals)
	Methods   []string
}

// TestControllerMethodData renders one suite method: request fields as case
// fields, guarded EXPECT (gomock.Any() ctx + concrete args) in body-call
// order, request built from the case fields, ErrorContains / NoError+Equal
// validations.
type TestControllerMethodData struct {
	SuiteName  string     // DemoControllerSuiteController
	StoreVar   string     // demoStore
	CtrlVar    string     // demoController
	Name       string     // OrderDirect
	StoreCall  string     // store dependency (GetOrderDetails)
	StoreArgs  []string   // EXPECT args after ctx (concrete literals)
	ReqFields  []ReqField // request fields driving the case struct
	ReqExpr    string     // models.OrderRequest{CompCode: testCase.CompCode} — pointer added by the template
	MockReturn string     // []any{<store row literal>, nil} — pre-rendered
	ExpectType string     // []*models.OrderResponse
	ExpectExpr string     // success expectedOutput literal — pre-rendered
}

// TestHandlerFileData renders a handler test file
// (examples/nav/handler/nav_test.txt shape).
type TestHandlerFileData struct {
	TestHeaderData
	Package       string // handler
	ControllerPkg string // controller import (MockNavController)
	LoggerPkg     string
	ModelsPkg     string
	NetworkPkg    string // <module>/pkg/network
	UtilsPkg      string
	SuiteName     string // NavHandlerSuite
	CtrlMockVar   string // navController
	CtrlMockType  string // controller.MockNavController
	MockCtor      string // controller.NewMockNavController(gomock.NewController(suite.T()))
	HandlerVar    string // navHandler
	IfaceName     string // NavHandler
	CtorCall      string // NewNavHandler(suite.navController)
	Methods       []string
}

// ReqField is one request-struct field mirrored into the handler test's
// case struct (GT-D2: the example's per-request case fields, derived).
type ReqField struct {
	Name  string // CompCode
	Value string // assumed fixture value
}

// TestHandlerMethodData renders one suite method: gin-context table cases
// (Error 500 / Failure 204 / Success 200) over the mocked controller.
type TestHandlerMethodData struct {
	SuiteName    string     // NavHandlerSuite
	CtrlMockVar  string     // navController
	HandlerVar   string     // navHandler
	Name         string     // NavList
	ReqFields    []ReqField // request fields driving the case struct
	ReqInit      string     // models.NavRequest{CompCode: testCase.CompCode}
	SuccessInput string     // []any{<response literal>, nil} — pre-rendered
	RespType     string     // []*models.NavResponse
}
