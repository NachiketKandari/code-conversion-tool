package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/plan"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

// runPlan implements `tuxgo plan <file|dir> -mapping <yaml>` — the Phase 5
// decomposition gate (plan-conversion §3). The IR is extracted fresh (the
// extractor is deterministic, so state can never go stale), the user's
// endpoint mapping decides which conditions become APIs (§4.2.8 — the tool
// never invents endpoints), and the plan lands in the ledger directory with
// an audit copy.
func runPlan(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	mappingPath := fs.String("mapping", "", "User mapping YAML: service identity + which conditions become endpoints (default: convert.mapping from config)")
	configPath := fs.String("config", "", "Path to .tuxgo.yaml (default: ./.tuxgo.yaml when present, else defaults)")
	ledgerDir := fs.String("ledger", "", "Ledger directory for plan.json/plan.md (default: paths.ledger from config)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	target, err := resolveInput(positional, cfg)
	if err != nil {
		return err
	}
	mappingResolved, err := resolveMapping(*mappingPath, cfg)
	if err != nil {
		return err
	}
	*mappingPath = mappingResolved

	mapping, err := plan.LoadMapping(*mappingPath)
	if err != nil {
		return err
	}
	log.Info("mapping loaded",
		"service", mapping.Service,
		"module", mapping.Module,
		"endpoints", len(mapping.Endpoints),
		"db_method_pins", len(mapping.DBMethods))

	files, main, err := extractPlanIR(target)
	if err != nil {
		return err
	}
	logFileIR(ctx, main)

	src, err := os.ReadFile(main.Path)
	if err != nil {
		return fmt.Errorf("plan: read source %s: %w", main.Path, err)
	}
	b := budget.New(cfg.Run.MaxPromptTokens, cfg.Run.MaxOutputTokens, cfg.Run.CharsPerToken)
	p, err := plan.Build(plan.Options{Main: main, Source: string(src), FnFiles: files, Mapping: mapping, Budget: b})
	if err != nil {
		return err
	}

	ledger := *ledgerDir
	if ledger == "" {
		ledger = cfg.Paths.Ledger
	}
	if err := os.MkdirAll(ledger, 0o755); err != nil {
		return fmt.Errorf("plan: create ledger dir %s: %w", ledger, err)
	}
	base := filepath.Join(ledger, mapping.Service)
	if err := writeArtifact(base+".plan.json", func(w io.Writer) error { return plan.WriteJSON(w, p) }); err != nil {
		return err
	}
	if err := writeArtifact(base+".plan.md", func(w io.Writer) error { return plan.WriteMD(w, p) }); err != nil {
		return err
	}
	log.Info("plan written", "units", len(p.Units), "json", base+".plan.json", "md", base+".plan.md")
	archivePlan(ctx, p)

	// Human summary.
	fmt.Printf("%s: %d units (%d db, %d controllers, %d handlers), %d skipped, %d blockers\n",
		mapping.Service, len(p.Units), countKind(p, plan.KindDBMethod), countKind(p, plan.KindControllerMethod),
		countKind(p, plan.KindHandlerMethod), len(p.Skipped), len(p.Blockers))
	for _, b := range p.Blockers {
		fmt.Printf("  blocked: %s — %s\n", b.Fn, strings.Join(b.Endpoints, ", "))
	}
	for _, d := range p.Dropped {
		fmt.Printf("  dropped: %s\n", d)
	}
	return nil
}

// extractPlanIR extracts the IR for the plan: file mode returns that file;
// directory mode returns all files and picks the one with a Tuxedo entry.
func extractPlanIR(target string) ([]*ir.File, *ir.File, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot access target path %s: %w", target, err)
	}
	if !fi.IsDir() {
		main, err := ir.ExtractFile(target)
		if err != nil {
			return nil, nil, err
		}
		return []*ir.File{main}, main, nil
	}
	files, err := ir.ExtractDir(target)
	if err != nil {
		return nil, nil, err
	}
	var main *ir.File
	for _, f := range files {
		if f.Entry != "" {
			if main != nil {
				return nil, nil, fmt.Errorf("plan: %s holds multiple Tuxedo entries (%s, %s) — plan a single file at a time",
					target, main.Entry, f.Entry)
			}
			main = f
		}
	}
	if main == nil {
		return nil, nil, fmt.Errorf("plan: no .pc file in %s declares a Tuxedo entry function", target)
	}
	return files, main, nil
}

func countKind(p *plan.Plan, k plan.Kind) int {
	n := 0
	for _, u := range p.Units {
		if u.Kind == k {
			n++
		}
	}
	return n
}

func writeArtifact(path string, produce func(w io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("plan: write %s: %w", path, err)
	}
	defer f.Close()
	return produce(f)
}

func archivePlan(ctx context.Context, p *plan.Plan) {
	log := telemetry.Log(ctx)
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		log.Warn("audit archive unavailable", "error", err)
		return
	}
	if _, err := rec.Write(p.Service+"_plan.json", func(w io.Writer) error { return plan.WriteJSON(w, p) }); err != nil {
		log.Warn("audit archive write failed", "error", err)
		return
	}
	if _, err := rec.Write(p.Service+"_plan.md", func(w io.Writer) error { return plan.WriteMD(w, p) }); err != nil {
		log.Warn("audit archive write failed", "error", err)
		return
	}
	log.Info("plan archived", "service", p.Service)
}
