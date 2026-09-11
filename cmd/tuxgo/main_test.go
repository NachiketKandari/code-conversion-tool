package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Public/convert-tux-to-go/internal/config"
)

func TestNewRunIDFormat(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 5, 0, time.Local)
	got := newRunID(t.TempDir(), t.TempDir(), now)
	// DDMMYYYY_HHMMSS: 06092026_120005
	if !regexp.MustCompile(`^\d{8}_\d{6}$`).MatchString(got) {
		t.Errorf("run id %q does not match DDMMYYYY_HHMMSS", got)
	}
	if want := "06092026_120005"; got != want {
		t.Errorf("run id = %q, want %q", got, want)
	}
}

func TestNewRunIDCollisionSuffix(t *testing.T) {
	logDir := t.TempDir()
	auditDir := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 5, 0, time.Local)

	// A prior run in the same second left a log file.
	if err := os.WriteFile(filepath.Join(logDir, "run-06092026_120005.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := newRunID(logDir, auditDir, now); got != "06092026_120005-2" {
		t.Errorf("expected -2 suffix on collision, got %q", got)
	}

	// An audit folder alone also collides.
	if err := os.Mkdir(filepath.Join(auditDir, "06092026_120005-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := newRunID(logDir, auditDir, now); got != "06092026_120005-3" {
		t.Errorf("expected -3 suffix on second collision, got %q", got)
	}
}

func TestReorderArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		flagArgs   []string
		positional []string
	}{
		{
			name:       "flags after path",
			args:       []string{"testdata/nav", "-csv", "out.csv", "-verbose"},
			flagArgs:   []string{"-csv", "out.csv", "-verbose"},
			positional: []string{"testdata/nav"},
		},
		{
			name:       "flags before path",
			args:       []string{"-csv", "out.csv", "testdata/nav"},
			flagArgs:   []string{"-csv", "out.csv"},
			positional: []string{"testdata/nav"},
		},
		{
			name:       "equals syntax",
			args:       []string{"testdata/nav", "-csv=out.csv", "-weights=prev.csv"},
			flagArgs:   []string{"-csv=out.csv", "-weights=prev.csv"},
			positional: []string{"testdata/nav"},
		},
		{
			name:       "double dash terminates flags",
			args:       []string{"-verbose", "--", "-csv", "out.csv"},
			flagArgs:   []string{"-verbose"},
			positional: []string{"-csv", "out.csv"},
		},
		{
			name:       "path only",
			args:       []string{"testdata/nav"},
			flagArgs:   nil,
			positional: []string{"testdata/nav"},
		},
	}
	for _, tc := range cases {
		flags, positional := reorderArgs(tc.args)
		if !reflect.DeepEqual(flags, tc.flagArgs) {
			t.Errorf("%s: flagArgs = %v, want %v", tc.name, flags, tc.flagArgs)
		}
		if !reflect.DeepEqual(positional, tc.positional) {
			t.Errorf("%s: positional = %v, want %v", tc.name, positional, tc.positional)
		}
	}
}

func TestExtractGlobalFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		verbose bool
		logDir  string
		rest    []string
	}{
		{
			name:   "no global flags",
			args:   []string{"analyze", "testdata/nav"},
			logDir: "conversion_logs/logs",
			rest:   []string{"analyze", "testdata/nav"},
		},
		{
			name:    "verbose after subcommand args",
			args:    []string{"analyze", "testdata/nav", "-csv", "out.csv", "--verbose"},
			verbose: true,
			logDir:  "conversion_logs/logs",
			rest:    []string{"analyze", "testdata/nav", "-csv", "out.csv"},
		},
		{
			name:   "log-dir with separate value",
			args:   []string{"-log-dir", "/var/logs", "analyze", "x.pc"},
			logDir: "/var/logs",
			rest:   []string{"analyze", "x.pc"},
		},
		{
			name:   "log-dir equals syntax",
			args:   []string{"analyze", "x.pc", "-log-dir=/var/logs"},
			logDir: "/var/logs",
			rest:   []string{"analyze", "x.pc"},
		},
		{
			name:    "verbose false via equals",
			args:    []string{"analyze", "-verbose=false", "x.pc"},
			verbose: false,
			logDir:  "conversion_logs/logs",
			rest:    []string{"analyze", "x.pc"},
		},
		{
			name:   "double dash passes flags through",
			args:   []string{"analyze", "--", "-verbose"},
			logDir: "conversion_logs/logs",
			rest:   []string{"analyze", "--", "-verbose"},
		},
		{
			name:    "path without dash prefix untouched",
			args:    []string{"analyze", "weird-report.pc", "-verbose"},
			verbose: true,
			logDir:  "conversion_logs/logs",
			rest:    []string{"analyze", "weird-report.pc"},
		},
	}
	for _, tc := range cases {
		verbose, logDir, rest := extractGlobalFlags(tc.args)
		if verbose != tc.verbose {
			t.Errorf("%s: verbose = %v, want %v", tc.name, verbose, tc.verbose)
		}
		if logDir != tc.logDir {
			t.Errorf("%s: logDir = %q, want %q", tc.name, logDir, tc.logDir)
		}
		if !reflect.DeepEqual(rest, tc.rest) {
			t.Errorf("%s: rest = %v, want %v", tc.name, rest, tc.rest)
		}
	}
}

// TestResolveBatchInput pins the yaml-driven input seam for batchpy: the
// CLI positional wins, else batchpy.input supplies the target (mirroring
// convert.input), else the error names both sources.
func TestResolveBatchInput(t *testing.T) {
	cfg := config.Default()
	cfg.Batchpy.Input = "tux/batch"

	got, err := resolveBatchInput([]string{"cli.pc"}, cfg)
	if err != nil || got != "cli.pc" {
		t.Errorf("positional = %q, %v; want cli.pc, nil", got, err)
	}
	got, err = resolveBatchInput(nil, cfg)
	if err != nil || got != "tux/batch" {
		t.Errorf("yaml fallback = %q, %v; want tux/batch, nil", got, err)
	}
	if _, err := resolveBatchInput(nil, config.Default()); err == nil || !strings.Contains(err.Error(), "batchpy.input") {
		t.Errorf("no-target err = %v, want batchpy.input guidance", err)
	}
}

// TestAnalysisTargets pins the analyze selector seam (user directives,
// 2026-09-10): an existing path passes through; `folder/file.pc` finds the
// file anywhere in the folder tree; `folder file.pc` is the explicit
// two-positional form; the extension is optional; zero/ambiguous matches
// are loud errors. The file-list forms are pinned by TestAnalysisFileList.
func TestAnalysisTargets(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "svc", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(sub, "SVC_DEMO_LIST.pc")
	if err := os.WriteFile(deep, []byte("void SVC_DEMO_LIST() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "fn_lib.pcf")
	if err := os.WriteFile(other, []byte("int fn_x() { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Existing path passes through untouched.
	got, err := analysisTargets([]string{deep})
	if err != nil || len(got) != 1 || got[0] != deep {
		t.Errorf("existing path = %q, %v; want passthrough", got, err)
	}

	// folder/file.pc where the file lies in subfolders.
	got, err = analysisTargets([]string{filepath.Join(root, "svc", "SVC_DEMO_LIST.pc")})
	if err != nil || len(got) != 1 || got[0] != deep {
		t.Errorf("folder/file selector = %q, %v; want %q", got, err, deep)
	}

	// Two positionals: folder + bare name, extension optional.
	got, err = analysisTargets([]string{root, "SVC_DEMO_LIST.pc"})
	if err != nil || len(got) != 1 || got[0] != deep {
		t.Errorf("folder + name = %q, %v; want %q", got, err, deep)
	}
	got, err = analysisTargets([]string{root, "svc_demo_list"}) // case + extension optional
	if err != nil || len(got) != 1 || got[0] != deep {
		t.Errorf("stem selector = %q, %v; want %q", got, err, deep)
	}

	// Zero and multiple matches are loud errors.
	if _, err := analysisTargets([]string{root, "NOPE"}); err == nil || !strings.Contains(err.Error(), "no .pc/.pcf") {
		t.Errorf("zero-match err = %v, want guidance", err)
	}
	dup := filepath.Join(root, "dup.pc")
	if err := os.WriteFile(dup, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dup2 := filepath.Join(sub, "dup.pcf")
	if err := os.WriteFile(dup2, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := analysisTargets([]string{root, "DUP"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("multi-match err = %v, want ambiguous listing", err)
	}
	// Missing folder, either form.
	if _, err := analysisTargets([]string{filepath.Join(root, "gone", "x.pc")}); err == nil {
		t.Error("missing folder must error")
	}
	if _, err := analysisTargets([]string{root, "gone"}); err == nil {
		t.Error("zero-match selector must error")
	}
}

// TestAnalyzePatternPins the -pattern seam end-to-end (user directive,
// 2026-09-10): directory mode keeps only the reports whose base name
// carries the case-insensitive substring; zero matches error loudly; a
// file target rejects the flag instead of silently ignoring it.
func TestAnalyzePattern(t *testing.T) {
	dirPath := filepath.Join("..", "..", "testdata", "nav")
	if _, err := os.Stat(dirPath); err != nil {
		t.Skip("testdata fixtures unavailable (fresh clone)")
	}
	csv := filepath.Join(t.TempDir(), "pattern.csv")
	if err := runAnalyze(context.Background(), []string{"-csv", csv, "-pattern", "svc_demo", dirPath}); err != nil {
		t.Fatalf("runAnalyze(pattern) failed: %v", err)
	}
	data, err := os.ReadFile(csv)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, "SVC_DEMO_LIST.pc") {
		t.Errorf("pattern run missing the matching file:\n%s", out)
	}
	if strings.Contains(out, "fn_demo_lib.pc") {
		t.Errorf("non-matching file leaked through -pattern:\n%s", out)
	}

	// Zero matches are a loud error, never an empty report.
	err = runAnalyze(context.Background(), []string{"-pattern", "zzz_nope", dirPath})
	if err == nil || !strings.Contains(err.Error(), "matches -pattern") {
		t.Errorf("zero-match err = %v, want pattern guidance", err)
	}

	// A file target rejects the flag loudly.
	if err := runAnalyze(context.Background(), []string{"-pattern", "x", filepath.Join(dirPath, "SVC_DEMO_LIST.pc")}); err == nil || !strings.Contains(err.Error(), "directory targets only") {
		t.Errorf("file-target pattern err = %v, want directory-targets-only", err)
	}
}

// TestAnalysisFileList pins the .txt file-list selector (user directive,
// 2026-09-10): a newline-separated name list evaluates exactly those files
// inside the folder tree — case-insensitive, extension optional, `#`
// comments and blanks skipped, duplicates collapsed, every miss a loud
// error naming the list. `folder list.txt` and a lone `list.txt` (its own
// folder) both work.
func TestAnalysisFileList(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "svc")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(sub, "SVC_DEMO_LIST.pc")
	if err := os.WriteFile(deep, []byte("void SVC_DEMO_LIST() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(root, "fn_lib.pcf")
	if err := os.WriteFile(lib, []byte("int fn_x() { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	listPath := filepath.Join(root, "files.txt")
	list := "# triage selection\n\nsvc_demo_list\nfn_lib.PCF\nsvc_demo_list   # duplicate collapses\n"
	if err := os.WriteFile(listPath, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}

	// folder + list: both files resolve in list order.
	got, err := analysisTargets([]string{root, "files.txt"})
	if err != nil || len(got) != 2 || got[0] != deep || got[1] != lib {
		t.Errorf("file-list resolution = %v, %v; want [%s %s]", got, err, deep, lib)
	}

	// A lone list evaluates against its own folder.
	got, err = analysisTargets([]string{listPath})
	if err != nil || len(got) != 2 || got[0] != deep {
		t.Errorf("lone list = %v, %v; want both files", got, err)
	}

	// A missing name errors naming the list.
	bad := filepath.Join(root, "bad.txt")
	if err := os.WriteFile(bad, []byte("svc_demo_list\nNOPE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := analysisTargets([]string{root, "bad.txt"}); err == nil || !strings.Contains(err.Error(), "bad.txt") {
		t.Errorf("list miss err = %v, want the list named", err)
	}

	// An empty (comment-only) list is a loud error.
	empty := filepath.Join(root, "empty.txt")
	if err := os.WriteFile(empty, []byte("# nothing here\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := analysisTargets([]string{root, "empty.txt"}); err == nil || !strings.Contains(err.Error(), "resolves to no files") {
		t.Errorf("empty-list err = %v, want guidance", err)
	}

	// End-to-end through runAnalyze: the CSV carries exactly the listed files.
	csv := filepath.Join(t.TempDir(), "list.csv")
	if err := runAnalyze(context.Background(), []string{"-csv", csv, root, listPath}); err != nil {
		t.Fatalf("runAnalyze(file list) failed: %v", err)
	}
	data, err := os.ReadFile(csv)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	rows := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, root) {
			rows++
		}
	}
	if rows != 2 {
		t.Errorf("file-list CSV rows = %d, want exactly the 2 listed files:\n%s", rows, out)
	}
	if !strings.Contains(out, "SVC_DEMO_LIST.pc") || !strings.Contains(out, "fn_lib.pcf") {
		t.Errorf("file-list CSV missing listed files:\n%s", out)
	}
}
