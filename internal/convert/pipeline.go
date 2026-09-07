// Package convert is the conversion orchestrator (PRD §4.2, architecture.md
// Phase 5): it executes the deterministic plan unit by unit — models, DB
// methods, accumulated interfaces, handler glue and router render with zero
// LLM calls — and fills exactly one template-shaped gap per endpoint with
// the LLM: the controller body, generated from the query-replaced branch
// view plus the DB signatures (never raw SQL, §4.2.4/§4.3). Every unit
// transitions the ledger (resumable) and leaves an audit record (§4.7).
package convert

import (
	"context"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/gen"
	"github.com/Public/convert-tux-to-go/internal/ledger"
	"github.com/Public/convert-tux-to-go/internal/llm"
	"github.com/Public/convert-tux-to-go/internal/plan"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
	"github.com/Public/convert-tux-to-go/internal/validate"
)

// Options carries one convert run's wiring.
type Options struct {
	Plan       *plan.Plan
	Main       *ir.File
	Source     string
	FnFiles    []*ir.File
	Client     llm.Client // the LLM seam — fake server in CI
	Budget     budget.Budget
	BaseDir    string // module root when the target service exists, else the staged root
	Ledger     *ledger.Ledger
	Validator  *validate.Validator
	MaxRetries int
	Audit      *audit.Recorder // per-run audit folder (§4.7); nil = skip
	Workers    int             // DB-unit render pool size (concurrency.workers); <1 → 1
	// SkipLLM is the deterministic-only mode (run.llm: false): pending
	// controller units are marked skipped (never failed) so a later
	// LLM-enabled run resumes them.
	SkipLLM bool
	// WithGorm renders the store with the legacy *gorm.DB handle alongside
	// sqlx (db.withGorm); default is the plain sqlx-only store.
	WithGorm bool
}

// Result summarizes one convert run.
type Result struct {
	Files        []string
	LLMCalls     int
	Blocked      []string
	Failed       []string
	Skipped      []string
	Placeholders []string
	TierB        *validate.Result
	// SQLDeviations lists the PF-6 fidelity findings: db methods whose
	// generated SQL drifted from the source Tux SQL, and SQL-free artifacts
	// that leaked SQL keywords. Flag-only — the run itself never fails.
	SQLDeviations []string
}

// Run executes the plan. Deterministic units regenerate byte-identically on
// resume; only pending LLM units consume the client.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Plan == nil || opts.Main == nil || opts.Ledger == nil || opts.Validator == nil {
		return nil, fmt.Errorf("convert: plan, main IR, ledger and validator are required")
	}
	svc, err := gen.NewService(gen.Options{Plan: opts.Plan, Main: opts.Main, FnFiles: opts.FnFiles, WithGorm: opts.WithGorm})
	if err != nil {
		return nil, err
	}
	res := &Result{}

	// Blocked endpoints (unresolved external fns) — visible, never silent.
	blockedEndpoints := map[string]string{}
	for _, b := range opts.Plan.Blockers {
		for _, e := range b.Endpoints {
			blockedEndpoints[e] = b.Fn
		}
	}

	// 1. Models — deterministic.
	modelsPath, err := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("models")+"/"+svc.Mapping.Service+".go")
	if err != nil {
		return nil, err
	}
	if err := generateFile(ctx, opts, res, "u01", "models", "models.go", modelsPath, func() (string, error) { return svc.ModelFile(opts.Plan) }); err != nil {
		return nil, err
	}

	// 2. DB methods — deterministic. Renders run through a bounded worker
	// pool (concurrency.workers); the ledger and the interface accumulate
	// serially in unit order, so bytes are identical to a workers=1 run.
	dbUnits := unitsOf(opts.Plan, plan.KindDBMethod)
	dbBodies, err := renderDBUnits(svc, dbUnits, opts.workerCount())
	if err != nil {
		return nil, err
	}
	for _, u := range dbUnits {
		e := opts.Ledger.Get(u.ID, string(u.Kind), u.Name)
		if e.Status == ledger.StatusAppended {
			continue // resume: already recorded
		}
		opts.Ledger.Set(u.ID, ledger.StatusGenerated, "")
		addMap(opts, res, u, []string{u.TargetPath})
	}
	dbFilePath, err := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("db")+"/"+svc.Mapping.Service+".go")
	if err != nil {
		return nil, err
	}
	dbFile, err := svc.DBMethodsFile(opts.Plan)
	if err != nil {
		return nil, err
	}
	if err := writeFileValidated(ctx, opts, res, dbFilePath, dbFile, "db-methods"); err != nil {
		return nil, err
	}
	for _, u := range dbUnits {
		opts.Ledger.Set(u.ID, ledger.StatusAppended, "", relPath(opts.BaseDir, dbFilePath))
	}
	checkDBFidelity(ctx, opts, res, svc, dbFilePath, dbUnits)

	ifacePath, err := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("db")+"/interface.go")
	if err != nil {
		return nil, err
	}
	for _, u := range unitsOf(opts.Plan, plan.KindDBMethod) {
		if err := svc.AccumulateDBInterface(ifacePath, dbBodies[u.ID].sig); err != nil {
			return nil, fmt.Errorf("convert: accumulate db interface: %w", err)
		}
	}
	if err := validateFile(ctx, opts, ifacePath); err != nil {
		return nil, err
	}
	res.Files = append(res.Files, ifacePath)

	// 3. Controller + handler interfaces, handler methods, router — deterministic.
	handlerIfaceID := unitID(opts.Plan, plan.KindHandlerInterface)
	artifacts := []struct {
		id, name, path string
		render         func() (string, error)
	}{
		{unitID(opts.Plan, plan.KindControllerInterface), "controller-interface.go",
			svc.Mapping.ImportPath("controller") + "/interface.go",
			func() (string, error) { return svc.ControllerInterface(opts.Plan) }},
		{handlerIfaceID, "handler-interface.go",
			svc.Mapping.ImportPath("handler") + "/interface.go",
			func() (string, error) { return svc.HandlerInterface(opts.Plan) }},
		{handlerIfaceID + "+methods", "handler-methods.go",
			svc.Mapping.ImportPath("handler") + "/" + svc.Mapping.Service + ".go",
			func() (string, error) { return svc.HandlerMethodsFile() }},
		{unitID(opts.Plan, plan.KindRouter), "router-snippet",
			svc.Mapping.ImportPath("handler") + "/router_snippet.txt",
			func() (string, error) { return svc.Router() }},
	}
	for _, a := range artifacts {
		path, err := opts.absPath(opts.BaseDir, a.path)
		if err != nil {
			return nil, err
		}
		var render = a.render
		if err := generateFile(ctx, opts, res, a.id, "file", a.name, path, render); err != nil {
			return nil, err
		}
	}

	// 4. Controller bodies — the one LLM gap per endpoint (§4.2.4).
	dbContract := dbSignatures(opts.Plan, dbBodies)
	ctrlFilePath, err := opts.absPath(opts.BaseDir, svc.Mapping.ImportPath("controller")+"/"+svc.Mapping.Service+".go")
	if err != nil {
		return nil, err
	}
	for _, u := range unitsOf(opts.Plan, plan.KindControllerMethod) {
		opts.Ledger.Get(u.ID, string(u.Kind), u.Name) // register before any transition
		if fn, blocked := blockedEndpoints[u.Name]; blocked {
			opts.Ledger.Set(u.ID, ledger.StatusBlocked, "unresolved external fn "+fn)
			res.Blocked = append(res.Blocked, u.Name)
			continue
		}
		if opts.Ledger.Get(u.ID, string(u.Kind), u.Name).Status == ledger.StatusAppended {
			continue // resume: already converted
		}
		if opts.SkipLLM {
			// Deterministic-only mode: leave controller bodies for a later
			// LLM-enabled resume — visible, never a failure.
			opts.Ledger.Set(u.ID, ledger.StatusSkipped, "llm disabled (run.llm: false)")
			res.Skipped = append(res.Skipped, u.Name)
			continue
		}
		if opts.Client == nil {
			return nil, fmt.Errorf("convert: endpoint %s needs the LLM client but none is configured", u.Name)
		}
		body, _, err := controllerBody(ctx, opts, res, svc, u, dbContract)
		if err != nil {
			opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
			res.Failed = append(res.Failed, u.Name)
			continue
		}
		if err := appendControllerMethod(ctx, opts, res, svc, u, ctrlFilePath, body); err != nil {
			opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
			res.Failed = append(res.Failed, u.Name)
			continue
		}
		opts.Ledger.Set(u.ID, ledger.StatusAppended, "", relPath(opts.BaseDir, ctrlFilePath))
		addMap(opts, res, u, []string{relPath(opts.BaseDir, ctrlFilePath)})
	}

	// 4b. TPCall placeholders — deterministic, compilable stubs (PF-4.5):
	// one `tuxgo:TODO` stub per site, ledger status placeholder, mapped in
	// the conversion map like every other unit.
	if err := renderTPCallPlaceholders(ctx, opts, res, svc); err != nil {
		return nil, err
	}
	checkSQLFreeArtifacts(ctx, opts, res, dbFilePath)
	if err := opts.Ledger.Save(); err != nil {
		return nil, err
	}

	// 5. Tier B — batched when the target service exists (plan-conversion §2).
	tb := opts.Validator.CompileAll(ctx)
	res.TierB = &tb
	if tb.DegradeReason != "" {
		telemetry.Log(ctx).Warn("tier B validation skipped", "reason", tb.DegradeReason)
	}
	return res, nil
}

// controllerBody assembles the controller unit's prompt (rewritten branch +
// DB signatures + FML contract, never raw SQL), calls the LLM with bounded
// retries feeding trimmed validation errors, and returns the accepted body.
func controllerBody(ctx context.Context, opts Options, res *Result, svc *gen.Service, u plan.Unit, dbContract string) (body, prompt string, err error) {
	c := svc.ConditionOf(u.Name)
	if c == nil {
		return "", "", fmt.Errorf("convert: no condition for endpoint %s", u.Name)
	}
	branch := branchSource(opts.Source, c.StartLine, c.EndLine)
	queries, calls, err := svc.BranchCalls(c, opts.Plan)
	if err != nil {
		return "", "", err
	}
	view, err := budget.ReplaceQueries(branch, queries, calls)
	if err != nil {
		return "", "", fmt.Errorf("convert: query replacement for %s: %w", u.Name, err)
	}
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		methods = append(methods, call.Name)
	}
	sort.Strings(methods)
	contract, err := svc.ControllerPromptContext(u.Name, opts.Plan, methods)
	if err != nil {
		return "", "", err
	}
	prompt = buildPrompt(view, dbContract, contract, u.Name)

	if err := opts.Budget.CheckInput(prompt); err != nil {
		return "", "", fmt.Errorf("convert: %w — trim the mapping or raise run.maxPromptTokens", err)
	}

	e := opts.Ledger.Get(u.ID, string(u.Kind), u.Name)
	var lastErrs []string
	for attempt := 0; attempt <= opts.MaxRetries; attempt++ {
		e.Attempts++
		messages := []llm.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		}
		if len(lastErrs) > 0 {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "Your previous output failed validation:\n" + strings.Join(lastErrs, "\n") + "\nFix these errors and emit only the corrected method body.",
			})
		}
		response, cerr := opts.Client.Chat(ctx, llm.ChatRequest{
			Model:       "", // endpoint default (resolved by the client's wiring)
			Messages:    messages,
			Temperature: 0.1,
		})
		res.LLMCalls++
		if cerr != nil {
			lastErrs = []string{cerr.Error()}
			recordAttempt(ctx, opts, u, attempt, prompt, "", lastErrs)
			continue
		}
		if oerr := opts.Budget.CheckOutput(response.Content); oerr != nil {
			lastErrs = []string{oerr.Error()}
			recordAttempt(ctx, opts, u, attempt, prompt, response.Content, lastErrs)
			continue
		}
		body = cleanBody(response.Content)
		if verr := validateBody(opts, body); len(verr) > 0 {
			lastErrs = verr
			recordAttempt(ctx, opts, u, attempt, prompt, response.Content, verr)
			continue
		}
		recordAttempt(ctx, opts, u, attempt, prompt, response.Content, nil)
		opts.Ledger.Set(u.ID, ledger.StatusValidated, "")
		return body, prompt, nil
	}
	return "", prompt, fmt.Errorf("validation failed after %d attempts: %s", opts.MaxRetries+1, strings.Join(lastErrs, "; "))
}

const systemPrompt = `You convert one legacy Pro*C/Tuxedo branch into the body of a Go controller method.
Rules:
- Emit ONLY the Go statements that go between the method's braces. No package, imports, or func declaration.
- The signature is fixed and provided verbatim: the context parameter is c, the request parameter is request, and the named returns are data and err. Never declare or use req, resp, or Response.
- Read inputs only as request.<Field>, using the request struct's verbatim field names.
- Call the database exclusively through the store signatures provided — exactly the parameters each signature shows, same count and order. Never write SQL.
- Store methods return []*models.X or *models.X per their signatures; the row structs are provided verbatim — use their field names exactly, never invent fields.
- String comparisons use double-quoted literals: flag == "Y", never 'Y'.
- Every identifier must be one of: request.<Field>, a store call, data, err, or a local you declare. No hallucinated variables.
- The method template already emits the START and END debug logs — never write logger START/END statements in the body.
- Preserve the branch's business logic (flag checks, loops, decodes) as idiomatic Go.
- On error return nil, err; on success return data, err.`

// buildPrompt assembles the deterministic context: rewritten branch view,
// DB contract, and the fixed signature + verbatim struct definitions.
func buildPrompt(view budget.View, dbContract, contract, endpoint string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Endpoint: %s\n\n", endpoint)
	sb.WriteString("DB layer contract (call these; never write SQL):\n" + dbContract + "\n\n")
	sb.WriteString("Fixed method signature and verbatim struct definitions (parameter and field names must match exactly):\n" + contract + "\n\n")
	sb.WriteString("Legacy branch, with every SQL block already replaced by its store call:\n\n" + view.Source + "\n")
	return sb.String()
}

// branchSource slices the 1-based inclusive line range out of src.
func branchSource(src string, from, to int) string {
	lines := strings.Split(src, "\n")
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return ""
	}
	return strings.Join(lines[from-1:to], "\n")
}

// validateBody checks the body wrapped in a synthetic method. gofmt
// normalization is deterministic — space-vs-tab indentation from the model
// is auto-fixed downstream (appendControllerMethod re-formats the file), so
// only parse errors reject an attempt.
func validateBody(opts Options, body string) []string {
	wrapped := "package controller\n\nimport (\n\t\"context\"\n\tmodels \"mutual-fund-be/pkg/services/nav/models\"\n)\n\ntype t struct{}\n\nfunc (t) Check(ctx context.Context) (err error) {\n" + body + "\n}\n"
	if _, ferr := format.Source([]byte(wrapped)); ferr != nil {
		return trimGoErrors(ferr.Error())
	}
	return nil
}

// trimGoErrors keeps compiler-shaped lines from a parse error, bounded.
func trimGoErrors(out string) []string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			continue
		}
		kept = append(kept, line)
		if len(kept) >= 40 {
			kept = append(kept, "… (output trimmed)")
			break
		}
	}
	return kept
}

// cleanBody strips code fences and stray blank lines the model may add —
// line-based, so the body's own indentation (which gofmt normalizes) is
// preserved exactly.
func cleanBody(content string) string {
	lines := strings.Split(content, "\n")
	var out []string
	for _, ln := range lines {
		if t := strings.TrimSpace(ln); t == "```" || t == "```go" {
			continue
		}
		out = append(out, ln)
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// appendControllerMethod appends the rendered method to the controller file
// (text append + import header on creation — bodies are LLM artifacts, the
// go/ast append model stays reserved for interfaces). The file is normalized
// with go/format after every append so Tier A passes deterministically.
func appendControllerMethod(ctx context.Context, opts Options, res *Result, svc *gen.Service, u plan.Unit, path, body string) error {
	method, err := svc.RenderControllerMethod(u.Name, body)
	if err != nil {
		return err
	}
	var merged string
	if _, statErr := os.Stat(path); statErr == nil {
		existing, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		merged = string(existing) + "\n" + strings.TrimRight(method, "\n") + "\n"
	} else {
		header := "package controller\n\nimport (\n\t\"context\"\n\t\"errors\"\n\t\"fmt\"\n\n\t\"" + svc.Module + "/pkg/logger\"\n\t\"" + svc.ModelsPkg + "\"\n)\n"
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		merged = header + "\n" + strings.TrimRight(method, "\n") + "\n"
	}
	formatted, ferr := format.Source([]byte(merged))
	if ferr != nil {
		return fmt.Errorf("convert: controller file does not parse after append: %w", ferr)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return err
	}
	if err := validateFile(ctx, opts, path); err != nil {
		return err
	}
	res.Files = append(res.Files, path)
	return nil
}

// renderTPCallPlaceholders writes controller/tpcall_placeholders.go for the
// plan's tpcall units and records each as a ledger placeholder with its
// conversion-map entry (PF-4.4/4.5/4.6). No-op for plans without tpcalls.
func renderTPCallPlaceholders(ctx context.Context, opts Options, res *Result, svc *gen.Service) error {
	units := unitsOf(opts.Plan, plan.KindTPCall)
	if len(units) == 0 {
		return nil
	}
	path, err := opts.absPath(opts.BaseDir, units[0].TargetPath)
	if err != nil {
		return err
	}
	for _, u := range units {
		opts.Ledger.Get(u.ID, string(u.Kind), u.Name) // register before transitions
	}
	if e := opts.Ledger.Get(units[0].ID, string(units[0].Kind), units[0].Name); e.Status == ledger.StatusPlaceholder {
		return nil // resume: the placeholder file already landed
	}
	content, err := svc.PlaceholderFile(opts.Plan)
	if err != nil {
		for _, u := range units {
			opts.Ledger.Set(u.ID, ledger.StatusFailed, err.Error())
			res.Failed = append(res.Failed, u.Name)
		}
		return fmt.Errorf("convert: render tpcall placeholders: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("convert: write tpcall placeholders: %w", err)
	}
	if err := validateFile(ctx, opts, path); err != nil {
		return err
	}
	res.Files = append(res.Files, path)
	for _, u := range units {
		reason := "external service call rendered as a tuxgo:TODO placeholder (no outbound-call convention, R8)"
		if u.TP != nil && u.TP.Ambiguous {
			reason = "external service call rendered as a tuxgo:TODO placeholder (ambiguous send/recv window)"
		}
		opts.Ledger.Set(u.ID, ledger.StatusPlaceholder, reason, relPath(opts.BaseDir, path))
		addMap(opts, res, u, []string{relPath(opts.BaseDir, path)})
		res.Placeholders = append(res.Placeholders, u.Name)
	}
	return nil
}

// dbOut is one rendered DB method: its body and interface signature.
type dbOut struct {
	body string
	sig  string
}

// dbSignatures renders the store contract lines for controller prompts.
func dbSignatures(p *plan.Plan, bodies map[string]dbOut) string {
	var lines []string
	for _, u := range unitsOf(p, plan.KindDBMethod) {
		b := bodies[u.ID]
		lines = append(lines, "s.store."+b.sig)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// recordAttempt writes the unit's audit record (§4.7): template, prompt,
// raw response, validation outcome. Best-effort.
func recordAttempt(ctx context.Context, opts Options, u plan.Unit, attempt int, prompt, response string, errs []string) {
	if opts.Audit == nil {
		return
	}
	record := map[string]any{
		"unit": u.ID, "kind": u.Kind, "name": u.Name, "attempt": attempt,
		"template": u.TemplateID, "llm": u.LLM,
		"prompt":   prompt,
		"response": response,
		"errors":   errs,
		"outcome":  "ok",
	}
	if len(errs) > 0 {
		record["outcome"] = "failed"
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	if _, err := opts.Audit.Write(fmt.Sprintf("unit-%s-attempt%d.json", u.ID, attempt), func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}); err != nil {
		telemetry.Log(ctx).Warn("audit record failed", "unit", u.ID, "error", err)
	}
}

// --- helpers ---

// renderDBUnits renders every DB unit through a bounded worker pool.
// svc.DBMethod is a pure read of the shared IR (queries, host vars, mapping
// pins), so parallel renders are race-free; results land indexed by unit
// position and the first unit-order error wins, so output and failures match
// a workers=1 run byte for byte.
func renderDBUnits(svc *gen.Service, units []plan.Unit, workers int) (map[string]dbOut, error) {
	out := make([]dbOut, len(units))
	errs := make([]error, len(units))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i := range units {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			body, sig, _, err := svc.DBMethod(units[i])
			if err != nil {
				errs[i] = err
				return
			}
			out[i] = dbOut{body, sig}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	bodies := make(map[string]dbOut, len(units))
	for i, u := range units {
		bodies[u.ID] = out[i]
	}
	return bodies, nil
}

func (o Options) workerCount() int {
	if o.Workers < 1 {
		return 1
	}
	return o.Workers
}

func unitsOf(p *plan.Plan, k plan.Kind) []plan.Unit {
	var out []plan.Unit
	for _, u := range p.Units {
		if u.Kind == k {
			out = append(out, u)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func unitID(p *plan.Plan, k plan.Kind) string {
	for _, u := range p.Units {
		if u.Kind == k {
			return u.ID
		}
	}
	return ""
}

// generateFile renders a deterministic artifact, writes it (unless the
// ledger already marked it appended — resume), validates Tier A, and
// records the ledger + audit trail.
func generateFile(ctx context.Context, opts Options, res *Result, id, kind, name, path string, render func() (string, error)) error {
	e := opts.Ledger.Get(id, kind, name)
	if e.Status == ledger.StatusAppended {
		return nil // resume
	}
	content, err := render()
	if err != nil {
		opts.Ledger.Set(id, ledger.StatusFailed, err.Error())
		return fmt.Errorf("convert: render %s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("convert: write %s: %w", path, err)
	}
	if err := validateFile(ctx, opts, path); err != nil {
		opts.Ledger.Set(id, ledger.StatusFailed, err.Error())
		return err
	}
	opts.Ledger.Set(id, ledger.StatusAppended, "", relPath(opts.BaseDir, path))
	res.Files = append(res.Files, path)
	return nil
}

// writeFileValidated writes a whole-file artifact and validates it.
func writeFileValidated(ctx context.Context, opts Options, res *Result, path, content, label string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("convert: write %s: %w", label, err)
	}
	if err := validateFile(ctx, opts, path); err != nil {
		return err
	}
	res.Files = append(res.Files, path)
	return nil
}

// validateFile runs Tier A on one generated file. Only .go sources go
// through the parser + gofmt check; other artifacts (e.g. the router
// snippet for the user's transport layer) need only exist.
func validateFile(ctx context.Context, opts Options, path string) error {
	if !strings.HasSuffix(path, ".go") {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("convert: %s missing", path)
		}
		return nil
	}
	res := opts.Validator.Syntax(path)
	if !res.OK {
		return fmt.Errorf("convert: %s: %s", path, strings.Join(res.Errors, "; "))
	}
	return nil
}

// addMap records the unit's conversion-map entries (§4.6).
func addMap(opts Options, res *Result, u plan.Unit, targets []string) {
	src := u.SourceFile
	if src == "" {
		src = opts.Plan.Source
	}
	span := ""
	if u.SourceLines != "" {
		span = " (L" + u.SourceLines + ")"
	}
	for _, t := range targets {
		opts.Ledger.AddMap(fmt.Sprintf("%s :: %s%s", filepath.Base(src), u.Name, span), t)
	}
}

// absPath resolves a plan unit's target path (an import path) to a file on
// disk under the run's base directory: the module's first segment maps to
// the base itself, so the same relative layout holds for the real target
// service and the staged fallback.
func (o Options) absPath(base, targetPath string) (string, error) {
	parts := strings.SplitN(targetPath, "/", 2)
	rel := targetPath
	if len(parts) == 2 {
		rel = parts[1]
	}
	return filepath.Join(base, rel), nil
}

func relPath(base, path string) string {
	if r, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}
