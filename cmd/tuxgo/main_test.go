package main

import (
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

// TestResolveAnalyzeTarget pins the analyze file-selector seam (user
// directive, 2026-09-10): an existing path passes through;
// `folder/file.pc` finds the file anywhere in the folder tree;
// `folder file.pc` is the explicit two-positional form; the extension is
// optional; zero/ambiguous matches are loud errors.
func TestResolveAnalyzeTarget(t *testing.T) {
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
	got, err := resolveAnalyzeTarget([]string{deep})
	if err != nil || got != deep {
		t.Errorf("existing path = %q, %v; want passthrough", got, err)
	}

	// folder/file.pc where the file lies in subfolders.
	got, err = resolveAnalyzeTarget([]string{filepath.Join(root, "svc", "SVC_DEMO_LIST.pc")})
	if err != nil || got != deep {
		t.Errorf("folder/file selector = %q, %v; want %q", got, err, deep)
	}

	// Two positionals: folder + bare name, extension optional.
	got, err = resolveAnalyzeTarget([]string{root, "SVC_DEMO_LIST.pc"})
	if err != nil || got != deep {
		t.Errorf("folder + name = %q, %v; want %q", got, err, deep)
	}
	got, err = resolveAnalyzeTarget([]string{root, "svc_demo_list"}) // case + extension optional
	if err != nil || got != deep {
		t.Errorf("stem selector = %q, %v; want %q", got, err, deep)
	}

	// Zero and multiple matches are loud errors.
	if _, err := resolveAnalyzeTarget([]string{root, "NOPE"}); err == nil || !strings.Contains(err.Error(), "no .pc/.pcf") {
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
	if _, err := resolveAnalyzeTarget([]string{root, "DUP"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("multi-match err = %v, want ambiguous listing", err)
	}
	// Missing folder, either form.
	if _, err := resolveAnalyzeTarget([]string{filepath.Join(root, "gone", "x.pc")}); err == nil {
		t.Error("missing folder must error")
	}
	if _, err := resolveAnalyzeTarget([]string{root, "gone"}); err == nil {
		t.Error("zero-match selector must error")
	}
}
