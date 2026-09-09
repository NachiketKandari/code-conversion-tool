package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/config"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
	"github.com/Public/convert-tux-to-go/internal/testgen"
	"github.com/Public/convert-tux-to-go/internal/testscan"
)

// gentestLayers enumerates the service layers gentest can target; the
// layout contract is pkg/services/<svc>/{db,controller,handler,models}
// (PRD-2026-09-09 GT-D1).
var gentestLayers = []string{"db", "controller", "handler"}

// runGentest implements `tuxgo gentest <file|dir>` (PRD-2026-09-09): the
// post-conversion function → Go test pipeline over a converted Go service
// tree. The scan phase (GT-1) resolves the target, inventories functions,
// and detects existing coverage; -check-only reports the gap and writes
// nothing. Templates (GT-2) and generation (GT-3+) are pending.
func runGentest(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("gentest", flag.ContinueOnError)
	layers := fs.String("layers", "", "Comma-separated subset of db,controller,handler (default: all layers found in the target)")
	checkOnly := fs.Bool("check-only", false, "Report the test gap (functions without tests) and exit without writing")
	baseDir := fs.String("base", "", "Output base directory override (default: paths.staged — staged-first, the target tree is never written implicitly)")
	noLLM := fs.Bool("no-llm", false, "Deterministic-only run: skip the LLM gap-fill seam (overrides run.llm)")
	configPath := fs.String("config", "", "Path to .tuxgo.yaml (default: ./.tuxgo.yaml when present, else defaults)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	if len(positional) == 0 {
		return fmt.Errorf("gentest: must provide a target (.go file, layer dir, service dir, or services root)")
	}
	target := positional[0]

	var layerFilter []testscan.Layer
	if *layers != "" {
		for _, l := range strings.Split(*layers, ",") {
			l = strings.TrimSpace(l)
			known := false
			for _, k := range gentestLayers {
				if l == k {
					known = true
					break
				}
			}
			if !known {
				return fmt.Errorf("gentest: -layers: got %q, want one of %v", l, gentestLayers)
			}
			layerFilter = append(layerFilter, testscan.Layer(l))
		}
	}

	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	tgt, err := testscan.Resolve(target, layerFilter)
	if err != nil {
		return err
	}
	log.Info("gentest invoked", "target", target, "mode", string(tgt.Mode),
		"layers", strings.Join(layerNames(layerFilter), ","), "check_only", *checkOnly,
		"base", *baseDir, "no_llm", *noLLM)
	rep, err := tgt.Scan()
	if err != nil {
		return err
	}
	total, tested, gaps := rep.Counts()
	log.Info("gentest scan complete", "target", tgt.Path, "mode", string(tgt.Mode),
		"services", len(rep.Services), "functions", total, "tested", tested, "gaps", gaps,
		"warnings", len(rep.Warnings))

	if *checkOnly {
		if err := rep.WriteText(os.Stdout); err != nil {
			return err
		}
		fmt.Println("check-only: nothing written")
		archiveGapReport(ctx, rep)
		return nil
	}

	// Staged-first output: -base wins, else paths.staged.
	base := *baseDir
	if base == "" {
		base = cfg.Paths.Staged
	}
	if base == "" {
		base = config.DefaultStagedDir
	}

	client := resolveLLMClient(ctx, cfg, *noLLM, "controller gap-fill")
	llmEnabled := client != nil

	wiring := newWiring(ctx, cfg)

	res, err := testgen.Generate(ctx, tgt, rep, testgen.Options{
		BaseDir: base, Workers: cfg.Concurrency.Workers, NoLLM: !llmEnabled,
		Client: client, Budget: wiring.budget,
		MaxRetries: cfg.ValidateCfg.MaxRetries, Audit: wiring.audit,
	})
	if err != nil {
		return err
	}
	printGentestSummary(base, res)
	return nil
}

// printGentestSummary reports one run's per-function outcomes in input
// order, convert-summary style.
func printGentestSummary(base string, res *testgen.Result) {
	counts := map[string]int{}
	for _, u := range res.Units {
		counts[u.Status]++
	}
	fmt.Printf("gentest: %d functions under %s — %d generated (template), %d generated (llm), %d llm-required, %d skipped-covered, %d skipped-by-design, %d unsupported, %d llm calls\n",
		len(res.Units), base, counts[testgen.StatusTemplate], counts[testgen.StatusLLM], counts[testgen.StatusLLMNeeded],
		counts["skipped-covered"], counts[testgen.StatusDesign], counts[testgen.StatusUnsupported], res.LLMCalls)
	for _, f := range res.Files {
		fmt.Println("  wrote:", f)
	}
	for _, u := range res.Units {
		switch u.Status {
		case testgen.StatusLLMNeeded, testgen.StatusUnsupported, testgen.StatusDesign:
			fmt.Printf("  %s: %s — %s\n", u.Status, u.Func, u.Detail)
		}
	}
	for _, g := range res.Gates {
		fmt.Println("  gate:", g)
	}
	for _, w := range res.Warnings {
		fmt.Println("  warning:", w)
	}
}

// layerNames renders a layer filter for logs; nil means every testable
// layer.
func layerNames(layers []testscan.Layer) []string {
	if len(layers) == 0 {
		return gentestLayers
	}
	names := make([]string, 0, len(layers))
	for _, l := range layers {
		names = append(names, string(l))
	}
	return names
}

// archiveGapReport persists the scan outcome (machine twin of the stdout
// gap report) into the run's audit trail — best-effort, never fatal.
func archiveGapReport(ctx context.Context, rep *testscan.Report) {
	log := telemetry.Log(ctx)
	if _, err := auditRecorder(ctx).WriteJSON("gentest_gap_report.json", rep); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
}
