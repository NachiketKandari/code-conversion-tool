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

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/convert"
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
// resumable; every unit leaves an audit record.
func runConvert(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	mappingPath := fs.String("mapping", "", "User mapping YAML (required — endpoints are user-specified)")
	configPath := fs.String("config", "", "Path to .tuxgo.yaml (default: ./.tuxgo.yaml when present, else defaults)")
	baseDir := fs.String("base", "", "Output base directory override (default: target module root when paths.mainGo resolves, else paths.staged)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) == 0 {
		return fmt.Errorf("must provide a .pc/.pcf file or directory to convert")
	}
	if *mappingPath == "" {
		return fmt.Errorf("must provide -mapping <yaml> — endpoints are user-specified (PRD §4.2.8)")
	}
	target := positional[0]

	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	mapping, err := plan.LoadMapping(*mappingPath)
	if err != nil {
		return err
	}
	files, main, err := extractPlanIR(target)
	if err != nil {
		return err
	}
	src, err := os.ReadFile(main.Path)
	if err != nil {
		return fmt.Errorf("convert: read source: %w", err)
	}
	b := budget.New(cfg.Run.MaxPromptTokens, cfg.Run.MaxOutputTokens, cfg.Run.CharsPerToken)
	p, err := plan.Build(plan.Options{Main: main, Source: string(src), FnFiles: files, Mapping: mapping, Budget: b})
	if err != nil {
		return err
	}

	base := *baseDir
	degrade := ""
	if base == "" {
		if root, rootErr := validate.ResolveModuleRoot(cfg.Paths.MainGo); rootErr == nil && cfg.Paths.MainGo != "" {
			base = root
		} else {
			base = cfg.Paths.Staged
			degrade = "generated code staged under " + base + " (target service absent: set paths.mainGo to compile there)"
		}
	}

	led, err := ledger.Load(cfg.Paths.Ledger, mapping.Service)
	if err != nil {
		return err
	}
	v := validate.New(validate.Options{MainGo: cfg.Paths.MainGo, Compile: cfg.ValidateCfg.Compile, RunSmoke: cfg.ValidateCfg.Run})

	client, err := llm.NewFromConfig(ctx, cfg, "")
	if err != nil {
		// The client is only needed for pending controller units; a fully
		// resumed run proceeds without it.
		log.Warn("llm client unavailable — pending controller units will fail", "error", err)
		client = nil
	}

	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		log.Warn("audit archive unavailable", "error", err)
		rec = nil
	}

	res, err := convert.Run(ctx, convert.Options{
		Plan: p, Main: main, Source: string(src), FnFiles: files,
		Client: client, Budget: b, BaseDir: base,
		Ledger: led, Validator: v, MaxRetries: cfg.ValidateCfg.MaxRetries, Audit: rec,
	})
	if err != nil {
		return err
	}
	if rec != nil {
		if _, err := rec.Write(mapping.Service+"_ledger.json", func(w io.Writer) error {
			data, merr := json.MarshalIndent(led, "", "  ")
			if merr != nil {
				return merr
			}
			_, werr := w.Write(data)
			return werr
		}); err != nil {
			log.Warn("audit archive write failed", "error", err)
		}
	}

	runMocks(ctx, base, p)

	appended, failed, blocked, _ := led.Counts()
	fmt.Printf("%s: %d files written under %s — units: %d appended, %d failed, %d blocked, %d llm calls\n",
		mapping.Service, len(res.Files), base, appended, failed, blocked, res.LLMCalls)
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
	for _, bl := range res.Blocked {
		fmt.Println("  blocked:", bl)
	}
	return nil
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
