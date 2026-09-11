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

	// A5.1: table-driven dispatch — one loop owns per-command error/exit;
	// the run-completed duration line below covers every command.
	commands := map[string]func(context.Context, []string) error{
		"analyze":  runAnalyze,
		"extract":  runExtract,
		"plan":     runPlan,
		"convert":  runConvert,
		"batchpy":  runBatchpy,
		"gentest":  runGentest,
		"flow":     runFlow,
		"discover": runDiscover,
	}
	run, ok := commands[rest[0]]
	switch {
	case ok:
		if err := run(ctx, rest[1:]); err != nil {
			log.Error(rest[0]+" failed", "error", err)
			os.Exit(1)
		}
	case rest[0] == "version":
		fmt.Printf("tuxgo version %s\n", version)
	case rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help":
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
               (selectors: analyze folder/file.pc, analyze folder file.pc, or
               analyze folder list.txt — a newline-separated file list)
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
// flag package otherwise stops parsing at the first positional. Value flags
// are derived from the subcommand FlagSets (A5.1): a flag is value-taking
// when some command registers it as a String/Duration/etc. (Visit reports
// it) and not a Bool — the hand-maintained map is gone.
func reorderArgs(args []string) (flagArgs, positional []string) {
	valueFlags := deriveValueFlags()
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

// deriveValueFlags registers every subcommand's flags into throwaway
// FlagSets and reports the names whose values are not boolean. A new value
// flag added to any subcommand is picked up automatically.
func deriveValueFlags() map[string]bool {
	valueFlags := map[string]bool{}
	register := func(fs *flag.FlagSet) {
		fs.VisitAll(func(f *flag.Flag) {
			if f.DefValue != "false" && f.DefValue != "true" {
				valueFlags[f.Name] = true
			}
		})
	}
	registerFlags := map[string]func(*flag.FlagSet){
		"analyze": func(fs *flag.FlagSet) {
			fs.String("csv", "", "")
			fs.String("weights", "", "")
			fs.String("pattern", "", "")
		},
		"extract": func(fs *flag.FlagSet) { fs.String("out", "", ""); fs.String("config", "", "") },
		"plan": func(fs *flag.FlagSet) {
			fs.String("mapping", "", "")
			fs.String("config", "", "")
			fs.String("ledger", "", "")
			fs.Bool("fragment", false, "")
		},
		"convert": func(fs *flag.FlagSet) {
			fs.String("mapping", "", "")
			fs.String("config", "", "")
			fs.String("base", "", "")
			fs.Bool("no-llm", false, "")
			fs.Bool("fragment", false, "")
		},
		"batchpy": func(fs *flag.FlagSet) {
			fs.String("out", "", "")
			fs.String("config", "", "")
			fs.Bool("no-llm", false, "")
			fs.String("shape", "", "")
			fs.String("dml-loop", "", "")
		},
		"gentest": func(fs *flag.FlagSet) {
			fs.String("layers", "", "")
			fs.Bool("check-only", false, "")
			fs.String("base", "", "")
			fs.Bool("no-llm", false, "")
			fs.String("config", "", "")
		},
		"flow": func(fs *flag.FlagSet) {
			fs.String("out", "", "")
			fs.Bool("go", false, "")
			fs.String("config", "", "")
		},
		"discover": func(fs *flag.FlagSet) {
			fs.String("out", "", "")
			fs.Bool("stdout", false, "")
			fs.Bool("no-llm", false, "")
			fs.String("config", "", "")
		},
	}
	for _, fn := range registerFlags {
		fs := flag.NewFlagSet("derive", flag.ContinueOnError)
		fn(fs)
		register(fs)
	}
	return valueFlags
}

// analysisTargets resolves the analyze targets from the positional
// arguments (user directives, 2026-09-10): an existing path passes through
// untouched (file → one report, directory → its tree); `analyze
// folder/file.pc` where the file lies deeper in the folder's tree walks it
// for a case-insensitive basename match; `analyze folder file.pc` (two
// positionals) is the explicit folder+name form. A selector may omit the
// extension (`SVC_DEMO_LIST` matches SVC_DEMO_LIST.pc). A `.txt` second
// positional (or a lone `.txt`) is a newline-separated file list: every
// non-blank, non-`#` line resolves inside the folder tree with the same
// one-match rule, duplicates collapse, and any miss errors naming the
// list. Exactly one match wins everywhere; zero or several are loud
// errors — never a silent pick.
func analysisTargets(positional []string) ([]string, error) {
	if len(positional) == 2 {
		if isFileList(positional[1]) {
			listPath := positional[1]
			if _, err := os.Stat(listPath); err != nil {
				listPath = filepath.Join(positional[0], listPath)
			}
			return resolveFileList(positional[0], listPath)
		}
		found, err := findAnalysisFile(positional[0], positional[1])
		if err != nil {
			return nil, err
		}
		return []string{found}, nil
	}
	p := positional[0]
	if _, err := os.Stat(p); err == nil {
		if isFileList(p) {
			// A lone list evaluates against its own folder.
			return resolveFileList(filepath.Dir(p), p)
		}
		return []string{p}, nil
	}
	dir, name := filepath.Split(p)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" {
		return []string{p}, nil // no folder to search — os.Stat reports it
	}
	found, err := findAnalysisFile(dir, name)
	if err != nil {
		return nil, err
	}
	return []string{found}, nil
}

// isFileList reports whether the argument names a newline-separated file
// list (.txt) — the analyze name-list selector form.
func isFileList(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".txt")
}

// resolveFileList reads the newline-separated name list and resolves every
// name inside dir's tree with the single-file matcher (case-insensitive,
// extension optional, one-match-wins). Blank lines and #-comments are
// skipped; duplicate resolutions collapse to one report.
func resolveFileList(dir, listPath string) ([]string, error) {
	data, err := os.ReadFile(listPath)
	if err != nil {
		return nil, fmt.Errorf("analyze: reading file list %s: %w", listPath, err)
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i] // inline comments ride the # convention
		}
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		found, err := findAnalysisFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("analyze: file list %s: %w", listPath, err)
		}
		if !seen[found] {
			seen[found] = true
			out = append(out, found)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("analyze: file list %s resolves to no files", listPath)
	}
	return out, nil
}

// findAnalysisFile walks dir recursively for the one .pc/.pcf file whose
// base name matches the selector (extension optional, case-insensitive).
func findAnalysisFile(dir, name string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("cannot access folder %s: %w", dir, err)
	}
	want := strings.ToLower(strings.TrimSpace(name))
	wantStem := strings.ToLower(strings.TrimSuffix(want, filepath.Ext(want)))
	var matches []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".pc" && ext != ".pcf" {
			return nil
		}
		base := strings.ToLower(filepath.Base(path))
		if base == want || strings.TrimSuffix(base, ext) == wantStem {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("searching %s: %w", dir, err)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no .pc/.pcf file matching %q under %s", name, dir)
	default:
		return "", fmt.Errorf("%q is ambiguous under %s — %d files match, name one exactly: %s",
			name, dir, len(matches), strings.Join(matches, ", "))
	}
}

func runAnalyze(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	csvPath := fs.String("csv", "", "Path to export CSV report (defaults to stdout)")
	weightsPath := fs.String("weights", "", "Path to a previously generated analysis CSV whose external_fns weights override the defaults (edit the CSV and re-run to re-score)")
	pattern := fs.String("pattern", "", "Directory mode only: analyze only the .pc/.pcf files whose base name contains this substring (case-insensitive), e.g. -pattern mf_")

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
			"marks", fmt.Sprintf("query=%d simple=%d complex=%d tpcall=%d branch=%d tier_high=%d tier_medium=%d",
				opts.Marks.Query, opts.Marks.Simple, opts.Marks.Complex, opts.Marks.TpCall,
				opts.Marks.Branch, opts.Marks.TierHigh, opts.Marks.TierMedium),
			"fn_overrides", len(opts.FnWeights))
	}

	remaining := positional
	if len(remaining) == 0 {
		return fmt.Errorf("must provide a file or directory to analyze")
	}

	paths, err := analysisTargets(remaining)
	if err != nil {
		return err
	}
	telemetry.Log(ctx).Info("analyze invoked", "targets", len(paths), "first", paths[0], "csv", *csvPath)

	dirMode := false
	var reports []*analyzer.Report
	if len(paths) == 1 {
		fi, err := os.Stat(paths[0])
		if err != nil {
			return fmt.Errorf("cannot access target path %s: %w", paths[0], err)
		}
		if !fi.IsDir() {
			if strings.TrimSpace(*pattern) != "" {
				return fmt.Errorf("analyze: -pattern applies to directory targets only (passed %q)", paths[0])
			}
			rep, err := analyzer.AnalyzeFile(paths[0], opts)
			if err != nil {
				return err
			}
			reports = []*analyzer.Report{rep}
		} else {
			dirMode = true
		}
	}

	if dirMode {
		targetPath := paths[0]
		reps, err := analyzer.AnalyzeDir(targetPath, opts)
		if err != nil {
			return err
		}
		if needle := strings.TrimSpace(*pattern); needle != "" {
			kept := make([]*analyzer.Report, 0, len(reps))
			for _, r := range reps {
				if strings.Contains(strings.ToLower(filepath.Base(r.File)), strings.ToLower(needle)) {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				return fmt.Errorf("analyze: no .pc/.pcf file in %s matches -pattern %q", targetPath, needle)
			}
			telemetry.Log(ctx).Info("analyze pattern applied",
				"pattern", needle, "matched", len(kept), "of", len(reps))
			reps = kept
		}
		reports = reps
	} else if len(paths) > 1 {
		if strings.TrimSpace(*pattern) != "" {
			return fmt.Errorf("analyze: -pattern applies to directory targets only (a file list selects its own files)")
		}
		for _, p := range paths {
			rep, err := analyzer.AnalyzeFile(p, opts)
			if err != nil {
				return err
			}
			reports = append(reports, rep)
		}
	}

	if len(reports) == 0 {
		fmt.Fprintf(os.Stderr, "no .pc or .pcf files found in %s\n", strings.Join(paths, ", "))
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
