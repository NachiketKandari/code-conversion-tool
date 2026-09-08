// Per-function fact extraction (PRD-2026-09-09 GT-D3): gentest reads the
// converted Go code itself — never the conversion IR — so it works on any
// tree, including hand-edited ones. Extraction is parse-only (no type
// checking): shapes are recognized by the reference conventions (store
// receivers, `query := `+backtick literals, sqlx call names, request
// selectors).
package testgen

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Param is one function parameter (name + type as written).
type Param struct {
	Name string
	Type string
}

// dbFact is one store method's extracted shape.
type dbFact struct {
	Name    string
	CtxName string
	Params  []Param
	Query   string
	Tables  []string // FROM list in source order
	Shape   string   // multi | single | scalar | dml
	RowType string   // models.NavDetails (as written)
	Scalar  string   // int64 / string / float64
	Args    []string // call args after the query arg, rendered verbatim
}

// storeCall is one dependency call inside a controller body.
type storeCall struct {
	Method string
	Args   []string // rendered verbatim, ctx arg dropped
}

// ctrlFact is one controller endpoint's extracted shape.
type ctrlFact struct {
	Name         string
	CtxName      string
	RequestType  string // models.NavRequest
	ResponseType string
	StoreCalls   []storeCall
	Passthrough  bool   // body returns the store result without field mapping
	Src          string // verbatim function source (LLM prompt input)
}

// handlerFact is one gin handler's extracted shape.
type handlerFact struct {
	Name        string
	RequestType string
	CtrlCall    string
}

// fieldInfo is one struct field of the models layer.
type fieldInfo struct {
	Name string
	Type string
	JSON string
	DB   string
}

// modelsInfo is the service's models inventory (model/ and models/ both).
type modelsInfo struct {
	Structs map[string][]fieldInfo
}

// layerFacts is one layer directory's extraction outcome.
type layerFacts struct {
	DB          map[string]*dbFact
	Ctrl        map[string]*ctrlFact
	Handler     map[string]*handlerFact
	DBCtor      string
	DBCtorArgs  int
	CtrlCtor    string
	HandlerCtor string
}

// sourceFiles lists the layer's non-test, non-mock .go files.
func sourceFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" ||
			strings.HasSuffix(name, "_test.go") ||
			strings.HasSuffix(name, "_mock.go") || strings.HasPrefix(name, "mock_") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// testFiles lists the layer's *_test.go files.
func testFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".go" && strings.HasSuffix(e.Name(), "_test.go") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// extractLayer parses one layer directory into facts; unparseable files are
// skipped (the scan already warned).
func extractLayer(dir string, layer string) *layerFacts {
	lf := &layerFacts{DB: map[string]*dbFact{}, Ctrl: map[string]*ctrlFact{}, Handler: map[string]*handlerFact{}}
	fset := token.NewFileSet()
	for _, name := range sourceFiles(dir) {
		af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			switch {
			case layer == "db":
				if fd.Recv != nil {
					if f := extractDBFact(fd, fset); f != nil {
						lf.DB[f.Name] = f
					}
				} else if strings.HasPrefix(fd.Name.Name, "New") && fd.Type.Results != nil && fd.Type.Results.NumFields() == 1 {
					if lf.DBCtor == "" {
						lf.DBCtor = fd.Name.Name
						lf.DBCtorArgs = fd.Type.Params.NumFields()
					}
				}
			case layer == "controller":
				if fd.Recv != nil {
					if f := extractCtrlFact(fd, fset); f != nil {
						lf.Ctrl[f.Name] = f
					}
				} else if strings.HasPrefix(fd.Name.Name, "New") && fd.Type.Results != nil && fd.Type.Results.NumFields() == 1 && lf.CtrlCtor == "" {
					lf.CtrlCtor = fd.Name.Name
				}
			case layer == "handler":
				if fd.Recv != nil {
					if f := extractHandlerFact(fd, fset); f != nil {
						lf.Handler[f.Name] = f
					}
				} else if strings.HasPrefix(fd.Name.Name, "New") && fd.Type.Results != nil && fd.Type.Results.NumFields() == 1 && lf.HandlerCtor == "" {
					lf.HandlerCtor = fd.Name.Name
				}
			}
		}
	}
	return lf
}

// extractModels parses the service's models dir (model/ or models/).
func extractModels(serviceDir string) *modelsInfo {
	mi := &modelsInfo{Structs: map[string][]fieldInfo{}}
	dir := ""
	for _, cand := range []string{"models", "model"} {
		if fi, err := os.Stat(filepath.Join(serviceDir, cand)); err == nil && fi.IsDir() {
			dir = filepath.Join(serviceDir, cand)
			break
		}
	}
	if dir == "" {
		return mi
	}
	fset := token.NewFileSet()
	for _, name := range sourceFiles(dir) {
		af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				var fields []fieldInfo
				for _, f := range st.Fields.List {
					info := fieldInfo{Type: renderExpr(f.Type, fset)}
					for _, n := range f.Names {
						info.Name = n.Name
						break
					}
					if f.Tag != nil {
						tag, _ := strconv.Unquote(f.Tag.Value)
						info.JSON = tagValue(tag, "json")
						info.DB = tagValue(tag, "db")
					}
					if info.Name != "" {
						fields = append(fields, info)
					}
				}
				mi.Structs[ts.Name.Name] = fields
			}
		}
	}
	return mi
}

func tagValue(tag, key string) string {
	for _, part := range strings.Split(tag, " ") {
		if strings.HasPrefix(part, key+":") {
			v, _ := strconv.Unquote(strings.TrimPrefix(part, key+":"))
			return v
		}
	}
	return ""
}

// extractDBFact recognizes the store-method conventions: ctx first param, a
// backtick query literal, one sqlx call (SelectContext/GetContext/
// ExecContext), row/scalar type from the scan target's var decl.
func extractDBFact(fd *ast.FuncDecl, fset *token.FileSet) *dbFact {
	f := &dbFact{Name: fd.Name.Name}
	if fd.Type.Params != nil && fd.Type.Params.NumFields() > 0 {
		if names := fd.Type.Params.List[0].Names; len(names) > 0 {
			f.CtxName = names[0].Name
		}
	}
	for _, p := range fd.Type.Params.List {
		for _, n := range p.Names {
			f.Params = append(f.Params, Param{Name: n.Name, Type: renderExpr(p.Type, fset)})
		}
	}
	var queryLit string
	var call *ast.CallExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, rhs := range x.Rhs {
				if lit, ok := rhs.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.HasPrefix(lit.Value, "`") {
					q, err := strconv.Unquote(lit.Value)
					if err == nil {
						queryLit = q
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "SelectContext", "GetContext", "ExecContext":
					call = x
				}
			}
		}
		return true
	})
	if call == nil {
		return nil
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		switch sel.Sel.Name {
		case "SelectContext":
			f.Shape = "multi"
		case "GetContext":
			f.Shape = "scan" // single-struct or scalar, resolved below
		case "ExecContext":
			f.Shape = "dml"
		}
	}
	for i, a := range call.Args {
		if i < 2 {
			continue // ctx, scan target
		}
		if lit, ok := a.(*ast.Ident); ok && lit.Name == "query" {
			continue
		}
		f.Args = append(f.Args, renderExpr(a, fset))
	}
	if queryLit != "" {
		f.Query = queryLit
		f.Tables = tablesOf(queryLit)
	}
	if f.Shape == "multi" || f.Shape == "scan" {
		if len(call.Args) > 1 {
			if star, ok := call.Args[1].(*ast.UnaryExpr); ok {
				if id, ok := star.X.(*ast.Ident); ok {
					f.RowType, f.Scalar = scanTargetType(fd, id.Name, fset)
					if f.Shape == "scan" {
						if f.Scalar != "" {
							f.Shape = "scalar"
						} else if f.RowType != "" {
							f.Shape = "single"
						}
					}
				}
			}
		}
	}
	return f
}

// scanTargetType finds the var decl for the scan target inside the method
// body: `var x []*models.T`, `var x models.T`, or `var x int64`.
func scanTargetType(fd *ast.FuncDecl, varName string, fset *token.FileSet) (rowType, scalar string) {
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeclStmt)
		if !ok {
			return true
		}
		gd, ok := d.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR || len(gd.Specs) == 0 {
			return true
		}
		vs, ok := gd.Specs[0].(*ast.ValueSpec)
		if !ok || len(vs.Names) == 0 || vs.Names[0].Name != varName || vs.Type == nil {
			return true
		}
		t := renderExpr(vs.Type, fset)
		switch {
		case strings.HasPrefix(t, "[]*"):
			rowType = strings.TrimPrefix(t, "[]*")
		case strings.HasPrefix(t, "models."):
			rowType = t
		default:
			scalar = t
		}
		return false
	})
	return rowType, scalar
}

// tablesOf extracts the FROM table list from a SQL query (source order,
// aliases dropped).
func tablesOf(query string) []string {
	up := strings.ToUpper(query)
	i := strings.Index(up, " FROM ")
	if i < 0 {
		return nil
	}
	rest := query[i+len(" FROM "):]
	end := len(rest)
	for _, kw := range []string{"\nWHERE", " WHERE", "\nORDER", " ORDER", "\nGROUP", " GROUP", "\nJOIN", " JOIN", "\nHAVING", " HAVING"} {
		if j := strings.Index(strings.ToUpper(rest), kw); j >= 0 && j < end {
			end = j
		}
	}
	segment := rest[:end]
	var out []string
	for _, t := range strings.Split(segment, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if sp := strings.Fields(t); len(sp) > 0 {
			t = sp[0]
		}
		out = append(out, t)
	}
	return out
}

// extractCtrlFact recognizes controller endpoints: (ctx context.Context,
// request *models.X) → (…, error), collecting `recv.store.M(...)` calls in
// body order.
func extractCtrlFact(fd *ast.FuncDecl, fset *token.FileSet) *ctrlFact {
	f := &ctrlFact{Name: fd.Name.Name, Passthrough: true}
	if fd.Type.Params == nil || fd.Type.Params.NumFields() < 2 {
		return nil
	}
	first := fd.Type.Params.List[0]
	if renderExpr(first.Type, fset) != "context.Context" {
		return nil
	}
	if len(first.Names) > 0 {
		f.CtxName = first.Names[0].Name
	}
	reqField := fd.Type.Params.List[len(fd.Type.Params.List)-1]
	rt := renderExpr(reqField.Type, fset)
	if !strings.HasPrefix(rt, "*models.") {
		return nil
	}
	f.RequestType = strings.TrimPrefix(rt, "*")
	if fd.Type.Results != nil && fd.Type.Results.NumFields() > 0 {
		f.ResponseType = renderExpr(fd.Type.Results.List[0].Type, fset)
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != "store" {
			return true
		}
		sc := storeCall{Method: sel.Sel.Name}
		for i, a := range call.Args {
			if i == 0 {
				continue // ctx
			}
			sc.Args = append(sc.Args, renderExpr(a, fset))
		}
		f.StoreCalls = append(f.StoreCalls, sc)
		return true
	})
	// Field mapping (composite literals over models types or .String
	// projections) is what the deterministic template cannot shape.
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if t := renderExpr(x.Type, fset); strings.Contains(t, "models.") {
				f.Passthrough = false
			}
		case *ast.SelectorExpr:
			if x.Sel.Name == "String" {
				f.Passthrough = false
			}
		}
		return true
	})
	if len(f.StoreCalls) == 0 {
		return nil
	}
	f.Src = renderNode(fd, fset)
	return f
}

// extractHandlerFact recognizes gin handlers: (c *gin.Context), a request
// var decl, and one controller call.
func extractHandlerFact(fd *ast.FuncDecl, fset *token.FileSet) *handlerFact {
	f := &handlerFact{Name: fd.Name.Name}
	if fd.Type.Params == nil || fd.Type.Params.NumFields() != 1 || renderExpr(fd.Type.Params.List[0].Type, fset) != "*gin.Context" {
		return nil
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR && len(gd.Specs) > 0 {
				if vs, ok := gd.Specs[0].(*ast.ValueSpec); ok && vs.Type != nil {
					t := renderExpr(vs.Type, fset)
					if strings.HasPrefix(t, "models.") {
						f.RequestType = strings.TrimPrefix(t, "models.")
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "controller" {
					f.CtrlCall = sel.Sel.Name
				}
			}
		}
		return true
	})
	if f.CtrlCall == "" {
		return nil
	}
	return f
}

// renderExpr prints an expression/type back to Go source.
func renderExpr(e ast.Expr, fset *token.FileSet) string {
	if e == nil {
		return ""
	}
	return renderNode(e, fset)
}

// renderNode prints any AST node back to Go source.
func renderNode(n ast.Node, fset *token.FileSet) string {
	if n == nil {
		return ""
	}
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, n); err != nil {
		return ""
	}
	return buf.String()
}
