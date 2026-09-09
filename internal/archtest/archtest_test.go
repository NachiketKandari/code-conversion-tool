// Package archtest is the machine check for the layering law (PRD
// 2026-09-10-architecture-first §3, A6.3): dependency direction only ever
// points downward. Forbidden arrows live in the tests below; every rule
// runs over `go list` output (stdlib-only, R4) and fails the test run when
// violated. A new violation = the PRD's layering law was broken — fix the
// import or amend the law deliberately.
package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

const mod = "github.com/Public/convert-tux-to-go"

// deps maps each internal package to its internal dependencies (go list).
type deps map[string][]string

func loadDeps(t *testing.T) deps {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{join .Imports \",\"}}", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	d := deps{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		var internal []string
		if parts[1] != "" {
			for _, imp := range strings.Split(parts[1], ",") {
				if strings.HasPrefix(imp, mod+"/internal/") {
					internal = append(internal, imp)
				}
			}
		}
		d[parts[0]] = internal
	}
	return d
}

// internalPkg extracts "plan" from the module's internal import path.
func internalPkg(importPath string) string {
	rel, ok := strings.CutPrefix(importPath, mod+"/internal/")
	if !ok {
		return ""
	}
	if i := strings.Index(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return rel
}

// TestNoPackageImportsCmd is forbidden arrow 1: nothing imports cmd/…
// (the runIndexed sin — shared seams live under internal/).
func TestNoPackageImportsCmd(t *testing.T) {
	for pkg, imps := range loadDeps(t) {
		for _, imp := range imps {
			if strings.Contains(imp, "/cmd/") {
				t.Errorf("%s imports %s — cmd is pure wiring, imported by nothing", pkg, imp)
			}
		}
	}
}

// TestGenPlanNeverLLM is forbidden arrow 2: plan and gen never import llm —
// the determinism contract ("generation is zero-LLM") as an import fact.
func TestGenPlanNeverLLM(t *testing.T) {
	d := loadDeps(t)
	for _, pkg := range []string{"plan", "gen"} {
		for _, imp := range d[mod+"/internal/"+pkg] {
			if imp == mod+"/internal/llm" {
				t.Errorf("%s imports llm — generation is zero-LLM by contract", pkg)
			}
		}
	}
}

// TestCommonIsLeaf is the common package law (AD1): internal/common imports
// no internal package — it is the leaf-of-leaves so the frozen parse stack
// can always depend on it.
func TestCommonIsLeaf(t *testing.T) {
	for _, imp := range loadDeps(t)[mod+"/internal/common"] {
		t.Errorf("internal/common imports %s — the leaf law forbids internal imports", imp)
	}
}

// TestProfilesLiveAboveSharedCore is the kind-separation rule (A8.2): the
// Go-service and Python-batch drivers never import each other's stacks —
// profile drivers stay thin per kind over the shared core. A violation
// means Go-shaped facts leaked into the batch stack (or reverse).
func TestProfilesLiveAboveSharedCore(t *testing.T) {
	d := loadDeps(t)
	goOnly := map[string]bool{
		"plan": true, "gen": true, "convert": true, "testgen": true, "testscan": true,
	}
	batchOnly := map[string]bool{
		"batchflow": true, "pyplan": true, "pygen": true, "pychk": true,
	}
	for pkg, imps := range d {
		rel, ok := strings.CutPrefix(pkg, mod+"/internal/")
		if !ok {
			continue
		}
		base := rel
		if i := strings.Index(rel, "/"); i >= 0 {
			base = rel[:i]
		}
		for _, imp := range imps {
			impBase := internalPkg(imp)
			if goOnly[base] && batchOnly[impBase] {
				t.Errorf("%s imports %s — Go-kind and batch-kind drivers never mix", base, impBase)
			}
			if batchOnly[base] && goOnly[impBase] {
				t.Errorf("%s imports %s — batch-kind and Go-kind drivers never mix", base, impBase)
			}
		}
	}
}
func TestParseStackNeverLooksUp(t *testing.T) {
	d := loadDeps(t)
	forbidden := map[string]bool{
		"gen": true, "convert": true, "testgen": true, "testscan": true,
		"pyplan": true, "pygen": true, "pychk": true,
	}
	for pkg, imps := range d {
		rel, ok := strings.CutPrefix(pkg, mod+"/internal/cproc/")
		if !ok {
			continue
		}
		base := rel
		if i := strings.Index(rel, "/"); i >= 0 {
			base = rel[:i]
		}
		for _, imp := range imps {
			impRel, ok := strings.CutPrefix(imp, mod+"/internal/")
			if !ok {
				continue
			}
			impBase := impRel
			if i := strings.Index(impRel, "/"); i >= 0 {
				impBase = impRel[:i]
			}
			if base != impRel && forbidden[impBase] {
				t.Errorf("cproc/%s imports %s — the parse stack never looks up", base, impRel)
			}
		}
	}
}
