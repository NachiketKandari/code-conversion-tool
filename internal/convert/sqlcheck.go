package convert

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/gen"
	"github.com/Public/convert-tux-to-go/internal/ledger"
	"github.com/Public/convert-tux-to-go/internal/plan"
	"github.com/Public/convert-tux-to-go/internal/sqlchk"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

// checkDBFidelity runs the PF-6 SQL fidelity gate over the written
// db-methods file: every db unit's embedded SQL must match the canonical
// source Query.SQL up to alias renames and bind style (PF-6.1–6.3).
// Flag-only: a deviation flips the unit's ledger status to deviated and
// counts in the run summary; the run itself never fails (PF-6.4).
func checkDBFidelity(ctx context.Context, opts Options, res *Result, svc *gen.Service, dbFilePath string, dbUnits []plan.Unit) {
	targets := make([]sqlchk.Target, 0, len(dbUnits))
	byMethod := make(map[string]plan.Unit, len(dbUnits))
	for _, u := range dbUnits {
		if len(u.QueryIDs) == 0 {
			continue
		}
		q := svc.Query(u.QueryIDs[0])
		if q == nil {
			continue
		}
		targets = append(targets, sqlchk.Target{Method: u.Name, QueryID: u.QueryIDs[0], Source: q.SQL})
		byMethod[u.Name] = u
	}
	if len(targets) == 0 {
		return
	}
	results, err := sqlchk.CheckDBFile(dbFilePath, targets)
	if err != nil {
		telemetry.Log(ctx).Warn("sql fidelity check unavailable", "error", err)
		return
	}
	for _, r := range results {
		switch r.Status {
		case sqlchk.StatusMatch:
			continue
		case sqlchk.StatusUnverifiable:
			telemetry.Log(ctx).Warn("sql fidelity: unverifiable db method", "method", r.Method, "query", r.QueryID)
			res.SQLDeviations = append(res.SQLDeviations, r.Method+" (unverifiable)")
			if u, ok := byMethod[r.Method]; ok {
				opts.Ledger.Set(u.ID, ledger.StatusAppended, "sql fidelity: unverifiable — no embedded SQL literal")
			}
		default:
			telemetry.Log(ctx).Warn("sql fidelity deviation", "method", r.Method, "query", r.QueryID, "deviations", deviationDetails(r.Deviations))
			res.SQLDeviations = append(res.SQLDeviations, fmt.Sprintf("%s (%s)", r.Method, sqlchk.Kinds(r.Deviations)))
			if u, ok := byMethod[r.Method]; ok {
				opts.Ledger.Set(u.ID, ledger.StatusDeviated, "sql fidelity: "+sqlchk.Kinds(r.Deviations))
			}
		}
	}
	writeAuditJSON(ctx, opts, "sql-fidelity.json", results)
}

// checkSQLFreeArtifacts runs the PF-6.5 negative gate over every generated
// Go file except the db store: controller/handler/view artifacts must never
// carry SQL keywords in string literals. A leak flips the owning unit (Go
// method name → plan unit) to deviated.
func checkSQLFreeArtifacts(ctx context.Context, opts Options, res *Result, dbFilePath string) {
	var paths []string
	for _, f := range res.Files {
		if f != dbFilePath && strings.HasSuffix(f, ".go") {
			paths = append(paths, f)
		}
	}
	if len(paths) == 0 {
		return
	}
	results, err := sqlchk.CheckSQLFree(paths)
	if err != nil {
		telemetry.Log(ctx).Warn("sql-free check unavailable", "error", err)
		return
	}
	byFn := map[string]plan.Unit{}
	for _, u := range opts.Plan.Units {
		if _, exists := byFn[u.Name]; !exists || u.Kind == plan.KindControllerMethod {
			byFn[u.Name] = u
		}
	}
	for _, r := range results {
		for _, d := range r.Deviations {
			telemetry.Log(ctx).Warn("sql-free leak", "file", r.Method, "detail", d.Detail)
			res.SQLDeviations = append(res.SQLDeviations, "sql-leak "+d.Detail)
			if u, ok := byFn[d.Fn]; ok {
				opts.Ledger.Set(u.ID, ledger.StatusDeviated, "sql fidelity: sql-leak ("+filepath.Base(r.Method)+")")
			}
		}
	}
	writeAuditJSON(ctx, opts, "sql-free.json", results)
}

func deviationDetails(devs []sqlchk.Deviation) string {
	parts := make([]string, 0, len(devs))
	for _, d := range devs {
		parts = append(parts, string(d.Kind)+": "+d.Detail)
	}
	return strings.Join(parts, "; ")
}

func writeAuditJSON(ctx context.Context, opts Options, name string, v any) {
	if opts.Audit == nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	if _, err := opts.Audit.Write(name, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}); err != nil {
		telemetry.Log(ctx).Warn("audit record failed", "file", name, "error", err)
	}
}
