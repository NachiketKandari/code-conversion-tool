package validate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyntaxTier(t *testing.T) {
	v := New(Options{})

	okFile := filepath.Join(t.TempDir(), "good.go")
	writeFile(t, okFile, "package db\n\nfunc Get() (int, error) { return 1, nil }\n")
	if res := v.Syntax(okFile); !res.OK {
		t.Errorf("clean file failed: %+v %v", res, res.Errors)
	}

	badFile := filepath.Join(t.TempDir(), "bad.go")
	writeFile(t, badFile, "package db\n\nfunc Get( { return nil }\n")
	res := v.Syntax(badFile)
	if res.OK {
		t.Error("syntax error must fail Tier A")
	}
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0], "bad.go") {
		t.Errorf("errors not trimmed to file lines: %v", res.Errors)
	}

	unformatted := filepath.Join(t.TempDir(), "fmt.go")
	writeFile(t, unformatted, "package db\n\nfunc  Get( ) (int,error) {\nreturn 1,nil}\n")
	if res := v.Syntax(unformatted); res.OK {
		t.Error("gofmt-dirty file must fail Tier A")
	}
}

func TestResolveModuleRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/svc\n\ngo 1.26\n")
	nested := filepath.Join(root, "pkg", "services", "nav", "db")
	writeFile(t, filepath.Join(nested, "x.go"), "package db\n")

	got, err := ResolveModuleRoot(filepath.Join(nested, "x.go"))
	if err != nil || got != root {
		t.Errorf("root = %q err = %v, want %q", got, err, root)
	}
	if _, err := ResolveModuleRoot(filepath.Join(t.TempDir(), "orphan.go")); err == nil {
		t.Error("anchor without go.mod must error")
	}
}

// tempModule builds a minimal dependency-free module for Tier-B tests.
func tempModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/svc\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"ok\") }\n")
	writeFile(t, filepath.Join(root, "pkg", "svc", "nav.go"), "package svc\n\nconst Name = \"nav\"\n")
	return root
}

func TestCompileTierGreen(t *testing.T) {
	root := tempModule(t)
	v := New(Options{MainGo: filepath.Join(root, "pkg", "svc", "nav.go"), Compile: "auto"})
	enabled, degrade := v.TierBEnabled()
	if !enabled || degrade != "" {
		t.Fatalf("tier B should run: enabled=%v degrade=%q", enabled, degrade)
	}
	if res := v.CompileAll(context.Background()); !res.OK {
		t.Errorf("clean module failed: %+v %v", res, res.Errors)
	}
}

func TestCompileTierFailureTrimsErrors(t *testing.T) {
	root := tempModule(t)
	writeFile(t, filepath.Join(root, "pkg", "svc", "broken.go"), "package svc\n\nfunc Broken( { }\n")
	v := New(Options{MainGo: filepath.Join(root, "main.go"), Compile: "auto"})
	res := v.CompileAll(context.Background())
	if res.OK {
		t.Fatal("broken module must fail")
	}
	joined := strings.Join(res.Errors, "\n")
	if !strings.Contains(joined, "broken.go") {
		t.Errorf("trimmed errors lost the file reference: %q", joined)
	}
	if len(res.Errors) > 45 {
		t.Errorf("errors not bounded: %d lines", len(res.Errors))
	}
}

func TestCompileTierDegrade(t *testing.T) {
	// auto + missing target → degrade with a reason; the run still succeeds
	// ("the environment is never a run failure" — plan-conversion §2).
	v := New(Options{MainGo: "/nowhere/main.go", Compile: "auto"})
	res := v.CompileAll(context.Background())
	if !res.OK || res.DegradeReason == "" {
		t.Errorf("auto degrade must succeed with a reason: %+v", res)
	}

	// always + missing target → error state (config told us it must exist).
	v = New(Options{MainGo: "/nowhere/main.go", Compile: "always"})
	res = v.CompileAll(context.Background())
	if res.OK || res.DegradeReason == "" {
		t.Errorf("always+missing must surface an error state: %+v", res)
	}

	// never → deliberate, successful skip with its reason recorded.
	v = New(Options{MainGo: "", Compile: "never"})
	res = v.CompileAll(context.Background())
	if !res.OK || res.DegradeReason == "" {
		t.Errorf("never must succeed and record its skip reason: %+v", res)
	}
}
