package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/config"
	"github.com/Public/convert-tux-to-go/internal/convert"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/gen"
	"github.com/Public/convert-tux-to-go/internal/ledger"
	"github.com/Public/convert-tux-to-go/internal/llm"
	"github.com/Public/convert-tux-to-go/internal/plan"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
	"github.com/Public/convert-tux-to-go/internal/validate"
)

// runConvert implements `tuxgo convert <file|dir> -mapping <yaml>` — the
// Phase 5 execution gate: deterministic plan units generate first (models,
// DB methods, interfaces, handler glue, router), then each mapped endpoint's
// controller body is filled through the LLM seam from the query-replaced
// branch view. Output lands in the target module when paths.mainGo resolves,
// else under paths.staged (the two-laptop degrade); the ledger makes runs
// resumable; every unit leaves an audit record. When the target directory
// holds several Tuxedo entry files, one worker converts each service
// end-to-end in parallel (convert_dir.go), each in its own output subtree.
func runConvert(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	mappingFlag := fs.String("mapping", "", "User mapping YAML, or a directory of per-service yamls (each with source: <entry file>) when the target dir holds multiple services (default: convert.mapping from config, else the mappings/ convention)")
	configPath := fs.String("config", "", "Path to .tuxgo.yaml (default: ./.tuxgo.yaml when present, else defaults)")
	baseDir := fs.String("base", "", "Output base directory override (default: target module root when paths.mainGo resolves, else paths.staged; dir fan-out appends each service name)")
	noLLM := fs.Bool("no-llm", false, "Deterministic-only run: skip controller bodies (overrides run.llm)")
	fragment := fs.Bool("fragment", false, "Force fragment mode on a single-file input (PF-3.1)")

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
	// Mapping resolution order: -mapping flag → convert.mapping → the
	// mappings/ convention (discover/auto-draft output). Nothing found →
	// scan the target, write drafts, and stop: the user reviews names,
	// then re-runs this same command to convert (§4.2.8 — the tool never
	// invents endpoints; it proposes and waits).
	mappingPath := mappingFor(*mappingFlag, cfg)
	if mappingPath == "" {
		return draftAndStop(ctx, target, cfg, *noLLM)
	}

	files, mains, excluded, err := extractPlanIR(ctx, target, cfg, *fragment)
	if err != nil {
		return err
	}

	// Run-wide wiring: one budget/audit bundle plus the per-purpose client
	// cover every service in the run (the bundle is immutable/mutex-guarded;
	// the client is stateless) — A5.2's shared runWiring base.
	wiring := newWiring(ctx, cfg)
	v := validate.New(validate.Options{MainGo: cfg.Paths.MainGo, Compile: cfg.ValidateCfg.Compile, RunSmoke: cfg.ValidateCfg.Run})
	client := resolveLLMClient(ctx, cfg, *noLLM, "controller bodies")
	w := &convertWiring{
		cfg: wiring.cfg, budget: wiring.budget, validator: v, client: client,
		audit: wiring.audit, llmEnabled: client != nil,
	}

	baseRoot, degrade := resolveBaseRoot(*baseDir, cfg)
	// Fan-out covers multi-entry dirs and filtered runs alike: when
	// convert.fileFilter dropped entries, the mapping path must be a
	// directory (per-service yamls) so the excluded services' mappings can
	// be exempted from the orphan check deliberately.
	if len(mains) > 1 || len(excluded) > 0 {
		return runConvertFanout(ctx, w, target, mains, files, excluded, mappingPath, baseRoot, degrade)
	}

	// A directory mapping for a single-entry target: pick the yaml whose
	// source:/stem matches the entry (the mappings/ convention). Nothing
	// matches — including a convention dir holding only foreign drafts —
	// and the run drafts-and-stops instead.
	if fi, serr := os.Stat(mappingPath); serr == nil && fi.IsDir() {
		mappingPath, err = mappingForEntry(mappingPath, filepath.Base(mains[0].Path))
		if err != nil {
			return err
		}
		if mappingPath == "" {
			return draftAndStop(ctx, target, cfg, *noLLM)
		}
	}
	mapping, err := plan.LoadMapping(mappingPath)
	if err != nil {
		return err
	}
	// Derived module (module == service): the staged tree roots at the
	// service folder, matching the dir fan-out's per-service subtrees —
	// absPath drops the target path's first segment (the module root), so
	// the base carries it.
	base := baseRoot
	if mapping.Module == mapping.Service {
		base = filepath.Join(base, mapping.Service)
	}
	res, led, err := convertOneService(ctx, w, mains[0], files, mapping, base, cfg.Concurrency.Workers)
	if err != nil {
		return err
	}
	printServiceSummary(mapping.Service, res, led, base, degrade)
	return nil
}

// convertWiring carries one convert invocation's shared, concurrency-safe
// collaborators: the LLM client (stateless), token budget (immutable
// value), validator (stateless options holder), and audit recorder
// (mutex-guarded Write).
type convertWiring struct {
	cfg        *config.Config
	budget     budget.Budget
	validator  *validate.Validator
	client     llm.Client
	audit      *audit.Recorder
	llmEnabled bool
}

// draftAndStop is convert's no-mapping fallback (draft, then stop): the
// target is scanned for API candidates and editable mapping drafts land in
// mappings/ — AI-named when the model is reachable, deterministic names
// otherwise — and the run exits before generating anything, so the user's
// review is a free re-run (no tree cleanup, no half-converted state).
func draftAndStop(ctx context.Context, target string, cfg *config.Config, noLLM bool) error {
	client := resolveLLMClient(ctx, cfg, noLLM, "endpoint naming")
	bd := budget.New(cfg.Run.MaxPromptTokens, cfg.Run.MaxOutputTokens, cfg.Run.CharsPerToken)
	n, err := discoverCore(ctx, target, discoverOutDir(""), false, client, bd)
	if err != nil {
		return err
	}
	telemetry.Log(ctx).Info("convert: no mapping found — drafts written, conversion deferred to the re-run",
		"target", target, "drafts", n)
	return nil
}

// resolveBaseRoot resolves the run's output base: the -base override wins,
// else the target module root when paths.mainGo resolves, else the staged
// tree with the two-laptop degrade note.
func resolveBaseRoot(baseFlag string, cfg *config.Config) (root, degrade string) {
	if baseFlag != "" {
		return baseFlag, ""
	}
	if root, err := validate.ResolveModuleRoot(cfg.Paths.MainGo); err == nil && cfg.Paths.MainGo != "" {
		return root, ""
	}
	return cfg.Paths.Staged, "generated code staged under " + cfg.Paths.Staged + " (target service absent: set paths.mainGo to compile there)"
}

// convertOneService runs the full per-service pipeline: plan build, ledger
// load, convert.Run (deterministic scaffold + LLM controller bodies), the
// ledger audit copy, and mock regeneration. base is this service's isolated
// output root (the shared baseRoot in single-service mode, a per-service
// subtree in dir fan-out); workers sizes the inner DB-render pool.
func convertOneService(ctx context.Context, w *convertWiring, main *ir.File, files []*ir.File, mapping *plan.Mapping, base string, workers int) (*convert.Result, *ledger.Ledger, error) {
	log := telemetry.Log(ctx)
	start := time.Now()
	log.Info("convert service started", "service", mapping.Service, "source", main.Path, "base", base)
	for _, u := range main.Unbalanced {
		log.Warn("unbalanced region in source — parse continues leniently, generated output may be incomplete",
			"service", mapping.Service, "kind", u.Kind, "line", u.Line, "col", u.Col)
	}
	src, err := os.ReadFile(main.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("convert: read source: %w", err)
	}
	p, err := plan.Build(plan.Options{Main: main, Source: string(src), FnFiles: files, Mapping: mapping, Budget: w.budget})
	if err != nil {
		return nil, nil, err
	}

	led, err := ledger.Load(w.cfg.Paths.Ledger, mapping.Service)
	if err != nil {
		return nil, nil, err
	}

	res, err := convert.Run(ctx, convert.Options{
		Plan: p, Main: main, Source: string(src), FnFiles: files,
		Client: w.client, Budget: w.budget, BaseDir: base,
		Ledger: led, Validator: w.validator, MaxRetries: w.cfg.ValidateCfg.MaxRetries, Audit: w.audit,
		Workers: workers,
		SkipLLM: !w.llmEnabled, WithGorm: w.cfg.DB.WithGorm,
		FlowDraft: w.cfg.Convert.FlowDraft == nil || *w.cfg.Convert.FlowDraft,
	})
	if err != nil {
		return nil, nil, err
	}
	if _, err := w.audit.WriteJSON(mapping.Service+"_ledger.json", led); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}

	runMocks(ctx, base, p)
	log.Info("convert service completed",
		"service", mapping.Service, "base", base,
		"files", len(res.Files), "llm_calls", res.LLMCalls,
		"duration_ms", time.Since(start).Milliseconds())
	return res, led, nil
}

// printServiceSummary reports one service's run outcome — the same lines in
// single-service and fan-out mode.
func printServiceSummary(service string, res *convert.Result, led *ledger.Ledger, base, degrade string) {
	appended, failed, blocked, skipped, placeholders, deviated := led.Counts()
	fmt.Printf("%s: %d files written under %s — units: %d appended, %d failed, %d blocked, %d skipped, %d placeholders, %d stubbed fns, %d sql deviations, %d llm calls\n",
		service, len(res.Files), base, appended, failed, blocked, skipped, placeholders, len(res.Stubs), deviated, res.LLMCalls)
	if degrade != "" {
		fmt.Println("  note:", degrade)
	}
	if res.TierB != nil && res.TierB.DegradeReason != "" {
		fmt.Println("  tier B:", res.TierB.DegradeReason)
	} else if res.TierB != nil {
		fmt.Println("  tier B:", res.TierB.Summary)
	}
	for _, f := range res.Failed {
		fmt.Println("  failed:", f)
	}
	for _, s := range res.Skipped {
		fmt.Println("  skipped:", s)
	}
	for _, d := range res.SQLDeviations {
		fmt.Println("  sql deviation:", d)
	}
	for _, st := range res.Stubs {
		fmt.Println("  stubbed fn (panics until implemented):", st)
	}
}

// runMocks regenerates the uber-go/mock doubles when the mockgen binary and
// the target module are both available; otherwise it is a WARN + skip —
// never a run failure (plan-conversion §4.7). The runner lives in
// internal/gen (shared with `gentest`, PRD-2026-09-09 GT-D7); this wrapper
// computes the plan's two interface targets.
func runMocks(ctx context.Context, base string, p *plan.Plan) {
	targets := gen.MockTargetsFor(base, p.Service)
	for i := range targets {
		// The plan's unit target paths may override the conventional
		// service subtree (custom layouts) — keep the mockRel resolution.
		folder := "db"
		if i == 1 {
			folder = "controller"
		}
		targets[i].Source = filepath.Join(base, mockRel(p, folder, "interface.go"))
		targets[i].Dest = filepath.Join(base, mockRel(p, folder, filepath.Base(targets[i].Dest)))
	}
	gen.RunMocks(ctx, targets)
}

func mockRel(p *plan.Plan, folder, file string) string {
	for _, u := range p.Units {
		if strings.HasSuffix(u.TargetPath, "/"+folder+"/"+file) {
			parts := strings.SplitN(u.TargetPath, "/", 2)
			if len(parts) == 2 {
				return parts[1]
			}
		}
	}
	return folder + "/" + file
}
