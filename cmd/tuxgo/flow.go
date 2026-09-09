package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/cproc/flow"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

// runFlow implements `tuxgo flow <file|dir>` — the PRD-2026-09-10 accuracy
// instrument: per-function flow trees (branches, loops, SQL spans, returns,
// residual statements), the coverage metric (classified vs residue), the
// idiom hints, and — with -go — the deterministic transpilation draft.
// Inspection only: nothing is staged, no LLM is called, no pipeline state
// changes.
func runFlow(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("flow", flag.ContinueOnError)
	outPath := fs.String("out", "", "Write the flow JSON to this path (defaults to stdout summary only)")
	showGo := fs.Bool("go", false, "Print the deterministic Go transpilation draft per function")
	configPath := fs.String("config", "", "Path to .tuxgo.yaml (default: ./.tuxgo.yaml when present, else defaults)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) == 0 {
		return fmt.Errorf("must provide a .pc/.pcf file or a directory to flow-analyze")
	}
	target := positional[0]

	if *configPath != "" {
		if _, _, err := loadRunConfig(*configPath); err != nil {
			return err
		}
	}

	irFiles, err := extractFlowIR(target)
	if err != nil {
		return err
	}
	if len(irFiles) == 0 {
		return fmt.Errorf("no .pc or .pcf files found in %s", target)
	}

	report := flowReport{Target: target}
	for _, f := range irFiles {
		src, err := os.ReadFile(f.Path)
		if err != nil {
			return fmt.Errorf("flow: read %s: %w", f.Path, err)
		}
		facts, err := scanner.ScanBytes(src, f.Path)
		if err != nil {
			return fmt.Errorf("flow: scan %s: %w", f.Path, err)
		}
		fr := flowFile{Path: f.Path}
		for _, fn := range flowTargets(facts, f.Entry) {
			tree := flow.Build(src, facts, fn, f)
			hints := flow.Match(tree)
			fr.Functions = append(fr.Functions, flowFunc{Name: fn, Tree: tree, Hints: hints})
			printFlowFn(fn, tree, hints, *showGo)
		}
		report.Files = append(report.Files, fr)
	}

	if *outPath != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			return fmt.Errorf("failed writing flow JSON to %s: %w", *outPath, err)
		}
		log.Info("flow json written", "path", *outPath)
	}
	log.Info("flow analysis complete", "files", len(report.Files))
	return nil
}

// extractFlowIR resolves the target (file or directory) through the
// extraction path so fragments and the buffer registry apply identically.
func extractFlowIR(target string) ([]*ir.File, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("cannot access target path %s: %w", target, err)
	}
	if fi.IsDir() {
		return ir.ExtractDirOpts(target, ir.Options{})
	}
	f, err := ir.ExtractFileOpts(target, ir.Options{})
	if err != nil {
		return nil, err
	}
	return []*ir.File{f}, nil
}

// flowTargets picks the functions to build: the IR entry when named, else
// every function (fn libraries).
func flowTargets(facts *scanner.SourceFacts, entry string) []string {
	if entry != "" {
		return []string{entry}
	}
	var out []string
	for _, f := range facts.Functions {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

func printFlowFn(fn string, tree *flow.Tree, hints []flow.Hint, showGo bool) {
	cov := tree.Coverage
	pct := 100
	if cov.CodeLines > 0 {
		pct = cov.Classified * 100 / cov.CodeLines
	}
	fmt.Printf("\n%s  (lines %d-%d)\n", fn, tree.StartLine, tree.EndLine)
	fmt.Printf("  coverage: %d/%d code lines classified (%d%%), unknown %d", cov.Classified, cov.CodeLines, pct, cov.Unknown)
	if len(cov.Residue) > 0 {
		fmt.Printf(" at lines %s", joinInts(cov.Residue, 8))
	}
	fmt.Println()
	for _, h := range hints {
		fmt.Printf("  hint [%s] line %d: %s\n", h.Kind, h.Line, h.Detail)
	}
	if showGo {
		out := flow.RenderTree(tree, nil)
		fmt.Println("  draft:")
		for _, line := range strings.Split(strings.TrimRight(out.Body, "\n"), "\n") {
			fmt.Printf("  %s\n", line)
		}
		for _, t := range out.TODOs {
			fmt.Printf("  gap: %s\n", t)
		}
	}
}

func joinInts(xs []int, max int) string {
	if len(xs) > max {
		xs = xs[:max]
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, ",")
}

type flowReport struct {
	Target string     `json:"target"`
	Files  []flowFile `json:"files"`
}

type flowFile struct {
	Path      string     `json:"path"`
	Functions []flowFunc `json:"functions"`
}

type flowFunc struct {
	Name  string      `json:"name"`
	Tree  *flow.Tree  `json:"tree"`
	Hints []flow.Hint `json:"hints"`
}
