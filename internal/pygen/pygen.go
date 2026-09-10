// Package pygen is the deterministic Python batch generator (PRD
// 2026-09-08 BP-3/BP-6): plan + templates → one Python module per batch
// program. The simple cursor-batch shape renders fully deterministically;
// the repository shape renders the SQL constants and repository methods
// deterministically and fills the service body through the LLM seam (the
// single LLM gap, -no-llm yields tuxgo:TODO placeholders). Every result
// passes the pychk gates and carries the logic-retention report.
package pygen

import (
	"context"
	"strconv"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/llm"
	"github.com/Public/convert-tux-to-go/internal/pychk"
	"github.com/Public/convert-tux-to-go/internal/pyplan"
	"github.com/Public/convert-tux-to-go/internal/sqlchk"
	"github.com/Public/convert-tux-to-go/internal/templates"
)

// Options carries one generation run's inputs.
type Options struct {
	Plan       *pyplan.Plan
	Source     string // full .pc source (the CodeView input)
	SourcePath string
	NoLLM      bool
	Client     llm.Client
	Budget     budget.Budget
	MaxRetries int
	// Audit archives each LLM attempt (prompt + raw response + gate errors)
	// when set; nil skips the archive — never the generation itself.
	Audit *audit.Recorder
	// Check is the structural gate (injectable for tests); nil = pychk.Check.
	Check func(src string) []pychk.Issue
}

// Result is one generated module with its gate outcomes and report.
type Result struct {
	Content   string
	LLMCalls  int
	LLMFilled bool
	Structure []pychk.Issue
	PyOK      bool
	PyMode    string // ast | structural | unavailable
	PyDetail  string
	Fidelity  []sqlchk.Result
	Retention Retention
	Notes     []string
}

// Generate renders the module for one plan. The LLM seam runs only for the
// repository shape when a client is supplied and NoLLM is false; failures
// degrade to the placeholder body with a note (never a silent SQL invention).
func Generate(ctx context.Context, opts Options) (Result, error) {
	res := Result{}
	p := opts.Plan
	check := opts.Check
	if check == nil {
		check = pychk.Check
	}

	var secs []string
	var serviceBody string
	secs = append(secs, renderHeader(p, opts.SourcePath))
	secs = append(secs, banner("SQL Query Constants (source fidelity-gated via sqlchk)"))
	for _, c := range p.Consts {
		secs = append(secs, render(templates.PyBatchConst, constData{Name: c.Name, SQL: c.SQL}))
	}

	if p.Shape == "simple" {
		secs = append(secs, banner("Data Access Layer (pure cursor functions — mockable with a cursor double)"))
		for _, fn := range p.DAL {
			if fn.Kind == "fetch" {
				secs = append(secs, render(templates.PyBatchDALFetch, dalFetchData{Name: fn.Name, Const: fn.Const, Source: strings.TrimPrefix(fn.Name, "fetch_")}))
				continue
			}
			secs = append(secs, render(templates.PyBatchDALDML, dalDMLData{
				Name: fn.Name, Const: fn.Const, Mode: p.DMLLoop,
				HasBinds:      len(fn.BindIdx) > 0,
				Projection:    projection("r", fn.BindIdx),
				RowProjection: projection("row", fn.BindIdx),
			}))
		}
		calls := make([]entryCall, 0, len(p.Phases))
		for _, ph := range p.Phases {
			calls = append(calls, entryCall{Key: resultKey(ph.Cursor), Method: ph.Method})
		}
		entry := render(templates.PyBatchEntrypoint, entrypointData{
			Entrypoint: p.Entrypoint, ServiceName: p.ServiceName, Calls: calls,
		})
		var svc []string
		svc = append(svc, render(templates.PyBatchServiceHead, serviceHeadData{
			ClassName: p.ClassName, ServiceName: p.ServiceName, SourcePath: opts.SourcePath, RouterClass: p.Wrapper.RouterClass,
		}))
		for _, ph := range p.Phases {
			svc = append(svc, render(templates.PyBatchPhase, phaseData{
				Index: ph.Index, Method: ph.Method, Cursor: ph.Cursor, Table: ph.Table,
				FetchFn: ph.FetchFn, DMLFn: ph.DMLFn, Mode: p.DMLLoop,
				ReadMode: p.Wrapper.ReadMode, WriteMode: p.Wrapper.WriteMode,
			}))
		}
		svc = append(svc, entry)
		serviceBody = strings.Join(svc, "\n\n")
		secs = append(secs, banner("Service Layer"))
		secs = append(secs, svc...)
	} else {
		body := placeholderBody(p.Entrypoint)
		if !opts.NoLLM && opts.Client != nil {
			filled, calls, notes, err := fillServiceBody(ctx, opts)
			res.LLMCalls = calls
			if err == nil {
				body = filled
				res.LLMFilled = true
			} else {
				// Rejected-attempt notes are failure context, not findings —
				// on success the audit trail already archives every attempt.
				res.Notes = append(res.Notes, notes...)
				res.Notes = append(res.Notes, "llm fill failed: "+err.Error()+" — placeholder body emitted")
			}
		} else if !opts.NoLLM {
			res.Notes = append(res.Notes, "no llm client available — placeholder body emitted")
		}
		serviceBody = body
		secs = append(secs, repoSection(p, opts.SourcePath, body))
	}

	res.Content = strings.Join(secs, "\n\n") + "\n"
	res.Structure = check(res.Content)
	res.Structure = append(res.Structure, txnIssues(res.Content)...)
	if p.Shape == "repo" {
		// The seam rule is repository-shape-only: simple-shape services hold
		// the cursor-function DAL calls with direct router connections (the
		// reference pythonEqTux1 design).
		res.Structure = append(res.Structure, seamIssues(serviceBody)...)
	}
	res.PyOK, res.PyMode, res.PyDetail = interpreterGate(res.Content)
	res.Fidelity = fidelityOf(p, res.Content)
	res.Retention = retentionOf(p, res.Content, serviceBody, res.Fidelity, res.LLMCalls, res.Structure, res.PyOK, res.PyMode, res.LLMFilled)
	return res, nil
}

// repoSection renders the repository banner + deterministic repo methods +
// the service body (class passthrough or shell) — the tail section of every
// repo-shape module. Generate and assembleModule (the seam gate's view)
// share it, so the structural gate always sees the exact bytes that would
// be written (A2.4: the one assembly).
func repoSection(p *pyplan.Plan, sourcePath, body string) string {
	var secs []string
	secs = append(secs, banner("Repository (deterministic scaffold — BP-3)"))
	var blocks []string
	for _, m := range p.Repo {
		blocks = append(blocks, renderRepoMethod(p, m))
	}
	if strings.HasPrefix(strings.TrimSpace(body), "class ") {
		secs = append(secs, strings.TrimRight(strings.TrimSpace(body), "\n"))
	} else {
		secs = append(secs, render(templates.PyBatchServiceShell, serviceShellData{
			RepoName: p.RepoName, ClassName: p.ClassName, ServiceName: p.ServiceName,
			SourcePath: sourcePath, RouterClass: p.Wrapper.RouterClass,
			RepoBlocks: strings.Join(blocks, "\n\n"), Body: body,
		}))
	}
	return strings.Join(secs, "\n\n")
}

// assembleModule builds the full repo-shape module with one service body —
// the seam gate's view of what Generate would write (the same repoSection
// the write path uses).
func assembleModule(p *pyplan.Plan, sourcePath, body string) string {
	var secs []string
	secs = append(secs, renderHeader(p, sourcePath))
	secs = append(secs, banner("SQL Query Constants (source fidelity-gated via sqlchk)"))
	for _, c := range p.Consts {
		secs = append(secs, render(templates.PyBatchConst, constData{Name: c.Name, SQL: c.SQL}))
	}
	secs = append(secs, repoSection(p, sourcePath, body))
	return strings.Join(secs, "\n\n") + "\n"
}

// interpreterGate upgrades the structural gate with python3's ast.parse via
// the shared pychk.CheckSource; the outcome is always reported, never faked.
func interpreterGate(content string) (bool, string, string) {
	return pychk.CheckSource(content)
}

// placeholderBody is the -no-llm service body (BP-6): a loud, resumable gap.
func placeholderBody(entrypoint string) string {
	return `    def ` + entrypoint + `(self, workers: int = 1) -> Dict[str, Any]:
        """Unified runner entry point orchestrating all phases."""
        # tuxgo:TODO service body — pending LLM fill (BP-6; -no-llm run)
        raise NotImplementedError("batchpy: service body pending LLM fill")`
}

func fidelityOf(p *pyplan.Plan, content string) []sqlchk.Result {
	targets := make([]pychk.FidelityTarget, 0, len(p.Consts))
	for _, c := range p.Consts {
		targets = append(targets, pychk.FidelityTarget{Const: c.Name, QueryID: c.ID, Source: c.SQL})
	}
	return pychk.Fidelity(targets, content)
}

// render executes one template id against data.
func render(id templates.ID, data any) string {
	out, err := templates.NewEmbeddedProvider().Render(id, data)
	if err != nil {
		return "# tuxgo: template error (" + string(id) + "): " + err.Error()
	}
	return strings.TrimRight(out, "\n")
}

func banner(title string) string {
	bar := strings.Repeat("=", 69)
	return "# " + bar + "\n# " + title + "\n# " + bar
}

// projection renders the fetch-row bind projection for executemany/execute:
// a bind tuple per row — "r[0], r[1]", or "r[0]," when a single bind needs
// the 1-tuple comma.
func projection(prefix string, idx []int) string {
	if len(idx) == 0 {
		return ""
	}
	parts := make([]string, len(idx))
	for i, ix := range idx {
		parts[i] = prefix + "[" + strconv.Itoa(ix) + "]"
	}
	out := strings.Join(parts, ", ")
	if len(idx) == 1 {
		out += ","
	}
	return out
}

// resultKey converts a cursor name to its result-dict key (mf_ti_reject →
// mf_ti_reject_records).
func resultKey(cursor string) string { return cursor + "_records" }
