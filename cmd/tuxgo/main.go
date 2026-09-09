package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/cproc/analyzer"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

const version = "0.1.0"

// Durable per-run artifact locations (architecture.md §4): conversion_logs/ is
// the single root for everything the tool writes at runtime — the unified
// slog stream in conversion_logs/logs/, the per-run audit trail in
// conversion_logs/audit/ (ledger + IR state join there in Phases 2/5).
const (
	defaultLogDir = "conversion_logs/logs"
	auditDir      = "conversion_logs/audit"
)

func main() {
	verbose, logDir, rest := extractGlobalFlags(os.Args[1:])
	if len(rest) == 0 {
		printUsage()
		os.Exit(1)
	}

	runID := newRunID(logDir, auditDir, time.Now())
	ctx := telemetry.WithRunID(context.Background(), runID)

	cleanup, err := telemetry.Init(telemetry.Config{
		Verbose: verbose,
		LogDir:  logDir,
		RunID:   runID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed initializing logger in %s: %v\n", logDir, err)
		os.Exit(1)
	}
	defer cleanup()

	log := telemetry.Log(ctx)
	started := time.Now()
	log.Info("run started", "version", version, "command", rest[0], "args", rest[1:], "log_dir", logDir, "verbose", verbose)

	switch rest[0] {
	case "analyze":
		if err := runAnalyze(ctx, rest[1:]); err != nil {
			log.Error("analyze failed", "error", err)
			os.Exit(1)
		}
	case "extract":
		if err := runExtract(ctx, rest[1:]); err != nil {
			log.Error("extract failed", "error", err)
			os.Exit(1)
		}
	case "plan":
		if err := runPlan(ctx, rest[1:]); err != nil {
			log.Error("plan failed", "error", err)
			os.Exit(1)
		}
	case "convert":
		if err := runConvert(ctx, rest[1:]); err != nil {
			log.Error("convert failed", "error", err)
			os.Exit(1)
		}
	case "batchpy":
		if err := runBatchpy(ctx, rest[1:]); err != nil {
			log.Error("batchpy failed", "error", err)
			os.Exit(1)
		}
	case "gentest":
		if err := runGentest(ctx, rest[1:]); err != nil {
			log.Error("gentest failed", "error", err)
			os.Exit(1)
		}
	case "flow":
		if err := runFlow(ctx, rest[1:]); err != nil {
			log.Error("flow failed", "error", err)
			os.Exit(1)
		}
	case "discover":
		if err := runDiscover(ctx, rest[1:]); err != nil {
			log.Error("discover failed", "error", err)
			os.Exit(1)
		}
	case "version":
		fmt.Printf("tuxgo version %s\n", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		log.Error("unknown command", "command", rest[0])
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", rest[0])
		printUsage()
		os.Exit(1)
	}

	// Every command's wall-clock duration lands in the run log — the
	// machine twin (JSONL) and human log carry the same field.
	log.Info("run completed", "command", rest[0], "duration_ms", time.Since(started).Milliseconds())
}

// newRunID returns the run identifier: local time as DDMMYYYY_HHMMSS (easy
// to eyeball and sort), with a -N suffix when a same-second run already left
// artifacts (log file or audit folder), so repeat runs never collide.
func newRunID(logDir, auditRoot string, now time.Time) string {
	base := now.Format("02012006_150405")
	runID := base
	for i := 2; ; i++ {
		taken := false
		if logDir != "" {
			if _, err := os.Stat(filepath.Join(logDir, "run-"+runID+".jsonl")); err == nil {
				taken = true
			}
		}
		if !taken && auditRoot != "" {
			if _, err := os.Stat(filepath.Join(auditRoot, runID)); err == nil {
				taken = true
			}
		}
		if !taken {
			return runID
		}
		runID = fmt.Sprintf("%s-%d", base, i)
	}
}

// extractGlobalFlags pulls the run-wide flags (-verbose, -log-dir) out of the
// argument list wherever they appear, leaving the subcommand and its flags in
// rest. `--` passes everything after it through untouched.
func extractGlobalFlags(args []string) (verbose bool, logDir string, rest []string) {
	logDir = defaultLogDir
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name := strings.TrimLeft(a, "-")
		value := ""
		hasValue := false
		if eq := strings.Index(name, "="); eq >= 0 {
			value, name, hasValue = name[eq+1:], name[:eq], true
		}
		switch name {
		case "verbose":
			if hasValue {
				b, err := strconv.ParseBool(value)
				if err != nil {
					rest = append(rest, a)
					continue
				}
				verbose = b
				continue
			}
			verbose = true
			continue
		case "log-dir":
			if hasValue {
				logDir = value
				continue
			}
			if i+1 < len(args) {
				i++
				logDir = args[i]
				continue
			}
		}
		rest = append(rest, a)
	}
	return verbose, logDir, rest
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `tuxgo — Pro*C/Tuxedo to idiomatic Go converter

Usage:
  tuxgo [global flags] <command> [arguments]

Global Flags:
  -verbose             enable debug-level console logging (file log always captures debug)
  -log-dir <dir>       structured JSONL log directory (default conversion_logs/logs)

Available Commands:
  analyze      Analyze Pro*C/Tuxedo complexity (+1/+5/+10/+20 rubric) and export CSV
  extract      Extract the deterministic IR (query units + QueryType marking, condition
               inventory, FML ops, external fns) as JSON
  plan         Generate the deterministic decomposition plan from the IR + the
               user's endpoint mapping (plan.json/plan.md in the ledger dir)
  convert      Convert a .pc/.pcf file or directory into the target Go service.
               With a mapping present (-mapping, convert.mapping, or the
               mappings/ convention) it converts; without one it first scans
               the target and writes editable mapping drafts to mappings/
               (AI-named when the model is reachable, deterministic names
               otherwise) and stops — review, then re-run the same command
               to convert. A directory with several Tuxedo entries converts
               one worker per service in parallel
  batchpy      Convert Pro*C batch programs into Python service modules
               (SQL constants + repository/DAL + service with process_daily_batch;
               pychk syntax gate + SQL fidelity + retention report)
  gentest      Generate db/controller/handler Go tests for a converted service
               tree (post-conversion: file | layer dir | service dir | services
               root; -check-only reports the function test gap)
  flow         Flow-IR accuracy report for a .pc/.pcf file or directory:
               per-function statement coverage, idiom hints, and (with -go)
               the deterministic Go transpilation draft
  discover     Endpoint scan-then-tag: find the API candidates (conditions
               enclosing Fget32 reads + non-error Fadd32 writes) and write a
               mapping draft per entry to mappings/ (default; -out overrides,
               -stdout prints). The same engine convert runs when no mapping
               exists; convert is the day-to-day entry, discover stays for
               drafting ahead of time
  version      Print version information

`)
}

// reorderArgs separates flag tokens (with their values) from positional
// arguments so flags may appear before or after the target path — the stdlib
// flag package otherwise stops parsing at the first positional.
func reorderArgs(args []string) (flagArgs, positional []string) {
	valueFlags := map[string]bool{"csv": true, "weights": true, "out": true, "config": true, "mapping": true, "ledger": true, "base": true, "shape": true, "dml-loop": true, "layers": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return flagArgs, positional
}

func runAnalyze(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	csvPath := fs.String("csv", "", "Path to export CSV report (defaults to stdout)")
	weightsPath := fs.String("weights", "", "Path to a previously generated analysis CSV whose external_fns weights override the defaults (edit the CSV and re-run to re-score)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	opts := analyzer.DefaultOptions()
	if *weightsPath != "" {
		loaded, err := analyzer.LoadOptionsCSV(*weightsPath)
		if err != nil {
			return fmt.Errorf("failed loading weights CSV %s: %w", *weightsPath, err)
		}
		opts = loaded
		telemetry.Log(ctx).Info("loaded scoring overrides",
			"path", *weightsPath,
			"marks", fmt.Sprintf("query=%d simple=%d complex=%d tpcall=%d",
				opts.Marks.Query, opts.Marks.Simple, opts.Marks.Complex, opts.Marks.TpCall),
			"fn_overrides", len(opts.FnWeights))
	}

	remaining := positional
	if len(remaining) == 0 {
		return fmt.Errorf("must provide a file or directory to analyze")
	}

	targetPath := remaining[0]
	telemetry.Log(ctx).Info("analyze invoked", "target", targetPath, "csv", *csvPath)

	fi, err := os.Stat(targetPath)
	if err != nil {
		return fmt.Errorf("cannot access target path %s: %w", targetPath, err)
	}

	var reports []*analyzer.Report
	if fi.IsDir() {
		reps, err := analyzer.AnalyzeDir(targetPath, opts)
		if err != nil {
			return err
		}
		reports = reps
	} else {
		rep, err := analyzer.AnalyzeFile(targetPath, opts)
		if err != nil {
			return err
		}
		reports = []*analyzer.Report{rep}
	}

	if len(reports) == 0 {
		fmt.Fprintf(os.Stderr, "no .pc or .pcf files found in %s\n", targetPath)
		return nil
	}

	archiveTriageCSV(ctx, reports, opts.Marks)
	telemetry.Log(ctx).Info("analysis complete", "files", len(reports))

	var out io.Writer = os.Stdout
	if *csvPath != "" {
		f, err := os.Create(*csvPath)
		if err != nil {
			return fmt.Errorf("failed creating CSV output file %s: %w", *csvPath, err)
		}
		defer f.Close()
		out = f
		telemetry.Log(ctx).Info("writing analysis report to CSV", "path", *csvPath, "records", len(reports))
	}

	if err := analyzer.WriteCSV(out, reports, opts.Marks); err != nil {
		return fmt.Errorf("failed writing CSV report: %w", err)
	}

	if *csvPath != "" {
		fmt.Printf("Wrote analysis report for %d files to %s\n", len(reports), *csvPath)
	}
	if *weightsPath == "" {
		target := *csvPath
		if target == "" {
			target = "<file>.csv (save with -csv)"
		}
		fmt.Fprintf(os.Stderr, "Tip: to re-score, edit the '# tuxgo marks' line or the external_fns weights in %s, then re-run with -weights %s\n", target, target)
	}
	return nil
}

// archiveTriageCSV persists the run's report to conversion_logs/audit/<run-id>/
// via the audit Recorder (§4.7). Best-effort: archival failures are logged,
// never fatal.
func archiveTriageCSV(ctx context.Context, reports []*analyzer.Report, marks analyzer.Marks) {
	log := telemetry.Log(ctx)
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		log.Warn("audit archive unavailable", "error", err)
		return
	}
	path, err := rec.Write("triage_report.csv", func(w io.Writer) error {
		return analyzer.WriteCSV(w, reports, marks)
	})
	if err != nil {
		log.Warn("audit archive write failed", "error", err)
		return
	}
	log.Info("analysis archived", "path", path)
}
