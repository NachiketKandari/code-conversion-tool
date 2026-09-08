// Package testgen is the gentest generation engine (PRD-2026-09-09 GT-3/GT-4):
// one function = one unit = one table-driven test block. Units are
// independent, so they render on a bounded worker pool (`concurrency.workers`)
// and merge in input order — workers=1 output is byte-identical. The
// deterministic templates shape db and handler blocks and passthrough
// controllers; anything else (field-mapping controllers) is the LLM gap,
// filled inside the unit worker with parse-gated bounded retries.
package testgen

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/gen"
	"github.com/Public/convert-tux-to-go/internal/llm"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
	"github.com/Public/convert-tux-to-go/internal/templates"
	"github.com/Public/convert-tux-to-go/internal/testscan"
)

// Options carries one gentest generation run's wiring.
type Options struct {
	BaseDir    string // output root (staged-first: paths.staged or -base)
	Workers    int    // per-function pool bound (concurrency.workers)
	NoLLM      bool
	MaxRetries int
	Audit      *audit.Recorder
	// LLM wiring for the gap-fill seam; zero values degrade to llm-required.
	Client llm.Client
	Budget budget.Budget
}

// Per-function outcome statuses (exported for the command summary).
const (
	StatusTemplate    = "generated"
	StatusLLM         = "generated-llm"
	StatusDesign      = "skipped-by-design"
	StatusLLMNeeded   = "llm-required"
	StatusUnsupported = "unsupported"
)

// UnitResult is one function's outcome.
type UnitResult struct {
	Service string `json:"service"`
	Layer   string `json:"layer"`
	Func    string `json:"func"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// Result is one run's outcome.
type Result struct {
	Files    []string     `json:"files"`
	Units    []UnitResult `json:"units"`
	LLMCalls int          `json:"llm_calls"`
	Gates    []string     `json:"gates,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
}

// unit is one function's generation work item.
type unit struct {
	sc      *serviceCtx
	layer   testscan.Layer
	dir     string
	outFile string // base name, e.g. nav_test.go
	suite   string // suite struct name for the block
	fn      testscan.Func
	db      *dbFact
	ctrl    *ctrlFact
	handler *handlerFact
}

// block is one rendered unit outcome.
type block struct {
	unit    *unit
	method  string // the rendered suite-method source
	status  string
	detail  string
	llmCall int
}

type serviceCtx struct {
	name         string
	dir          string
	moduleRoot   string
	module       string
	models       *modelsInfo
	fixtures     FixtureSource
	dbFacts      *layerFacts
	ctrlFacts    *layerFacts
	handlerFacts *layerFacts
}

// Generate runs the per-function pipeline over the scanned target: build
// units from the scan gaps, render on the worker pool, assemble per-layer
// test files, parse-gate, write under BaseDir, regenerate mocks.
func Generate(ctx context.Context, tgt *testscan.Target, rep *testscan.Report, opts Options) (*Result, error) {
	log := telemetry.Log(ctx)
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	res := &Result{}

	svcs := buildServiceCtxs(rep)
	if len(svcs) == 0 {
		return res, nil
	}

	var units []*unit
	for _, sc := range svcs {
		for _, sr := range rep.Services {
			if sr.Dir != sc.dir {
				continue
			}
			for _, lr := range sr.Layers {
				lf := sc.factsFor(lr.Layer)
				outFile, suite := outNames(sc, lr.Layer, lr.Dir)
				for i := range lr.Funcs {
					if u := buildUnit(sc, lr.Layer, lr.Dir, outFile, suite, lr.Funcs[i], lf, res); u != nil {
						units = append(units, u)
					}
				}
			}
		}
	}

	// Per-function worker pool: units are independent; results merge in
	// input order so workers=1 and workers=N produce identical bytes.
	blocks := make([]*block, len(units))
	var wg sync.WaitGroup
	sem := make(chan struct{}, opts.Workers)
	for i, u := range units {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, u *unit) {
			defer wg.Done()
			defer func() { <-sem }()
			blocks[i] = renderUnit(ctx, u, opts)
		}(i, u)
	}
	wg.Wait()

	for _, b := range blocks {
		if b == nil {
			continue
		}
		res.Units = append(res.Units, UnitResult{
			Service: b.unit.sc.name, Layer: string(b.unit.layer), Func: b.unit.fn.Name,
			Status: b.status, Detail: b.detail,
		})
		res.LLMCalls += b.llmCall
	}

	for _, sc := range svcs {
		for _, layer := range []testscan.Layer{testscan.LayerDB, testscan.LayerController, testscan.LayerHandler} {
			var lb []*block
			for _, b := range blocks {
				if b == nil || b.unit.sc != sc || b.unit.layer != layer {
					continue
				}
				if b.status == StatusTemplate || b.status == StatusLLM {
					lb = append(lb, b)
				}
			}
			if len(lb) == 0 {
				continue
			}
			methods := make([]string, 0, len(lb))
			for _, b := range lb {
				methods = append(methods, b.method)
			}
			content, err := composeFile(sc, layer, lb[0].unit.outFile, lb[0].unit.suite, methods)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s/%s: compose failed: %v", sc.name, layer, err))
				continue
			}
			outPath, err := outPathFor(opts.BaseDir, sc, lb[0].unit.dir, lb[0].unit.outFile)
			if err != nil {
				res.Warnings = append(res.Warnings, err.Error())
				continue
			}
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("mkdir %s: %v", filepath.Dir(outPath), err))
				continue
			}
			if err := os.WriteFile(outPath, []byte(content), 0o644); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("write %s: %v", outPath, err))
				continue
			}
			res.Files = append(res.Files, outPath)
			log.Info("gentest file written", "path", outPath, "methods", len(methods), "suite", lb[0].unit.suite)
		}
	}

	runServiceMocks(ctx, svcs)
	compileGate(res)
	archiveSummary(opts.Audit, res)
	return res, nil
}

// compileGate is the degrade-safe compile pass (PRD-2026-09-09 GT-D6): for
// every written package inside a module, `go vet` then a test-binary
// compile (`go test -run '^$'` — builds the _test.go files without running
// anything) run with a timeout; outcomes are recorded gate lines — never
// run failures (staged trees without the target module's deps degrade
// visibly here).
func compileGate(res *Result) {
	pkgs := map[string]string{} // package dir → module root
	for _, f := range res.Files {
		dir := filepath.Dir(f)
		root := moduleRootOf(dir)
		if root == dir {
			continue // no go.mod anywhere — not a module, skip silently
		}
		if _, ok := pkgs[dir]; !ok {
			pkgs[dir] = root
		}
	}
	for dir, root := range pkgs {
		rel, err := filepath.Rel(root, dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		pkg := "./" + filepath.ToSlash(rel)
		res.Gates = append(res.Gates, gateOne(root, "go", "vet", pkg)...)
		res.Gates = append(res.Gates, gateOne(root, "go", "test", "-count=1", "-run", "^$", pkg)...)
	}
}

// gateOne runs one gate command (bounded) and renders its outcome line.
func gateOne(dir, name string, args ...string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	label := name + " " + strings.Join(args, " ") + " [" + dir + "]"
	if err != nil {
		detail := string(out)
		if len(detail) > 400 {
			detail = detail[:400]
		}
		return []string{label + ": FAILED — " + strings.TrimSpace(detail)}
	}
	return []string{label + ": clean"}
}

// buildUnit classifies one scanned function into a unit, recording skip
// outcomes on the run result; nil return = no work item.
func buildUnit(sc *serviceCtx, layer testscan.Layer, dir, outFile, suite string, fn testscan.Func, lf *layerFacts, res *Result) *unit {
	skip := func(status, detail string) *unit {
		res.Units = append(res.Units, UnitResult{Service: sc.name, Layer: string(layer), Func: fn.Name, Status: status, Detail: detail})
		return nil
	}
	if fn.HasTest {
		return skip("skipped-covered", "existing test detected by scan")
	}
	if !fn.Exported {
		return skip(StatusDesign, "unexported")
	}
	if fn.Ctor {
		return skip(StatusDesign, "constructor (mocks cover construction)")
	}
	u := &unit{sc: sc, layer: layer, dir: dir, outFile: outFile, suite: suite, fn: fn}
	switch layer {
	case testscan.LayerDB:
		f := lf.DB[fn.Name]
		if f == nil {
			return skip(StatusUnsupported, "no db query shape recognized (tx/DML blocks land with a later pass)")
		}
		if f.Shape == "dml" || f.Query == "" {
			return skip(StatusUnsupported, "DML/tx-variant db blocks land with a later pass")
		}
		u.db = f
	case testscan.LayerController:
		f := lf.Ctrl[fn.Name]
		if f == nil {
			return skip(StatusUnsupported, "no controller shape recognized")
		}
		u.ctrl = f
	case testscan.LayerHandler:
		f := lf.Handler[fn.Name]
		if f == nil {
			return skip(StatusUnsupported, "no handler shape recognized")
		}
		if cf := sc.ctrlFacts.Ctrl[f.CtrlCall]; cf == nil || cf.ResponseType == "" {
			return skip(StatusUnsupported, "controller signature for "+f.CtrlCall+" not found (response type unknown)")
		}
		u.handler = f
	default:
		return skip(StatusUnsupported, "layer not testable")
	}
	return u
}

// renderUnit renders one function's test block on a worker: deterministic
// template when the shape allows, else the LLM seam, else llm-required.
func renderUnit(ctx context.Context, u *unit, opts Options) *block {
	b := &block{unit: u}
	switch u.layer {
	case testscan.LayerDB:
		m, err := renderDBMethod(u)
		if err != nil {
			b.status = StatusUnsupported
			b.detail = err.Error()
			return b
		}
		b.method, b.status = m, StatusTemplate
		return b
	case testscan.LayerHandler:
		m, err := renderHandlerMethod(u)
		if err != nil {
			b.status = StatusUnsupported
			b.detail = err.Error()
			return b
		}
		b.method, b.status = m, StatusTemplate
		return b
	case testscan.LayerController:
		if u.ctrl.Passthrough && len(u.ctrl.StoreCalls) == 1 {
			m, err := renderCtrlMethod(u)
			if err != nil {
				b.status = StatusUnsupported
				b.detail = err.Error()
				return b
			}
			b.method, b.status = m, StatusTemplate
			return b
		}
		if opts.NoLLM || opts.Client == nil {
			b.status = StatusLLMNeeded
			b.detail = "field-mapping controller body needs the LLM seam (-no-llm skips it)"
			return b
		}
		m, calls, err := fillCtrlMethod(ctx, u, opts)
		b.llmCall = calls
		if err != nil {
			b.status = StatusLLMNeeded
			b.detail = fmt.Sprintf("llm fill failed: %v", err)
			return b
		}
		b.method, b.status = m, StatusLLM
		return b
	}
	b.status = StatusUnsupported
	b.detail = "unknown layer"
	return b
}

// outNames picks the output test file and suite name for a layer: the
// reference shape (<service>.go → <service>_test.go, <Svc><Noun>Suite —
// the controller layer's reference appends the layer noun again,
// <Svc>ControllerSuiteController) when the layer has no test files; a
// gentest-suffixed twin when it does, so existing suites are never touched.
func outNames(sc *serviceCtx, layer testscan.Layer, dir string) (file, suite string) {
	noun := map[testscan.Layer]string{
		testscan.LayerDB:         "Store",
		testscan.LayerController: "Controller",
		testscan.LayerHandler:    "Handler",
	}[layer]
	tail := "Suite"
	if layer == testscan.LayerController {
		tail = "SuiteController"
	}
	base := strings.ToUpper(sc.name[:1]) + sc.name[1:]
	if len(testFiles(dir)) > 0 {
		if layer == testscan.LayerController {
			return sc.name + "_gentest_test.go", base + noun + tail + "Gen"
		}
		return sc.name + "_gentest_test.go", base + noun + "GenSuite"
	}
	stem := sc.name
	if src := sourceFiles(dir); len(src) > 0 {
		for _, s := range src {
			if s != "interface.go" {
				stem = strings.TrimSuffix(s, ".go")
				break
			}
		}
		if stem == sc.name && strings.HasSuffix(src[0], ".go") && src[0] != "interface.go" {
			stem = strings.TrimSuffix(src[0], ".go")
		}
	}
	return stem + "_test.go", base + noun + tail
}

// outPathFor computes the staged output path: module-root-relative when the
// service tree sits in a module, else service-relative.
func outPathFor(baseDir string, sc *serviceCtx, dir, outFile string) (string, error) {
	if baseDir == "" {
		return "", fmt.Errorf("gentest: no output base directory (paths.staged or -base required)")
	}
	rel, err := filepath.Rel(sc.moduleRoot, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = ""
	}
	return filepath.Join(baseDir, rel, outFile), nil
}

// composeFile renders the full test file: scaffold + suite + method blocks,
// parse-gated and gofmt-canonicalized.
func composeFile(sc *serviceCtx, layer testscan.Layer, outFile, suite string, methods []string) (string, error) {
	prov := templates.NewEmbeddedProvider()
	mockCmd, covCmd := headerCmds(sc, layer)
	var out string
	var err error
	switch layer {
	case testscan.LayerDB:
		joined := strings.Join(methods, "\n")
		out, err = prov.Render(templates.TestDBFile, templates.TestDBFileData{
			TestHeaderData: templates.TestHeaderData{MockGenCmd: mockCmd, CoverageCmd: covCmd},
			Package:        "db",
			LoggerPkg:      sc.module + "/pkg/logger",
			ModelsPkg:      sc.module + "/pkg/services/" + sc.name + "/models",
			UtilsPkg:       sc.module + "/pkg/utils",
			SuiteName:      suite,
			StoreVar:       strings.ToLower(sc.name) + "Store",
			IfaceName:      dbIfaceName(sc),
			CtorCall:       dbCtorCall(sc),
			NeedsSQL:       strings.Contains(joined, "sql.Null") || strings.Contains(joined, "database/sql"),
			NeedsModels:    strings.Contains(joined, "models."),
			Methods:        methods,
		})
	case testscan.LayerController:
		joined := strings.Join(methods, "\n")
		out, err = prov.Render(templates.TestControllerFile, templates.TestControllerFileData{
			TestHeaderData: templates.TestHeaderData{MockGenCmd: mockCmd, CoverageCmd: covCmd},
			Package:        "controller",
			DBPkg:          sc.module + "/pkg/services/" + sc.name + "/db",
			LoggerPkg:      sc.module + "/pkg/logger",
			ModelsPkg:      sc.module + "/pkg/services/" + sc.name + "/models",
			SuiteName:      suite,
			StoreVar:       strings.ToLower(sc.name) + "Store",
			CtrlVar:        strings.ToLower(sc.name) + "Controller",
			CtrlIface:      ctrlIfaceName(sc),
			MockType:       "db.Mock" + dbIfaceName(sc),
			MockCtor:       "db.NewMock" + dbIfaceName(sc) + "(suite.mockController)",
			CtorCall:       ctrlCtorCall(sc),
			NeedsSQL:       strings.Contains(joined, "sql.Null"),
			NeedsTime:      strings.Contains(joined, "time."),
			Methods:        methods,
		})
	case testscan.LayerHandler:
		out, err = prov.Render(templates.TestHandlerFile, templates.TestHandlerFileData{
			TestHeaderData: templates.TestHeaderData{MockGenCmd: mockCmd, CoverageCmd: covCmd},
			Package:        "handler",
			ControllerPkg:  sc.module + "/pkg/services/" + sc.name + "/controller",
			LoggerPkg:      sc.module + "/pkg/logger",
			ModelsPkg:      sc.module + "/pkg/services/" + sc.name + "/models",
			NetworkPkg:     sc.module + "/pkg/network",
			UtilsPkg:       sc.module + "/pkg/utils",
			SuiteName:      suite,
			CtrlMockVar:    strings.ToLower(sc.name) + "Controller",
			CtrlMockType:   "controller.Mock" + ctrlIfaceName(sc),
			MockCtor:       "controller.NewMock" + ctrlIfaceName(sc) + "(gomock.NewController(suite.T()))",
			HandlerVar:     strings.ToLower(sc.name) + "Handler",
			IfaceName:      handlerIfaceName(sc),
			CtorCall:       handlerCtorCall(sc),
			Methods:        methods,
		})
	default:
		return "", fmt.Errorf("unknown layer %s", layer)
	}
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, outFile, out, parser.SkipObjectResolution); err != nil {
		return "", fmt.Errorf("parse gate: %w", err)
	}
	if formatted, ferr := format.Source([]byte(out)); ferr == nil {
		out = string(formatted)
	}
	return out, nil
}

// buildServiceCtxs builds per-service extraction contexts from the scan.
func buildServiceCtxs(rep *testscan.Report) []*serviceCtx {
	seen := map[string]bool{}
	var svcs []*serviceCtx
	for _, sr := range rep.Services {
		if seen[sr.Dir] {
			continue
		}
		seen[sr.Dir] = true
		sc := &serviceCtx{name: sr.Name, dir: sr.Dir, moduleRoot: moduleRootOf(sr.Dir)}
		sc.module = moduleName(sc.moduleRoot, sr.Dir)
		sc.models = extractModels(sr.Dir)
		sc.fixtures = &AssumedFixtureSource{Models: sc.models}
		sc.dbFacts = extractLayer(filepath.Join(sr.Dir, "db"), "db")
		sc.ctrlFacts = extractLayer(filepath.Join(sr.Dir, "controller"), "controller")
		sc.handlerFacts = extractLayer(filepath.Join(sr.Dir, "handler"), "handler")
		svcs = append(svcs, sc)
	}
	return svcs
}

func (sc *serviceCtx) factsFor(l testscan.Layer) *layerFacts {
	switch l {
	case testscan.LayerDB:
		return sc.dbFacts
	case testscan.LayerController:
		return sc.ctrlFacts
	case testscan.LayerHandler:
		return sc.handlerFacts
	}
	return &layerFacts{DB: map[string]*dbFact{}, Ctrl: map[string]*ctrlFact{}, Handler: map[string]*handlerFact{}}
}

// moduleRootOf walks up from dir for the nearest go.mod; dir itself when
// none exists.
func moduleRootOf(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if d == filepath.Dir(d) {
			return dir
		}
	}
}

// moduleName resolves the target module: the go.mod module line, else the
// prefix of any source import path above /pkg/.
func moduleName(root, dir string) string {
	if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "module ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "module "))
			}
		}
	}
	for _, name := range sourceFiles(dir) {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(src), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "\"") && strings.Contains(line, "/pkg/") {
				p := strings.Trim(line, "\"")
				if i := strings.Index(p, "/pkg/"); i > 0 {
					return p[:i]
				}
			}
		}
	}
	return "module"
}

func dbIfaceName(sc *serviceCtx) string {
	if n := ifaceFromFile(filepath.Join(sc.dir, "db", "interface.go")); n != "" {
		return n
	}
	for _, name := range sourceFiles(filepath.Join(sc.dir, "db")) {
		if n := ifaceFromFile(filepath.Join(sc.dir, "db", name)); n != "" {
			return n
		}
	}
	return strings.ToUpper(sc.name[:1]) + sc.name[1:] + "Store"
}

func ctrlIfaceName(sc *serviceCtx) string {
	if n := ifaceFromFile(filepath.Join(sc.dir, "controller", "interface.go")); n != "" {
		return n
	}
	for _, name := range sourceFiles(filepath.Join(sc.dir, "controller")) {
		if n := ifaceFromFile(filepath.Join(sc.dir, "controller", name)); n != "" {
			return n
		}
	}
	return strings.ToUpper(sc.name[:1]) + sc.name[1:] + "Controller"
}

func handlerIfaceName(sc *serviceCtx) string {
	if n := ifaceFromFile(filepath.Join(sc.dir, "handler", "interface.go")); n != "" {
		return n
	}
	for _, name := range sourceFiles(filepath.Join(sc.dir, "handler")) {
		if n := ifaceFromFile(filepath.Join(sc.dir, "handler", name)); n != "" {
			return n
		}
	}
	return strings.ToUpper(sc.name[:1]) + sc.name[1:] + "Handler"
}

// ifaceFromFile returns the first interface type declared in the file.
func ifaceFromFile(path string) string {
	src, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return ""
	}
	for _, d := range af.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok {
				if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
					return ts.Name.Name
				}
			}
		}
	}
	return ""
}

func dbCtorCall(sc *serviceCtx) string {
	name := sc.dbFacts.DBCtor
	if name == "" {
		name = "New" + dbIfaceName(sc)
	}
	if sc.dbFacts.DBCtorArgs > 1 {
		return name + "(nil, suite.sqlDB)"
	}
	return name + "(suite.sqlDB)"
}

func ctrlCtorCall(sc *serviceCtx) string {
	name := sc.ctrlFacts.CtrlCtor
	if name == "" {
		name = "New" + ctrlIfaceName(sc)
	}
	return name + "(suite." + strings.ToLower(sc.name) + "Store)"
}

func handlerCtorCall(sc *serviceCtx) string {
	name := sc.handlerFacts.HandlerCtor
	if name == "" {
		name = "New" + handlerIfaceName(sc)
	}
	return name + "(suite." + strings.ToLower(sc.name) + "Controller)"
}

// runServiceMocks regenerates db/controller mocks for every touched service
// (best-effort, shared runner; the target tree's interfaces only).
func runServiceMocks(ctx context.Context, svcs []*serviceCtx) {
	var targets []gen.MockTarget
	for _, sc := range svcs {
		dbIface := filepath.Join(sc.dir, "db", "interface.go")
		ctrlIface := filepath.Join(sc.dir, "controller", "interface.go")
		if _, err := os.Stat(dbIface); err == nil {
			targets = append(targets, gen.MockTarget{
				Source: dbIface, Dest: filepath.Join(sc.dir, "db", "mock_store.go"), Name: dbIfaceName(sc),
			})
		}
		if _, err := os.Stat(ctrlIface); err == nil {
			targets = append(targets, gen.MockTarget{
				Source: ctrlIface, Dest: filepath.Join(sc.dir, "controller", "mock_controller.go"), Name: ctrlIfaceName(sc),
			})
		}
	}
	if len(targets) == 0 {
		return
	}
	gen.RunMocks(ctx, targets)
}

// archiveSummary persists the run summary into the audit trail (best-effort).
func archiveSummary(rec *audit.Recorder, res *Result) {
	if rec == nil {
		return
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_, _ = rec.Write("gentest_summary.json", func(w io.Writer) error {
		_, werr := w.Write(data)
		return werr
	})
}

// headerCmds renders the mockgen + coverage header comments for a layer.
func headerCmds(sc *serviceCtx, layer testscan.Layer) (mockCmd, covCmd string) {
	rel := "pkg/services/" + sc.name + "/" + string(layer)
	test := sc.name + "_test.go"
	switch layer {
	case testscan.LayerDB:
		mockCmd = fmt.Sprintf("mockgen -source=%s/interface.go -destination=%s/mock_store.go -package=%s", rel, rel, layer)
	case testscan.LayerController:
		mockCmd = fmt.Sprintf("mockgen -source=%s/interface.go -destination=%s/mock_controller.go -package=%s", rel, rel, layer)
	case testscan.LayerHandler:
		mockCmd = fmt.Sprintf("mockgen -source=%s/interface.go -destination=%s/interface_mock.go -package=%s", rel, rel, layer)
	}
	covCmd = fmt.Sprintf("go test %s/%s %s/%s.go %s/interface.go -v -coverprofile=coverage.txt -covermode count && go tool cover -html=coverage.txt", rel, test, rel, sc.name, rel)
	return mockCmd, covCmd
}
