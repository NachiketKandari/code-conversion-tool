package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/config"
	"github.com/Public/convert-tux-to-go/internal/convert"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
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
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	mappingPath := fs.String("mapping", "", "User mapping YAML, or a directory of per-service yamls (each with source: <entry file>) when the target dir holds multiple services (default: convert.mapping from config)")
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
	mappingResolved, err := resolveMapping(*mappingPath, cfg)
	if err != nil {
		return err
	}
	*mappingPath = mappingResolved

	files, mains, excluded, err := extractPlanIR(ctx, target, cfg, *fragment)
	if err != nil {
		return err
	}

	// Run-wide wiring: one LLM client, budget, validator, and audit
	// recorder cover every service in the run — the fan-out shares them
	// (the client is stateless, the budget immutable, the validator a
	// stateless options holder, the recorder mutex-guarded).
	b := budget.New(cfg.Run.MaxPromptTokens, cfg.Run.MaxOutputTokens, cfg.Run.CharsPerToken)
	v := validate.New(validate.Options{MainGo: cfg.Paths.MainGo, Compile: cfg.ValidateCfg.Compile, RunSmoke: cfg.ValidateCfg.Run})
	llmEnabled := cfg.Run.LLM && !*noLLM
	var client llm.Client
	if llmEnabled {
		c, cerr := llm.NewFromConfig(ctx, cfg, "")
		if cerr != nil {
			// The client is only needed for pending controller units; a fully
			// resumed run proceeds without it.
			log.Warn("llm client unavailable — pending controller units will fail", "error", cerr)
			client = nil
		} else {
			client = c
		}
	} else {
		log.Info("llm disabled — deterministic-only run, controller bodies will be skipped")
		client = nil
	}
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		log.Warn("audit archive unavailable", "error", err)
		rec = nil
	}
	w := &convertWiring{
		cfg: cfg, budget: b, validator: v, client: client,
		audit: rec, llmEnabled: llmEnabled,
	}

	baseRoot, degrade := resolveBaseRoot(*baseDir, cfg)
	// Fan-out covers multi-entry dirs and filtered runs alike: when
	// convert.fileFilter dropped entries, the mapping path must be a
	// directory (per-service yamls) so the excluded services' mappings can
	// be exempted from the orphan check deliberately.
	if len(mains) > 1 || len(excluded) > 0 {
		return runConvertFanout(ctx, w, target, mains, files, excluded, *mappingPath, baseRoot, degrade)
	}

	mapping, err := plan.LoadMapping(*mappingPath)
	if err != nil {
		return err
	}
	res, led, err := convertOneService(ctx, w, mains[0], files, mapping, baseRoot, cfg.Concurrency.Workers)
	if err != nil {
		return err
	}
	printServiceSummary(mapping.Service, res, led, baseRoot, degrade)
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
	})
	if err != nil {
		return nil, nil, err
	}
	if w.audit != nil {
		if _, err := w.audit.Write(mapping.Service+"_ledger.json", func(wr io.Writer) error {
			data, merr := json.MarshalIndent(led, "", "  ")
			if merr != nil {
				return merr
			}
			_, werr := wr.Write(data)
			return werr
		}); err != nil {
			log.Warn("audit archive write failed", "error", err)
		}
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
	fmt.Printf("%s: %d files written under %s — units: %d appended, %d failed, %d blocked, %d skipped, %d placeholders, %d sql deviations, %d llm calls\n",
		service, len(res.Files), base, appended, failed, blocked, skipped, placeholders, deviated, res.LLMCalls)
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
	for _, bl := range res.Blocked {
		fmt.Println("  blocked:", bl)
	}
}

// runMocks regenerates the uber-go/mock doubles when the mockgen binary and
// the target module are both available; otherwise it is a WARN + skip —
// never a run failure (plan-conversion §4.7).
func runMocks(ctx context.Context, base string, p *plan.Plan) {
	log := telemetry.Log(ctx)
	mockgen, err := exec.LookPath("mockgen")
	if err != nil {
		log.Warn("mockgen not found — mocks skipped (uber-go/mock requires docs/dependencies.md onboarding before wiring)")
		return
	}
	type ifc struct{ src, dst, name string }
	targets := []ifc{
		{filepath.Join(base, mockRel(p, "db", "interface.go")), filepath.Join(base, mockRel(p, "db", "mock_store.go")), serviceName(p) + "Store"},
		{filepath.Join(base, mockRel(p, "controller", "interface.go")), filepath.Join(base, mockRel(p, "controller", "mock_controller.go")), serviceName(p) + "Controller"},
	}
	for _, t := range targets {
		if _, err := os.Stat(t.src); err != nil {
			continue
		}
		cmd := exec.Command(mockgen, "-source", t.src, "-destination", t.dst, "-package", filepath.Base(filepath.Dir(t.dst)), t.name)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Warn("mockgen failed (best-effort)", "interface", t.name, "output", string(out))
		} else {
			log.Info("mocks generated", "interface", t.name, "path", t.dst)
		}
	}
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

func serviceName(p *plan.Plan) string { return p.Service }
