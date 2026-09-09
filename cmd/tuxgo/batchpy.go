package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/common"
	"github.com/Public/convert-tux-to-go/internal/config"
	"github.com/Public/convert-tux-to-go/internal/cproc/batchflow"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
	"github.com/Public/convert-tux-to-go/internal/pygen"
	"github.com/Public/convert-tux-to-go/internal/pyplan"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

// runBatchpy implements `tuxgo batchpy <file|dir>` (PRD-2026-09-08): Pro*C
// batch programs → Python service modules reusing the tux parse stack. The
// deterministic scaffold (SQL constants, DAL/repository, service shell)
// always generates; the service body of repository-shape batches fills
// through the LLM seam unless -no-llm. Every module passes the pychk syntax
// gate + SQL fidelity check and reports its logic retention per mode.
func runBatchpy(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("batchpy", flag.ContinueOnError)
	outDir := fs.String("out", "", "Output directory for generated modules (default: batchpy.outDir from config)")
	configPath := fs.String("config", "", "Path to .tuxgo.yaml (default: ./.tuxgo.yaml when present, else defaults)")
	noLLM := fs.Bool("no-llm", false, "Deterministic-only run: skip the service-body LLM seam (overrides run.llm)")
	shape := fs.String("shape", "", "auto|repo — shape rubric override (default: batchpy.shape from config)")
	dmlLoop := fs.String("dml-loop", "", "batch|rowbyrow — cursor-DML semantics (default: batchpy.dmlLoop from config)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	target, err := resolveBatchInput(positional, cfg)
	if err != nil {
		return fmt.Errorf("batchpy: %w", err)
	}

	b := cfg.Batchpy
	for _, o := range []struct {
		v, name string
		allowed []string
	}{
		{*shape, "batchpy.shape", []string{"auto", "repo"}},
		{*dmlLoop, "batchpy.dmlLoop", []string{"batch", "rowbyrow"}},
	} {
		if o.v == "" {
			continue
		}
		ok := false
		for _, a := range o.allowed {
			if o.v == a {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("-%s: got %q, want one of %v", strings.TrimPrefix(o.name, "batchpy."), o.v, o.allowed)
		}
	}
	if *shape != "" {
		b.Shape = *shape
	}
	if *dmlLoop != "" {
		b.DMLLoop = *dmlLoop
	}

	out := *outDir
	if out == "" {
		out = b.OutDir
	}
	if out == "" {
		out = config.DefaultBatchpyOut
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("batchpy: create output dir %s: %w", out, err)
	}

	paths, err := batchTargets(ctx, target, cfg.Batchpy.FileFilter)
	if err != nil {
		return err
	}

	client := resolveLLMClient(ctx, cfg, *noLLM, "batch service bodies")
	llmEnabled := client != nil
	wiring := newWiring(ctx, cfg)
	bg := wiring.budget
	rec := wiring.audit

	// Per-file worker fan-out (PRD-2026-09-08 BP-2.1): each worker converts
	// one batch file end-to-end — scan → flow → plan → generate → gates →
	// write — bounded by concurrency.workers (default 1 keeps runs serial;
	// workers>1 parallelizes the per-file pipeline including LLM calls, so
	// endpoint rate limits are the ceiling). Results print in input order.
	workers := cfg.Concurrency.Workers
	log.Info("batchpy fan-out", "target", target, "files", len(paths),
		"workers", workers, "out", out)
	type fileResult struct {
		path  string
		res   pygen.Result
		plan  *pyplan.Plan
		name  string
		start time.Time
		skip  string
		err   error
	}
	results := make([]fileResult, len(paths))
	common.RunIndexed(len(paths), workers, func(i int) {
		path := paths[i]
		start := time.Now()
		telemetry.Log(ctx).Info("batch file started", "source", path)
		res, plan, name, err := convertBatchFile(ctx, path, b, pygen.Options{
			NoLLM: !llmEnabled || client == nil, Client: client, Budget: bg, MaxRetries: cfg.ValidateCfg.MaxRetries,
			Audit: rec,
		})
		var wp *wrongPipelineError
		if errors.As(err, &wp) {
			results[i] = fileResult{path: path, skip: err.Error(), start: start}
			return
		}
		results[i] = fileResult{path: path, res: res, plan: plan, name: name, start: start, err: err}
	})

	// Module-collision gate: the module name comes from the batch's
	// c_ServiceName literal, so distinct files can declare the same module —
	// concurrent writes to one .py would silently mix generations. Hard
	// error before anything is written (deterministic-first, loud).
	seen := map[string]string{}
	for _, r := range results {
		if r.err != nil || r.skip != "" {
			continue
		}
		if prev, dup := seen[r.name]; dup {
			return fmt.Errorf("batchpy: %s and %s both produce module %s.py — batch service names must be unique within one directory run (convert the files individually if intentional)", prev, r.path, r.name)
		}
		seen[r.name] = r.path
	}

	summary := 0
	skipped := 0
	var firstErr error
	for _, r := range results {
		if r.skip != "" {
			log.Warn("wrong pipeline — file skipped (severity F4 guard)", "source", r.path, "reason", r.skip)
			fmt.Printf("skipped: %s — %s\n", filepath.Base(r.path), r.skip)
			skipped++
			continue
		}
		if r.err != nil {
			log.Error("batchpy file failed", "source", r.path, "error", r.err,
				"duration_ms", time.Since(r.start).Milliseconds())
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		dst := filepath.Join(out, r.name+".py")
		if werr := os.WriteFile(dst, []byte(r.res.Content), 0o644); werr != nil {
			log.Error("batchpy file failed", "source", r.path, "error", werr)
			if firstErr == nil {
				firstErr = fmt.Errorf("batchpy: write %s: %w", dst, werr)
			}
			continue
		}
		log.Info("batch module written", "source", r.path, "output", dst,
			"shape", r.res.Retention.Shape, "dml_loop", r.res.Retention.DMLLoop,
			"retention_pct", fmt.Sprintf("%.1f", r.res.Retention.Percent()),
			"sql_deviations", r.res.Retention.SQLDeviations, "llm_calls", r.res.LLMCalls,
			"duration_ms", time.Since(r.start).Milliseconds())
		archiveBatchArtifacts(ctx, rec, r.name, r.plan, r.res)
		res := r.res
		ret := res.Retention
		summary += ret.SQLDeviations
		fmt.Printf("%s: shape=%s dml=%s consts=%d phases=%d syntax=%s sql_deviations=%d retention=%.0f%% llm_calls=%d -> %s\n",
			r.name, ret.Shape, ret.DMLLoop, ret.SelectTotal+ret.DMLTotal, ret.PhasesTotal, ret.PyMode, ret.SQLDeviations, ret.Percent(), ret.LLMCalls, filepath.Join(out, r.name+".py"))
		for _, n := range res.Notes {
			fmt.Println("  note:", n)
		}
		for _, d := range res.Fidelity {
			if d.Status == "deviated" {
				for _, dv := range d.Deviations {
					fmt.Printf("  sql deviation: %s [%s] %s\n", d.Method, dv.Kind, dv.Detail)
				}
			}
			if d.Status == "unverifiable" {
				fmt.Println("  sql unverifiable:", d.Method)
			}
		}
		if !res.PyOK && ret.PyMode == "ast" {
			fmt.Println("  python syntax:", res.PyDetail)
		}
	}
	fmt.Printf("batchpy: %d module(s) written under %s — %d sql deviations total", len(paths)-skipped, out, summary)
	if skipped > 0 {
		fmt.Printf(", %d skipped", skipped)
	}
	fmt.Println()
	return firstErr
}

// wrongPipelineError marks a Tuxedo service entry (SVC_*) fed to the batch
// pipeline (severity F4) — a visible skip, never a plausible-looking empty
// module.
type wrongPipelineError struct{ entry string }

func (e *wrongPipelineError) Error() string {
	return fmt.Sprintf("%s is a Tuxedo service entry, not a batch program — wrong pipeline; convert it with `tuxgo convert`", e.entry)
}

// convertBatchFile runs the batchpy pipeline for one .pc file — scan →
// flow → plan → generate → gates — and returns the generated module; the
// caller owns the ordered write phase (collision gate + input-order
// writes), so workers never race one output file.
func convertBatchFile(ctx context.Context, path string, b config.Batchpy, gOpts pygen.Options) (pygen.Result, *pyplan.Plan, string, error) {
	facts, err := scanner.ScanFile(path)
	if err != nil {
		return pygen.Result{}, nil, "", fmt.Errorf("batchpy: scan %s: %w", path, err)
	}
	irf, err := ir.ExtractFileOpts(path, ir.Options{})
	if err != nil {
		return pygen.Result{}, nil, "", fmt.Errorf("batchpy: extract %s: %w", path, err)
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return pygen.Result{}, nil, "", fmt.Errorf("batchpy: read %s: %w", path, err)
	}

	flow := batchflow.Build(irf, facts, string(src))
	if strings.HasPrefix(flow.Entry, "SVC_") {
		// Wrong-pipeline guard (severity F4): the flow entry fell back to a
		// Tuxedo service function — no batch main exists here.
		return pygen.Result{}, nil, "", &wrongPipelineError{entry: flow.Entry}
	}
	if len(flow.Queries) == 0 {
		telemetry.Log(ctx).Warn("batch carries no SQL — the module will have no constants and no DAL (degenerate but visible)",
			"source", path)
	}
	plan := pyplan.Build(flow, pyplan.Options{
		Shape: b.Shape, DMLLoop: b.DMLLoop, ChunkSize: b.ChunkSize,
		LoggerPrefix: b.LoggerPrefix, Entrypoint: b.Entrypoint,
		Wrapper: pyplan.Wrapper{
			Import: b.WrapperModule, RouterClass: b.RouterClass,
			ReadMode: b.ReadMode, WriteMode: b.WriteMode,
		},
	})
	gOpts.Plan = plan
	gOpts.Source = string(src)
	gOpts.SourcePath = path

	res, err := pygen.Generate(ctx, gOpts)
	if err != nil {
		return pygen.Result{}, nil, "", fmt.Errorf("batchpy: generate %s: %w", path, err)
	}
	return res, plan, plan.Module, nil
}

// batchTargets resolves the input to a list of .pc/.pcf files (file or dir).
// A directory target is scoped by the batchpy.fileFilter yaml when set —
// base names containing the substring (case-insensitive) survive; an empty
// filter keeps every file. An explicitly passed file always converts.
func batchTargets(ctx context.Context, target string, filter string) ([]string, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("batchpy: cannot access %s: %w", target, err)
	}
	if !fi.IsDir() {
		return []string{target}, nil
	}
	var paths []string
	err = filepath.Walk(target, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if !info.IsDir() {
			switch strings.ToLower(filepath.Ext(p)) {
			case ".pc", ".pcf":
				paths = append(paths, p)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(filter) != "" {
		needle := strings.ToLower(filter)
		var kept []string
		for _, p := range paths {
			if strings.Contains(strings.ToLower(filepath.Base(p)), needle) {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("batchpy: no .pc/.pcf file in %s matches batchpy.fileFilter %q", target, filter)
		}
		telemetry.Log(ctx).Info("batchpy.fileFilter applied",
			"filter", filter, "matched", len(kept), "of", len(paths))
		paths = kept
	}
	return paths, nil
}

// archiveBatchArtifacts writes the module, its plan, and its retention
// report into the run's audit trail (best-effort, never fatal). JSON
// artifacts go through the shared WriteJSON (nil-receiver tolerated, A5.2).
func archiveBatchArtifacts(ctx context.Context, rec *audit.Recorder, name string, plan *pyplan.Plan, res pygen.Result) {
	if rec == nil {
		return
	}
	log := telemetry.Log(ctx)
	if _, err := rec.Write(name+".py", func(w io.Writer) error {
		_, werr := w.Write([]byte(res.Content))
		return werr
	}); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
	if _, err := rec.WriteJSON(name+".batchplan.json", plan); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
	if _, err := rec.WriteJSON(name+".retention.json", res.Retention); err != nil {
		log.Warn("audit archive write failed", "error", err)
	}
	if len(res.Notes) > 0 {
		if _, err := rec.Write(name+".notes.txt", func(w io.Writer) error {
			for _, n := range res.Notes {
				if _, werr := w.Write([]byte(n + "\n")); werr != nil {
					return werr
				}
			}
			return nil
		}); err != nil {
			log.Warn("audit archive write failed", "error", err)
		}
	}
}
