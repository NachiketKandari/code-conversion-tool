package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/flow"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/cproc/scanner"
)

const nestedSrc = `void SVC_D(TPSVCINFO *rqst) {
	if (flag == 'A') {
		if (Fget32(ibuf, FML_COMP_CD, 0, (char *)&comp, 0) == -1) {
			Fadd32(ibuf, FML_ERR_MSG, msg, 0);
			tpreturn(TPFAIL, 0L, ibuf, 0L, 0);
		}
		EXEC SQL INSERT INTO T VALUES (:comp);
		Fadd32(obuf, FML_A, (char *)&a, 0);
		if (cnt > 0) {
			if (Fget32(ibuf, FML_B, 0, (char *)&b, 0) == -1) {
				Fadd32(ibuf, FML_ERR_MSG, msg, 0);
				tpreturn(TPFAIL, 0L, ibuf, 0L, 0);
			}
			Fadd32(obuf, FML_B, (char *)&b, 0);
			Fadd32(obuf, FML_C, (char *)&c, 0);
		}
		Fadd32(obuf, FML_D, (char *)&d, 0);
	} else {
		tpreturn(TPSUCCESS, 0L, obuf, 0L, 0);
	}
}
`

func discoverFixture(t *testing.T) (*ir.File, string, *flow.Tree, []flow.Candidate) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "SVC_D.pc")
	if err := os.WriteFile(path, []byte(nestedSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := ir.ExtractFileOpts(path, ir.Options{})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := scanner.ScanBytes([]byte(nestedSrc), path)
	if err != nil {
		t.Fatal(err)
	}
	tree := flow.Build([]byte(nestedSrc), facts, f.Entry, f)
	return f, nestedSrc, tree, flow.Discover(tree, f.Conditions)
}

func TestDiscoverNestedKeys(t *testing.T) {
	f, _, tree, candidates := discoverFixture(t)
	_ = f
	var keys []string
	for _, c := range candidates {
		keys = append(keys, c.Key)
	}
	if len(keys) != 2 || keys[0] != "c1" || keys[1] != "c1.1" {
		t.Fatalf("candidate keys = %v, want [c1 c1.1]", keys)
	}
	if !candidates[1].Redundant {
		t.Errorf("c1.1 must be marked redundant (subset of c1): %+v", candidates[1])
	}
	if candidates[0].Redundant {
		t.Errorf("c1 has no qualifying parent: %+v", candidates[0])
	}
	// Round trip: the reference resolves to the inner branch's span.
	synth, err := flow.ConditionFor(tree, f.Conditions, "c1.1")
	if err != nil {
		t.Fatal(err)
	}
	if synth.Index != 0 {
		t.Errorf("synthesized Index = %d, want 0", synth.Index)
	}
	hasErrFlag := false
	for _, op := range synth.FmlOps {
		if op.Error {
			hasErrFlag = true
		}
	}
	if !hasErrFlag {
		t.Error("synthesized condition carries no Error-flagged ops (the inner guard's err add)")
	}
}

func TestPlanBuildWithConditionRef(t *testing.T) {
	f, src, _, _ := discoverFixture(t)
	m := &Mapping{
		Service: "svcd",
		Module:  "app/pkg/services/svcd",
		Endpoints: []Endpoint{
			{ConditionRef: "c1.1", Name: "Nested", Route: "/nested"},
			{Condition: 2, Name: "Default", Route: "/default"},
		},
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := Build(Options{Main: f, Source: src, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err != nil {
		t.Fatal(err)
	}
	var ctrl, dbUnits int
	names := map[string]bool{}
	for _, u := range p.Units {
		switch u.Kind {
		case KindControllerMethod:
			ctrl++
			names[u.Name] = true
		case KindDBMethod:
			dbUnits++
		}
	}
	if ctrl != 2 || !names["Nested"] || !names["Default"] {
		t.Errorf("controller units = %d %v, want 2 with Nested present", ctrl, names)
	}
	if dbUnits != 0 {
		t.Errorf("db units = %d, want 0 (the INSERT belongs to c1, not the nested c1.1)", dbUnits)
	}
	// The INSERT (q1) belongs only to the unmapped c1 — a visible skip.
	found := false
	for _, s := range p.Skipped {
		if s.QueryID == "q1" {
			found = true
		}
	}
	if !found {
		t.Errorf("q1 not recorded as skipped: %+v", p.Skipped)
	}
}

func TestValidateConditionRefRules(t *testing.T) {
	base := func(e Endpoint) *Mapping {
		return &Mapping{
			Service:   "svcd",
			Module:    "app/pkg/services/svcd",
			Endpoints: []Endpoint{e},
		}
	}
	if err := base(Endpoint{Condition: 1, ConditionRef: "c1", Name: "X", Route: "/x"}).Validate(); err == nil {
		t.Error("both condition and conditionRef must be rejected")
	}
	if err := base(Endpoint{Name: "X", Route: "/x"}).Validate(); err == nil {
		t.Error("neither condition nor conditionRef must be rejected")
	}
	if err := (&Mapping{
		Service: "svcd", Module: "app/pkg/services/svcd",
		Endpoints: []Endpoint{
			{ConditionRef: "c1.1", Name: "X", Route: "/x"},
			{ConditionRef: "c1.1", Name: "Y", Route: "/y"},
		},
	}).Validate(); err == nil {
		t.Error("duplicate conditionRef must be rejected")
	}
	if err := base(Endpoint{ConditionRef: "c1", Name: "X", Route: "/x"}).Validate(); err != nil {
		t.Errorf("valid ref rejected: %v", err)
	}
}

func TestPlanBuildUnknownRefFails(t *testing.T) {
	f, src, _, _ := discoverFixture(t)
	m := &Mapping{
		Service: "svcd", Module: "app/pkg/services/svcd",
		Endpoints: []Endpoint{{ConditionRef: "c9", Name: "X", Route: "/x"}},
	}
	_, err := Build(Options{Main: f, Source: src, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err == nil || !strings.Contains(err.Error(), "inventory has") {
		t.Errorf("unknown ref must fail loudly, got: %v", err)
	}
}
