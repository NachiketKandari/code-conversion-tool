package plan

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
)

// navMapping maps ALL FOUR conditions — the §4.8.5 improvement over the
// reference (which left 'I' unconverted) — and pins the reference-quality
// DB method names. This mirrors the user's mapping file (F3).
func navMapping() *Mapping {
	return &Mapping{
		Service:    "nav",
		Module:     "mutual-fund-be/pkg/services/nav",
		ReadDBs:    []string{"EBATEST", "MF"},
		RouteGroup: "/nav",
		Endpoints: []Endpoint{
			{Condition: 1, Name: "NavHistory", Route: "/mfnavhistory"},
			{Condition: 2, Name: "SipFreedem", Route: "/mf_sipfreedem_schemes"},
			{Condition: 3, Name: "SipInsurance", Route: "/mf_sipinsurance_schemes"},
			{Condition: 4, Name: "NavList", Route: "/mfnavschemelist"},
		},
		DBMethods: map[string]MethodPin{
			"q1":                  {Name: "GetDateDetails"},
			"cur_mf_nav_hist":     {Name: "GetNavHistory"},
			"q3":                  {Name: "GetCount", Params: []string{"matchAccount:string"}},
			"cur_mf_freed":        {Name: "GetSipFreedem"},
			"cur_mf_nav":          {Name: "GetSipInsurance"},
			"cur_mf_nav_list":     {Name: "GetNavDetails"},
			"fn_is_d2u_active:q1": {Name: "IsD2uActive"},
		},
	}
}

func navOptions(t *testing.T) Options {
	t.Helper()
	files, err := ir.ExtractDir("../../testdata/nav")
	if err != nil {
		t.Fatal(err)
	}
	var main *ir.File
	var fns []*ir.File
	for _, f := range files {
		if strings.HasSuffix(f.Path, "SVC_MF_NAV_LIST.pc") {
			main = f
		} else {
			fns = append(fns, f)
		}
	}
	if main == nil {
		t.Fatal("nav fixture IR not extracted")
	}
	src, err := osReadFile(main.Path)
	if err != nil {
		t.Fatal(err)
	}
	return Options{Main: main, Source: src, FnFiles: fns, Mapping: navMapping(), Budget: budget.New(12000, 4000, 4)}
}

// TestPlanGateNavGolden is the Phase 5 planning gate: the nav fixture +
// all-4-endpoint mapping → 7 db units (6 dedup-collapsed + the external-fn
// unit), 4 controller/handler units, zero orphans, byte-identical on re-run.
func TestPlanGateNavGolden(t *testing.T) {
	p, err := Build(navOptions(t))
	if err != nil {
		t.Fatal(err)
	}

	var db, ctrl, handler int
	var dbNames []string
	for _, u := range p.Units {
		switch u.Kind {
		case KindDBMethod:
			db++
			dbNames = append(dbNames, u.Name)
		case KindControllerMethod:
			ctrl++
		case KindHandlerMethod:
			handler++
		}
	}
	if db != 7 {
		t.Errorf("db units = %d, want 7 (6 main unique + fn_is_d2u_active:q1): %v", db, dbNames)
	}
	if ctrl != 4 || handler != 4 {
		t.Errorf("controller/handler units = %d/%d, want 4/4", ctrl, handler)
	}

	// q5→q3 dedup: exactly one GetCount unit, referenced by both F and I.
	getCount := 0
	for _, u := range p.Units {
		if u.Name == "GetCount" {
			getCount++
		}
	}
	if getCount != 1 {
		t.Errorf("GetCount units = %d, want 1 (q5→q3 collapsed)", getCount)
	}

	// External fn unit: namespaced query, real source file.
	var fnUnit *Unit
	for i := range p.Units {
		if p.Units[i].Name == "IsD2uActive" {
			fnUnit = &p.Units[i]
		}
	}
	if fnUnit == nil {
		t.Fatal("external-fn db unit missing")
	}
	if !strings.HasSuffix(fnUnit.SourceFile, "fn_d2u_mf.pc") || !strings.Contains(fnUnit.QueryIDs[0], "fn_is_d2u_active") {
		t.Errorf("fn unit = %+v", fnUnit)
	}

	// chk_* dropped, unresolved fns are visible blockers, zero orphans.
	if len(p.Dropped) != 1 || !strings.Contains(p.Dropped[0], "chk_sssn") {
		t.Errorf("dropped = %v, want chk_sssn", p.Dropped)
	}
	if len(p.Blockers) != 1 || p.Blockers[0].Fn != "fn_long_to_int" || len(p.Blockers[0].Endpoints) == 0 {
		t.Errorf("blockers = %+v, want fn_long_to_int with affected endpoints", p.Blockers)
	}
	if len(p.Orphans) != 0 {
		t.Errorf("orphans = %v, want none", p.Orphans)
	}
	if len(p.Skipped) != 0 {
		t.Errorf("skipped = %v, want none (all conditions mapped)", p.Skipped)
	}

	// Determinism: identical inputs → byte-identical JSON.
	var b1, b2 bytes.Buffer
	if err := WriteJSON(&b1, p); err != nil {
		t.Fatal(err)
	}
	p2, err := Build(navOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&b2, p2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Error("plan is not deterministic — identical inputs produced different JSON")
	}

	// The human twin renders without error and names the endpoints.
	var md bytes.Buffer
	if err := WriteMD(&md, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "NavHistory") || !strings.Contains(md.String(), "SipInsurance") {
		t.Error("plan.md missing mapped endpoints")
	}
}

// TestPlanSkipsUnmappedBranchQueries: with the reference 3-endpoint mapping
// the 'I'-branch query is a recorded skip, not an orphan.
func TestPlanSkipsUnmappedBranchQueries(t *testing.T) {
	opts := navOptions(t)
	m := navMapping()
	// Drop the 'I' endpoint (condition 3) — the reference mapping kept 'I'
	// unconverted while H/F/default became APIs.
	var eps []Endpoint
	for _, e := range m.Endpoints {
		if e.Condition != 3 {
			eps = append(eps, e)
		}
	}
	m.Endpoints = eps
	delete(m.DBMethods, "cur_mf_nav")
	opts.Mapping = m
	p, err := Build(opts)
	if err != nil {
		t.Fatal(err)
	}
	skipped := map[string]string{}
	for _, s := range p.Skipped {
		skipped[s.QueryID] = s.Reason
	}
	if _, ok := skipped["cur_mf_nav"]; !ok {
		t.Errorf("cur_mf_nav must be a recorded skip, got %v", p.Skipped)
	}
	if len(p.Orphans) != 0 {
		t.Errorf("orphans = %v", p.Orphans)
	}
	for _, u := range p.Units {
		if u.Name == "GetSipInsurance" {
			t.Error("unmapped endpoint must not produce units")
		}
	}
}

func TestMappingValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Mapping)
	}{
		{"no service", func(m *Mapping) { m.Service = "" }},
		{"no endpoints", func(m *Mapping) { m.Endpoints = nil }},
		{"duplicate condition", func(m *Mapping) { m.Endpoints[1].Condition = m.Endpoints[0].Condition }},
		{"duplicate name", func(m *Mapping) { m.Endpoints[1].Name = m.Endpoints[0].Name }},
		{"bad route", func(m *Mapping) { m.Endpoints[0].Route = "mfnavhistory" }},
		{"bad ident", func(m *Mapping) { m.Endpoints[0].Name = "nav-history" }},
		{"bad pin ident", func(m *Mapping) { m.DBMethods["q1"] = MethodPin{Name: "get-date"} }},
	}
	for _, tc := range cases {
		m := navMapping()
		tc.mut(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
}

func osReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
